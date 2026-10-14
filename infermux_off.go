package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"os"
	"strings"
	"time"
)

// The off file (0025): while it is there, this host's local models are off,
// and its text says why. A controller on the host, such as the zbox's quiet
// hours, writes it as root and removes it; the daemon only reads it. A file
// rather than the control socket, because the daemon must start off: one
// restarted at night reads it before it takes a request.

var flagOffFile = flag.String("off-file", "", "InferMux: while this file exists, this host's local models are off and its text says why: every request for one is refused, the models stop (0025)")

// offPoll is how often the off file is read.
const offPoll = time.Second

// offReason is why the local models are off, "" when they are on. A file
// that is there but cannot be read takes them off: something wrote it.
func offReason(path string) string {
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ""
	case err != nil:
		return path + " could not be read: " + err.Error()
	}
	if reason := strings.TrimSpace(string(raw)); reason != "" {
		return reason
	}
	return path + " is there"
}

// watchOff sets the warden's off state from path now, then every offPoll
// until ctx ends. Each read while off also stops a model that started since.
func watchOff(ctx context.Context, path string, set func(reason string)) {
	set(offReason(path))
	go func() {
		ticker := time.NewTicker(offPoll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				set(offReason(path))
			}
		}
	}()
}
