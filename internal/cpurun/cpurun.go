// Package cpurun runs a CPU device's model servers outside InferMux's own
// cgroup, in a slice at the root of the tree set to `CPUWeight=idle` (0022).
// A slice nested anywhere else is idle only among its siblings, and systemd
// keeps a service's children under the service, so the daemon cannot put
// them there itself. A runner, a second service in that slice, starts them
// as its own children instead.
//
// llama-swap still starts and stops each one. The model's command is
// `infermux-cpu run`, which hands the real command to the runner over a
// socket and relays its output. When llama-swap stops it, or it dies, the
// connection ends and the runner stops the server.
package cpurun

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// GracePeriod is how long a server has between SIGTERM and SIGKILL once its
// client is gone. llama-swap killing the client sooner changes nothing: the
// server has its SIGTERM already.
const GracePeriod = 10 * time.Second

// Request is what a client sends, one JSON line: the command and the
// environment llama-swap gave it.
type Request struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
}

// A frame is one byte of kind, four of length, and the payload.
const (
	frameOutput = 'o' // the server's stdout and stderr
	frameExit   = 'x' // its exit code, in decimal; the last frame
	frameError  = 'e' // it could not be started; the last frame
)

const maxFrame = 1 << 20

func writeFrame(w io.Writer, kind byte, payload []byte) error {
	head := [5]byte{kind}
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("a frame of %d bytes", n)
	}
	payload := make([]byte, n)
	_, err := io.ReadFull(r, payload)
	return head[0], payload, err
}

// Serve runs each connection's command until ln is closed. Only a process of
// the runner's own user may connect: the daemon, which may run any command
// anyway.
func Serve(ln *net.UnixListener, logf func(format string, args ...any)) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			if err := handle(conn); err != nil {
				logf("cpurun: %v", err)
			}
		}()
	}
}

func peerCred(conn *net.UnixConn) (*unix.Ucred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, credErr
}

func handle(conn *net.UnixConn) error {
	cred, err := peerCred(conn)
	if err != nil || cred.Uid != uint32(os.Getuid()) {
		uid := -1
		if cred != nil {
			uid = int(cred.Uid)
		}
		return fmt.Errorf("refused a connection from uid %d: %v", uid, err)
	}
	in := bufio.NewReader(io.LimitReader(conn, maxFrame))
	line, err := in.ReadBytes('\n')
	if errors.Is(err, io.EOF) && len(line) == 0 {
		// Unit's connection, which only reads who the runner is.
		return nil
	}
	if err != nil {
		return fmt.Errorf("no request: %w", err)
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil || len(req.Argv) == 0 {
		writeFrame(conn, frameError, []byte("a request needs argv"))
		return fmt.Errorf("a bad request: %v", err)
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env, cmd.Dir = req.Env, req.Dir
	// Its own process group, as llama-swap starts a model, so what it forks
	// goes with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pr.Close()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		writeFrame(conn, frameError, []byte(err.Error()))
		return nil
	}
	pw.Close()
	pid := cmd.Process.Pid

	var writeMu sync.Mutex
	send := func(kind byte, payload []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		// A client that is gone only stops the output; the pipe is still
		// drained, so the server never blocks on a full one.
		writeFrame(conn, kind, payload)
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		buf := make([]byte, 32<<10)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				send(frameOutput, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// The group is signalled only while its leader is unreaped, so its id
	// cannot have been reused.
	var groupMu sync.Mutex
	reaped := false
	signal := func(sig syscall.Signal) {
		groupMu.Lock()
		defer groupMu.Unlock()
		if !reaped {
			syscall.Kill(-pid, sig)
		}
	}
	exited := make(chan struct{})
	go func() {
		// Whatever ends the client's side, a half-close or the connection
		// going, stops the server.
		io.Copy(io.Discard, in)
		select {
		case <-exited:
			return
		default:
		}
		signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(GracePeriod):
			signal(syscall.SIGKILL)
		}
	}()

	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	groupMu.Lock()
	// What the server left behind in its group goes with it.
	syscall.Kill(-pid, syscall.SIGKILL)
	reaped = true
	groupMu.Unlock()
	close(exited)
	cmd.Wait()
	select {
	case <-copied:
	case <-time.After(2 * time.Second):
		pr.Close()
		<-copied
	}
	code := cmd.ProcessState.ExitCode()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		code = 128 + int(ws.Signal())
	}
	send(frameExit, []byte(strconv.Itoa(code)))
	return nil
}

// Run hands req to the runner at socket, writes the server's output to out,
// and returns its exit code. Closing stop asks the runner to end it.
// Unit is the runner's unit, the last component of its cgroup path, so the
// daemon's warden counts the CPU time of the servers under it as InferMux's
// own (0022). The runner's PID comes from the socket's peer credentials.
func Unit(socket string) (string, error) {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return "", fmt.Errorf("the CPU runner: %w", err)
	}
	defer conn.Close()
	cred, err := peerCred(conn)
	if err != nil {
		return "", fmt.Errorf("the CPU runner's credentials: %w", err)
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(int(cred.Pid)) + "/cgroup")
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	i := strings.LastIndexByte(line, '/')
	if i < 0 || i == len(line)-1 {
		return "", fmt.Errorf("the CPU runner's cgroup is %q", line)
	}
	return line[i+1:], nil
}

func Run(socket string, req Request, out io.Writer, stop <-chan struct{}) (int, error) {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return 0, fmt.Errorf("the CPU runner: %w", err)
	}
	defer conn.Close()
	line, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return 0, err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			conn.CloseWrite()
		case <-done:
		}
	}()
	in := bufio.NewReader(conn)
	for {
		kind, payload, err := readFrame(in)
		if err != nil {
			return 0, fmt.Errorf("the CPU runner went away: %w", err)
		}
		switch kind {
		case frameOutput:
			out.Write(payload)
		case frameExit:
			return strconv.Atoi(string(payload))
		case frameError:
			return 0, fmt.Errorf("the CPU runner could not start it: %s", payload)
		}
	}
}
