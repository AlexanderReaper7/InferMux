package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/warden"
	configwatcher "github.com/mostlygeek/llama-swap/internal/watcher"
)

// InferMux's additions to main live here, so llama-swap.go carries only the
// flag and one call, and a merge from upstream rarely touches them.

// activeModels reaches the models through whichever server is active. A config
// reload replaces the server; the warden, like perfMon, outlives it.
type activeModels func() *server.Server

func (a activeModels) Running() map[string]string { return a().RunningModelStates() }
func (a activeModels) UnloadAll() []string        { return a().UnloadAllModels() }

// startWarden puts the warden in front of the HTTP server, starts its loop,
// and watches the configuration. Without -warden-config InferMux is plain
// llama-swap.
//
// InferMux watches llama-swap's files itself rather than through
// -watch-config. llama-swap's reload replaces the server and stops every
// model, which would cut off a reply the user is waiting for, so the reload is
// sent as llama-swap's own SIGHUP once no interactive request is in flight
// (0005). The warden's file reloads at once: that only moves settings.
func startWarden(path, configPath, configDir string, httpServer *http.Server, active func() *server.Server, log *logmon.Monitor) {
	if path == "" {
		return
	}
	cfg, err := warden.LoadConfig(path)
	if err != nil {
		slog.Error("failed to load warden config", "warden-config", path, "error", err)
		os.Exit(1)
	}
	w := warden.New(cfg, activeModels(active), log)
	httpServer.Handler = w.Wrap(httpServer.Handler)
	w.Start()

	ctx := context.Background()
	go (&configwatcher.Watcher{Path: absolute(path), OnChange: func() {
		cfg, err := warden.LoadConfig(path)
		if err != nil {
			log.Warnf("Warden settings not reloaded: %v", err)
			return
		}
		w.Reload(cfg)
	}}).Run(ctx)

	reload := func() {
		w.WhenNoInteractive("llama-swap config reload", func() {
			syscall.Kill(os.Getpid(), syscall.SIGHUP)
		})
	}
	if configPath != "" {
		go (&configwatcher.Watcher{Path: absolute(configPath), OnChange: reload}).Run(ctx)
	}
	if configDir != "" {
		go (&configwatcher.DirWatcher{Path: absolute(configDir), OnChange: reload}).Run(ctx)
	}
}

func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
