package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestControl_TheSocketIsTheGroupsOrOnlyOurs(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(me.Gid)
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { io.WriteString(rw, "control") })
	for _, c := range []struct {
		group string
		mode  os.FileMode
	}{{"", 0o600}, {group.Name, 0o660}} {
		path := filepath.Join(t.TempDir(), "control.sock")
		// A socket left by a killed daemon is replaced.
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		stop, err := serveControl(path, c.group, h)
		if err != nil {
			t.Fatalf("group %q: %v", c.group, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != c.mode || info.Mode()&os.ModeSocket == 0 {
			t.Errorf("group %q: mode %v, want a socket at %v", c.group, info.Mode(), c.mode)
		}
		if gid := strconv.Itoa(int(info.Sys().(*syscall.Stat_t).Gid)); c.group != "" && gid != group.Gid {
			t.Errorf("group %q: gid %s", c.group, gid)
		}
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}}
		resp, err := client.Get("http://infermux/warden/verdict")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "control" {
			t.Errorf("group %q: %q", c.group, body)
		}
		stop()
	}
	if _, err := serveControl(filepath.Join(t.TempDir(), "s"), "no-such-group-infermux", h); err == nil {
		t.Error("an unknown group was accepted")
	}
}
