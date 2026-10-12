package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
)

// The control socket (0024): the warden's controls without a key, for a
// command on this host such as infermux-hold. The socket's file mode is the
// authorization, so it answers nothing a key would have refused to someone
// outside the group.

var flagControlSocket = flag.String("control-socket", "", "InferMux: a Unix socket that answers the warden's controls without a key (0024)")

var flagControlGroup = flag.String("control-group", "", "InferMux: the group that may open -control-socket; without one only this user can (0024)")

// serveControl listens on path, 0660 for group or 0600 without one, and
// serves h there until the returned stop is called.
func serveControl(path, group string, h http.Handler) (stop func(), err error) {
	gid := -1
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return nil, err
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return nil, fmt.Errorf("group %s: gid %q: %w", group, g.Gid, err)
		}
	}
	// A socket left by a daemon that was killed would refuse the bind.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(0o600)
	if gid >= 0 {
		mode = 0o660
		err = os.Chown(path, -1, gid)
	}
	if err == nil {
		err = os.Chmod(path, mode)
	}
	if err != nil {
		ln.Close()
		return nil, err
	}
	server := &http.Server{Handler: h}
	go server.Serve(ln)
	return func() { server.Close() }, nil
}
