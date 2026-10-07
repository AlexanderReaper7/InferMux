package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/mostlygeek/llama-swap/internal/catalog"
	"github.com/mostlygeek/llama-swap/internal/failover"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/remote"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/stats"
	"github.com/mostlygeek/llama-swap/internal/stream"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/internal/warden"
	configwatcher "github.com/mostlygeek/llama-swap/internal/watcher"
)

// InferMux's additions to main live here, so llama-swap.go carries only the
// flag and one call, and a merge from upstream rarely touches them.

// Declared here rather than in llama-swap.go, so upstream's main stays as it
// is; flag.Parse there reads it all the same.
var flagCodexPrompt = flag.String("codex-prompt", "", "InferMux: Codex's prompt.md, for the catalog Codex asks /v1/models for (internal/catalog)")

// activeModels reaches the models through whichever server is active. A config
// reload replaces the server; the warden, like perfMon, outlives it.
type activeModels struct {
	server func() *server.Server
	host   string
	remote *remoteHolder
}

func (a activeModels) Running() map[string]string { return a.server().RunningModelStates() }
func (a activeModels) UnloadAll() []string        { return a.server().UnloadAllModels() }

// Qualify names the model as a key's allow list sees it (0006, 4): another
// host's as <host>/<model>, a peer's as <peer>/<model>, a local one as
// <this host>/<model>.
func (a activeModels) Qualify(r *http.Request) (string, bool, bool) {
	if name, ok := a.remote.get().Qualify(r); ok {
		return name, false, true
	}
	srv := a.server()
	if strings.HasPrefix(r.URL.Path, "/upstream/") {
		peer, model, ok := srv.UpstreamModel(strings.TrimPrefix(r.URL.Path, "/upstream"))
		if peer != "" {
			return peer + "/" + model, false, ok
		}
		return a.host + "/" + model, true, ok
	}
	requested, err := swaputil.ExtractModel(r)
	if err != nil || requested == "" {
		return "", false, false
	}
	return a.qualifyHere(requested)
}

// QualifyName names a model /v1/models lists the way Qualify names a request
// for it.
func (a activeModels) QualifyName(requested string) (string, bool) {
	if name, ok := a.remote.get().QualifyName(requested); ok {
		return name, true
	}
	name, _, ok := a.qualifyHere(requested)
	return name, ok
}

// qualifyHere is a name this host's llama-swap serves: a local model or a
// peer's.
func (a activeModels) qualifyHere(requested string) (string, bool, bool) {
	peer, model, ok := a.server().QualifyModel(requested)
	if !ok {
		return "", false, false
	}
	if peer != "" {
		return peer + "/" + model, false, true
	}
	return a.host + "/" + model, true, true
}

