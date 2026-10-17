package placement

import (
	_ "embed"
	"encoding/json"
	"reflect"
	"strings"
)

// The editor's completion and hover read these (0022, 9). infermux.yaml's
// schema is built from Config's types and the place settings' table, so a
// field the loader reads is a field the editor offers; a model file's is
// llama-swap's own model entry plus files: and vram_mb:.
//
// The editor fills two things in from the draft itself: "x-complete-keys"
// names what a mapping's keys are, "x-complete" what a string is. The kinds
// are place (host/device), device, model, lane (a model or group) and gguf
// (a file under this host's GGUF directories).

// upstreamModel is llama-swap's model entry, models.additionalProperties of
// its config-schema.json, with the definitions it refers to. A test keeps it
// equal to upstream's.
//
//go:embed llama-swap-model.schema.json
var upstreamModel []byte

// docs describe Config's fields and the types under it, by Type.Field. A
// test fails on a field without one and on one without a field.
var docs = map[string]string{
	"Config.Hosts":      "The computers that run InferMux, by name. The name is the one in each host's warden.yaml `host:`.",
	"Config.Clouds":     "Providers every host offers through llama-swap's peers, such as OpenRouter.",
	"Config.Groups":     "Placement labels: a group placed in a lane places each of its models there. Clients still ask for a model by its own name.",
	"Config.Balance":    "Where a model or group runs normally: its places in order, each up to its limit, as 0019's overflow works.",
	"Config.Failover":   "Where a model or group runs when every balance place is down or refusing, not when one is only busy (0022, 2).",
	"Host.URL":          "The host's HTTPS address on the tailnet, where the other hosts reach it.",
	"Host.Devices":      "The host's GPUs and its CPU, by name. A model is placed on host/device.",
	"Device.Kind":       "gpu or cpu. A GPU's models see only its card; a CPU's see none, and a command with -ngl or --device gets --device none -ngl 0 in their place.",
	"Device.Index":      "The GPU's CUDA index, CUDA_VISIBLE_DEVICES for its models. 0 when unset.",
	"Device.Concurrent": "Run several models at once. Otherwise a model starting stops the one running, as the GPUs have always swapped.",
	"Device.Together":   "Groups of models that stay loaded together on a device that swaps, by name (0029). Starting one member stops no other member; a model outside the group stops them all. Each member is placed on the device in a lane as usual.",
	"Device.Beside":     "Models that stay loaded beside whatever a device that swaps runs, by name (0030). A swap never stops one, and starting one stops nothing. For ComfyUI, which frees its own VRAM through its API. Each is placed on the device in a lane as usual.",
	"Cloud.URL":         "The provider's OpenAI-compatible base URL.",
	"Cloud.APIKey":      "The key, as ${env.VAR}, so it stays in each host's credentials and out of the config.",
	"Cloud.Models":      "The provider's models that the hosts offer.",
	"File.SHA256":       "The file's SHA-256, 64 lowercase hex digits. A file fetched from another host or Hugging Face must match it.",
	"File.HF":           "The file on Hugging Face as org/repo/file, where a host fetches it when no other host has it.",
}

// enums are the closed sets of Type.Field.
var enums = map[string][]any{
	"Device.Kind": {KindGPU, KindCPU},
}

// completes add the editor's kinds to Type.Field's schema.
var completes = map[string]func(map[string]any){
	"Config.Balance":  keysAre("lane"),
	"Config.Failover": keysAre("lane"),
	"Config.Groups":   membersAreModels,
	"Device.Together": membersAreModels,
	"Device.Beside":   itemsAreModels,
}

func membersAreModels(s map[string]any) {
	s["additionalProperties"].(map[string]any)["items"].(map[string]any)["x-complete"] = "model"
}

func itemsAreModels(s map[string]any) {
	s["items"].(map[string]any)["x-complete"] = "model"
}

func keysAre(kind string) func(map[string]any) {
	return func(s map[string]any) { s["x-complete-keys"] = kind }
}

// placeSchema is a place: host/device, or a mapping of the settings.
func placeSchema() map[string]any {
	props := map[string]any{}
	for _, s := range placeSettings {
		p := map[string]any{"description": s.doc}
		for k, v := range s.schema {
			p[k] = v
		}
		props[s.name] = p
	}
	return map[string]any{
		"description": "A device as host/device, or a mapping with place: and the settings for that device.",
		"oneOf": []any{
			map[string]any{"type": "string", "pattern": "^[^/]+/[^/]+$", "x-complete": "place"},
			map[string]any{"type": "object", "properties": props, "required": []any{"place"}, "additionalProperties": false},
		},
	}
}

// Schema is infermux.yaml's JSON Schema.
func Schema() map[string]any {
	s := typeSchema(reflect.TypeOf(Config{}))
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["title"] = MainFile
	return s
}

// ModelSchema is models/<name>.yaml's: llama-swap's model entry, files:
// and vram_mb:.
func ModelSchema() map[string]any {
	var s map[string]any
	if err := json.Unmarshal(upstreamModel, &s); err != nil {
		panic(err)
	}
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["title"] = ModelsDir + "/<name>.yaml"
	file := typeSchema(reflect.TypeOf(File{}))
	file["required"] = []any{"sha256"}
	s["properties"].(map[string]any)["files"] = map[string]any{
		"type":                 "object",
		"description":          "The model's weights by absolute path, each with the SHA-256 the user signs, so a host that lacks one fetches it and checks it (0022, 8).",
		"propertyNames":        map[string]any{"pattern": "^/"},
		"additionalProperties": file,
		"x-complete-keys":      "gguf",
	}
	s["properties"].(map[string]any)["vram_mb"] = map[string]any{
		"type":        "integer",
		"minimum":     1,
		"description": "The VRAM the model holds once serving, in MB. A start on the GPU answers 503 at once, for every key, while the card's own free figure is less.",
	}
	return s
}

func typeSchema(t reflect.Type) map[string]any {
	if t == reflect.TypeOf(Place{}) {
		return placeSchema()
	}
	switch t.Kind() {
	case reflect.Pointer:
		return typeSchema(t.Elem())
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int:
		return map[string]any{"type": "integer"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": typeSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "propertyNames": map[string]any{"pattern": namePattern.String()}, "additionalProperties": typeSchema(t.Elem())}
	case reflect.Struct:
		props := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "" || name == "-" {
				continue
			}
			key := t.Name() + "." + f.Name
			p := typeSchema(f.Type)
			p["description"] = docs[key]
			if e, ok := enums[key]; ok {
				p["enum"] = e
			}
			if c, ok := completes[key]; ok {
				c(p)
			}
			props[name] = p
		}
		return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	}
	panic("placement: no schema for " + t.String())
}
