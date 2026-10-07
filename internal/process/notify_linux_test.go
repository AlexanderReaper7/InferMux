package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// The backends in these tests are this test binary run again as
// TestNotifyHelperProcess, which does what NOTIFY_HELPER says. Nothing has to
// be built first.

const helperEnv = "INFERMUX_NOTIFY_HELPER"

type helper struct {
	mode  string
	delay time.Duration
	dir   string // where the helper writes what it saw, and waits for "go"
	port  int
}

func newHelper(t *testing.T, mode string, delay time.Duration) helper {
	return helper{mode: mode, delay: delay, dir: t.TempDir(), port: getFreePort(t)}
}

func (h helper) config(readiness string) config.ModelConfig {
	conf := config.ModelConfig{
		Cmd:           fmt.Sprintf("%s -test.run=TestNotifyHelperProcess", os.Args[0]),
		Proxy:         fmt.Sprintf("http://127.0.0.1:%d", h.port),
		CheckEndpoint: "/health",
		Env: []string{
			helperEnv + "=" + h.mode,
			"HELPER_DELAY_MS=" + strconv.Itoa(int(h.delay.Milliseconds())),
			"HELPER_DIR=" + h.dir,
			"HELPER_PORT=" + strconv.Itoa(h.port),
		},
	}
	if readiness != "" {
		conf.Metadata = map[string]any{"readiness": readiness}
	}
	return conf
}

