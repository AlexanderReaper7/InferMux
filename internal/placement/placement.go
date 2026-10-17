// Package placement is InferMux's one config (0022): the hosts and their
// devices, the models, and the two lanes that place each model on devices.
// The same directory goes to every host, and each host derives its own part
// from it (derive.go): llama-swap's models and one group per device, the
// places a request for each model is tried at, and the device every local
// model occupies.
//
// The directory is infermux.yaml and models/<name>.yaml, one file per model,
// which keeps each model's dated notes beside its command.
package placement

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MainFile is the bundle's top file; ModelsDir holds one file per model.
const (
	MainFile  = "infermux.yaml"
	ModelsDir = "models"
)

// Config is the bundle, parsed.
type Config struct {
	Hosts  map[string]Host     `yaml:"hosts"`
	Clouds map[string]Cloud    `yaml:"clouds"`
	Groups map[string][]string `yaml:"groups"`
	// Balance is where a model runs normally: its places in order, each up to
	// its limit, as 0019's overflow works. Failover is tried only when every
	// balance place is down or refusing, not when one is only busy (0022, 2).
	// A key is a model or a group; a model's own key replaces its group's.
	Balance  map[string][]Place `yaml:"balance"`
	Failover map[string][]Place `yaml:"failover"`

	// Models are models/<name>.yaml, by name.
	Models map[string]*Model `yaml:"-"`
}

// Host is one computer running InferMux. URL is its HTTPS address on the
// tailnet, where the other hosts reach it (0006, 5).
type Host struct {
	URL     string            `yaml:"url"`
	Devices map[string]Device `yaml:"devices"`
}

// Device is a GPU or the CPU of one host. Kind decides the environment its
// models start in, so a model cannot reach a card it was not placed on.
type Device struct {
	Kind string `yaml:"kind"`
	// Index is the GPU's CUDA index, CUDA_VISIBLE_DEVICES for its models.
	Index *int `yaml:"index"`
	// Concurrent runs several models at once; otherwise a model starting
	// stops the one running, as the GPUs have always swapped (0022, 7).
	Concurrent bool `yaml:"concurrent"`
	// Together are groups of models that stay loaded together on a device
	// that swaps, by name (0029). Starting one member stops no other member,
	// and a model outside the group stops them all.
	Together map[string][]string `yaml:"together"`
	// Beside are models that stay loaded beside whatever a device that swaps
	// runs, by name (0030). A swap never stops one, and starting one stops
	// nothing. ComfyUI is the one: it frees its own VRAM through its API.
	Beside []string `yaml:"beside"`
}

const (
	KindGPU = "gpu"
	KindCPU = "cpu"
)

// GPUIndex is the CUDA index, 0 unless set.
func (d Device) GPUIndex() int {
	if d.Index == nil {
		return 0
	}
	return *d.Index
}

// Cloud is a provider every host offers through llama-swap's peers, such as
// OpenRouter. APIKey takes llama-swap's ${env.VAR}, so the key stays in each
// host's credentials and out of the config.
type Cloud struct {
	URL    string   `yaml:"url"`
	APIKey string   `yaml:"api_key"`
	Models []string `yaml:"models"`
}

// Place is one entry of a lane: a device as host/device, with the conditions
// 0019 and 0021 put on a destination, the other devices the model occupies
// there (0022, the warden section, 3), and the overrides for that device.
type Place struct {
	Place       string
	MaxInflight int
	OnlyIfIdle  bool
	BatchOnly   bool
	Occupies    []string
	Flags       []Flag
	TTL         *int
}

// Flag is one override of the model's command: Values replace the flag's
// values, an empty Values with Present keeps it bare, and Present false
// removes it. Only the exact spelling is matched: -c does not override
// --ctx-size.
type Flag struct {
	Name    string
	Values  []string
	Present bool
}

// Host and Device split a place.
func (p Place) Host() string   { h, _, _ := strings.Cut(p.Place, "/"); return h }
func (p Place) Device() string { _, d, _ := strings.Cut(p.Place, "/"); return d }

