// Package hold clears this host's card before a program that sizes its GPU
// memory at start, a game, keeps every model off it while the program runs,
// and lets the warden decide again once it exits (0024). It talks to the
// daemon on the control socket, which takes no key.
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
	// Poll is how often the card is read while InferMux's VRAM comes back,
	// and how often a hold dials a daemon it lost.
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
		Poll: 250 * time.Millisecond,
	}
}

// Clear pauses the warden by hand, unloads the models, and waits up to
// timeout until InferMux holds no VRAM. A game launch always wins over every
// client (the user, 2026-10-17; 0024, 4): the daemon's unload cuts the
// requests on the GPU and is never refused, so nothing is tried again, and
// whatever did not happen is a warning, never a reason not to launch. err
// says what did not happen. release hands the verdict back to the warden
// once the command exits, and is nil only when the daemon could not be read
// at all. A pause the user had set by hand is left in place.
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
	var missed []error
	// A disabled warden refuses a manual verdict; its models unload all the
	// same.
	if err := c.call(ctx, http.MethodPost, "/warden/manual", map[string]string{"action": "pause"}, nil); err != nil {
		missed = append(missed, fmt.Errorf("not paused: %w", err))
	}
	if err := c.call(ctx, http.MethodPost, "/warden/unload", nil, nil); err != nil {
		return release, errors.Join(append(missed, fmt.Errorf("not unloaded: %w", err))...)
	}
	// The models are stopped; the driver frees their memory a moment after
	// the processes exit.
	deadline := time.Now().Add(timeout)
	for {
		var res struct {
			OurVRAMMB int `json:"our_vram_mb"`
		}
		err := c.call(ctx, http.MethodGet, "/warden/resources", nil, &res)
		if err == nil && res.OurVRAMMB == 0 {
			return release, errors.Join(missed...)
		}
		if err == nil {
			err = fmt.Errorf("InferMux still holds %d MiB of VRAM", res.OurVRAMMB)
		}
		if time.Now().Add(c.Poll).After(deadline) {
			return release, errors.Join(append(missed, fmt.Errorf("the card is not clear after %s: %w", timeout, err))...)
		}
		select {
		case <-ctx.Done():
			return release, errors.Join(append(missed, ctx.Err())...)
		case <-time.After(c.Poll):
		}
	}
}

// Hold keeps every model off the GPU until release is called (0024, 5). The
// daemon counts a hold while one request to /warden/hold stays open, so the
// hold of a process that dies, by SIGKILL too, ends with its connection. A
// daemon that stops takes the hold with it, so Hold dials again every Poll
// and holds again once the daemon is back, until release. Hold returns once
// the daemon has counted the hold, or with why its first try did not, at the
// latest when ctx ends; it goes on trying either way.
func (c *Client) Hold(ctx context.Context) (release func(), err error) {
	holding, stop := context.WithCancel(context.Background())
	first := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		answered := false
		answer := func(err error) {
			if !answered {
				answered = true
				first <- err
			}
		}
		for {
			answer(c.holdOnce(holding, func() { answer(nil) }))
			select {
			case <-holding.Done():
				return
			case <-time.After(c.Poll):
			}
		}
	}()
	release = func() { stop(); <-done }
	select {
	case err = <-first:
	case <-ctx.Done():
		err = ctx.Err()
	}
	return release, err
}

// holdOnce is one hold: held is called once the daemon has counted it, and
// holdOnce returns when the connection ends.
func (c *Client) holdOnce(ctx context.Context, held func()) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://infermux/warden/hold", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return notRunning(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /warden/hold: %s", resp.Status)
	}
	held()
	io.Copy(io.Discard, resp.Body)
	return errors.New("the daemon ended the hold")
}

// notRunning is ErrNotRunning for a socket that is not there or that no
// daemon listens on.
func notRunning(err error) error {
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return ErrNotRunning
	}
	return err
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
		return notRunning(err)
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
