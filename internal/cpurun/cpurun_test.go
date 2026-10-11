package cpurun

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func runner(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "socket")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		Serve(ln, t.Logf)
	}()
	t.Cleanup(func() {
		ln.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("a server outlived its client")
		}
	})
	return socket
}

// syncBuffer is out, read while Run writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func sh(script string) Request {
	return Request{Argv: []string{"sh", "-c", script}, Env: append(os.Environ(), "WHO=runner"), Dir: os.TempDir()}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

func TestRunRelaysOutputAndExitCode(t *testing.T) {
	socket := runner(t)
	var out syncBuffer
	code, err := Run(socket, sh(`echo "out $WHO"; echo err >&2; pwd; exit 3`), &out, nil)
	if err != nil || code != 3 {
		t.Fatalf("code %d, %v", code, err)
	}
	want := "out runner\nerr\n" + os.TempDir() + "\n"
	if out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
}

func TestStopEndsTheServer(t *testing.T) {
	socket := runner(t)
	var out syncBuffer
	stop := make(chan struct{})
	done := make(chan struct{})
	var code int
	var err error
	go func() {
		defer close(done)
		code, err = Run(socket, sh(`trap 'echo stopping; exit 0' TERM; echo ready; while :; do sleep 0.05; done`), &out, stop)
	}()
	waitFor(t, "the server starts", func() bool { return strings.Contains(out.String(), "ready") })
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not stop")
	}
	if err != nil || code != 0 || !strings.Contains(out.String(), "stopping") {
		t.Errorf("code %d, %v, output %q", code, err, out.String())
	}
}

// A client that dies, as when llama-swap kills it, takes its server with
// it, and with the server what it forked.
func TestAClientThatGoesTakesItsServer(t *testing.T) {
	socket := runner(t)
	pids := filepath.Join(t.TempDir(), "pids")
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(sh(`sleep 600 & echo $$ $! > ` + pids + `; wait`))
	conn.Write(append(req, '\n'))
	var raw []byte
	waitFor(t, "the server writes its pids", func() bool {
		raw, _ = os.ReadFile(pids)
		return len(strings.Fields(string(raw))) == 2
	})
	conn.Close()
	for _, field := range strings.Fields(string(raw)) {
		pid, _ := strconv.Atoi(field)
		waitFor(t, "pid "+field+" ends", func() bool { return syscall.Kill(pid, 0) != nil })
	}
}

func TestACommandThatCannotStart(t *testing.T) {
	socket := runner(t)
	_, err := Run(socket, Request{Argv: []string{"/nonexistent/llama-server"}}, &syncBuffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "could not start") {
		t.Errorf("err %v", err)
	}
	if _, err := Run(filepath.Join(t.TempDir(), "none"), sh("true"), &syncBuffer{}, nil); err == nil || !strings.Contains(err.Error(), "the CPU runner") {
		t.Errorf("no runner: %v", err)
	}
}

// The runner here is this test process, so its unit is this process's own,
// and the connection that read it is no request to complain about.
func TestUnitIsTheRunnersCgroupAndLogsNothing(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "socket")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var logged syncBuffer
	served := make(chan struct{})
	go func() {
		defer close(served)
		Serve(ln, func(format string, args ...any) { logged.Write([]byte(format)) })
	}()
	raw, _ := os.ReadFile("/proc/self/cgroup")
	line := strings.TrimSpace(string(raw))
	want := line[strings.LastIndexByte(line, '/')+1:]
	got, err := Unit(socket)
	if err != nil || got != want || got == "" {
		t.Fatalf("Unit = %q, %v; want %q", got, err, want)
	}
	ln.Close()
	<-served
	if logged.String() != "" {
		t.Fatalf("logged %q", logged.String())
	}
}
