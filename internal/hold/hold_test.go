package hold

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake daemon on a real Unix socket, so Dial is what the tests use.
type daemon struct {
	mu       sync.Mutex
	calls    []string
	manual   string // the manual verdict before Clear: "" or "pause"
	refusals int    // unloads refused before one succeeds; -1 for all of them
	disabled bool   // a disabled warden, which refuses a manual verdict
	ourVRAM  []int  // our_vram_mb, one per /warden/resources, the last repeated
}

func (d *daemon) record(s string) {
	d.mu.Lock()
	d.calls = append(d.calls, s)
	d.mu.Unlock()
}

func (d *daemon) log() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.calls, " ")
}

func (d *daemon) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /warden/verdict", func(rw http.ResponseWriter, r *http.Request) {
		var manual any
		if d.manual != "" {
			manual = map[string]string{"action": d.manual}
		}
		json.NewEncoder(rw).Encode(map[string]any{"manual": manual})
	})
	mux.HandleFunc("POST /warden/manual", func(rw http.ResponseWriter, r *http.Request) {
		var body struct{ Action string }
		json.NewDecoder(r.Body).Decode(&body)
		if d.disabled {
			d.record(body.Action + "-refused")
			rw.WriteHeader(http.StatusConflict)
			rw.Write([]byte(`{"detail":"the warden is disabled"}`))
			return
		}
		d.record(body.Action)
		rw.Write([]byte("{}"))
	})
	mux.HandleFunc("POST /warden/unload", func(rw http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		refuse := d.refusals != 0
		if d.refusals > 0 {
			d.refusals--
		}
		d.mu.Unlock()
		if refuse {
			d.record("unload-refused")
			rw.WriteHeader(http.StatusConflict)
			rw.Write([]byte(`{"detail":"1 interactive request(s) in flight"}`))
			return
		}
		d.record("unload")
		rw.Write([]byte(`{"unloaded":["qwen"]}`))
	})
	mux.HandleFunc("GET /warden/resources", func(rw http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		mb := 0
		if len(d.ourVRAM) > 0 {
			mb = d.ourVRAM[0]
			if len(d.ourVRAM) > 1 {
				d.ourVRAM = d.ourVRAM[1:]
			}
		}
		d.mu.Unlock()
		json.NewEncoder(rw).Encode(map[string]int{"our_vram_mb": mb})
	})
	path := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	return path
}

func client(socket string) *Client {
	c := Dial(socket)
	c.Poll = 10 * time.Millisecond
	return c
}

func TestHold_ClearPausesUnloadsAndHandsBack(t *testing.T) {
	d := &daemon{ourVRAM: []int{5838, 0}}
	release, err := client(d.serve(t)).Clear(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// One unload; the probe is read again until it reads zero.
	if got := d.log(); got != "pause unload" {
		t.Fatalf("before the command: %q", got)
	}
	if err := release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := d.log(); got != "pause unload auto" {
		t.Fatalf("after the command: %q", got)
	}
}

// A game launch always wins (0024, 4): an unload that fails is a warning
// and is not tried again, and the pause stays for the command's run.
func TestHold_AFailedUnloadIsAWarningAndKeepsThePause(t *testing.T) {
	d := &daemon{refusals: -1}
	release, err := client(d.serve(t)).Clear(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "not unloaded") || !strings.Contains(err.Error(), "interactive request(s) in flight") {
		t.Fatalf("the warning is lost: %v", err)
	}
	if release == nil {
		t.Fatal("no release, so the pause would outlive the command")
	}
	if got := d.log(); got != "pause unload-refused" {
		t.Fatalf("before the command: %q", got)
	}
	release(context.Background())
	if got := d.log(); got != "pause unload-refused auto" {
		t.Fatalf("after the command: %q", got)
	}
}

// A card that stays busy past the timeout is a warning, and the pause stays.
func TestHold_ACardThatStaysBusyIsAWarning(t *testing.T) {
	d := &daemon{ourVRAM: []int{5838}}
	release, err := client(d.serve(t)).Clear(context.Background(), 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "still holds 5838 MiB") || release == nil {
		t.Fatalf("%v, release %v", err, release != nil)
	}
	if got := d.log(); got != "pause unload" {
		t.Fatalf("%q", got)
	}
}

// A disabled warden refuses the manual pause; the models unload all the same.
func TestHold_ADisabledWardenStillUnloads(t *testing.T) {
	d := &daemon{disabled: true}
	release, err := client(d.serve(t)).Clear(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "not paused") || release == nil {
		t.Fatalf("%v, release %v", err, release != nil)
	}
	if got := d.log(); got != "pause-refused unload" {
		t.Fatalf("%q", got)
	}
}

func TestHold_AUsersOwnPauseIsLeftInPlace(t *testing.T) {
	d := &daemon{manual: "pause"}
	release, err := client(d.serve(t)).Clear(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	release(context.Background())
	if got := d.log(); strings.Contains(got, "auto") {
		t.Fatalf("the user's pause was cleared: %q", got)
	}
}

func TestHold_NoDaemonIsNotRunning(t *testing.T) {
	_, err := client(filepath.Join(t.TempDir(), "absent.sock")).Clear(context.Background(), time.Second)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("%v", err)
	}
}

// holdDaemon answers /warden/hold on a Unix socket as the daemon does, and
// counts the holds open.
type holdDaemon struct {
	mu    sync.Mutex
	holds int
}

func (d *holdDaemon) open() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.holds
}

func (d *holdDaemon) serve(t *testing.T, socket string) (stop func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /warden/hold", func(rw http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.holds++
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			d.holds--
			d.mu.Unlock()
		}()
		rw.WriteHeader(http.StatusOK)
		rw.Write([]byte(`{"holds":1}` + "\n"))
		http.NewResponseController(rw).Flush()
		<-r.Context().Done()
	})
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(ln)
	return func() { server.Close() }
}

func (d *holdDaemon) until(t *testing.T, holds int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); d.open() != holds; {
		if time.Now().After(deadline) {
			t.Fatalf("holds %d, want %d", d.open(), holds)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// shortSocket is a socket path under the 108 bytes a socket's may have.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hold")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// A hold is open from Hold's return until release (0024, 5).
func TestHold_AHoldLastsUntilReleased(t *testing.T) {
	socket := shortSocket(t)
	d := &holdDaemon{}
	defer d.serve(t, socket)()
	release, err := client(socket).Hold(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.open() != 1 {
		t.Fatalf("holds %d after Hold returned", d.open())
	}
	release()
	d.until(t, 0)
}

// A daemon that is not running yet, or that restarts, is held as soon as it
// is back: the hold dials again until release.
func TestHold_AHoldDialsAgainUntilReleased(t *testing.T) {
	socket := shortSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := client(socket).Hold(ctx)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("no daemon: %v", err)
	}
	d := &holdDaemon{}
	stop := d.serve(t, socket)
	d.until(t, 1)
	stop()
	d.until(t, 0)
	os.Remove(socket)
	stop = d.serve(t, socket)
	defer stop()
	d.until(t, 1)
	release()
	d.until(t, 0)
	// Released, it dials no more.
	time.Sleep(50 * time.Millisecond)
	if d.open() != 0 {
		t.Fatalf("holds %d after release", d.open())
	}
}
