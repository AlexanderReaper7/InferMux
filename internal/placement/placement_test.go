package placement

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func loadToday(t *testing.T) *Config {
	t.Helper()
	cfg, warnings, err := Load("testdata/today")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M: /srv/models/Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf has no entry in files:, so a host without it cannot fetch it and nothing checks it",
	}
	if !reflect.DeepEqual(warnings, want) {
		t.Fatalf("warnings:\n%q\nwant\n%q", warnings, want)
	}
	return cfg
}

// The routes are today's failover.yaml, written in the new places: the CPU
// embedder was the peer strix/embed-cpu/Octen-Embedding-4B.Q8_0.
func TestTodayRoutes(t *testing.T) {
	cfg := loadToday(t)
	for _, host := range []string{"strix", "zbox"} {
		part, err := cfg.Derive(host)
		if err != nil {
			t.Fatal(err)
		}
		want := []Route{
			{Place: "zbox", Host: "zbox", Device: "gpu", MaxInflight: 1},
			{Place: "strix", Host: "strix", Device: "gpu", MaxInflight: 1, OnlyIfIdle: true},
			{Place: "strix/Octen-Embedding-4B.Q8_0@cpu", Host: "strix", Device: "cpu", BatchOnly: true},
		}
		if got := part.Routes["Octen-Embedding-4B.Q8_0"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: routes %+v, want %+v", host, got, want)
		}
		if got := part.Routes["Qwen3.8-27B-MTP-Q4_K_M"]; len(got) != 1 || got[0].Place != "strix" {
			t.Errorf("%s: the 27B routes %+v, want strix through the group", host, got)
		}
	}
}

func TestTodayStrix(t *testing.T) {
	part, err := loadToday(t).Derive("strix")
	if err != nil {
		t.Fatal(err)
	}
	wantLocal := map[string]Local{
		"Octen-Embedding-4B.Q8_0":                  {Model: "Octen-Embedding-4B.Q8_0", Devices: []string{"gpu"}},
		"Octen-Embedding-4B.Q8_0@cpu":              {Model: "Octen-Embedding-4B.Q8_0", Devices: []string{"cpu"}},
		"Qwen3.8-27B-MTP-Q4_K_M":                   {Model: "Qwen3.8-27B-MTP-Q4_K_M", Devices: []string{"gpu", "cpu"}},
		"Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M": {Model: "Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M", Devices: []string{"gpu", "cpu"}},
		"jina-reranker-v3.5":                       {Model: "jina-reranker-v3.5", Devices: []string{"gpu"}},
	}
	if !reflect.DeepEqual(part.Local, wantLocal) {
		t.Errorf("local %+v", part.Local)
	}
	if !reflect.DeepEqual(part.Remotes, map[string]string{"zbox": "https://zbox.java-tint.ts.net:5001"}) {
		t.Errorf("remotes %v", part.Remotes)
	}
	ls := loadLlamaSwap(t, part)
	if len(ls.Models) != 5 {
		t.Fatalf("llama-swap has %d models", len(ls.Models))
	}
	gpu := ls.Models["Octen-Embedding-4B.Q8_0"]
	if !slices.Contains(gpu.Env, "CUDA_VISIBLE_DEVICES=0") || gpu.Unlisted {
		t.Errorf("the GPU embedder: env %v, unlisted %v", gpu.Env, gpu.Unlisted)
	}
	cpu := ls.Models["Octen-Embedding-4B.Q8_0@cpu"]
	if !slices.Contains(cpu.Env, "CUDA_VISIBLE_DEVICES=") || !cpu.Unlisted {
		t.Errorf("the CPU embedder: env %v, unlisted %v", cpu.Env, cpu.Unlisted)
	}
	args, err := config.SanitizeCommand(cpu.Cmd)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--n-gpu-layers") || !strings.HasSuffix(joined, "--device none -ngl 0") {
		t.Errorf("the CPU embedder's command: %s", joined)
	}
	if !strings.Contains(joined, "--port 5") {
		t.Errorf("the port macro did not survive: %s", joined)
	}
	rerank := ls.Models["jina-reranker-v3.5"]
	if !rerank.Capabilities.Reranker {
		t.Error("the reranker lost its capability")
	}
	if g := ls.Groups["cpu"]; g.Swap || g.Exclusive || !reflect.DeepEqual(g.Members, []string{"Octen-Embedding-4B.Q8_0@cpu"}) {
		t.Errorf("cpu group %+v", g)
	}
	if g := ls.Groups["gpu"]; !g.Swap || g.Exclusive || len(g.Members) != 4 {
		t.Errorf("gpu group %+v", g)
	}
	if p, ok := ls.Peers["openrouter"]; !ok || p.ApiKey != "sk-test" || len(p.Models) != 2 {
		t.Errorf("peers %+v", ls.Peers)
	}
}

