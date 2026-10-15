package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/configstore"
	"github.com/mostlygeek/llama-swap/internal/fetch"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/placement"
	"github.com/mostlygeek/llama-swap/internal/remote"
)

// The one config (0022). With -config-trusted-keys set, this host's models,
// its routes and the other hosts come from the signed config in
// -config-state, and llama-swap's -config-dir holds what is derived from it.
// The keys come from Nix's command line, never from a file the UI writes:
// a config that names its own trusted key proves nothing (0022, 10).
var (
	flagConfigTrustedKeys = flag.String("config-trusted-keys", "", "InferMux: the public keys a config must be signed with, one a line; set, the one config runs this host (0022)")
	flagConfigState       = flag.String("config-state", "", "InferMux: where the signed config is kept; default $STATE_DIRECTORY/config")
	flagConfigSeed        = flag.String("config-seed", "", "InferMux: a signed config to start from, nixcfg's copy; used when newer than the one kept")
	flagCPUSocket         = flag.String("cpu-socket", "", "InferMux: the CPU runner's socket (infermux-cpu serve); set, a CPU device's models run as the runner's children, in its slice (0022)")
	flagFetchDirs         = flag.String("fetch-dirs", "", "InferMux: the directories a missing model file may be fetched into, comma-separated; unset, this host fetches none (0022, 8)")
)

// derivedFile is the one file this host's part becomes in -config-dir.
const derivedFile = "infermux.yaml"

// configSyncEvery is how often each other host's config is compared with
// this one's, as the model lists are polled (remote.PollEvery).
const configSyncEvery = remote.PollEvery

// oneConfig is the daemon's side of the one config.
type oneConfig struct {
	host    string
	options placement.Options
	store   *configstore.Store
	trusted []ed25519.PublicKey
	// base is llama-swap's -config, which a part must load with.
	base       string
	derivedDir string
	log        *logmon.Monitor
	// activate takes a new part: the routes, the other hosts, the devices.
	activate func(*placement.Part)
	// reload is llama-swap's reload once interactive traffic is quiet.
	reload func()
	// hostKey reads this host's key for the other hosts.
	hostKey func() (string, error)
	client  *http.Client
	// fetcher brings the files of this host's models (0022, 8).
	fetcher *fetch.Fetcher

	// applying keeps two configs arriving at once, a Publish and a pull,
	// from running in the order opposite to the store's.
	applying sync.Mutex

	mu       sync.Mutex
	part     *placement.Part
	warnings []string
	rejected *rejection
}

// rejection is the last signed config this host would not run, shown until
// one runs.
type rejection struct {
	Hash  string    `json:"hash"`
	From  string    `json:"from"`
	At    time.Time `json:"at"`
	Error string    `json:"error"`
}

// configFlags is the trusted keys and the store's directory.
func configFlags() ([]ed25519.PublicKey, string, error) {
	trusted, err := configstore.ReadKeys(*flagConfigTrustedKeys)
	if err != nil {
		return nil, "", err
	}
	state := *flagConfigState
	if state == "" {
		dir := os.Getenv("STATE_DIRECTORY")
		if dir == "" {
			return nil, "", errors.New("-config-state is needed outside systemd's StateDirectory")
		}
		state = filepath.Join(dir, "config")
	}
	return trusted, state, nil
}