func (p *Place) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		p.Place = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a place is host/device or a mapping with place:", n.Line)
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		if err := plainKey(n.Content[i]); err != nil {
			return err
		}
		key, value := n.Content[i].Value, n.Content[i+1]
		if seen[key] {
			return fmt.Errorf("line %d: %s is set twice", n.Content[i].Line, key)
		}
		seen[key] = true
		j := slices.IndexFunc(placeSettings, func(s placeSetting) bool { return s.name == key })
		if j < 0 {
			return fmt.Errorf("line %d: unknown place setting %q", n.Content[i].Line, key)
		}
		if err := placeSettings[j].decode(p, value); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

// placeSetting is one key of a place's mapping. The loader and the editor's
// schema both read this table, so they cannot disagree.
type placeSetting struct {
	name, doc string
	schema    map[string]any
	decode    func(p *Place, n *yaml.Node) error
}

var placeSettings = []placeSetting{
	{"place", "The device, as host/device.", map[string]any{"type": "string", "x-complete": "place"},
		func(p *Place, n *yaml.Node) error { return decodeTagged(n, "!!str", &p.Place) }},
	{"max_inflight", "How many requests this place takes at once before the next place gets one (0019). 0 is no limit.", map[string]any{"type": "integer", "minimum": 0},
		func(p *Place, n *yaml.Node) error { return decodeTagged(n, "!!int", &p.MaxInflight) }},
	{"only_if_idle", "Use this place only while its GPU is not yielded to other work (0021).", map[string]any{"type": "boolean"},
		func(p *Place, n *yaml.Node) error { return decodeTagged(n, "!!bool", &p.OnlyIfIdle) }},
	{"batch_only", "Only batch requests come here; the user's own go elsewhere.", map[string]any{"type": "boolean"},
		func(p *Place, n *yaml.Node) error { return decodeTagged(n, "!!bool", &p.BatchOnly) }},
	{"occupies", "Other devices of the host the model uses here, such as the CPU for offloaded layers, so the warden counts it on them too.", map[string]any{"type": "array", "items": map[string]any{"type": "string", "x-complete": "device"}},
		func(p *Place, n *yaml.Node) error { return n.Decode(&p.Occupies) }},
	{"ttl", "Seconds idle before the model is stopped here, replacing the model's ttl.", map[string]any{"type": "integer", "minimum": 0},
		func(p *Place, n *yaml.Node) error {
			var ttl int
			err := decodeTagged(n, "!!int", &ttl)
			p.TTL = &ttl
			return err
		}},
	{"flags", "Overrides of the model's command on this device: a value sets the flag, a list sets several, true keeps it bare, false or null removes it. The exact spelling only: -c does not override --ctx-size.",
		map[string]any{"type": "object", "propertyNames": map[string]any{"pattern": "^-"}, "additionalProperties": map[string]any{"type": []any{"string", "number", "boolean", "null", "array"}}},
		func(p *Place, n *yaml.Node) (err error) { p.Flags, err = decodeFlags(n); return err }},
}

func decodeTagged(n *yaml.Node, tag string, out any) error {
	if n.Tag != tag {
		return fmt.Errorf("line %d: must be %s", n.Line, map[string]string{"!!str": "a string", "!!int": "an integer", "!!bool": "true or false"}[tag])
	}
	return n.Decode(out)
}

// decodeFlags reads a mapping in order: a string or number sets the values,
// a list sets several, true keeps the flag bare, false or null removes it.
func decodeFlags(n *yaml.Node) ([]Flag, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: flags is a mapping of flag to value", n.Line)
	}
	var out []Flag
	for i := 0; i < len(n.Content); i += 2 {
		if err := plainKey(n.Content[i]); err != nil {
			return nil, err
		}
		name, value := n.Content[i].Value, n.Content[i+1]
		if !isFlag(name) {
			return nil, fmt.Errorf("line %d: %q is not a flag", n.Content[i].Line, name)
		}
		if slices.ContainsFunc(out, func(f Flag) bool { return f.Name == name }) {
			return nil, fmt.Errorf("line %d: %s is set twice", n.Content[i].Line, name)
		}
		f := Flag{Name: name, Present: true}
		switch {
		case value.Tag == "!!null" || (value.Tag == "!!bool" && value.Value == "false"):
			f.Present = false
		case value.Tag == "!!bool":
		case value.Kind == yaml.ScalarNode:
			f.Values = []string{value.Value}
		case value.Kind == yaml.SequenceNode:
			if err := value.Decode(&f.Values); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("line %d: %s takes a value, a list, true or false", value.Line, name)
		}
		out = append(out, f)
	}
	return out, nil
}

