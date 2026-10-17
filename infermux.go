package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mostlygeek/llama-swap/internal/catalog"
	"github.com/mostlygeek/llama-swap/internal/cpurun"
	"github.com/mostlygeek/llama-swap/internal/drain"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/failover"
	"github.com/mostlygeek/llama-swap/internal/fetch"
	"github.com/mostlygeek/llama-swap/internal/jobs"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/placement"
	"github.com/mostlygeek/llama-swap/internal/process"
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

var flagJobsFile = flag.String("jobs-file", "", "InferMux: where the unfinished jobs are written on shutdown and read back at start (0023); default $STATE_DIRECTORY/jobs.json")

// activeModels reaches the models through whichever server is active. A config
// reload replaces the server; the warden, like perfMon, outlives it.
type activeModels struct {
	server func() *server.Server
	host   string
	remote *remoteHolder
}

func (a activeModels) Running() map[string]string { return a.server().RunningModelStates() }
func (a activeModels) UnloadAll() []string        { return a.server().UnloadAllModels() }
func (a activeModels) Unload(ids []string) []string {
	return a.server().UnloadModels(ids)
}

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
// request for a model this host serves (internal/stats), then admission
// (0019), then, under the one config, a 503 for a local model whose files are
// still being fetched (internal/fetch), then Codex's catalog,
// then the list cut to the key's allow list, then remote (another host's
// model goes there), then the derived settings on the local models, then a
// WebSocket that names its model in ?model= sent to that model's /upstream/
// path (0018), then llama-swap (0006). In front of all of it, failover sends a request for a
// model in failover.yaml through that chain once per place it lists, until
// one answers (0016).
//
// A job (0023) sends each attempt through the whole of it, failover first,
// as a client would. The returned func writes the unfinished jobs to disk,
// then ends the goroutines started here, the warden's loop last. It is
// called on a shutdown signal before the server stops.
func startWarden(path, configPath, configDir string, httpServer *http.Server, active func() *server.Server, log *logmon.Monitor) (stop func()) {
	if path == "" {
		return func() {}
	}
	cfg, err := warden.LoadConfig(path)
	if err != nil {
		slog.Error("failed to load warden config", "warden-config", path, "error", err)
		os.Exit(1)
	}
	var one *oneConfig
	if *flagConfigTrustedKeys != "" {
		if err := cfg.CheckOneConfig(); err != nil {
			slog.Error("the warden config does not fit the one config", "warden-config", path, "error", err)
			os.Exit(1)
		}
		trusted, state, err := configFlags()
		if err == nil {
			one, err = openConfig(cfg.Host, state, configPath, configDir, trusted, log)
		}
		if err == nil {
			one.options.CPURunner, err = cpuRunner(*flagCPUSocket)
		}
		var dirs []string
		if err == nil {
			dirs, err = fetchDirs(*flagFetchDirs)
		}
		if err == nil {
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.ResponseHeaderTimeout = 30 * time.Second
			one.fetcher = &fetch.Fetcher{Dirs: dirs, Hosts: one.fetchHosts, Client: &http.Client{Transport: transport}, Logf: log.Infof}
		}
		if err != nil {
			slog.Error("failed to open the signed config", "error", err)
			os.Exit(1)
		}
	}
	// Cancelled by the returned stop: the remote hosts' polls, the config
	// watchers and the one config's sync and fetches end with it.
	ctx, cancel := context.WithCancel(context.Background())
	local := func(model string) bool {
		_, _, ok := active().QualifyModel(model)
		return ok
	}
	command := func(id string) ([]string, bool) { return active().ModelCommand(id) }
	peerModel := func(id string) (string, string, bool) { return active().PeerModel(id) }
	// Mark is next to llama-swap, so its marker tells failover on any host
	// that this host's llama-swap never sent the request to a model (0016, 3).
	enriched := catalog.Enrich(stream.Route(failover.Mark(httpServer.Handler)), &catalog.Deriver{}, command, &catalog.Peers{Lookup: peerModel})
	remotes := &remoteHolder{next: enriched, local: local, log: log}
	keyFile := &keyFileHolder{file: cfg.RemoteKeyFile}
	// Under the one config, the other hosts arrive with its part (one.start).
	if err := remotes.set(ctx, cfg.Remotes); err != nil {
		slog.Error("failed to set up the remote hosts", "warden-config", path, "error", err)
		os.Exit(1)
	}
	// The CPU runner's servers are InferMux's own work (0022): its unit is
	// added to our_units wherever the warden reads them.
	runner := &runnerUnit{}
	if one != nil && *flagCPUSocket != "" {
		runner.ask(*flagCPUSocket)
	}
	w := warden.NewWithProbes(runner.add(cfg), activeModels{server: active, host: cfg.Host, remote: remotes}, log, probes)
	// A model that stops before it is ready, most often a start that failed,
	// leaves a line saying what held the card (0016, 3). Any end of a start
	// frees the GPU's start slot for the next (0029).
	unsubscribe := event.On(func(e swaputil.ProcessStateChangeEvent) {
		if e.OldState != string(process.StateStarting) {
			return
		}
		w.StartEnded(e.ProcessName)
		if e.NewState == string(process.StateStopped) {
			go w.NotStarted(e.ProcessName)
		}
	})
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
	admission := &failover.Admission{Model: func(r *http.Request) (string, bool, bool) {
		if _, ok := remotes.get().Qualify(r); ok {
			return "", false, false
		}
		name, local, ok := models.Qualify(r)
		return name, local && w.OnGPU(name), ok
	}, Yielded: w.YieldedFor}
	if one != nil {
		one.handle(w.Handle)
	}
	jobStore := &jobs.Store{File: jobsFile(), KeysRequired: w.KeysRequired, Logf: log.Infof}
	w.Handle("/warden/jobs", jobStore.Handler())
	w.Handle("/warden/jobs/", jobStore.Handler())
	w.OnResume(jobStore.Wake)
	w.ReportJobs(func() any { return jobStore.Summary() })
	var served http.Handler = codex
	if one != nil {
		// A local model whose files are still coming is a place that is
		// down, so the failover handler moves on (0022, 8).
		served = (&fetch.Gate{Model: func(r *http.Request) (string, bool) {
			if _, ok := remotes.get().Qualify(r); ok {
				return "", false
			}
			name, local, ok := models.Qualify(r)
			return strings.TrimPrefix(name, cfg.Host+"/"), ok && local
		}, Files: one.localFiles, Fetcher: one.fetcher}).Wrap(codex)
	}
	failovers := failover.New(w.Wrap(recorder.Wrap(admission.Wrap(served))), cfg.Host, log)
	failovers.Batch = w.IsBatch
	jobStore.Serve = failovers
	setFailover := func(routes warden.Failover) {
		places := make(map[string][]failover.Place, len(routes))
		for model, route := range routes {
			for _, p := range route {
				places[model] = append(places[model], failover.Place{Place: p.Place, MaxInflight: p.MaxInflight, OnlyIfIdle: p.OnlyIfIdle, BatchOnly: p.BatchOnly})
			}
		}
		failovers.Set(places)
	}
	reload := func() {
		w.WhenNoInteractive("llama-swap config reload", func() {
			// The reload stops every model: an idle session gets 1012 rather
			// than a dropped connection (0018, 7).
			if n := w.CloseSessions(stream.ServiceRestart, "infermux: the model configuration was reloaded"); n > 0 {
				log.Infof("Closed %d idle session(s) for the config reload", n)
			}
			reloadSignal()
		})
	}
	if one != nil {
		one.reload = reload
		one.hostKey = func() (string, error) { return readKey(keyFile.get()) }
		// activate runs under one's lock, so it must not call back into one.
		one.activate = func(part *placement.Part) {
			if err := remotes.set(ctx, remotesOf(part, keyFile.get())); err != nil {
				log.Warnf("Remote hosts not set: %v", err)
			}
			failovers.Set(routesOf(part))
			w.SetPlacement(placedOf(part))
			// New places may take a job every old one refused.
			jobStore.Wake()
		}
		if one.start(*flagConfigSeed) {
			// llama-swap loaded -config-dir before the warden started, and
			// catches SIGHUP only from just before it serves: Serve calls
			// BaseContext, so the reload waits for that.
			var once sync.Once
			httpServer.BaseContext = func(net.Listener) context.Context {
				once.Do(func() { go reload() })
				return context.Background()
			}
		}
		go one.sync(ctx)
		go one.fetcher.Run(ctx)
	} else {
		setFailover(cfg.Failover)
	}
	// Before the first request and the first job: the gate's fit check, and
	// a daemon started at night starts off (0025).
	gateWarden.Store(w)
	if *flagOffFile != "" {
		watchOff(ctx, *flagOffFile, w.SetOff)
	}
	// Outermost, so a deploy's stop refuses every new request before
	// anything starts work for it (0026).
	drains := &drain.Drain{RetryAfter: drainRetryAfter}
	httpServer.Handler = drains.Wrap(failovers)
	w.Start()
	stopControl := func() {}
	if *flagControlSocket != "" {
		stopControl, err = serveControl(*flagControlSocket, *flagControlGroup, w.LocalControl())
		if err != nil {
			slog.Error("failed to open the control socket", "control-socket", *flagControlSocket, "error", err)
			os.Exit(1)
		}
	}
	jobStore.Start()

	reloadWarden := func() {
		cfg, err := warden.LoadConfig(path)
		if err != nil {
			log.Warnf("Warden settings not reloaded: %v", err)
			return
		}
		if one != nil {
			if err := cfg.CheckOneConfig(); err != nil {
				log.Warnf("Warden settings not reloaded: %v", err)
				return
			}
			keyFile.set(cfg.RemoteKeyFile)
			err = remotes.set(ctx, remotesOf(one.current(), keyFile.get()))
		} else {
			setFailover(cfg.Failover)
			err = remotes.set(ctx, cfg.Remotes)
		}
		if err != nil {
			log.Warnf("Remote hosts not reloaded: %v", err)
		}
		w.Reload(runner.add(cfg))
	}
	if runner.waiting() {
		go runner.retry(ctx, reloadWarden, log)
	}
	go (&configwatcher.Watcher{Path: absolute(path), OnChange: reloadWarden}).Run(ctx)
	if cfg.KeysPath != "" {
		go (&configwatcher.Watcher{Path: absolute(cfg.KeysPath), OnChange: reloadWarden}).Run(ctx)
	}
	if cfg.FailoverPath != "" {
		go (&configwatcher.Watcher{Path: absolute(cfg.FailoverPath), OnChange: reloadWarden}).Run(ctx)
	}

	if configPath != "" {
		go (&configwatcher.Watcher{Path: absolute(configPath), OnChange: reload}).Run(ctx)
	}
	// Under the one config, -config-dir is written here and reloaded as it
	// is written.
	if configDir != "" && one == nil {
		go (&configwatcher.DirWatcher{Path: absolute(configDir), OnChange: reload}).Run(ctx)
	}
	return func() {
		// A stop lets the requests and job attempts running finish first
		// (0026), with the local models off too: off has cut every local
		// request already, and the zbox's controller kills the models itself.
		drains.Start()
		jobStore.Drain()
		if n, a := drains.Running(), jobStore.Attempting(); n > 0 || a > 0 {
			log.Infof("Draining for up to %s: %d request(s), %d job attempt(s) running", drainTimeout, n, a)
			if drain.Wait(drainTimeout, drains.Running, jobStore.Attempting) {
				log.Infof("Drained")
			} else {
				log.Warnf("Drain ended with %d request(s), %d job attempt(s) still running: jobs pause, the rest are cut", drains.Running(), jobStore.Attempting())
			}
		}
		unsubscribe()
		stopControl()
		if err := jobStore.Stop(10 * time.Second); err != nil {
			log.Warnf("Jobs not written: %v", err)
		}
		// An idle session gets 1012 rather than a dropped connection (0018,
		// 7), and then every request left ends.
		w.CloseSessions(stream.ServiceRestart, "infermux: restarting")
		drains.Cut()
		cancel()
		// Waits for a tick in flight, after the jobs are written.
		w.Stop()
	}
}