// startWarden puts the warden in front of the HTTP server, starts its loop,
// and watches the configuration. Without -warden-config InferMux is plain
// llama-swap.
//
// InferMux watches llama-swap's files itself rather than through
// -watch-config. llama-swap's reload replaces the server and stops every
// model, which would cut off a reply the user is waiting for, so the reload is
// sent as llama-swap's own SIGHUP once no interactive request is in flight
// (0005). The warden's file and the keys reload at once: that only moves
// settings.
//
// The request goes warden (key, class, never-kill), then the timing of each
// request for a model this host serves (internal/stats), then Codex's catalog,
// then the list cut to the key's allow list, then remote (another host's
// model goes there), then the derived settings on the local models, then a
// WebSocket that names its model in ?model= sent to that model's /upstream/
// path (0018), then llama-swap (0006). In front of all of it, failover sends a request for a
// model in failover.yaml through that chain once per place it lists, until
// one answers (0016).
func startWarden(path, configPath, configDir string, httpServer *http.Server, active func() *server.Server, log *logmon.Monitor) {
	if path == "" {
		return
	}
	cfg, err := warden.LoadConfig(path)
	if err != nil {
		slog.Error("failed to load warden config", "warden-config", path, "error", err)
		os.Exit(1)
	}
	ctx := context.Background()
	local := func(model string) bool {
		_, _, ok := active().QualifyModel(model)
		return ok
	}
	command := func(id string) ([]string, bool) { return active().ModelCommand(id) }
	peerModel := func(id string) (string, string, bool) { return active().PeerModel(id) }
	enriched := catalog.Enrich(stream.Route(httpServer.Handler), &catalog.Deriver{}, command, &catalog.Peers{Lookup: peerModel})
	remotes := &remoteHolder{next: enriched, local: local, log: log}
	if err := remotes.set(ctx, cfg.Remotes); err != nil {
		slog.Error("failed to set up the remote hosts", "warden-config", path, "error", err)
		os.Exit(1)
	}
	w := warden.New(cfg, activeModels{server: active, host: cfg.Host, remote: remotes}, log)
	w.ReportRemotes(func() any { return remotes.get().State() })
	codex, err := catalog.Codex(w.FilterModels(remotes), *flagCodexPrompt)
	if err != nil {
		slog.Error("failed to read Codex's prompt", "codex-prompt", *flagCodexPrompt, "error", err)
		os.Exit(1)
	}
	models := activeModels{server: active, host: cfg.Host, remote: remotes}
	recorder := &stats.Recorder{
		Capacity: 500,
		Model: func(r *http.Request) (string, bool) {
			if _, ok := remotes.get().Qualify(r); ok {
				return "", false // recorded on the host that runs it
			}
			name, _, ok := models.Qualify(r)
			return name, ok
		},
		Client: warden.Client,
	}
	w.Handle("GET /warden/requests", stats.Handler(recorder, cfg.Host, func(ctx context.Context) []stats.Host {
		var hosts []stats.Host
		for _, reply := range remotes.get().Each(ctx, "/warden/requests") {
			h := stats.Host{Host: reply.Host}
			if reply.Err != nil {
				h.Error = reply.Err.Error()
			} else {
				var got struct{ Hosts []stats.Host }
				if err := json.Unmarshal(reply.Body, &got); err != nil || len(got.Hosts) == 0 {
					h.Error = "an answer that is not a list of requests"
				} else {
					h = got.Hosts[0]
				}
			}
			hosts = append(hosts, h)
		}
		return hosts
	}))
	failovers := failover.New(w.Wrap(recorder.Wrap(codex)), cfg.Host, log)
	failovers.Set(cfg.Failover)
	httpServer.Handler = failovers
	w.Start()

	reloadWarden := func() {
		cfg, err := warden.LoadConfig(path)
		if err != nil {
			log.Warnf("Warden settings not reloaded: %v", err)
			return
		}
		if err := remotes.set(ctx, cfg.Remotes); err != nil {
			log.Warnf("Remote hosts not reloaded: %v", err)
		}
		failovers.Set(cfg.Failover)
		w.Reload(cfg)
	}
	go (&configwatcher.Watcher{Path: absolute(path), OnChange: reloadWarden}).Run(ctx)
	if cfg.KeysPath != "" {
		go (&configwatcher.Watcher{Path: absolute(cfg.KeysPath), OnChange: reloadWarden}).Run(ctx)
	}
	if cfg.FailoverPath != "" {
		go (&configwatcher.Watcher{Path: absolute(cfg.FailoverPath), OnChange: reloadWarden}).Run(ctx)
	}

	reload := func() {
		w.WhenNoInteractive("llama-swap config reload", func() {
			// The reload stops every model: an idle session gets 1012 rather
			// than a dropped connection (0018, 7).
			if n := w.CloseSessions(stream.ServiceRestart, "infermux: the model configuration was reloaded"); n > 0 {
				log.Infof("Closed %d idle session(s) for the config reload", n)
			}
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

// remoteHolder is the remote router, replaced when the warden file's
// remotes change. A replaced router's requests finish on it.
type remoteHolder struct {
	next  http.Handler
	local func(string) bool
	log   *logmon.Monitor

	mu     sync.Mutex
	hosts  []warden.Remote
	router *remote.Router
	cancel context.CancelFunc
}

func (h *remoteHolder) get() *remote.Router {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.router
}

func (h *remoteHolder) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	h.get().ServeHTTP(rw, r)
}

func (h *remoteHolder) set(ctx context.Context, hosts []warden.Remote) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.router != nil && slices.Equal(h.hosts, hosts) {
		return nil
	}
	var list []remote.Host
	for _, r := range hosts {
		key := ""
		if r.KeyFile != "" {
			raw, err := os.ReadFile(r.KeyFile)
			if err != nil {
				return err
			}
			key = strings.TrimSpace(string(raw))
		}
		list = append(list, remote.Host{Name: r.Name, URL: r.URL, Key: key})
	}
	router, err := remote.New(list, h.next, h.local, os.Getenv("STATE_DIRECTORY"), h.log)
	if err != nil {
		return err
	}
	if h.cancel != nil {
		h.cancel()
	}
	routerCtx, cancel := context.WithCancel(ctx)
	router.Start(routerCtx)
	h.hosts, h.router, h.cancel = hosts, router, cancel
	return nil
}

func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
