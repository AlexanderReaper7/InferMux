package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// The warden gates only this host's models (InferMux 0009), so a peer's model
// must come back with its peer ID whether the request names it in the body or
// in an /upstream path.
func TestAPeersModelIsQualifiedWithItsPeer(t *testing.T) {
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  local:
    cmd: echo ${PORT}
    aliases: [nick]
peers:
  openrouter:
    proxy: https://openrouter.ai/api
    models: [openrouter/free, qwen/qwen3.8-27b:free]
`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg}
	type q struct{ peer, model string }
	body := map[string]q{
		"local":                            {"", "local"},
		"nick":                             {"", "local"},
		"openrouter/qwen/qwen3.8-27b:free": {"openrouter", "qwen/qwen3.8-27b:free"},
		"qwen/qwen3.8-27b:free":            {"openrouter", "qwen/qwen3.8-27b:free"},
		"openrouter/free":                  {"openrouter", "openrouter/free"},
	}
	for name, want := range body {
		peer, model, ok := s.QualifyModel(name)
		if !ok || (q{peer, model}) != want {
			t.Errorf("QualifyModel(%q) = %q %q %v, want %v", name, peer, model, ok, want)
		}
	}
	upstream := map[string]q{
		"/local/props": {"", "local"},
		"/nick/props":  {"", "local"},
		"/openrouter/qwen/qwen3.8-27b:free/props": {"openrouter", "qwen/qwen3.8-27b:free"},
		"/openrouter/openrouter/free/props":       {"openrouter", "openrouter/free"},
	}
	for path, want := range upstream {
		peer, model, ok := s.UpstreamModel(path)
		if !ok || (q{peer, model}) != want {
			t.Errorf("UpstreamModel(%q) = %q %q %v, want %v", path, peer, model, ok, want)
		}
	}
	if _, _, ok := s.UpstreamModel("/nobody/props"); ok {
		t.Error("an unknown model was found")
	}
	proxy, model, ok := s.PeerModel("openrouter/openrouter/free")
	if !ok || proxy != "https://openrouter.ai/api" || model != "openrouter/free" {
		t.Errorf("PeerModel = %q %q %v", proxy, model, ok)
	}
	if _, _, ok := s.PeerModel("local"); ok {
		t.Error("a local model was taken for a peer's")
	}
}

// The stats read a request's ready_ms through ModelReady (0020): when the
// model last became ready, and whether READY=1, a health poll or nothing
// said so. A model that is not ready, or not local, has no answer.
func TestModelReadyIsTheReadySinceAndHowItWasLearnt(t *testing.T) {
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  notified:
    cmd: echo ${PORT}
    metadata: {readiness: notify}
  polled:
    cmd: echo ${PORT}
  started:
    cmd: echo ${PORT}
    checkEndpoint: none
  loading:
    cmd: echo ${PORT}
`))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 8, 1, 2, 3, 0, time.UTC)
	local := &stubRouter{
		running: map[string]process.ProcessState{
			"notified": process.StateReady,
			"polled":   process.StateReady,
			"started":  process.StateReady,
			"loading":  process.StateStarting,
		},
		readySince: map[string]time.Time{
			"notified": t0,
			"polled":   t0.Add(time.Second),
			"started":  t0.Add(2 * time.Second),
		},
	}
	s := &Server{cfg: cfg, local: local}
	type answer struct {
		at time.Time
		by string
		ok bool
	}
	want := map[string]answer{
		"notified": {t0, "notify", true},
		"polled":   {t0.Add(time.Second), "health", true},
		"started":  {t0.Add(2 * time.Second), "start", true},
		"loading":  {},
		"nobody":   {},
	}
	for id, w := range want {
		at, by, ok := s.ModelReady(id)
		if got := (answer{at, by, ok}); got != w {
			t.Errorf("ModelReady(%q) = %v, want %v", id, got, w)
		}
	}
}

// What ModelReady costs a request, which the stats pay on every one (0020),
// over as many models as strix has, one of them loaded. The stub's
// RunningStatus builds its map as the router's does, less the router's one
// atomic load per process.
func BenchmarkModelReady(b *testing.B) {
	var yaml strings.Builder
	yaml.WriteString("models:\n")
	local := &stubRouter{running: map[string]process.ProcessState{}, readySince: map[string]time.Time{}}
	for i := range 11 {
		fmt.Fprintf(&yaml, "  m%d:\n    cmd: echo ${PORT}\n    metadata: {readiness: notify}\n", i)
	}
	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml.String()))
	if err != nil {
		b.Fatal(err)
	}
	local.running["m0"] = process.StateReady
	local.readySince["m0"] = time.Now()
	s := &Server{cfg: cfg, local: local}
	for b.Loop() {
		if _, _, ok := s.ModelReady("m0"); !ok {
			b.Fatal("m0 is not ready")
		}
	}
}
