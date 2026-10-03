package muxui

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Builder runs the one Nix build the UI can start: Installable, which the
// NixOS module points at what the daemon's runtimes are built from. A cache
// type the compiled kernels lack runs on the slow path until a switch, and
// the switch needs root, so the UI only builds ahead of it (0005).
type Builder struct {
	Installable string // empty hides the button
	// Prepare runs before the build; Store.IntentToAdd, so a model file
	// nobody committed yet is part of the flake the build sees.
	Prepare func() error
	run     func(args []string, stdout, stderr io.Writer) error

	mu    sync.Mutex
	state BuildState
}

// BuildState is what GET /api/build answers.
type BuildState struct {
	Configured  bool       `json:"configured"`
	Installable string     `json:"installable"`
	Running     bool       `json:"running"`
	Started     *time.Time `json:"started"`
	Ended       *time.Time `json:"ended"`
	OK          *bool      `json:"ok"`
	// Stale is set by a model change after the build started: its result
	// says nothing about the files as they are now.
	Stale  bool     `json:"stale"`
	Error  string   `json:"error"`
	Output string   `json:"output"` // the store paths built
	Log    []string `json:"log"`    // the last lines
}

const buildLogLines = 200

var ErrBuildRunning = errors.New("a build is already running")

func (b *Builder) State() BuildState {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state
	st.Configured = b.Installable != ""
	st.Installable = b.Installable
	st.Log = append([]string(nil), b.state.Log...)
	return st
}

// Start begins a build in the background. The nix-daemon does the work, so
// the UI's own process only waits and keeps the log's tail.
func (b *Builder) Start() error {
	if b.Installable == "" {
		return errors.New("no build is configured")
	}
	b.mu.Lock()
	if b.state.Running {
		b.mu.Unlock()
		return ErrBuildRunning
	}
	now := time.Now()
	b.state = BuildState{Running: true, Started: &now}
	b.mu.Unlock()

	go func() {
		var stdout strings.Builder
		err := b.prepare()
		if err == nil {
			pr, pw := io.Pipe()
			done := make(chan struct{})
			go func() {
				b.tail(pr)
				close(done)
			}()
			args := []string{"build", "--no-link", "--print-out-paths", "--print-build-logs", b.Installable}
			err = b.runner()(args, &stdout, pw)
			pw.Close()
			<-done
		}
		ended := time.Now()
		ok := err == nil
		b.mu.Lock()
		b.state.Running = false
		b.state.Ended = &ended
		b.state.OK = &ok
		b.state.Output = strings.TrimSpace(stdout.String())
		if err != nil {
			b.state.Error = err.Error()
		}
		b.mu.Unlock()
	}()
	return nil
}

// Outdated marks the last build, finished or running, as older than the
// model files.
func (b *Builder) Outdated() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Started != nil {
		b.state.Stale = true
	}
}

func (b *Builder) prepare() error {
	if b.Prepare == nil {
		return nil
	}
	return b.Prepare()
}

func (b *Builder) tail(r io.Reader) {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 64*1024), 1024*1024)
	for scan.Scan() {
		b.mu.Lock()
		b.state.Log = append(b.state.Log, scan.Text())
		if over := len(b.state.Log) - buildLogLines; over > 0 {
			b.state.Log = b.state.Log[over:]
		}
		b.mu.Unlock()
	}
	io.Copy(io.Discard, r)
}

func (b *Builder) runner() func([]string, io.Writer, io.Writer) error {
	if b.run != nil {
		return b.run
	}
	return func(args []string, stdout, stderr io.Writer) error {
		cmd := exec.Command("nix", args...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		return cmd.Run()
	}
}