// file waits for the helper to write name and returns it.
func (h helper) file(t *testing.T, name string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(h.dir, name)); err == nil {
			return string(b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the helper never wrote %s", name)
	return ""
}

func (h helper) proceed(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func startNotifyProcess(t *testing.T, conf config.ModelConfig) (*ProcessCommand, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := logmon.NewWriter(logs)
	logger.SetLogLevel(logmon.LevelDebug)
	p, err := New(context.Background(), t.Name(), conf, logmon.NewWriter(os.Stderr), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop(testStopTimeout) })
	return p, logs
}

func TestNotify_ReadyAfterTheLoad(t *testing.T) {
	h := newHelper(t, "ready", 300*time.Millisecond)
	p, _ := startNotifyProcess(t, h.config("notify"))

	start := time.Now()
	if err := p.EnsureReady(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	// Polled, a 300 ms load is seen at the second check, 1250 ms in.
	if took < 300*time.Millisecond || took > 900*time.Millisecond {
		t.Fatalf("ready after %v, want 300 ms and a little", took)
	}
	socket := h.file(t, "socket")
	if !strings.HasPrefix(socket, os.TempDir()) || !strings.HasSuffix(socket, "/notify") {
		t.Fatalf("NOTIFY_SOCKET is %q", socket)
	}
	sent, err := time.Parse(time.RFC3339Nano, h.file(t, "sent"))
	if err != nil {
		t.Fatal(err)
	}
	// ReadySince is when READY=1 arrived, not when the run loop got to it.
	since := p.Status().ReadySince
	if d := since.Sub(sent); d < 0 || d > 50*time.Millisecond {
		t.Fatalf("ReadySince is %v after READY=1 was sent", d)
	}
	// The socket and its directory go once the start is over.
	if _, err := os.Stat(filepath.Dir(socket)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the notify directory is still there: %v", err)
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("a request after READY=1 got %d", rr.Code)
	}
}

// ReadySince is READY=1's arrival even when the check after it is slow.
func TestNotify_ReadySinceIsTheReadyNotTheCheck(t *testing.T) {
	h := newHelper(t, "ready-slow-health", 0)
	p, _ := startNotifyProcess(t, h.config("notify"))

	if err := p.EnsureReady(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	ready := time.Now()
	sent, err := time.Parse(time.RFC3339Nano, h.file(t, "sent"))
	if err != nil {
		t.Fatal(err)
	}
	if d := ready.Sub(sent); d < 200*time.Millisecond {
		t.Fatalf("EnsureReady returned %v after READY=1, before the 200 ms check could end", d)
	}
	if d := p.Status().ReadySince.Sub(sent); d < 0 || d > 50*time.Millisecond {
		t.Fatalf("ReadySince is %v after READY=1 was sent", d)
	}
}

func TestNotify_NoReadyTimesOut(t *testing.T) {
	h := newHelper(t, "silent", 0)
	p, _ := startNotifyProcess(t, h.config("notify"))

	start := time.Now()
	err := p.EnsureReady(context.Background(), 700*time.Millisecond)
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "no READY=1 within 700ms") || !strings.Contains(err.Error(), `last STATUS="loading weights"`) {
		t.Fatalf("got %v", err)
	}
	if took < 700*time.Millisecond || took > 2*time.Second {
		t.Fatalf("gave up after %v", took)
	}
	if st := p.State(); st != StateStopped {
		t.Fatalf("state %s after the timeout", st)
	}
	pid, _ := strconv.Atoi(h.file(t, "pid"))
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("pid %d is still there after the timeout: %v", pid, err)
	}
}

func TestNotify_ExitBeforeReady(t *testing.T) {
	h := newHelper(t, "exit", 0)
	p, _ := startNotifyProcess(t, h.config("notify"))

	start := time.Now()
	err := p.EnsureReady(context.Background(), 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "exited prematurely, before READY=1") ||
		!strings.Contains(err.Error(), `last STATUS="out of memory"`) || !strings.Contains(err.Error(), "ERRNO=12") {
		t.Fatalf("got %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the exit was noticed after %v", took)
	}
}

// Another process of the same user, here the test itself, which is the
// backend's parent and not its descendant, cannot make it ready.
func TestNotify_AnotherPidIsIgnored(t *testing.T) {
	h := newHelper(t, "wait-go", 0)
	p, logs := startNotifyProcess(t, h.config("notify"))

	done := make(chan error, 1)
	go func() { done <- p.EnsureReady(context.Background(), 5*time.Second) }()
	socket := h.file(t, "socket")
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	select {
	case err := <-done:
		t.Fatalf("ready on the test's own READY=1: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	want := fmt.Sprintf("ignored a message: pid %d is not pid", os.Getpid())
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("no %q in the log:\n%s", want, logs.String())
	}

	h.proceed(t)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A wrapper that forks the server, as `uv run` and `cargo run` do, leaves
// the READY=1 to a descendant.
func TestNotify_ADescendantMaySend(t *testing.T) {
	h := newHelper(t, "grandchild", 0)
	p, logs := startNotifyProcess(t, h.config("notify"))

	if err := p.EnsureReady(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(h.file(t, "pid"))
	sender, _ := strconv.Atoi(h.file(t, "sender"))
	if sender == child {
		t.Fatalf("the sender is the child itself, %d", sender)
	}
	if want := fmt.Sprintf("READY=1 from pid %d", sender); !strings.Contains(logs.String(), want) {
		t.Fatalf("no %q in the log:\n%s", want, logs.String())
	}
}

func TestNotify_ReadyButTheHealthCheckFails(t *testing.T) {
	h := newHelper(t, "ready-unhealthy", 0)
	p, _ := startNotifyProcess(t, h.config("notify"))

	err := p.EnsureReady(context.Background(), 5*time.Second)
	want := fmt.Sprintf("READY=1, but http://127.0.0.1:%d/health answered 503", h.port)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want %q", err, want)
	}
	if st := p.State(); st != StateStopped {
		t.Fatalf("state %s", st)
	}
}

func TestNotify_CheckEndpointNoneTakesReadyAlone(t *testing.T) {
	h := newHelper(t, "ready-unhealthy", 0)
	conf := h.config("notify")
	conf.CheckEndpoint = "none"
	p, _ := startNotifyProcess(t, conf)

	if err := p.EnsureReady(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

// A model that has not opted in is polled as before, and gets no
// NOTIFY_SOCKET from InferMux.
func TestNotify_WithoutTheOptInPollingIsUnchanged(t *testing.T) {
	h := newHelper(t, "poll", 300*time.Millisecond)
	p, _ := startNotifyProcess(t, h.config(""))

	start := time.Now()
	if err := p.EnsureReady(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	// 250 ms, a check that finds 503, a second, a check that finds 200.
	if took < 1250*time.Millisecond || took > 1700*time.Millisecond {
		t.Fatalf("ready after %v, want the polling's 1.25 s", took)
	}
	want := os.Getenv("NOTIFY_SOCKET")
	if want == "" {
		want = "unset"
	}
	if got := h.file(t, "socket"); got != want {
		t.Fatalf("NOTIFY_SOCKET is %q, want %q", got, want)
	}
}

// A sender that attaches a descriptor, as sd_notify_barrier does, has it
// closed by the kernel: InferMux leaves room for the credentials only.
func TestNotify_ADescriptorIsNotKept(t *testing.T) {
	h := newHelper(t, "ready-fd", 0)
	p, _ := startNotifyProcess(t, h.config("notify"))

	if err := p.EnsureReady(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := h.file(t, "barrier"); got != "eof" {
		t.Fatalf("the barrier's pipe: %s", got)
	}
}

func TestNotify_AnUnknownValueFailsTheStart(t *testing.T) {
	h := newHelper(t, "ready", 0)
	p, _ := startNotifyProcess(t, h.config("notfy"))

	err := p.EnsureReady(context.Background(), 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "metadata.readiness is notfy") {
		t.Fatalf("got %v", err)
	}
}

func TestNotify_Readiness(t *testing.T) {
	for _, c := range []struct {
		conf config.ModelConfig
		want string
	}{
		{config.ModelConfig{CheckEndpoint: "/health"}, "health"},
		{config.ModelConfig{CheckEndpoint: "none"}, "start"},
		{config.ModelConfig{CheckEndpoint: "/health", Metadata: map[string]any{"readiness": "notify"}}, "notify"},
		{config.ModelConfig{CheckEndpoint: "none", Metadata: map[string]any{"readiness": "notify"}}, "notify"},
	} {
		if got := Readiness(c.conf); got != c.want {
			t.Errorf("Readiness(%+v) = %q, want %q", c.conf, got, c.want)
		}
	}
}

func TestNotify_ParseMessage(t *testing.T) {
	m := parseNotify([]byte("STATUS=loading=50%\nEXTEND_TIMEOUT_USEC=5000000\nERRNO=2\nREADY=1\nWATCHDOG=1"))
	if !m.ready || m.status != "loading=50%" || m.errno != "2" || m.extend != "5000000" {
		t.Fatalf("%+v", m)
	}
	if parseNotify([]byte("READY=0")).ready || parseNotify([]byte("XREADY=1")).ready {
		t.Fatal("only READY=1 is ready")
	}
}

// TestNotifyHelperProcess is the backend, when NOTIFY_HELPER is set.
func TestNotifyHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("run by the notify tests as a backend")
	}
	dir := os.Getenv("HELPER_DIR")
	delay, _ := strconv.Atoi(os.Getenv("HELPER_DELAY_MS"))
	write := func(name, value string) { os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600) }
	socket := os.Getenv("NOTIFY_SOCKET")
	forever := func() {
		for {
			time.Sleep(time.Hour)
		}
	}

	// As sd_notify(3) does it: an unbound datagram socket, one sendmsg.
	send := func(msg string, rights []byte) {
		fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
		if err == nil {
			err = syscall.Sendmsg(fd, []byte(msg), rights, &syscall.SockaddrUnix{Name: socket}, 0)
			syscall.Close(fd)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper:", err)
			os.Exit(2)
		}
	}

	if mode == "child-ready" {
		write("sender", strconv.Itoa(os.Getpid()))
		send("READY=1", nil)
		forever() // a server that is ready keeps running
	}
	if socket == "" {
		write("socket", "unset")
	} else {
		write("socket", socket)
	}
	write("pid", strconv.Itoa(os.Getpid()))

	var healthy atomic.Bool
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("HELPER_PORT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(2)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && mode == "ready-slow-health" {
			time.Sleep(200 * time.Millisecond)
		}
		if r.URL.Path == "/health" && !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok")
	}))
	time.Sleep(time.Duration(delay) * time.Millisecond)

	switch mode {
	case "ready", "ready-slow-health":
		healthy.Store(true)
		send("STATUS=loaded", nil)
		write("sent", time.Now().Format(time.RFC3339Nano))
		send("READY=1", nil)
	case "silent":
		send("STATUS=loading weights", nil)
	case "exit":
		send("STATUS=out of memory\nERRNO=12", nil)
		os.Exit(3)
	case "wait-go":
		for {
			if _, err := os.Stat(filepath.Join(dir, "go")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		healthy.Store(true)
		send("READY=1", nil)
	case "grandchild":
		healthy.Store(true)
		child := exec.Command(os.Args[0], "-test.run=TestNotifyHelperProcess")
		child.Env = append(os.Environ(), helperEnv+"=child-ready")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "helper:", err)
			os.Exit(2)
		}
	case "poll":
		healthy.Store(true)
	case "ready-unhealthy":
		send("READY=1", nil)
	case "ready-fd":
		healthy.Store(true)
		var fds [2]int
		if err := syscall.Pipe2(fds[:], syscall.O_CLOEXEC); err != nil {
			fmt.Fprintln(os.Stderr, "helper:", err)
			os.Exit(2)
		}
		send("READY=1", syscall.UnixRights(fds[1]))
		syscall.Close(fds[1])
		// EOF once no write end is open anywhere: the receiver did not keep
		// the one it was sent.
		go func() {
			n, _ := syscall.Read(fds[0], make([]byte, 1))
			if n == 0 {
				write("barrier", "eof")
			}
		}()
	}
	forever()
}