// Model is one models/<name>.yaml: a llama-swap model entry (cmd, env, ttl,
// capabilities, ...) kept as written, plus Files.
type Model struct {
	Name string
	// Body is the llama-swap entry, without files. Derivation copies it.
	Body *yaml.Node
	// Files are the model's weights by path, each with the SHA-256 the user
	// signed, so a file fetched from another host is checked against it
	// (0022). Not every model has one: immich-ml downloads its own.
	Files map[string]File
	// VRAMMB is the VRAM the model holds once serving, from vram_mb. A start
	// on the GPU is refused while the card has less free (the user's
	// decision 17 of 2026-10-15). 0 is unchecked.
	VRAMMB int
}

// File is a file a model needs on the host that runs it.
type File struct {
	SHA256 string `yaml:"sha256"`
	// HF is the file on Hugging Face as org/repo/file (0012), where a host
	// fetches it when no other host has it.
	HF string `yaml:"hf,omitempty"`
}

// Cmd is the model's command as written.
func (m *Model) Cmd() string {
	if v := mapValue(m.Body, "cmd"); v != nil {
		return v.Value
	}
	return ""
}

var (
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ReadDir reads the bundle's files: infermux.yaml and models/*.yaml, keyed
// by their path in the bundle.
func ReadDir(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	main, err := os.ReadFile(filepath.Join(dir, MainFile))
	if err != nil {
		return nil, err
	}
	files[MainFile] = main
	entries, err := os.ReadDir(filepath.Join(dir, ModelsDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, ModelsDir, e.Name()))
		if err != nil {
			return nil, err
		}
		files[path.Join(ModelsDir, e.Name())] = raw
	}
	return files, nil
}

// Load reads and checks the bundle in dir.
func Load(dir string) (*Config, []string, error) {
	files, err := ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	return Parse(files)
}

// Parse reads the bundle and checks it. Warnings are what a reader should
// look at but that does not stop the config from running, such as a model
// placed nowhere.
func Parse(files map[string][]byte) (*Config, []string, error) {
	cfg := &Config{Models: map[string]*Model{}}
	main, ok := files[MainFile]
	if !ok {
		return nil, nil, fmt.Errorf("%s is missing", MainFile)
	}
	dec := yaml.NewDecoder(bytes.NewReader(main))
	dec.KnownFields(true)
	// An empty file is io.EOF: an empty config, not an error.
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%s: %w", MainFile, err)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dir, file := path.Split(name)
		if dir != ModelsDir+"/" || !strings.HasSuffix(file, ".yaml") {
			continue
		}
		model := strings.TrimSuffix(file, ".yaml")
		m, err := parseModel(model, files[name])
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		cfg.Models[model] = m
	}
	warnings, err := cfg.check()
	if err != nil {
		return nil, nil, err
	}
	return cfg, warnings, nil
}

