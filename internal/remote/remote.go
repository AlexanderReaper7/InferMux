// Package remote routes a request for another host's model to that host
// (0006, 2). Each host is a front door: it lists the other hosts' local
// models as <host>/<model>, learned from their /v1/models, and forwards a
// request for one with the client's own key, so the warden there sees the
// real class.
//
// It sits between the warden and llama-swap. It is not llama-swap's peers:
// a peer list changes only through a config reload, which stops every model.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// HopHeader marks a forwarded request. A request carrying it is served where
// it lands and never forwarded again, so two hosts with a stale list each
// cannot pass one request back and forth.
const HopHeader = "X-InferMux-Hop"

// PollEvery is how often a host's list is read while nothing asks sooner.
const PollEvery = 30 * time.Second

// Host is another InferMux: its name, its HTTPS address, and this host's
// key there, for reading the list.
type Host struct {
	Name string
	URL  string
	Key  string
}

// Logger is the part of llama-swap's proxy log this writes to.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Router answers for the other hosts' models and hands everything else to
// next. Local is true for a name this host's llama-swap resolves itself
// (a local model, an alias, or a peer such as OpenRouter); local names win
// over an unqualified remote one.
type Router struct {
	next     http.Handler
	local    func(model string) bool
	stateDir string
	log      Logger
	client   *http.Client

	mu    sync.Mutex
	hosts map[string]*host
	names []string
}

type host struct {
	Host
	target *url.URL
	proxy  *httputil.ReverseProxy
	repoll chan struct{}
	models []string
	// facts is each model's meta.infermux as that host derived it: its
	// context window, input and reasoning efforts (internal/catalog).
	facts   map[string]json.RawMessage
	online  bool
	checked time.Time
	lastErr string
}

// New builds a router over hosts. stateDir keeps each host's last list, so a
// restart while a host is down does not forget its models (0006, 3); empty
// keeps nothing.
func New(hosts []Host, next http.Handler, local func(string) bool, stateDir string, log Logger) (*Router, error) {
	rt := &Router{
		next:     next,
		local:    local,
		stateDir: stateDir,
		log:      log,
		client:   &http.Client{Timeout: 10 * time.Second},
		hosts:    map[string]*host{},
	}
	for _, h := range hosts {
		target, err := url.Parse(strings.TrimRight(h.URL, "/"))
		if err != nil || target.Host == "" {
			return nil, fmt.Errorf("remote %s: %q is not a URL", h.Name, h.URL)
		}
		hs := &host{Host: h, target: target, repoll: make(chan struct{}, 1)}
		hs.models, hs.facts = rt.loadState(h.Name)
		hs.proxy = rt.newProxy(hs)
		rt.hosts[h.Name] = hs
		rt.names = append(rt.names, h.Name)
	}
	slices.Sort(rt.names)
	return rt, nil
}

// Start polls every host until ctx ends.
func (rt *Router) Start(ctx context.Context) {
	for _, name := range rt.names {
		go rt.watch(ctx, rt.hosts[name])
	}
}

func (rt *Router) watch(ctx context.Context, h *host) {
	ticker := time.NewTicker(PollEvery)
	defer ticker.Stop()
	for {
		rt.Poll(h.Name)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-h.repoll:
		}
	}
}

// Poll reads one host's list now.
func (rt *Router) Poll(name string) {
	h := rt.hosts[name]
	models, facts, err := rt.fetch(h)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	wasOnline, first := h.online, h.checked.IsZero()
	h.checked = time.Now()
	if err != nil {
		h.online, h.lastErr = false, err.Error()
		if wasOnline || first {
			rt.log.Warnf("Remote %s offline, keeping its %d models listed: %v", name, len(h.models), err)
		}
		return
	}
	h.online, h.lastErr = true, ""
	if !wasOnline {
		rt.log.Infof("Remote %s online, %d models", name, len(models))
	}
	if !slices.Equal(models, h.models) || !maps.EqualFunc(facts, h.facts, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) }) {
		h.models, h.facts = models, facts
		rt.saveState(name, models, facts)
	}
}

