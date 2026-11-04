package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary runs main when asked, so the command is tested as a
// process: its exit and whether it started the game. Copied to a Steam
// root's ubuntu12_32/reaper, it is a game's reaper there, which runs until
// its stdin closes.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "reaper" {
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	if args := os.Getenv("INFERMUX_HOLD_ARGS"); args != "" {
		os.Args = append([]string{"infermux-hold"}, strings.Split(args, "\x1f")...)
		main()
		return
	}
	os.Exit(m.Run())
}

// The launch always wins (0024, 4): a daemon whose unload fails, or whose
// card never comes clear, still gets the command started, with the reason
// on stderr.
func TestHold_TheCommandStartsWhateverTheDaemonAnswers(t *testing.T) {
	for _, c := range []struct {
		name, unload, vram, want string
	}{
		{"unload refused", `{"detail":"1 interactive request(s) in flight"}`, `{"our_vram_mb":0}`, "not unloaded"},
		{"card busy", `{"unloaded":["qwen"]}`, `{"our_vram_mb":5838}`, "still holds 5838 MiB"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A short path: a socket's is limited to 108 bytes.
			dir, err := os.MkdirTemp("", "hold")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "s")
			mux := http.NewServeMux()
			mux.HandleFunc("GET /warden/verdict", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte(`{"manual":null}`)) })
			mux.HandleFunc("POST /warden/manual", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("{}")) })
			mux.HandleFunc("POST /warden/unload", func(rw http.ResponseWriter, r *http.Request) {
				if strings.Contains(c.unload, "detail") {
					rw.WriteHeader(http.StatusConflict)
				}
				rw.Write([]byte(c.unload))
			})
			mux.HandleFunc("GET /warden/resources", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte(c.vram)) })
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: mux}
			go server.Serve(ln)
			defer server.Close()

			ran := filepath.Join(dir, "ran")
			args := []string{"-socket", socket, "-timeout", "300ms", "--", "/bin/sh", "-c", "echo > " + ran}
			cmd := exec.Command(os.Args[0])
			// No PATH, so no notify-send puts the warning on the desktop.
			cmd.Env = []string{"INFERMUX_HOLD_ARGS=" + strings.Join(args, "\x1f")}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if _, err := os.Stat(ran); err != nil {
				t.Fatalf("the command did not run: %s", out)
			}
			if !strings.Contains(string(out), c.want) {
				t.Errorf("stderr %q, want %q", out, c.want)
			}
		})
	}
}

// The hold is open before the unload and while the command runs, and ends
// with it (0024, 5).
func TestHold_TheGPUIsHeldWhileTheCommandRuns(t *testing.T) {
	dir, err := os.MkdirTemp("", "hold")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket, ran, gate := filepath.Join(dir, "s"), filepath.Join(dir, "ran"), filepath.Join(dir, "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	holds := 0
	record := func(s string, d int) {
		mu.Lock()
		calls = append(calls, s)
		holds += d
		mu.Unlock()
	}
	open := func() int {
		mu.Lock()
		defer mu.Unlock()
		return holds
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /warden/hold", func(rw http.ResponseWriter, r *http.Request) {
		record("hold", 1)
		defer record("unhold", -1)
		rw.Write([]byte(`{"holds":1}` + "\n"))
		http.NewResponseController(rw).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /warden/verdict", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte(`{"manual":null}`)) })
	mux.HandleFunc("POST /warden/manual", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("{}")) })
	mux.HandleFunc("POST /warden/unload", func(rw http.ResponseWriter, r *http.Request) {
		record("unload", 0)
		rw.Write([]byte(`{"unloaded":[]}`))
	})
	mux.HandleFunc("GET /warden/resources", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte(`{"our_vram_mb":0}`)) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(ln)
	defer server.Close()

	// The command waits on the FIFO until the test has looked.
	args := []string{"-socket", socket, "--", "/bin/sh", "-c", "echo > " + ran + "; read x < " + gate}
	cmd := exec.Command(os.Args[0])
	cmd.Env = []string{"INFERMUX_HOLD_ARGS=" + strings.Join(args, "\x1f")}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ran); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command never ran: %s", out.String())
		}
	}
	if open() != 1 {
		t.Fatalf("holds %d while the command runs", open())
	}
	if err := os.WriteFile(gate, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	for deadline := time.Now().Add(2 * time.Second); open() != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("holds %d after the command exited", open())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(calls, " "); got != "hold unload unhold" {
		t.Fatalf("%q, stderr %s", got, out.String())
	}
}