func parseModel(name string, raw []byte) (*Model, error) {
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("%q is not a model name: letters, digits, '.', '_' and '-'", name)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("a model is a mapping with at least cmd:")
	}
	body := doc.Content[0]
	if err := writeOutMerge(body); err != nil {
		return nil, err
	}
	m := &Model{Name: name, Body: body, Files: map[string]File{}}
	if v := mapValue(body, "models"); v != nil {
		return nil, fmt.Errorf("this is a llama-swap file: a model file holds one model's entry, without models: and its name")
	}
	if files := mapValue(body, "files"); files != nil {
		if err := files.Decode(&m.Files); err != nil {
			return nil, fmt.Errorf("files: %w", err)
		}
		deleteKey(body, "files")
	}
	if v := mapValue(body, "vram_mb"); v != nil {
		// The tag first: yaml.v3 decodes 3.1 into an int as 3.
		if v.ShortTag() != "!!int" || v.Decode(&m.VRAMMB) != nil || m.VRAMMB <= 0 {
			return nil, fmt.Errorf("line %d: vram_mb is the VRAM the model holds once serving, in MB: a whole number above 0", v.Line)
		}
		deleteKey(body, "vram_mb")
	}
	if strings.TrimSpace(m.Cmd()) == "" {
		return nil, fmt.Errorf("cmd is missing")
	}
	for p, f := range m.Files {
		if !path.IsAbs(p) || path.Clean(p) != p {
			return nil, fmt.Errorf("files: %q is not a clean absolute path", p)
		}
		if !sha256Pattern.MatchString(f.SHA256) {
			return nil, fmt.Errorf("files: %s: sha256 must be 64 lowercase hex digits", p)
		}
	}
	return m, nil
}

