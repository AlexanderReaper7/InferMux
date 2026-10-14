package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTheOffFileSaysWhy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "off")
	if got := offReason(path); got != "" {
		t.Fatalf("no file: %q", got)
	}
	os.WriteFile(path, []byte("too hot: CPU 84.0 C\n"), 0o644)
	if got := offReason(path); got != "too hot: CPU 84.0 C" {
		t.Fatalf("%q", got)
	}
	os.WriteFile(path, nil, 0o644)
	if got := offReason(path); got != path+" is there" {
		t.Fatalf("empty: %q", got)
	}
	// A directory reads as an error, as a file nobody may read would.
	if got := offReason(dir); got == "" {
		t.Fatal("an unreadable off file left the models on")
	}
}

// A daemon restarted at night is off before startWarden returns, so before
// the server takes a request (0025).
func TestTheOffFileIsReadBeforeTheFirstRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "off")
	os.WriteFile(path, []byte("quiet hours"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var first []string
	watchOff(ctx, path, func(reason string) {
		if first == nil {
			first = []string{reason}
		}
	})
	if len(first) != 1 || first[0] != "quiet hours" {
		t.Fatalf("not set when watchOff returned: %v", first)
	}
}
