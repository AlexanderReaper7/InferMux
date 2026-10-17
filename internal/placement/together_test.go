package placement

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// evicts is what llama-swap's own matrix solver stops to start target while
// running are loaded, sorted.
func evicts(ls config.Config, target string, running ...string) []string {
	d := ls.Routing.Router.Settings.Matrix.Program().Solve(target, running, ls.Routing.Router.Settings.Matrix.ResolvedEvictCosts())
	return slices.Sorted(slices.Values(d.Evict))
}

const togetherHosts = `
hosts:
  a:
    url: https://a
    devices:
      gpu:
        kind: gpu
        together:
          deep-search: [agent, reranker]
      cpu: {kind: cpu, concurrent: true}
  b:
    url: https://b
    devices:
      gpu: {kind: gpu}
`

var togetherModels = map[string]string{
	"agent":    "cmd: ${llama-server} --port ${PORT}\nvram_mb: 4200\n",
	"reranker": "cmd: ${llama-server} --port ${PORT}\nvram_mb: 3000\n",
	"chat":     "cmd: ${llama-server} --port ${PORT}\n",
	"embed":    "cmd: ${llama-server} --port ${PORT}\n",
}

// The deep search agent and the reranker stay loaded together on a's GPU
// (0029): starting one stops neither, any other model on the GPU stops
// both, and a starting member stops a chat model. The CPU's models are never
// stopped by the GPU's, nor the GPU's by them.
func TestAGroupStaysLoadedTogether(t *testing.T) {
	cfg, warnings, err := Parse(files(togetherHosts+`
balance:
  agent: [a/gpu]
  reranker: [a/gpu]
  chat: [a/gpu]
  embed: [a/cpu, a/gpu]
`, togetherModels))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings %q", warnings)
	}
	part, err := cfg.Derive("a")
	if err != nil {
		t.Fatal(err)
	}
	ls := loadLlamaSwap(t, part)
	for _, c := range []struct {
		target  string
		running []string
		want    []string
	}{
		{"reranker", []string{"agent", "embed"}, nil},
		{"agent", []string{"reranker"}, nil},
		{"chat", []string{"agent", "reranker", "embed"}, []string{"agent", "reranker"}},
		{"agent", []string{"chat", "embed"}, []string{"chat"}},
		// embed@gpu is a second device's name, which the expressions hold
		// through a variable.
		{"embed@gpu", []string{"agent", "reranker", "embed"}, []string{"agent", "reranker"}},
		{"reranker", []string{"embed@gpu"}, []string{"embed@gpu"}},
		{"embed", []string{"agent", "reranker"}, nil},
		{"embed", []string{"chat"}, nil},
	} {
		if got := evicts(ls, c.target, c.running...); !slices.Equal(got, c.want) {
			t.Errorf("%s beside %v stopped %v, want %v", c.target, c.running, got, c.want)
		}
	}
	d := ls.Routing.Router.Settings.Matrix.Program().Solve("reranker", []string{"agent"}, nil)
	if d.SetName != "deep-search" {
		t.Errorf("the set llama-swap logs is %q", d.SetName)
	}
	if !strings.Contains(string(part.LlamaSwap), "v1: embed@gpu") {
		t.Errorf("no variable for embed@gpu:\n%s", part.LlamaSwap)
	}
	// The warden hears each member's siblings, by their names here, for the
	// start that does not fit beside them.
	for name, want := range map[string][]string{"agent": {"reranker"}, "reranker": {"agent"}, "chat": nil, "embed": nil, "embed@gpu": nil} {
		if got := part.Local[name].Together; !slices.Equal(got, want) {
			t.Errorf("%s is together with %v, want %v", name, got, want)
		}
	}

	// b has no group: its GPU swaps one model at a time.
	part, err = cfg.Derive("b")
	if err != nil {
		t.Fatal(err)
	}
	if len(part.Local) != 0 {
		t.Fatalf("b runs %v", part.Local)
	}
}

// A variable never takes a local model's name, since llama-swap reads a
// variable before a model of the same name: with a model named v1 here,
// embed@gpu is v2, and both still swap with each other (review of 0029, 4).
func TestAVariableSkipsALocalModelsName(t *testing.T) {
	models := map[string]string{"v1": "cmd: ${llama-server} --port ${PORT}\n"}
	for name, m := range togetherModels {
		models[name] = m
	}
	cfg, _, err := Parse(files(togetherHosts+`
balance:
  agent: [a/gpu]
  reranker: [a/gpu]
  v1: [a/gpu]
  embed: [a/cpu, a/gpu]
`, models))
	if err != nil {
		t.Fatal(err)
	}
	part, err := cfg.Derive("a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(part.LlamaSwap), "v2: embed@gpu") || strings.Contains(string(part.LlamaSwap), "v1: embed@gpu") {
		t.Fatalf("embed@gpu's variable:\n%s", part.LlamaSwap)
	}
	ls := loadLlamaSwap(t, part)
	for _, c := range []struct {
		target  string
		running []string
		want    []string
	}{
		{"embed@gpu", []string{"v1", "embed"}, []string{"v1"}},
		{"v1", []string{"embed@gpu", "embed"}, []string{"embed@gpu"}},
		{"v1", []string{"agent", "reranker"}, []string{"agent", "reranker"}},
	} {
		if got := evicts(ls, c.target, c.running...); !slices.Equal(got, c.want) {
			t.Errorf("%s beside %v stopped %v, want %v", c.target, c.running, got, c.want)
		}
	}
}