// check refuses what would derive wrongly or ambiguously, and warns about
// what derives but is probably not meant.
func (c *Config) check() ([]string, error) {
	var warnings []string
	for name, h := range c.Hosts {
		if !namePattern.MatchString(name) {
			return nil, fmt.Errorf("host %q: letters, digits, '.', '_' and '-'", name)
		}
		if h.URL == "" && len(c.Hosts) > 1 {
			return nil, fmt.Errorf("host %s: url is needed, for the other hosts to reach it", name)
		}
		for dname, d := range h.Devices {
			if !namePattern.MatchString(dname) {
				return nil, fmt.Errorf("device %s/%s: letters, digits, '.', '_' and '-'", name, dname)
			}
			switch d.Kind {
			case KindGPU:
				if d.Index != nil && *d.Index < 0 {
					return nil, fmt.Errorf("device %s/%s: index is negative", name, dname)
				}
			case KindCPU:
				if d.Index != nil {
					return nil, fmt.Errorf("device %s/%s: a cpu has no index", name, dname)
				}
			default:
				return nil, fmt.Errorf("device %s/%s: kind is gpu or cpu, not %q", name, dname, d.Kind)
			}
			if len(d.Together) > 0 && d.Concurrent {
				return nil, fmt.Errorf("device %s/%s: together is for a device that swaps, and a concurrent one runs its models together already", name, dname)
			}
			for g, members := range d.Together {
				if !namePattern.MatchString(g) {
					return nil, fmt.Errorf("device %s/%s: together %q: letters, digits, '.', '_' and '-'", name, dname, g)
				}
				if _, clash := c.Models[g]; clash {
					return nil, fmt.Errorf("device %s/%s: together %s has a model's name", name, dname, g)
				}
				if len(members) < 2 {
					return nil, fmt.Errorf("device %s/%s: together %s needs two models or more", name, dname, g)
				}
				for i, m := range members {
					if _, ok := c.Models[m]; !ok {
						return nil, fmt.Errorf("device %s/%s: together %s: %s is not a model in %s/", name, dname, g, m, ModelsDir)
					}
					if slices.Contains(members[:i], m) {
						return nil, fmt.Errorf("device %s/%s: together %s names %s twice", name, dname, g, m)
					}
				}
			}
			if len(d.Beside) > 0 && d.Concurrent {
				return nil, fmt.Errorf("device %s/%s: beside is for a device that swaps, and a concurrent one runs its models together already", name, dname)
			}
			for i, m := range d.Beside {
				if _, ok := c.Models[m]; !ok {
					return nil, fmt.Errorf("device %s/%s: beside: %s is not a model in %s/", name, dname, m, ModelsDir)
				}
				if slices.Contains(d.Beside[:i], m) {
					return nil, fmt.Errorf("device %s/%s: beside names %s twice", name, dname, m)
				}
				for _, g := range sortedKeys(d.Together) {
					if slices.Contains(d.Together[g], m) {
						return nil, fmt.Errorf("device %s/%s: %s is beside every model and in together %s, which keeps it beside only the group's", name, dname, m, g)
					}
				}
			}
		}
	}
	for name, cl := range c.Clouds {
		if cl.URL == "" || len(cl.Models) == 0 {
			return nil, fmt.Errorf("cloud %s: url and models are needed", name)
		}
		if _, clash := c.Hosts[name]; clash {
			return nil, fmt.Errorf("cloud %s has a host's name", name)
		}
	}
	for g, members := range c.Groups {
		if _, clash := c.Models[g]; clash {
			return nil, fmt.Errorf("group %s has a model's name", g)
		}
		if len(members) == 0 {
			return nil, fmt.Errorf("group %s has no members", g)
		}
		for _, m := range members {
			if _, ok := c.Models[m]; !ok {
				return nil, fmt.Errorf("group %s: %s is not a model in %s/", g, m, ModelsDir)
			}
		}
	}
	for lane, entries := range map[string]map[string][]Place{"balance": c.Balance, "failover": c.Failover} {
		for key, places := range entries {
			if _, model := c.Models[key]; !model {
				if _, group := c.Groups[key]; !group {
					return nil, fmt.Errorf("%s: %s is not a model or a group", lane, key)
				}
			}
			if len(places) == 0 {
				return nil, fmt.Errorf("%s: %s has no places", lane, key)
			}
			for _, p := range places {
				if err := c.checkPlace(p); err != nil {
					return nil, fmt.Errorf("%s: %s: %w", lane, key, err)
				}
			}
		}
	}
	// One path is one file, so a host that has it can serve it for either.
	named := map[string]string{}
	for _, name := range c.ModelNames() {
		for p, f := range c.Models[name].Files {
			if other, ok := named[p]; ok && c.Models[other].Files[p] != f {
				return nil, fmt.Errorf("files: %s and %s name %s differently", other, name, p)
			}
			named[p] = name
		}
	}
	for _, name := range c.ModelNames() {
		balance, failover, err := c.Lanes(name)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, p := range append(slices.Clone(balance), failover...) {
			if seen[p.Place] {
				return nil, fmt.Errorf("%s is placed on %s twice", name, p.Place)
			}
			seen[p.Place] = true
		}
		if len(balance)+len(failover) == 0 {
			warnings = append(warnings, fmt.Sprintf("%s is placed nowhere, so no host runs it", name))
		}
		if len(balance) == 0 && len(failover) > 0 {
			warnings = append(warnings, fmt.Sprintf("%s has failover places and no balance place, so it is never tried first anywhere", name))
		}
		m := c.Models[name]
		onGPU := false
		for _, p := range append(balance, failover...) {
			warnings = append(warnings, c.cpuWarnings(m, p)...)
			for _, d := range append([]string{p.Device()}, p.Occupies...) {
				onGPU = onGPU || c.Hosts[p.Host()].Devices[d].Kind == KindGPU
			}
		}
		if m.VRAMMB > 0 && !onGPU {
			warnings = append(warnings, fmt.Sprintf("%s: vram_mb is checked only at a start on a GPU, and no place of it is on one", name))
		}
		for _, file := range ggufPaths(m.Cmd()) {
			if _, ok := m.Files[file]; !ok {
				warnings = append(warnings, fmt.Sprintf("%s: %s has no entry in files:, so a host without it cannot fetch it and nothing checks it", name, file))
			}
		}
	}
	for name, h := range c.Hosts {
		for dname, d := range h.Devices {
			for g, members := range d.Together {
				for _, m := range members {
					if !c.placedOn(m, name+"/"+dname) {
						warnings = append(warnings, fmt.Sprintf("together %s on %s/%s: %s is not placed there, so the group keeps nothing loaded beside it", g, name, dname, m))
					}
				}
			}
			for _, m := range d.Beside {
				if !c.placedOn(m, name+"/"+dname) {
					warnings = append(warnings, fmt.Sprintf("beside on %s/%s: %s is not placed there, so nothing stays loaded beside the device's models", name, dname, m))
				}
			}
		}
	}
	sort.Strings(warnings)
	return slices.Compact(warnings), nil
}

// placedOn is whether a lane of model places it on place, host/device.
func (c *Config) placedOn(model, place string) bool {
	balance, failover, err := c.Lanes(model)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(append(balance, failover...), func(p Place) bool { return p.Place == place })
}

