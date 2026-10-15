package placement

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A key a model's merge key brings in is read as if it were written out:
// vram_mb reaches the warden and not llama-swap, and is checked as whole
// (sol, review 15, M2: TestR15MergedVRAMNeed, TestR15MergedFloatVRAMNeed).
func TestAMergedVRAMNeedIsRead(t *testing.T) {
	cfg, _, err := Parse(files(hosts+"balance: {m: [a/gpu]}\n", map[string]string{"m": "cmd: run --port ${PORT}\n<<: {vram_mb: 3100}\n"}))
	if err != nil {
		t.Fatal(err)
	}
	part, err := cfg.Derive("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := part.LoadsWith("", true); err != nil {
		t.Fatalf("the daemon would not load it: %v", err)
	}
	if part.Local["m"].VRAMMB != 3100 || strings.Contains(string(part.LlamaSwap), "vram_mb") {
		t.Fatalf("need %d, llama-swap's entry:\n%s", part.Local["m"].VRAMMB, part.LlamaSwap)
	}
	if _, _, err := Parse(files(hosts+"balance: {m: [a/gpu]}\n", map[string]string{"m": "cmd: run --port ${PORT}\n<<: {vram_mb: 3.1}\n"})); err == nil || !strings.Contains(err.Error(), "vram_mb is the VRAM the model holds once serving") {
		t.Errorf("a merged 3.1: %v", err)
	}
}

// The other keys InferMux reads or rewrites by name, merged in: env keeps
// its entries and gains the card, and may not pick it; a second device's
// copy drops the aliases; the files are read and left out of the entry. An
// alias to an anchor in the source reads the source.
func TestMergedKeysAreReadAsWritten(t *testing.T) {
	sum := strings.Repeat("a", 64)
	cfg, _, err := Parse(files(hosts+"balance: {m: [a/gpu, a/cpu]}\n", map[string]string{
		"m": "base: &base\n  env: &env [A=1]\n  aliases: [mm]\n  files: {/m.gguf: {sha256: " + sum + "}}\ncmd: run --port ${PORT} --model /m.gguf\n<<: *base\nmetadata: {env: *env}\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	part, err := cfg.Derive("a")
	if err != nil {
		t.Fatal(err)
	}
	ls := loadLlamaSwap(t, part)
	gpu, cpu := ls.Models["m"], ls.Models["m@cpu"]
	if !slices.Equal(gpu.Env, []string{"A=1", "CUDA_VISIBLE_DEVICES=0"}) || !slices.Equal(gpu.Aliases, []string{"mm"}) {
		t.Errorf("m: env %q, aliases %q", gpu.Env, gpu.Aliases)
	}
	if !slices.Equal(cpu.Env, []string{"A=1", "CUDA_VISIBLE_DEVICES="}) || len(cpu.Aliases) != 0 {
		t.Errorf("m@cpu: env %q, aliases %q", cpu.Env, cpu.Aliases)
	}
	// The anchor is still the source's env, not the entry's with the card.
	if got := gpu.Metadata["env"]; !reflect.DeepEqual(got, []any{"A=1"}) {
		t.Errorf("m: metadata's alias to the merged env reads %v", got)
	}
	var entries struct {
		Models map[string]map[string]any
	}
	if err := yaml.Unmarshal(part.LlamaSwap, &entries); err != nil {
		t.Fatal(err)
	}
	if _, in := entries.Models["m"]["files"]; in || !slices.Equal(part.Local["m"].Files, []string{"/m.gguf"}) {
		t.Errorf("files %q, llama-swap's entry:\n%s", part.Local["m"].Files, part.LlamaSwap)
	}

	cfg, _, err = Parse(files(hosts+"balance: {m: [a/gpu]}\n", map[string]string{"m": "cmd: x\n<<: {env: [CUDA_VISIBLE_DEVICES=1]}\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Derive("a"); err == nil || !strings.Contains(err.Error(), "which the device decides") {
		t.Errorf("a merged CUDA_VISIBLE_DEVICES: %v", err)
	}
}

// What yaml.v3 refuses in a merge is refused, and so is an anchor inside
// the merge key's value, which the written-out model would no longer hold.
func TestMergeRefused(t *testing.T) {
	for _, c := range []struct{ name, model, want string }{
		{"two merge keys", "cmd: x\n<<: {a: 1}\n<<: {b: 2}\n", `mapping key "<<" already defined`},
		{"not a mapping", "cmd: x\n<<: 3\n", "map merge requires map or sequence of maps"},
		{"a list of not mappings", "cmd: x\n<<: [3]\n", "map merge requires map or sequence of maps"},
		{"a key twice in a source", "cmd: x\n<<: {a: 1, a: 2}\n", `mapping key "a" already defined`},
		{"an anchor inside", "cmd: x\n<<: &d {ttl: 3}\n", "an anchor inside the merge key's value"},
		{"an anchor deeper inside", "cmd: x\n<<: [{env: &e [A=1]}]\n", "an anchor inside the merge key's value"},
	} {
		_, _, err := Parse(files(hosts, map[string]string{"m": c.model}))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// Written out and encoded, a model decodes to what yaml.v3 decodes from it
// as written, which is what llama-swap reads, and has no merge key left.
func TestAMergeWrittenOutDecodesAsWritten(t *testing.T) {
	for _, body := range []string{
		"cmd: x\n",
		"cmd: x\n<<: {ttl: 3}\n",
		"<<: {ttl: 3, cmd: y}\ncmd: x\n",
		"cmd: x\n<<: {ttl: 3, cmd: y}\n",
		"cmd: x\n<<: [{ttl: 3}, {ttl: 4, name: b}]\n",
		"d: &d {ttl: 3, env: [A=1]}\ne: &e {ttl: 4, aliases: [z]}\ncmd: x\n<<: [*e, *d]\n",
		"d: &d {ttl: 3}\ncmd: x\n<<: *d\nttl: 5\n",
		"d: &d {ttl: 3, <<: {ttl: 4, name: inner}}\ncmd: x\n<<: *d\n",
		"d: &d {<<: {name: inner}, name: outer}\ncmd: x\n<<: [*d, {name: last}]\n",
		"d: &d {ttl: 3}\ncmd: x\n<<: [*d, *d, {ttl: 4}]\n",
		"d: &d {env: &e [A=1]}\ncmd: x\n<<: *d\nf: *e\n",
		"d: &d {ttl: 3}\ncmd: x\n!!merge <<: *d\n",
		"cmd: x\n\"<<\": {ttl: 3}\n",
		"cmd: x\n<<: {filters: {<<: {strip_params: temperature}}}\n",
	} {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		var want map[string]any
		if err := doc.Decode(&want); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if err := writeOutMerge(doc.Content[0]); err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		for i := 0; i < len(doc.Content[0].Content); i += 2 {
			if isMerge(doc.Content[0].Content[i]) {
				t.Errorf("%q: a merge key is left", body)
			}
		}
		out, err := yaml.Marshal(&doc)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		var got map[string]any
		if err := yaml.Unmarshal(out, &got); err != nil {
			t.Errorf("%q written out does not decode: %v\n%s", body, err, out)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q written out:\n%s\ndecodes to\n%v\nwant\n%v", body, out, got, want)
		}
	}
}
