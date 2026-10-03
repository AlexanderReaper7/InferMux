// infermux-ui is InferMux's web UI, a process of its own beside the daemon
// (0005). It runs as the user, so it can edit and commit the configuration in
// the user's repository, which the sandboxed daemon only reads.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/muxui"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:5010", "address to serve the UI on")
	daemon := flag.String("daemon", "http://127.0.0.1:5001", "the InferMux daemon")
	modelsDir := flag.String("models-dir", "", "the daemon's -config-dir: one YAML file per model")
	wardenFile := flag.String("warden-config", "", "the daemon's -warden-config")
	baseConfig := flag.String("base-config", "", "the daemon's -config: the runtimes' macros and global settings")
	ggufDirs := flag.String("gguf-dirs", "", "comma-separated directories to look for .gguf files in")
	flag.Parse()

	if *modelsDir == "" || *wardenFile == "" || *baseConfig == "" {
		slog.Error("-models-dir, -warden-config and -base-config are required")
		os.Exit(2)
	}
	daemonURL, err := url.Parse(*daemon)
	if err != nil {
		slog.Error("bad -daemon", "error", err)
		os.Exit(2)
	}
	store := &muxui.Store{ModelsDir: *modelsDir, WardenFile: *wardenFile, BaseConfig: *baseConfig}
	for _, d := range strings.Split(*ggufDirs, ",") {
		if d = strings.TrimSpace(d); d != "" {
			store.GGUFDirs = append(store.GGUFDirs, d)
		}
	}
	slog.Info("infermux-ui listening", "address", "http://"+*listen, "daemon", *daemon, "models-dir", *modelsDir)
	if err := http.ListenAndServe(*listen, muxui.Handler(store, daemonURL)); err != nil {
		slog.Error("infermux-ui stopped", "error", err)
		os.Exit(1)
	}
}