func (c *Config) checkPlace(p Place) error {
	host, ok := c.Hosts[p.Host()]
	if !ok {
		return fmt.Errorf("%q: %s is not a host", p.Place, p.Host())
	}
	if _, ok := host.Devices[p.Device()]; !ok {
		return fmt.Errorf("%q: a place is host/device, and %s has no device %q", p.Place, p.Host(), p.Device())
	}
	if p.MaxInflight < 0 {
		return fmt.Errorf("%s: max_inflight is negative", p.Place)
	}
	for _, other := range p.Occupies {
		if other == p.Device() {
			return fmt.Errorf("%s: occupies names its own device", p.Place)
		}
		if _, ok := host.Devices[other]; !ok {
			return fmt.Errorf("%s: occupies %q, which is not a device of %s", p.Place, other, p.Host())
		}
	}
	if len(p.Occupies) != len(slices.Compact(slices.Sorted(slices.Values(p.Occupies)))) {
		return fmt.Errorf("%s: occupies names a device twice", p.Place)
	}
	if p.TTL != nil && *p.TTL < 0 {
		return fmt.Errorf("%s: ttl is negative", p.Place)
	}
	return nil
}

// cpuWarnings is the editor's hint that a model on a GPU runs layers on CPU
// threads the placement does not declare (0022, the warden section, 3).
func (c *Config) cpuWarnings(m *Model, p Place) []string {
	host := c.Hosts[p.Host()]
	if host.Devices[p.Device()].Kind != KindGPU {
		return nil
	}
	for _, d := range p.Occupies {
		if host.Devices[d].Kind == KindCPU {
			return nil
		}
	}
	args := applyFlags(splitCmd(m.Cmd()), p.Flags)
	var why string
	switch {
	case args.value("--fit") == "on":
		why = "--fit on puts the layers that do not fit on CPU threads"
	case args.has("--cpu-moe") || args.has("-cmoe") || args.has("--n-cpu-moe") || args.has("-ncmoe"):
		why = "it keeps MoE experts on the CPU"
	default:
		for _, alias := range gpuLayerFlags {
			if v := args.value(alias); v != "" && v != "99" && v != "999" && v != "all" && v != "auto" {
				why = alias + " " + v + " leaves layers on the CPU"
			}
		}
	}
	if why == "" {
		return nil
	}
	return []string{fmt.Sprintf("%s on %s: %s; add the host's CPU device to occupies so a busy CPU refuses its batch work", m.Name, p.Place, why)}
}