// fetch reads the host's own local models: llama-swap marks them type
// "model", its peers "peer", and the remote models this package adds
// "remote". Taking only the first is what keeps two hosts from re-exporting
// each other's lists.
func (rt *Router) fetch(h *host) ([]string, map[string]json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodGet, h.target.String()+"/v1/models", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Key)
	resp, err := rt.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("/v1/models answered %s", resp.Status)
	}
	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				LlamaSwap struct {
					Type string `json:"type"`
				} `json:"llamaswap"`
				InferMux json.RawMessage `json:"infermux"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, nil, fmt.Errorf("/v1/models: %w", err)
	}
	models := []string{}
	facts := map[string]json.RawMessage{}
	for _, m := range list.Data {
		if m.Meta.LlamaSwap.Type == "model" && m.ID != "" {
			models = append(models, m.ID)
			if len(m.Meta.InferMux) > 0 {
				facts[m.ID] = m.Meta.InferMux
			}
		}
	}
	slices.Sort(models)
	return slices.Compact(models), facts, nil
}

// Target is where a request goes: the host and the model's name there. False
// for a request this host serves itself.
func (rt *Router) Target(r *http.Request) (hostName, model string, ok bool) {
	if r.Header.Get(HopHeader) != "" {
		return "", "", false
	}
	if rest, found := strings.CutPrefix(r.URL.Path, "/upstream/"); found {
		name, after, _ := strings.Cut(rest, "/")
		if _, known := rt.hosts[name]; !known {
			return "", "", false
		}
		model, _, _ = strings.Cut(after, "/")
		return name, model, model != ""
	}
	requested, err := swaputil.ExtractModel(r)
	if err != nil || requested == "" {
		return "", "", false
	}
	return rt.resolve(requested)
}

// resolve is a model name to a host: <host>/<model> for a host this one
// knows, or an unqualified name no local model, alias or peer takes and
// exactly one host lists.
func (rt *Router) resolve(requested string) (hostName, model string, ok bool) {
	if name, rest, found := strings.Cut(requested, "/"); found {
		if _, known := rt.hosts[name]; known && rest != "" {
			return name, rest, true
		}
	}
	if rt.local(requested) {
		return "", "", false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, name := range rt.names {
		if slices.Contains(rt.hosts[name].models, requested) {
			if ok {
				return "", "", false // on two hosts: the client has to say which
			}
			hostName, model, ok = name, requested, true
		}
	}
	return hostName, model, ok
}

// Qualify is the name a key's allow list sees for a remote model.
func (rt *Router) Qualify(r *http.Request) (string, bool) {
	name, model, ok := rt.Target(r)
	if !ok {
		return "", false
	}
	return name + "/" + model, true
}

// QualifyName is Qualify for a model as /v1/models lists it.
func (rt *Router) QualifyName(requested string) (string, bool) {
	name, model, ok := rt.resolve(requested)
	if !ok {
		return "", false
	}
	return name + "/" + model, true
}

func (rt *Router) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		rt.listModels(rw, r)
		return
	}
	name, model, ok := rt.Target(r)
	if !ok {
		rt.next.ServeHTTP(rw, r)
		return
	}
	h := rt.hosts[name]
	rt.mu.Lock()
	offline := !h.online && !h.checked.IsZero()
	rt.mu.Unlock()
	if offline {
		// Fail at once, and look again, so the first request after it is
		// back gets through (0006, 3).
		rt.repoll(h)
		swaputil.SendResponse(rw, r, http.StatusBadGateway, fmt.Sprintf("%s is offline", name))
		return
	}
	out, err := rewrite(r, name, model)
	if err != nil {
		swaputil.SendResponse(rw, r, http.StatusBadRequest, err.Error())
		return
	}
	h.proxy.ServeHTTP(rw, out)
}

// rewrite gives the request the model's name on the other host.
func rewrite(r *http.Request, hostName, model string) (*http.Request, error) {
	out := r.Clone(r.Context())
	out.Header.Set(HopHeader, "1")
	if rest, found := strings.CutPrefix(r.URL.Path, "/upstream/"+hostName+"/"); found {
		out.URL.Path = "/upstream/" + rest
		out.URL.RawPath = ""
		return out, nil
	}
	out.Body = r.Body
	requested, err := swaputil.ExtractModel(out)
	if err != nil {
		return nil, err
	}
	if requested == model {
		return out, nil
	}
	return swaputil.ReplaceRequestModel(out, requested, model)
}

func (rt *Router) newProxy(h *host) *httputil.ReverseProxy {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(h.target)
			pr.SetXForwarded()
			// This host's guard has checked the browser's origin. Passed on,
			// it would be another origin than the host it now goes to.
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
		},
		Transport: transport,
		// A stream is handed on as it comes, chunk by chunk.
		FlushInterval: -1,
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the client left, or the warden cancelled it
			}
			rt.log.Warnf("Remote %s: %v", h.Name, err)
			rt.repoll(h)
			swaputil.SendResponse(rw, r, http.StatusBadGateway, fmt.Sprintf("%s did not answer: %v", h.Name, err))
		},
	}
}

func (rt *Router) repoll(h *host) {
	select {
	case h.repoll <- struct{}{}:
	default:
	}
}

// listModels is llama-swap's /v1/models with every host's models added.
func (rt *Router) listModels(rw http.ResponseWriter, r *http.Request) {
	rec := &recorder{header: http.Header{}, code: http.StatusOK}
	rt.next.ServeHTTP(rec, r)
	var list map[string]any
	if rec.code != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil {
		rec.replay(rw)
		return
	}
	data, _ := list["data"].([]any)
	rt.mu.Lock()
	for _, name := range rt.names {
		h := rt.hosts[name]
		for _, model := range h.models {
			infermux := map[string]any{}
			json.Unmarshal(h.facts[model], &infermux)
			infermux["host"], infermux["online"] = name, h.online || h.checked.IsZero()
			data = append(data, map[string]any{
				"id":       name + "/" + model,
				"object":   "model",
				"owned_by": "infermux",
				"name":     model + " on " + name,
				"meta": map[string]any{
					"llamaswap": map[string]any{"type": "remote", "host": name},
					"infermux":  infermux,
				},
			})
		}
	}
	rt.mu.Unlock()
	list["data"] = data
	for k, v := range rec.header {
		if k != "Content-Length" {
			rw.Header()[k] = v
		}
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(list)
}

// HostState is one host as /warden/verdict's neighbours would report it.
type HostState struct {
	Name    string     `json:"name"`
	URL     string     `json:"url"`
	Online  bool       `json:"online"`
	Checked *time.Time `json:"checked"`
	Error   string     `json:"error,omitempty"`
	Models  []string   `json:"models"`
}

// State is every host, for the UI.
func (rt *Router) State() []HostState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := []HostState{}
	for _, name := range rt.names {
		h := rt.hosts[name]
		s := HostState{Name: name, URL: h.URL, Online: h.online, Error: h.lastErr, Models: slices.Clone(h.models)}
		if !h.checked.IsZero() {
			checked := h.checked
			s.Checked = &checked
		}
		out = append(out, s)
	}
	return out
}

func (rt *Router) statePath(name string) string {
	return filepath.Join(rt.stateDir, "remote-"+name+".json")
}

// state is what is kept of a host between restarts. A file from before the
// facts were kept is a bare list of names.
type state struct {
	Models []string                   `json:"models"`
	Facts  map[string]json.RawMessage `json:"facts"`
}

func (rt *Router) loadState(name string) ([]string, map[string]json.RawMessage) {
	if rt.stateDir == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(rt.statePath(name))
	if err != nil {
		return nil, nil
	}
	var st state
	if json.Unmarshal(raw, &st) == nil {
		return st.Models, st.Facts
	}
	var models []string
	if json.Unmarshal(raw, &models) != nil {
		return nil, nil
	}
	return models, nil
}

func (rt *Router) saveState(name string, models []string, facts map[string]json.RawMessage) {
	if rt.stateDir == "" {
		return
	}
	raw, _ := json.Marshal(state{Models: models, Facts: facts})
	tmp := rt.statePath(name) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err == nil {
		err = os.Rename(tmp, rt.statePath(name))
		if err != nil {
			rt.log.Warnf("Remote %s: list not kept: %v", name, err)
		}
	}
}

type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.code = code }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

func (r *recorder) replay(rw http.ResponseWriter) {
	for k, v := range r.header {
		rw.Header()[k] = v
	}
	rw.WriteHeader(r.code)
	io.Copy(rw, &r.body)
}
