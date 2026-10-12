// Package hold clears this host's card before a program that sizes its GPU
// memory at start, a game, and lets the warden decide again once it exits
// (0024). It talks to the daemon on the control socket, which takes no key.
//
// The warden yields only once it measures the program's load, seconds after
// the program read its VRAM budget. A game that starts beside a loaded model
// places its render targets in system memory and keeps them there: Warframe
// on 2026-10-12 ran at 2 fps with 255 MB of budget, and at 97 fps with 7 GB.
package hold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrNotRunning is a daemon that is not there. Its models are its children,
// so none of them holds the card either.
var ErrNotRunning = errors.New("InferMux is not running")

type Client struct {
	HTTP *http.Client
	// Poll is how often a refused unload is tried again.
	Poll time.Duration
}

// Dial is a client for the daemon's control socket.
func Dial(socket string) *Client {
	return &Client{
		HTTP: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		}},
		Poll: time.Second,
	}
}

// Clear pauses the warden by hand, unloads the models, and waits until
// InferMux holds no VRAM. An unload is refused while an interactive request
// is in flight (0004), so it is tried again until timeout; once one is
// taken, only the probe is read again. On success,
// release hands the verdict back to the warden; on failure the verdict is
// already handed back. A pause the user had set by hand is left in place
// either way.
func (c *Client) Clear(ctx context.Context, timeout time.Duration) (release func(context.Context) error, err error) {
	var state struct {
		Manual *struct {
			Action string `json:"action"`
		} `json:"manual"`
	}
	if err := c.call(ctx, http.MethodGet, "/warden/verdict", nil, &state); err != nil {
		return nil, err
	}
	release = func(ctx context.Context) error {
		if state.Manual != nil && state.Manual.Action == "pause" {
			return nil
		}
		return c.call(ctx, http.MethodPost, "/warden/manual", map[string]string{"action": "auto"}, nil)
	}
	fail := func(err error) (func(context.Context) error, error) {
		if rerr := release(context.WithoutCancel(ctx)); rerr != nil {
			err = fmt.Errorf("%w; and the pause was not handed back: %v", err, rerr)
		}
		return nil, err
	}
	if err := c.call(ctx, http.MethodPost, "/warden/manual", map[string]string{"action": "pause"}, nil); err != nil {
		return fail(err)
	}
	deadline := time.Now().Add(timeout)
	unloaded := false
	for {
		var err error
		if !unloaded {
			err = c.call(ctx, http.MethodPost, "/warden/unload", nil, nil)
			unloaded = err == nil
		}
		if unloaded {
			// The models are stopped; the driver frees their memory a moment
			// after the processes exit.
			var res struct {
				OurVRAMMB int `json:"our_vram_mb"`
			}
			if err = c.call(ctx, http.MethodGet, "/warden/resources", nil, &res); err == nil && res.OurVRAMMB > 0 {
				err = fmt.Errorf("InferMux still holds %d MiB of VRAM", res.OurVRAMMB)
			}
			if err == nil {
				return release, nil
			}
		}
		if time.Now().Add(c.Poll).After(deadline) {
			return fail(fmt.Errorf("the card is not clear after %s: %w", timeout, err))
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(c.Poll):
		}
	}
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://infermux"+path, r)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return ErrNotRunning
		}
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var detail struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(data, &detail) == nil && detail.Detail != "" {
			return fmt.Errorf("%s %s: %s", method, path, detail.Detail)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
