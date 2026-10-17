package placement

import (
	"slices"
	"strings"
	"testing"
)

const besideHosts = `
hosts:
  a:
    url: https://a
    devices:
      gpu:
        kind: gpu
        beside: [comfyui_auto]
        together:
          deep-search: [agent, reranker]
      cpu: {kind: cpu, concurrent: true}
`

func besideModels() map[string]string {
	models := map[string]string{"comfyui_auto": "cmd: /bin/comfyui --port ${PORT}\nproxy: http://127.0.0.1:${PORT}\n"}
	for name, m := range togetherModels {
		models[name] = m
	}
	return models
}

// ComfyUI stays up beside whatever the GPU swaps (0030): starting any model
// there stops no ComfyUI, starting ComfyUI stops nothing, and the GPU's other
// models still swap with each other and with the group.
func TestABesideModelIsInEverySet(t *testing.T) {
	cfg, warnings, err := Parse(files(besideHosts+`
balance:
  comfyui_auto: [a/gpu]
  agent: [a/gpu]
  reranker: [a/gpu]
  chat: [a/gpu]
  embed: [a/cpu]
`, besideModels()))
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
		{"chat", []string{"comfyui_auto"}, nil},
		{"agent", []string{"comfyui_auto", "chat"}, []string{"chat"}},
		{"chat", []string{"comfyui_auto", "agent", "reranker", "embed"}, []string{"agent", "reranker"}},
		{"comfyui_auto", []string{"chat", "embed"}, nil},
		{"comfyui_auto", []string{"agent", "reranker"}, nil},
		{"reranker", []string{"agent", "comfyui_auto"}, nil},
	} {
		if got := evicts(ls, c.target, c.running...); !slices.Equal(got, c.want) {
			t.Errorf("%s beside %v stopped %v, want %v", c.target, c.running, got, c.want)
		}
	}
	d := ls.Routing.Router.Settings.Matrix.Program().Solve("chat", []string{"comfyui_auto"}, nil)
	if d.SetName != "chat" {
		t.Errorf("the set llama-swap logs is %q", d.SetName)
	}
}

// A device whose only model is beside still has a set, the one with it.
func TestABesideModelAloneOnItsDevice(t *testing.T) {
	cfg, _, err := Parse(files(besideHosts+`
balance:
  comfyui_auto: [a/gpu]
  embed: [a/cpu]
`, besideModels()))
	if err != nil {
		t.Fatal(err)
	}
	part, err := cfg.Derive("a")
	if err != nil {
		t.Fatal(err)
	}
	ls := loadLlamaSwap(t, part)
	if got := evicts(ls, "comfyui_auto", "embed"); len(got) != 0 {
		t.Errorf("comfyui_auto stopped %v", got)
	}
	if got := evicts(ls, "embed", "comfyui_auto"); len(got) != 0 {
		t.Errorf("embed stopped %v", got)
	}
}

func TestBesideRefusedAndWarned(t *testing.T) {
	device := func(gpu string) string {
		return "hosts:\n  a:\n    url: https://a\n    devices:\n      gpu: " + gpu + "\n"
	}
	for _, c := range []struct{ name, main, want string }{
		{"concurrent", device("{kind: gpu, concurrent: true, beside: [comfyui_auto]}"), "beside is for a device that swaps"},
		{"not a model", device("{kind: gpu, beside: [x]}"), "beside: x is not a model"},
		{"twice", device("{kind: gpu, beside: [comfyui_auto, comfyui_auto]}"), "beside names comfyui_auto twice"},
		{"in a group", device("{kind: gpu, beside: [agent], together: {g: [agent, reranker]}}"), "agent is beside every model and in together g"},
	} {
		_, _, err := Parse(files(c.main, besideModels()))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	_, warnings, err := Parse(files(besideHosts+"balance:\n  chat: [a/gpu]\n", besideModels()))
	if err != nil {
		t.Fatal(err)
	}
	want := "beside on a/gpu: comfyui_auto is not placed there, so nothing stays loaded beside the device's models"
	if !slices.Contains(warnings, want) {
		t.Errorf("warnings %q", warnings)
	}
}