func TestTodayZbox(t *testing.T) {
	part, err := loadToday(t).Derive("zbox")
	if err != nil {
		t.Fatal(err)
	}
	ls := loadLlamaSwap(t, part)
	if len(ls.Models) != 2 {
		t.Fatalf("zbox runs %v", part.Local)
	}
	if ls.Models["Octen-Embedding-4B.Q8_0"].UnloadAfter != 0 {
		t.Errorf("the placement's ttl 0 did not apply")
	}
	immich := ls.Models["immich-ml"]
	if immich.CheckEndpoint != "/ping" || !immich.Unlisted || !slices.Equal(immich.Env, []string{"IMMICH_HOST=127.0.0.1", "CUDA_VISIBLE_DEVICES=0"}) {
		t.Errorf("immich-ml %+v", immich)
	}
}

// loadLlamaSwap runs the part through llama-swap's own loader, with the
// macros Nix gives each host.
func loadLlamaSwap(t *testing.T, part *Part) config.Config {
	t.Helper()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(settings, []byte("macros:\n  llama-server: /bin/llama-server\n  semsearch-rerank: /bin/rerank\n  immich-ml: /bin/immich-ml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	derived := filepath.Join(dir, "derived")
	if err := os.Mkdir(derived, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(derived, "infermux.yaml"), part.LlamaSwap, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-test")
	cfg, err := config.LoadConfigSources(settings, derived)
	if err != nil {
		t.Fatalf("llama-swap refused the part: %v\n%s", err, part.LlamaSwap)
	}
	return cfg
}

func TestFlags(t *testing.T) {
	a := splitCmd("${llama-server}\n  --port ${PORT}\n  --ctx-size 65536\n  --temp -1\n  --no-mmproj-offload\n  --ctx-size 4096\n")
	if a.value("--ctx-size") != "4096" || a.value("--temp") != "-1" || !a.has("--no-mmproj-offload") {
		t.Fatalf("split %+v", a)
	}
	a = applyFlags(a, []Flag{
		{Name: "--ctx-size", Values: []string{"32768"}, Present: true},
		{Name: "--no-mmproj-offload"},
		{Name: "--jinja", Present: true},
	})
	want := "${llama-server}\n  --port ${PORT}\n  --ctx-size 32768\n  --temp -1\n  --jinja"
	if got := a.render(); got != want {
		t.Errorf("render\n%s\nwant\n%s", got, want)
	}
	if q := quote("it's here"); q != `'it'\''s here'` {
		t.Errorf("quote %s", q)
	}
}

func TestInstance(t *testing.T) {
	places := []Place{{Place: "zbox/gpu"}, {Place: "strix/gpu"}, {Place: "strix/cpu"}}
	for _, c := range []struct{ host, device, want string }{
		{"zbox", "gpu", "m"},
		{"strix", "gpu", "m"},
		{"strix", "cpu", "m@cpu"},
	} {
		if got := Instance("m", places, c.host, c.device); got != c.want {
			t.Errorf("%s/%s: %s, want %s", c.host, c.device, got, c.want)
		}
	}
}

// files is a bundle in memory: the main file and models by name.
func files(main string, models map[string]string) map[string][]byte {
	out := map[string][]byte{MainFile: []byte(main)}
	for name, body := range models {
		out["models/"+name+".yaml"] = []byte(body)
	}
	return out
}

const hosts = `
hosts:
  a:
    url: https://a
    devices:
      gpu: {kind: gpu}
      cpu: {kind: cpu}
  b:
    url: https://b
    devices:
      gpu: {kind: gpu, index: 1}
`

func TestRefused(t *testing.T) {
	model := map[string]string{"m": "cmd: run --model /x.gguf\n", "n": "cmd: run\n"}
	for _, c := range []struct {
		name, main string
		models     map[string]string
		want       string
	}{
		{"unknown key", hosts + "balanse: {}\n", model, "field balanse not found"},
		{"unknown device", hosts + "balance: {m: [a/tpu]}\n", model, `a has no device "tpu"`},
		{"unknown host", hosts + "balance: {m: [c/gpu]}\n", model, "c is not a host"},
		{"placed twice", hosts + "balance: {m: [a/gpu]}\nfailover: {m: [a/gpu]}\n", model, "m is placed on a/gpu twice"},
		{"two groups", hosts + "groups: {g1: [m], g2: [m]}\nbalance: {g1: [a/gpu], g2: [b/gpu]}\n", model, "both group g1 and group g2"},
		{"group named as a model", hosts + "groups: {m: [n]}\n", model, "group m has a model's name"},
		{"not a model", hosts + "balance: {x: [a/gpu]}\n", model, "x is not a model or a group"},
		{"occupies itself", hosts + "balance: {m: [{place: a/gpu, occupies: [gpu]}]}\n", model, "occupies names its own device"},
		{"cpu index", "hosts: {a: {devices: {cpu: {kind: cpu, index: 0}}}}\n", model, "a cpu has no index"},
		{"bad kind", "hosts: {a: {devices: {x: {kind: tpu}}}}\n", model, "kind is gpu or cpu"},
		{"no url", hosts + "  c: {devices: {}}\n", model, "host c: url is needed"},
		{"llama-swap file", hosts, map[string]string{"m": "models:\n  m: {cmd: x}\n"}, "this is a llama-swap file"},
		{"no cmd", hosts, map[string]string{"m": "ttl: 3\n"}, "cmd is missing"},
		{"bad hash", hosts, map[string]string{"m": "cmd: x\nfiles: {/x.gguf: {sha256: abc}}\n"}, "sha256 must be 64"},
		{"flag", hosts + "balance: {m: [{place: a/gpu, flags: {ctx: 3}}]}\n", model, `"ctx" is not a flag`},
		{"place setting", hosts + "balance: {m: [{place: a/gpu, weight: 3}]}\n", model, `unknown place setting "weight"`},
	} {
		_, _, err := Parse(files(c.main, c.models))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func TestOwnEntryReplacesGroup(t *testing.T) {
	cfg, warnings, err := Parse(files(hosts+`
groups: {g: [m, n]}
balance:
  g: [a/gpu]
  m: [b/gpu]
failover:
  g: [a/cpu]
`, map[string]string{"m": "cmd: x\n", "n": "cmd: y\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings %q", warnings)
	}
	b, f, _ := cfg.Lanes("m")
	if len(b) != 1 || b[0].Place != "b/gpu" || len(f) != 1 || f[0].Place != "a/cpu" {
		t.Errorf("m: balance %+v failover %+v", b, f)
	}
	b, _, _ = cfg.Lanes("n")
	if len(b) != 1 || b[0].Place != "a/gpu" {
		t.Errorf("n: balance %+v", b)
	}
}

func TestWarnings(t *testing.T) {
	_, warnings, err := Parse(files(hosts+`
balance:
  fit: [a/gpu]
  partial: [{place: a/gpu, flags: {-ngl: 20}}]
  declared: [{place: a/gpu, occupies: [cpu]}]
failover:
  lonely: [b/gpu]
`, map[string]string{
		"fit":      "cmd: run --fit on\n",
		"partial":  "cmd: run -ngl 99\n",
		"declared": "cmd: run --cpu-moe\n",
		"lonely":   "cmd: run\n",
		"nowhere":  "cmd: run\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"fit on a/gpu: --fit on puts the layers that do not fit on CPU threads; add the host's CPU device to occupies so a busy CPU refuses its batch work",
		"lonely has failover places and no balance place, so it is never tried first anywhere",
		"nowhere is placed nowhere, so no host runs it",
		"partial on a/gpu: -ngl 20 leaves layers on the CPU; add the host's CPU device to occupies so a busy CPU refuses its batch work",
	}
	if !reflect.DeepEqual(warnings, want) {
		t.Errorf("warnings\n%q\nwant\n%q", warnings, want)
	}
}

func TestEnvMayNotPickTheCard(t *testing.T) {
	cfg, _, err := Parse(files(hosts+"balance: {m: [a/gpu]}\n", map[string]string{"m": "cmd: x\nenv: [CUDA_VISIBLE_DEVICES=1]\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Derive("a"); err == nil || !strings.Contains(err.Error(), "which the device decides") {
		t.Errorf("derive: %v", err)
	}
}

func TestGPUIndexAndUnknownHost(t *testing.T) {
	cfg, _, err := Parse(files(hosts+"balance: {m: [b/gpu]}\n", map[string]string{"m": "cmd: x\n"}))
	if err != nil {
		t.Fatal(err)
	}
	part, err := cfg.Derive("b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(part.LlamaSwap), "CUDA_VISIBLE_DEVICES=1") {
		t.Errorf("index 1:\n%s", part.LlamaSwap)
	}
	part, err = cfg.Derive("elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	if len(part.Local) != 0 || len(part.Remotes) != 2 || part.Routes["m"][0].Place != "b" {
		t.Errorf("a host the config does not list: %+v", part)
	}
}