// probes are the warden's readings of the GPU and the CPU: the host's own,
// NVML and /proc, unless a test sets its own.
var probes warden.Probes

// A deploy's stop waits up to drainTimeout for the requests running (0026).
// The unit's TimeoutStopSec is above it, the jobs' 10 s and llama-swap's own
// 35 s (nix/module.nix).
var drainTimeout = 40 * time.Second

// drainRetryAfter is the Retry-After of a request refused while draining:
// about how long a restart takes.
const drainRetryAfter = 5

// jobsFile is -jobs-file, or jobs.json in systemd's StateDirectory. Outside
// both, an unfinished job is lost on shutdown, and the log says so.
func jobsFile() string {
	if *flagJobsFile != "" {
		return *flagJobsFile
	}
	if dir := os.Getenv("STATE_DIRECTORY"); dir != "" {
		return filepath.Join(dir, "jobs.json")
	}
	return ""
}

// placedOf is each local model's devices as the warden measures them, the
// CPU and the GPU at CUDA index 0, which is the one NVML reads (0022), and
// its VRAM need and group. A model on another GPU is on none; derive warns
// about it.
func placedOf(part *placement.Part) warden.Placement {
	p := warden.Placement{}
	for name, l := range part.Local {
		on := []string{}
		for _, d := range l.Devices {
			dev := part.Devices[d]
			var w string
			switch {
			case dev.Kind == placement.KindCPU:
				w = warden.DeviceCPU
			case dev.Kind == placement.KindGPU && dev.GPUIndex() == 0:
				w = warden.DeviceGPU
			}
			if w != "" && !slices.Contains(on, w) {
				on = append(on, w)
			}
		}
		p[name] = warden.Placed{Devices: on, VRAMMB: l.VRAMMB, Together: l.Together}
	}
	return p
}

