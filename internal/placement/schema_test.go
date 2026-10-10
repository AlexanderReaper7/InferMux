package placement

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite llama-swap-model.schema.json from upstream's config-schema.json")

// The embedded model entry is upstream's, as of the last merge. A merge that
// changes it fails here until -update copies it again.
func TestUpstreamModelSchema(t *testing.T) {
	raw, err := os.ReadFile("../../config-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var upstream struct {
		Definitions map[string]any `json:"definitions"`
		Properties  struct {
			Models struct {
				AdditionalProperties map[string]any `json:"additionalProperties"`
			} `json:"models"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &upstream); err != nil {
		t.Fatal(err)
	}
	want := upstream.Properties.Models.AdditionalProperties
	refs := map[string]any{}
	var walk func(any)
	walk = func(n any) {
		switch n := n.(type) {
		case map[string]any:
			for k, v := range n {
				if ref, ok := v.(string); k == "$ref" && ok {
					name := strings.TrimPrefix(ref, "#/definitions/")
					refs[name] = upstream.Definitions[name]
					walk(refs[name])
				}
				walk(v)
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(want)
	want["definitions"] = refs
	if *update {
		out, _ := json.MarshalIndent(want, "", "  ")
		if err := os.WriteFile("llama-swap-model.schema.json", append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return // the embedded copy is the old one until the next build
	}
	var got map[string]any
	if err := json.Unmarshal(upstreamModel, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Error("llama-swap-model.schema.json is not upstream's model entry: go test ./internal/placement -run TestUpstreamModelSchema -update")
	}
}

// Every field the loader reads is in the schema with a description, and
// every description has its field.
func TestSchemaFollowsTheTypes(t *testing.T) {
	used := map[string]bool{}
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			walk(rt.Elem())
		case reflect.Struct:
			if rt == reflect.TypeOf(Place{}) {
				return
			}
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				if name, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); name == "" || name == "-" {
					continue
				}
				key := rt.Name() + "." + f.Name
				if docs[key] == "" {
					t.Errorf("%s has no description", key)
				}
				used[key] = true
				walk(f.Type)
			}
		}
	}
	walk(reflect.TypeOf(Config{}))
	walk(reflect.TypeOf(File{}))
	for key := range docs {
		if !used[key] {
			t.Errorf("a description for %s, which is not a field", key)
		}
	}
	for key := range enums {
		if !used[key] {
			t.Errorf("an enum for %s, which is not a field", key)
		}
	}
	for key := range completes {
		if !used[key] {
			t.Errorf("a completion for %s, which is not a field", key)
		}
	}
	for _, s := range placeSettings {
		if s.doc == "" {
			t.Errorf("place setting %s has no description", s.name)
		}
	}
	// The schema is JSON as the UI serves it.
	for _, s := range []map[string]any{Schema(), ModelSchema()} {
		if _, err := json.Marshal(s); err != nil {
			t.Fatal(err)
		}
	}
	props := Schema()["properties"].(map[string]any)
	if got := slices.Sorted(maps.Keys(props)); !slices.Equal(got, []string{"balance", "clouds", "failover", "groups", "hosts"}) {
		t.Errorf("infermux.yaml's keys: %q", got)
	}
	modelProps := ModelSchema()["properties"].(map[string]any)
	if _, ok := modelProps["files"]; !ok {
		t.Error("a model's files: is missing")
	}
	if _, ok := modelProps["cmd"]; !ok {
		t.Error("a model's cmd: is missing")
	}
}

// The schema's place settings are the loader's: each one it offers loads,
// and a name it does not offer is refused.
func TestPlaceSchemaIsTheLoaders(t *testing.T) {
	samples := map[string]string{"string": "a/gpu", "integer": "3", "boolean": "true", "array": "[cpu]", "object": "{--ctx-size: 4096}"}
	props := placeSchema()["oneOf"].([]any)[1].(map[string]any)["properties"].(map[string]any)
	var doc bytes.Buffer
	for name, p := range props {
		typ, _ := p.(map[string]any)["type"].(string)
		doc.WriteString(name + ": " + samples[typ] + "\n")
	}
	var place Place
	if err := yaml.Unmarshal(doc.Bytes(), &place); err != nil {
		t.Fatalf("%v\n%s", err, doc.String())
	}
	if place.Place != "a/gpu" || place.MaxInflight != 3 || !place.OnlyIfIdle || !place.BatchOnly || len(place.Occupies) != 1 || place.TTL == nil || len(place.Flags) != 1 {
		t.Errorf("not every setting loaded: %+v", place)
	}
	if err := yaml.Unmarshal([]byte("place: a/gpu\nweight: 3\n"), &place); err == nil || !strings.Contains(err.Error(), `unknown place setting "weight"`) {
		t.Errorf("an unknown setting: %v", err)
	}
}