func openConfig(host, state, base, derivedDir string, trusted []ed25519.PublicKey, log *logmon.Monitor) (*oneConfig, error) {
	if derivedDir == "" {
		return nil, errors.New("-config-dir is needed: it is where this host's part goes for llama-swap")
	}
	store, err := configstore.Open(state, trusted)
	if err != nil {
		if store == nil {
			return nil, err
		}
		// The kept version no longer verifies: run none (0022, 10).
		log.Warnf("Config: %v", err)
	}
	return &oneConfig{host: host, store: store, trusted: trusted, base: base, derivedDir: derivedDir, log: log,
		client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// start runs the version kept, after the seed if that is newer. It returns
// whether llama-swap's file changed, which llama-swap has loaded already.
func (c *oneConfig) start(seed string) bool {
	c.applying.Lock()
	defer c.applying.Unlock()
	if seed != "" {
		if b, err := configstore.ReadBundle(seed); err != nil {
			c.log.Warnf("Config: the seed %s is not a signed config: %v", seed, err)
		} else if res, err := c.acceptLocked(b, "seed", true); err != nil {
			c.log.Warnf("Config: the seed was refused: %v", err)
		} else if res != configstore.Same && res != configstore.Old {
			c.log.Infof("Config: the seed is newer, %s", res)
		}
	}
	c.mu.Lock()
	running := c.part != nil
	c.mu.Unlock()
	if running {
		return c.writeDerived()
	}
	b := c.store.Current()
	if b == nil {
		c.log.Warnf("Config: none signed yet, so this host runs no model of its own until one arrives")
		c.mu.Lock()
		c.activateLocked(&placement.Part{Host: c.host, Routes: map[string][]placement.Route{}, Local: map[string]placement.Local{}, Remotes: map[string]string{}}, nil)
		c.mu.Unlock()
		return c.writeDerived()
	}
	part, warnings, err := c.derive(b)
	if err != nil {
		// Kept and signed, but this build cannot run it: llama-swap keeps the
		// file derived from it before, or none if that did not load either
		// (setAsideDerived).
		c.log.Warnf("Config: revision %d does not load here: %v", b.Info().Revision, err)
		c.mu.Lock()
		c.rejected = &rejection{Hash: b.Hash(), From: "start", At: time.Now(), Error: err.Error()}
		c.mu.Unlock()
		return false
	}
	c.mu.Lock()
	c.activateLocked(part, warnings)
	c.mu.Unlock()
	m := b.Info()
	c.log.Infof("Config: revision %d by %s at %s", m.Revision, m.Host, m.Time.Format(time.RFC3339))
	return c.writeDerived()
}

// current is the part running, nil before start.
func (c *oneConfig) current() *placement.Part {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.part
}

func (c *oneConfig) derive(b *configstore.Bundle) (*placement.Part, []string, error) {
	cfg, warnings, err := placement.Parse(b.Files)
	if err != nil {
		return nil, nil, err
	}
	part, err := cfg.DeriveFor(c.host, c.options)
	if err != nil {
		return nil, nil, err
	}
	if err := part.LoadsWith(c.base, true); err != nil {
		return nil, nil, fmt.Errorf("llama-swap: %w", err)
	}
	for name, l := range part.Local {
		if len(c.options.CPURunner) == 0 && part.Devices[l.Devices[0]].Kind == placement.KindCPU {
			warnings = append(warnings, fmt.Sprintf("%s runs in the daemon's own cgroup, not the idle slice: -cpu-socket is not set", name))
		}
		for _, d := range l.Devices {
			if dev := part.Devices[d]; dev.Kind == placement.KindGPU && dev.GPUIndex() != 0 {
				warnings = append(warnings, fmt.Sprintf("%s is on %s, CUDA index %d, which the warden does not measure: it reads index 0 only", name, d, dev.GPUIndex()))
			}
		}
	}
	sort.Strings(warnings)
	return part, warnings, nil
}

// cpuRunner is what a CPU device's commands start through: infermux-cpu,
// beside this binary, at socket.
func cpuRunner(socket string) ([]string, error) {
	if socket == "" {
		return nil, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(filepath.Dir(exe), "infermux-cpu")
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("-cpu-socket is set, and infermux-cpu is not beside this binary: %w", err)
	}
	return []string{bin, "run", "-socket", socket, "--"}, nil
}

// accept checks a bundle, then hands it to the store, and runs it if the
// store took it. A config this host cannot derive, or whose part llama-swap
// does not load with this host's -config, is refused before the store sees
// it, so it never replaces one that runs.
func (c *oneConfig) accept(b *configstore.Bundle, from string) (configstore.Result, error) {
	c.applying.Lock()
	defer c.applying.Unlock()
	return c.acceptLocked(b, from, false)
}

func (c *oneConfig) acceptLocked(b *configstore.Bundle, from string, seed bool) (configstore.Result, error) {
	if _, err := b.Verify(c.trusted, time.Now()); err != nil {
		return "", err
	}
	part, warnings, err := c.derive(b)
	if err != nil {
		c.mu.Lock()
		c.rejected = &rejection{Hash: b.Hash(), From: from, At: time.Now(), Error: err.Error()}
		c.mu.Unlock()
		return "", fmt.Errorf("signed, but it does not load here: %w", err)
	}
	res, err := c.store.Offer(b, seed)
	if err != nil {
		return "", err
	}
	m := b.Info()
	switch res {
	case configstore.Applied, configstore.Replaced:
		c.mu.Lock()
		c.rejected = nil
		c.activateLocked(part, warnings)
		c.mu.Unlock()
		if res == configstore.Replaced {
			c.log.Warnf("Config: revision %d by %s replaced a version edited apart from it; the other is kept (0022, 6)", m.Revision, m.Host)
			// The host that published the loser may never have seen this
			// one's parent, and would not know it lost: both hosts show it.
			if conflict := c.store.Conflict(); conflict != nil && !seed {
				if lost, err := c.store.Version(conflict.Lost.Hash); err == nil {
					go c.push(lost)
				}
			}
		} else {
			c.log.Infof("Config: revision %d by %s, from %s", m.Revision, m.Host, from)
		}
		if seed {
			// llama-swap has not started yet; start's caller reloads.
			return res, nil
		}
		if c.writeDerived() {
			c.reload()
		}
	case configstore.Lost:
		c.log.Warnf("Config: revision %d by %s from %s was edited apart from the one running and is older; kept, not run (0022, 6)", m.Revision, m.Host, from)
	}
	return res, nil
}

// activateLocked records the part and hands it on; the caller holds c.mu.
func (c *oneConfig) activateLocked(part *placement.Part, warnings []string) {
	c.part, c.warnings = part, warnings
	if c.fetcher != nil {
		c.fetcher.Want(filesOf(part))
	}
	if c.activate != nil {
		c.activate(part)
	}
}

// filesOf is what a part's models need here.
func filesOf(part *placement.Part) []fetch.File {
	seen := map[string]bool{}
	var files []fetch.File
	for _, local := range part.Local {
		for _, p := range local.Files {
			if !seen[p] {
				seen[p] = true
				files = append(files, fetch.File{Path: p, SHA256: part.Files[p].SHA256, HF: part.Files[p].HF})
			}
		}
	}
	return files
}

// fetchDirs is -fetch-dirs, each absolute.
func fetchDirs(flag string) ([]string, error) {
	var dirs []string
	for _, d := range strings.Split(flag, ",") {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			return nil, fmt.Errorf("-fetch-dirs: %s is not absolute", d)
		}
		dirs = append(dirs, filepath.Clean(d))
	}
	return dirs, nil
}

// fetchHosts are the other hosts, asked with this host's key, in name order.
func (c *oneConfig) fetchHosts() []fetch.Host {
	part := c.current()
	if part == nil {
		return nil
	}
	key, err := c.hostKey()
	if err != nil {
		c.log.Warnf("Fetch: the other hosts are asked without a key: %v", err)
	}
	var hosts []fetch.Host
	for name, url := range part.Remotes {
		hosts = append(hosts, fetch.Host{Name: name, URL: url, Key: key})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	return hosts
}

// listed is whether the config names path, the only files this host serves.
func (c *oneConfig) listed(path string) bool {
	part := c.current()
	if part == nil {
		return false
	}
	_, ok := part.Files[path]
	return ok
}

// localFiles are the paths a model of this host needs, by its name here.
func (c *oneConfig) localFiles(model string) []string {
	part := c.current()
	if part == nil {
		return nil
	}
	return part.Local[model].Files
}

// writeDerived writes llama-swap's file when it changed, and says so.
func (c *oneConfig) writeDerived() bool {
	var content []byte
	c.mu.Lock()
	if c.part != nil {
		content = c.part.LlamaSwap
	}
	c.mu.Unlock()
	if content == nil {
		content = []byte("# Derived by InferMux: no signed config yet, so no model of this host's own (0022).\n")
	}
	file := filepath.Join(c.derivedDir, derivedFile)
	if old, err := os.ReadFile(file); err == nil && bytes.Equal(old, content) {
		return false
	}
	if err := os.MkdirAll(c.derivedDir, 0o755); err != nil {
		c.log.Warnf("Config: %v", err)
		return false
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		c.log.Warnf("Config: %v", err)
		return false
	}
	// The temporary file is not .yaml, so llama-swap never reads half of it.
	if err := os.Rename(tmp, file); err != nil {
		c.log.Warnf("Config: %v", err)
		return false
	}
	return true
}

// refusedSuffix is the derived file's when it was moved aside at start. Not
// .yaml, so llama-swap no longer reads it.
const refusedSuffix = ".refused"

// setAsideDerived moves the derived file out of llama-swap's way, before
// llama-swap loads its config, when the file does not load with base and
// base loads alone (the user's decision of 2026-10-15; 0022, 11). An earlier
// binary wrote it, or it reads an ${env.VAR} the daemon's environment no
// longer holds, and llama-swap exits on a config that does not load, so the
// unit would never start. The daemon then starts with base alone, no model
// of this host's own, and start derives the kept version again. It does
// nothing outside the one config: there -config-dir is the user's.
func setAsideDerived(base, dir string) {
	if *flagConfigTrustedKeys == "" || dir == "" || base == "" {
		return
	}
	file := filepath.Join(dir, derivedFile)
	if _, err := os.Stat(file); err != nil {
		return
	}
	_, loadErr := config.LoadConfigSources(base, dir)
	if loadErr == nil {
		return
	}
	// A base that does not load stops llama-swap with or without the file,
	// and says why.
	if _, err := config.LoadConfigSources(base, ""); err != nil {
		return
	}
	if err := os.Rename(file, file+refusedSuffix); err != nil {
		slog.Error("InferMux: the derived file does not load and could not be moved aside", "file", file, "error", err)
		return
	}
	slog.Warn("InferMux: the derived file does not load, moved aside; starting with -config alone until a config derives here", "file", file+refusedSuffix, "error", loadErr)
}

// configState is GET /warden/config/state: what runs here and why not
// something else.
type configState struct {
	Host     string                       `json:"host"`
	Current  *configstore.Summary         `json:"current"`
	Conflict *configstore.Conflict        `json:"conflict"`
	Rejected *rejection                   `json:"rejected"`
	Warnings []string                     `json:"warnings"`
	Routes   map[string][]placement.Route `json:"routes"`
	Local    map[string]placement.Local   `json:"local"`
	Files    []fetch.Status               `json:"files"`
}

func (c *oneConfig) state() configState {
	s := configState{Host: c.host, Conflict: c.store.Conflict(), Files: []fetch.Status{}}
	if c.fetcher != nil {
		s.Files = c.fetcher.Status()
	}
	if b := c.store.Current(); b != nil {
		m := b.Info()
		s.Current = &configstore.Summary{Hash: b.Hash(), Revision: m.Revision, Time: m.Time, Host: m.Host}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s.Rejected, s.Warnings = c.rejected, c.warnings
	if c.part != nil {
		s.Routes, s.Local = c.part.Routes, c.part.Local
	}
	if s.Warnings == nil {
		s.Warnings = []string{}
	}
	return s
}

// handle adds the config's routes to the warden's API, behind its keys and
// its write guard. Any key may offer a config: the signature is what makes
// it trusted, so the host key carries one without being able to write one
// (0022, 10).
func (c *oneConfig) handle(handle func(string, http.Handler)) {
	// Another host fetching a model's file (0022, 8).
	handle("GET /warden/files/{path...}", fetch.Handler(c.listed))
	handle("GET /warden/config", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b := c.store.Current()
		if b == nil {
			writeJSONResponse(rw, http.StatusNotFound, map[string]string{"detail": "no signed config yet"})
			return
		}
		writeJSONResponse(rw, http.StatusOK, b.Wire())
	}))
	handle("GET /warden/config/state", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		writeJSONResponse(rw, http.StatusOK, c.state())
	}))
	handle("GET /warden/config/version/{hash}", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, err := c.store.Version(r.PathValue("hash"))
		if err != nil {
			writeJSONResponse(rw, http.StatusNotFound, map[string]string{"detail": err.Error()})
			return
		}
		writeJSONResponse(rw, http.StatusOK, b.Wire())
	}))
	handle("POST /warden/config", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var w configstore.Wire
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&w); err != nil {
			writeJSONResponse(rw, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		b, err := w.Bundle()
		if err != nil {
			writeJSONResponse(rw, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		from := "a client"
		if r.Header.Get(remote.HopHeader) != "" {
			from = "another host"
		}
		res, err := c.accept(b, from)
		if err != nil {
			writeJSONResponse(rw, http.StatusUnprocessableEntity, map[string]string{"detail": err.Error()})
			return
		}
		// A config published here goes to every other host; one pushed from
		// another host has been sent everywhere by that host.
		if r.Header.Get(remote.HopHeader) == "" && (res == configstore.Applied || res == configstore.Replaced) {
			go c.push(b)
		}
		writeJSONResponse(rw, http.StatusOK, map[string]any{"result": res, "state": c.state()})
	}))
	handle("DELETE /warden/config/conflict", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if err := c.store.Dismiss(); err != nil {
			writeJSONResponse(rw, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
			return
		}
		// Both hosts show the warning; dismissing it once clears both.
		if r.Header.Get(remote.HopHeader) == "" {
			go c.eachRemote(func(name, url, key string) error {
				_, err := c.call(http.MethodDelete, url+"/warden/config/conflict", key, nil)
				return err
			})
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
}

// push sends a config published here to every other host.
func (c *oneConfig) push(b *configstore.Bundle) {
	body, err := json.Marshal(b.Wire())
	if err != nil {
		return
	}
	c.eachRemote(func(name, url, key string) error {
		if _, err := c.call(http.MethodPost, url+"/warden/config", key, body); err != nil {
			return fmt.Errorf("push: %w", err)
		}
		c.log.Infof("Config: revision %d pushed to %s", b.Info().Revision, name)
		return nil
	})
}

// sync compares each other host's config with this one's every
// configSyncEvery and fetches one that is newer, so a host that was down
// for a Publish catches up (0022, 3).
func (c *oneConfig) sync(ctx context.Context) {
	ticker := time.NewTicker(configSyncEvery)
	defer ticker.Stop()
	for {
		c.eachRemote(func(name, url, key string) error {
			return c.pull(name, url, key)
		})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *oneConfig) pull(name, url, key string) error {
	raw, err := c.call(http.MethodGet, url+"/warden/config/state", key, nil)
	if err != nil {
		return nil // offline: the model list's poll reports it
	}
	var theirs configState
	if err := json.Unmarshal(raw, &theirs); err != nil || theirs.Current == nil {
		return nil
	}
	if mine, ok := c.store.CurrentInfo(); ok {
		if !configstore.Newer(configstore.Manifest{Time: theirs.Current.Time, Host: theirs.Current.Host}, mine) {
			return nil
		}
	}
	c.mu.Lock()
	refused := c.rejected != nil && c.rejected.Hash == theirs.Current.Hash
	c.mu.Unlock()
	if refused {
		return nil
	}
	raw, err = c.call(http.MethodGet, url+"/warden/config", key, nil)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	var w configstore.Wire
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	b, err := w.Bundle()
	if err != nil {
		return err
	}
	if _, err := c.accept(b, name); err != nil {
		return fmt.Errorf("revision %d: %w", theirs.Current.Revision, err)
	}
	return nil
}

// eachRemote runs fn for every other host, at once, and logs what fails.
func (c *oneConfig) eachRemote(fn func(name, url, key string) error) {
	c.mu.Lock()
	var remotes map[string]string
	if c.part != nil {
		remotes = c.part.Remotes
	}
	c.mu.Unlock()
	if len(remotes) == 0 || c.hostKey == nil {
		return
	}
	key, err := c.hostKey()
	if err != nil {
		c.log.Warnf("Config: this host's key: %v", err)
		return
	}
	names := make([]string, 0, len(remotes))
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(name, strings.TrimRight(remotes[name], "/"), key); err != nil {
				c.log.Warnf("Config: %s: %v", name, err)
			}
		}()
	}
	wg.Wait()
}

// call is one request to another host's warden API, as a host: its own key,
// the header a /warden/ write needs, and the hop mark, so the host there
// does not pass it on.
func (c *oneConfig) call(method, url, key string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-InferMux", "1")
	req.Header.Set(remote.HopHeader, "config")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var detail struct{ Detail string }
		if json.Unmarshal(raw, &detail) == nil && detail.Detail != "" {
			return nil, fmt.Errorf("%s: %s", resp.Status, detail.Detail)
		}
		return nil, fmt.Errorf("%s", resp.Status)
	}
	return raw, nil
}

func writeJSONResponse(rw http.ResponseWriter, status int, body any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	enc := json.NewEncoder(rw)
	enc.SetIndent("", "  ")
	enc.Encode(body)
}
