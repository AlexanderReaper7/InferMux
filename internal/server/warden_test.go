package server

import (
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
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
}
