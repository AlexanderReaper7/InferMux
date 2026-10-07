package process

// InferMux: the notify socket (0019). One per start, in a directory of its
// own that only InferMux's user can enter, bound before the process starts so
// no datagram is lost, and removed when the start ends.

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type notifySocket struct {
	dir  string
	conn *net.UnixConn

	once sync.Once
	done chan struct{}
}

// listenNotify binds the start's notify socket and puts its path in the
// command's NOTIFY_SOCKET, after the model's own env so it wins. nil when the
// model has not opted in.
func (p *ProcessCommand) listenNotify(cmd *exec.Cmd) (*notifySocket, error) {
	want, err := notifyWanted(p.config)
	if err != nil || !want {
		return nil, err
	}
	// os.MkdirTemp makes the directory 0700.
	dir, err := os.MkdirTemp("", "infermux-notify-")
	if err != nil {
		return nil, fmt.Errorf("notify socket: %w", err)
	}
	path := filepath.Join(dir, "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("notify socket: %w", err)
	}
	// The kernel then attaches each sender's pid, uid and gid, which a sender
	// cannot forge without CAP_SYS_ADMIN.
	if err := setPassCred(conn); err != nil {
		conn.Close()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("notify socket: SO_PASSCRED: %w", err)
	}
	cmd.Env = append(cmd.Env, "NOTIFY_SOCKET="+path)
	return &notifySocket{dir: dir, conn: conn, done: make(chan struct{})}, nil
}

func setPassCred(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_PASSCRED, 1)
	}); err != nil {
		return err
	}
	return serr
}

// close stops the reader and removes the socket. Safe on nil and twice. A
// backend's later sd_notify calls then fail with ENOENT, which sd_notify
// clients are expected to ignore.
func (n *notifySocket) close() {
	if n == nil {
		return
	}
	n.once.Do(func() {
		close(n.done)
		n.conn.Close()
		os.RemoveAll(n.dir)
	})
}

// serve reads datagrams until close and passes each on, with its sender
// checked against pid, the process InferMux started.
func (n *notifySocket) serve(pid int) <-chan notifyEvent {
	events := make(chan notifyEvent, 16)
	go func() {
		buf := make([]byte, notifyMax)
		// Room for the credentials only. A sender that attaches file
		// descriptors gets MSG_CTRUNC, and the kernel closes the descriptors
		// instead of installing them here.
		oob := make([]byte, syscall.CmsgSpace(syscall.SizeofUcred))
		for {
			nb, noob, flags, _, err := n.conn.ReadMsgUnix(buf, oob)
			if err != nil {
				return // closed
			}
			e := notifyEvent{at: time.Now()}
			e.pid = senderPid(oob[:noob])
			switch {
			case e.pid <= 0:
				e.rejected = "no sender credentials"
			case !descends(e.pid, pid):
				e.rejected = fmt.Sprintf("pid %d is not pid %d or its descendant", e.pid, pid)
			case flags&syscall.MSG_TRUNC != 0:
				e.rejected = fmt.Sprintf("from pid %d, longer than %d bytes", e.pid, notifyMax)
			default:
				e.msg = parseNotify(buf[:nb])
			}
			select {
			case events <- e:
			case <-n.done:
				return
			}
		}
	}()
	return events
}

// senderPid is the pid in the datagram's SCM_CREDENTIALS, 0 when there is
// none.
func senderPid(oob []byte) int {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return 0
	}
	for i := range msgs {
		if cred, err := syscall.ParseUnixCredentials(&msgs[i]); err == nil {
			return int(cred.Pid)
		}
	}
	return 0
}

// descends is whether pid is root or below it, read up the parent chain in
// /proc. A sender that has already exited cannot be placed and is refused.
func descends(pid, root int) bool {
	for range 64 {
		if pid == root {
			return true
		}
		if pid <= 1 {
			return false
		}
		parent, err := parentPid(pid)
		if err != nil {
			return false
		}
		pid = parent
	}
	return false
}

// parentPid reads the fourth field of /proc/<pid>/stat. The second, the
// command name in parentheses, may itself hold spaces and parentheses, so
// the fields are counted from the last ')'.
func parentPid(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, fmt.Errorf("/proc/%d/stat: no command name", pid)
	}
	fields := bytes.Fields(b[i+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("/proc/%d/stat: too short", pid)
	}
	return strconv.Atoi(string(fields[1]))
}
