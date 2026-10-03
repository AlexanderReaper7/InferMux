package muxui

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// The property the table editor rests on: whatever the UI accepts and writes,
// it reads back as the same model, through renderCmd, the YAML file and
// parseCmd. A model that came back as raw text, or with a flag changed, is
// an edit the user did not make.
func FuzzAModelTheUIAcceptsReadsBackUnchanged(f *testing.F) {
	f.Add("llama-server", "/srv/models/x.gguf", "--ctx-size", "262144", true, "everyday", "qwen")
	f.Add("llama-server", "/srv/models/x.gguf", "--temp", "-0.5", true, "", "")
	f.Add("bonsai-server", "/srv/a b/x.gguf", "--jinja", "", false, "it's", "a:b")
	f.Add("llama-server", "/m.gguf", "--chat-template", "line one\nline two", true, "", "")
	f.Add("llama-server", "/m.gguf", "--prompt", `ends in \`, true, "", "")
	f.Add("llama-server", "/m.gguf", "--x", "  padded  ", true, "true", "null")
	f.Fuzz(func(t *testing.T, runtime, gguf, flag, value string, hasValue bool, description, alias string) {
		in := Model{Name: "m", Runtime: runtime, GGUF: gguf, Description: description, Aliases: []string{}, Flags: []Flag{}}
		if alias != "" {
			in.Aliases = []string{alias}
		}
		if flag != "" {
			fl := Flag{Name: flag}
			if hasValue {
				fl.Value = &value
			}
			in.Flags = []Flag{fl}
		}
		if !macroToken.MatchString("${" + runtime + "}") {
			t.Skip()
		}

		file := modelFile{name: "m.yaml", doc: &yaml.Node{}}
		node := &yaml.Node{Kind: yaml.MappingNode}
		file.models().Content = append(file.models().Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "m"}, node)
		if err := encodeModel(node, in); err != nil {
			t.Skip() // refused, which is allowed
		}
		raw, err := file.bytes()
		if err != nil {
			t.Skip() // refused at encoding: invalid UTF-8, which JSON from the browser cannot carry
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("the file does not parse back: %v\n%s", err, raw)
		}
		out := listModels([]modelFile{{name: "m.yaml", doc: &doc}})
		if len(out) != 1 {
			t.Fatalf("models %+v\n%s", out, raw)
		}
		got := out[0]
		if got.Raw {
			t.Fatalf("came back as raw text\nin: %+v\n%s", in, raw)
		}
		if got.Runtime != in.Runtime || got.GGUF != in.GGUF || got.Description != in.Description ||
			!reflect.DeepEqual(got.Aliases, in.Aliases) || !reflect.DeepEqual(got.Flags, in.Flags) {
			t.Fatalf("changed on the way back\nin:  %+v %v\ngot: %+v %v\n%s", in, flagsString(in.Flags), got, flagsString(got.Flags), raw)
		}
	})
}

func flagsString(flags []Flag) []string {
	var out []string
	for _, f := range flags {
		if f.Value == nil {
			out = append(out, f.Name)
		} else {
			out = append(out, f.Name+"="+*f.Value)
		}
	}
	return out
}