// runnerUnit is the CPU runner's unit, read over its socket. The runner may
// start after the daemon, so it is asked again until it answers.
type runnerUnit struct {
	mu     sync.Mutex
	socket string
	unit   string
}

// ask reads the unit once.
func (u *runnerUnit) ask(socket string) {
	unit, _ := cpurun.Unit(socket)
	u.mu.Lock()
	u.socket, u.unit = socket, unit
	u.mu.Unlock()
}

func (u *runnerUnit) waiting() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.socket != "" && u.unit == ""
}

// retry asks every 5 s until the runner answers, then calls found.
func (u *runnerUnit) retry(ctx context.Context, found func(), log *logmon.Monitor) {
	for u.waiting() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		u.ask(u.socket)
	}
	log.Infof("The CPU runner is %s, counted as InferMux's own", u.unit)
	found()
}

// add puts the runner's unit in cfg's our_units, once it is known.
func (u *runnerUnit) add(cfg warden.Config) warden.Config {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.unit != "" && !slices.Contains(cfg.OurUnits, u.unit) {
		cfg.OurUnits = append(slices.Clone(cfg.OurUnits), u.unit)
	}
	return cfg
}

// remotesOf is the other hosts of a part, read with this host's key.
func remotesOf(part *placement.Part, keyFile string) []warden.Remote {
	if part == nil {
		return nil
	}
	var list []warden.Remote
	for name, url := range part.Remotes {
		list = append(list, warden.Remote{Name: name, URL: url, KeyFile: keyFile})
	}
	slices.SortFunc(list, func(a, b warden.Remote) int { return strings.Compare(a.Name, b.Name) })
	return list
}

// routesOf is the failover handler's table from a part.
func routesOf(part *placement.Part) map[string][]failover.Place {
	places := make(map[string][]failover.Place, len(part.Routes))
	for model, routes := range part.Routes {
		for _, r := range routes {
			places[model] = append(places[model], failover.Place{Place: r.Place, MaxInflight: r.MaxInflight, OnlyIfIdle: r.OnlyIfIdle, BatchOnly: r.BatchOnly, Failover: r.Failover})
		}
	}
	return places
}

// keyFileHolder is remote_key_file, which a warden reload may change.
type keyFileHolder struct {
	mu   sync.Mutex
	file string
}

func (k *keyFileHolder) get() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.file
}

func (k *keyFileHolder) set(file string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.file = file
}

func readKey(file string) (string, error) {
	if file == "" {
		return "", errors.New("remote_key_file is not set")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// reloadSignal is llama-swap's own reload. A test replaces it.
var reloadSignal = func() { syscall.Kill(os.Getpid(), syscall.SIGHUP) }

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