// A group with one member placed on the device keeps nothing beside it: the
// member swaps like any other model there, and the config says so.
func TestAGroupWithOneMemberPlacedSwaps(t *testing.T) {
	cfg, warnings, err := Parse(files(togetherHosts+`
balance:
  agent: [a/gpu]
  reranker: [b/gpu]
  chat: [a/gpu]
`, togetherModels))
	if err != nil {
		t.Fatal(err)
	}
	want := "together deep-search on a/gpu: reranker is not placed there, so the group keeps nothing loaded beside it"
	if !slices.Contains(warnings, want) {
		t.Errorf("warnings %q", warnings)
	}
	part, err := cfg.Derive("a")
	if err != nil {
		t.Fatal(err)
	}
	ls := loadLlamaSwap(t, part)
	if got := evicts(ls, "chat", "agent"); !slices.Equal(got, []string{"agent"}) {
		t.Errorf("chat beside the agent stopped %v", got)
	}
	if got := part.Local["agent"].Together; got != nil {
		t.Errorf("the agent is together with %v", got)
	}
}

func TestTogetherRefused(t *testing.T) {
	device := func(gpu string) string {
		return "hosts:\n  a:\n    url: https://a\n    devices:\n      gpu: " + gpu + "\n"
	}
	for _, c := range []struct{ name, main, want string }{
		{"concurrent", device("{kind: gpu, concurrent: true, together: {g: [agent, reranker]}}"), "together is for a device that swaps"},
		{"one member", device("{kind: gpu, together: {g: [agent]}}"), "together g needs two models or more"},
		{"not a model", device("{kind: gpu, together: {g: [agent, x]}}"), "together g: x is not a model"},
		{"twice", device("{kind: gpu, together: {g: [agent, agent]}}"), "together g names agent twice"},
		{"a model's name", device("{kind: gpu, together: {chat: [agent, reranker]}}"), "together chat has a model's name"},
		{"bad name", device("{kind: gpu, together: {'g+h': [agent, reranker]}}"), `together "g+h": letters`},
	} {
		_, _, err := Parse(files(c.main, togetherModels))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// The build loads a part without the models that read the environment
// (0022, 11). A member left out is taken out of the matrix's sets too, since
// llama-swap checks each name in them at load, and a set left empty goes.
func TestLoadsWithTakesAModelLeftOutOutOfTheMatrix(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(base, []byte("macros:\n  llama-server: /bin/llama-server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	models := map[string]string{}
	for k, v := range togetherModels {
		models[k] = v
	}
	models["reranker"] = "cmd: ${llama-server} --port ${PORT}\nttl: ${env.INFERMUX_TEST_UNSET_TOGETHER}\n"
	models["embed"] = "cmd: ${llama-server} --port ${PORT} --api-key ${env.INFERMUX_TEST_UNSET_TOGETHER}\n"
	cfg, _, err := Parse(files(togetherHosts+`
balance:
  agent: [a/gpu]
  reranker: [a/gpu]
  chat: [a/gpu]
  embed: [a/cpu, a/gpu]
`, models))
	if err != nil {
		t.Fatal(err)
	}
	part, err := cfg.Derive("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := part.LoadsWith(base, false); err != nil {
		t.Fatalf("without env: %v\n%s", err, part.LlamaSwap)
	}
	kept, err := withoutEnv(part.LlamaSwap)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"reranker", "embed", "v1"} {
		if strings.Contains(string(kept), gone) {
			t.Errorf("%s is still in\n%s", gone, kept)
		}
	}

	// With every model left out, the router goes too.
	only := map[string]string{"reranker": models["reranker"], "agent": "cmd: ${llama-server} --port ${env.INFERMUX_TEST_UNSET_TOGETHER}\n"}
	cfg, _, err = Parse(files(togetherHosts+"balance:\n  agent: [a/gpu]\n  reranker: [a/gpu]\n", only))
	if err != nil {
		t.Fatal(err)
	}
	if part, err = cfg.Derive("a"); err != nil {
		t.Fatal(err)
	}
	if err := part.LoadsWith(base, false); err != nil {
		t.Fatalf("with every model left out: %v", err)
	}
}
