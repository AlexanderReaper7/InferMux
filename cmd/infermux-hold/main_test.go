package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test binary runs main when asked, so the command is tested as a
// process: its exit and whether it started the game.
func TestMain(m *testing.M) {
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
