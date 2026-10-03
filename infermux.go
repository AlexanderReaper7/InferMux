package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/warden"
)

// InferMux's additions to main live here, so llama-swap.go carries only the
// flag and one call, and a merge from upstream rarely touches them.

// activeModels reaches the models through whichever server is active. A config
// reload replaces the server; the warden, like perfMon, outlives it.
type activeModels func() *server.Server

func (a activeModels) Running() map[string]string { return a().RunningModelStates() }
func (a activeModels) UnloadAll() []string        { return a().UnloadAllModels() }

// startWarden puts the warden in front of the HTTP server and starts its loop.
// Without -warden-config InferMux is plain llama-swap.
func startWarden(path string, httpServer *http.Server, active func() *server.Server, log *logmon.Monitor) {
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
}