// ModelNames is every model, sorted.
func (c *Config) ModelNames() []string {
	names := make([]string, 0, len(c.Models))
	for n := range c.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Lanes is a model's balance and failover places. Per lane, the model's own
// key wins; without one, the one group that has the model and a key in that
// lane gives the places. Two such groups is an error: their order would be a
// guess.
func (c *Config) Lanes(model string) (balance, failover []Place, err error) {
	balance, err = c.lane("balance", c.Balance, model)
	if err != nil {
		return nil, nil, err
	}
	failover, err = c.lane("failover", c.Failover, model)
	return balance, failover, err
}

func (c *Config) lane(lane string, entries map[string][]Place, model string) ([]Place, error) {
	if own, ok := entries[model]; ok {
		return own, nil
	}
	var from string
	var out []Place
	groups := make([]string, 0, len(c.Groups))
	for g := range c.Groups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		places, ok := entries[g]
		if !ok || !slices.Contains(c.Groups[g], model) {
			continue
		}
		if from != "" {
			return nil, fmt.Errorf("%s: %s gets places from both group %s and group %s; give it its own entry", lane, model, from, g)
		}
		from, out = g, places
	}
	return out, nil
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// writeOutMerge puts the keys a mapping's merge key, <<, brings in where the
// merge key was, in the order yaml.v3's decode applies them, llama-swap's
// included: the mapping's own keys win, then each source's own keys before
// its own merge, the first source first. InferMux reads and rewrites a
// model's keys by name, cmd, env, aliases, files and vram_mb, so none of them
// may reach llama-swap through a merge InferMux did not read (sol, review
// 15, M2). It compares keys by their text, so each key of the mapping and of
// a source, with a merge or without, is first plainKey's. llama-swap decodes
// the result as it decodes m.
func writeOutMerge(m *yaml.Node) error {
	at := -1
	set := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if isMerge(m.Content[i]) {
			at = i
			continue
		}
		if err := plainKey(m.Content[i]); err != nil {
			return err
		}
		set[m.Content[i].Value] = true
	}
	if at < 0 {
		return nil
	}
	// What yaml.v3 refuses in a merge, a second merge key, a key twice in a
	// source, a source that is not a mapping, is refused here as it would be
	// in llama-swap's load.
	var decoded any
	if err := m.Decode(&decoded); err != nil {
		return err
	}
	if a := anchorIn(m.Content[at+1]); a != nil {
		return fmt.Errorf("line %d: an anchor inside the merge key's value: InferMux writes the merge out, and an alias to the anchor would be left without it", a.Line)
	}
	var in []*yaml.Node
	seen := map[*yaml.Node]bool{}
	var merge func(n *yaml.Node) error
	from := func(src *yaml.Node) error {
		if src.Kind == yaml.AliasNode {
			src = src.Alias
		}
		if seen[src] {
			// Every key it brings is set already.
			return nil
		}
		seen[src] = true
		var next *yaml.Node
		for i := 0; i+1 < len(src.Content); i += 2 {
			k, v := src.Content[i], src.Content[i+1]
			if isMerge(k) {
				next = v
				continue
			}
			// Each key, the ones the mapping's own keys win over too: which
			// it is is read from its text.
			if err := plainKey(k); err != nil {
				return err
			}
			if !set[k.Value] {
				set[k.Value] = true
				// Each anchor stays where it is defined, in the source, and
				// is not defined a second time here.
				kc, vc := deepCopy(k), deepCopy(v)
				dropAnchors(kc)
				dropAnchors(vc)
				in = append(in, kc, vc)
			}
		}
		if next != nil {
			return merge(next)
		}
		return nil
	}
	merge = func(n *yaml.Node) error {
		if n.Kind == yaml.SequenceNode {
			for _, src := range n.Content {
				if err := from(src); err != nil {
					return err
				}
			}
			return nil
		}
		return from(n)
	}
	if err := merge(m.Content[at+1]); err != nil {
		return err
	}
	m.Content = append(m.Content[:at], append(in, m.Content[at+2:]...)...)
	return nil
}

// plainKey refuses a key that yaml.v3 does not read as its own text, a
// string: an alias, which it reads as the aliased node, a tag but !!str,
// such as !!binary, and a key it reads as a number, a bool or null.
// InferMux finds and removes a key by its text, so such a key would be one
// key to InferMux and another to llama-swap: `? *key : 3100`, with
// `&key vram_mb`, is vram_mb to llama-swap and key to InferMux (sol, review
// 16, M2).
func plainKey(k *yaml.Node) error {
	if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
		return fmt.Errorf("line %d: a key InferMux reads is a string as written: not an alias, a tag but !!str, or a number, a bool or null", k.Line)
	}
	return nil
}

// isMerge is yaml.v3's own test for a merge key.
func isMerge(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Value == "<<" && (k.Tag == "" || k.Tag == "!" || k.ShortTag() == "!!merge")
}

// anchorIn is the first node in n that defines an anchor, without following
// an alias, or nil.
func anchorIn(n *yaml.Node) *yaml.Node {
	if n.Anchor != "" {
		return n
	}
	if n.Kind == yaml.AliasNode {
		return nil
	}
	for _, c := range n.Content {
		if a := anchorIn(c); a != nil {
			return a
		}
	}
	return nil
}

func dropAnchors(n *yaml.Node) {
	n.Anchor = ""
	for _, c := range n.Content {
		dropAnchors(c)
	}
}

func deleteKey(n *yaml.Node, key string) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content = append(n.Content[:i], n.Content[i+2:]...)
			return
		}
	}
}

func setKey(n *yaml.Node, key string, value *yaml.Node) {
	if v := mapValue(n, key); v != nil {
		*v = *value
		return
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}