// lockedBuffer is a process's output that the test reads while it runs.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// With -steam, each game Steam starts has the card held and cleared from its
// reaper's start until its exit, and a game still running when
// infermux-hold stops is handed back first (0024, 6).
func TestHold_SteamHoldsEachGameUntilItExits(t *testing.T) {
	dir, err := os.MkdirTemp("", "hold")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket, root := filepath.Join(dir, "s"), filepath.Join(dir, "steam")
	var mu sync.Mutex
	var calls []string
	holds := 0
	record := func(s string, d int) {
		mu.Lock()
		calls = append(calls, s)
		holds += d
		mu.Unlock()
	}
	log := func() (string, int) {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(calls, " "), holds
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /warden/hold", func(rw http.ResponseWriter, r *http.Request) {
		record("hold", 1)
		defer record("unhold", -1)
		rw.Write([]byte(`{"holds":1}` + "\n"))
		http.NewResponseController(rw).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /warden/verdict", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte(`{"manual":null}`)) })
	mux.HandleFunc("POST /warden/manual", func(rw http.ResponseWriter, r *http.Request) {
		var body struct{ Action string }
		json.NewDecoder(r.Body).Decode(&body)
		record(body.Action, 0)
		// A hand back answered late, to see that infermux-hold waits for it.
		if body.Action == "auto" {
			time.Sleep(100 * time.Millisecond)
			if r.Context().Err() != nil {
				record("unanswered", 0)
			}
		}
		rw.Write([]byte("{}"))
	})
	mux.HandleFunc("POST /warden/unload", func(rw http.ResponseWriter, r *http.Request) {
		record("unload", 0)
		rw.Write([]byte(`{"unloaded":[]}`))
	})
	mux.HandleFunc("GET /warden/resources", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte(`{"our_vram_mb":0}`)) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(ln)
	defer server.Close()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	reaper := filepath.Join(root, "ubuntu12_32", "reaper")
	if err := os.MkdirAll(filepath.Dir(reaper), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reaper, b, 0o755); err != nil {
		t.Fatal(err)
	}
	launch := func(app string) (exit func()) {
		game := exec.Command(reaper, "SteamLaunch", "AppId="+app, "--", "game")
		in, err := game.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := game.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			in.Close()
			game.Wait()
		})
		return func() { in.Close() }
	}

	args := []string{"-socket", socket, "-steam", root}
	cmd := exec.Command(os.Args[0])
	// No PATH, so no notify-send puts a warning on the desktop.
	cmd.Env = []string{"INFERMUX_HOLD_ARGS=" + strings.Join(args, "\x1f")}
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	until := func(want string, open int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			got, held := log()
			if got == want && held == open {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("calls %q with %d hold(s) open, want %q with %d; stderr:\n%s", got, held, want, open, out.String())
			}
		}
	}

	exit := launch("42")
	until("hold pause unload", 1)
	// The card stays held while the game runs.
	time.Sleep(200 * time.Millisecond)
	until("hold pause unload", 1)
	exit()
	until("hold pause unload auto unhold", 0)

	launch("43")
	until("hold pause unload auto unhold hold pause unload", 1)
	cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	until("hold pause unload auto unhold hold pause unload auto unhold", 0)
	// A hand back that infermux-hold did not wait for is marked unanswered
	// 100 ms after it came.
	time.Sleep(200 * time.Millisecond)
	until("hold pause unload auto unhold hold pause unload auto unhold", 0)
	for _, want := range []string{"Steam app 42", "Steam app 43"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stderr has no %q: %s", want, out.String())
		}
	}
}
