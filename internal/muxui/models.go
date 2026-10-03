// Package muxui is infermux-ui: the web UI's server, a separate process from
// the daemon (0005). It edits the files InferMux reads, validates them with
// llama-swap's own loader before writing, commits them when the user asks,
// and passes status and controls through to the daemon.
package muxui

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"gopkg.in/yaml.v3"
)

// Model is one entry of llama-swap's `models:` as the UI edits it. The cmd is
// split into the runtime macro, the GGUF and the rest of the flags, in the
// form infermux-ui writes:
//
//	${llama-server}
//	  --port ${PORT}
//	  --model /srv/models/x.gguf
//	  --ctx-size 262144
//
// A cmd in any other form is kept as Cmd with Raw set, and edited as text.
type Model struct {
	Name        string   `json:"name"`
	File        string   `json:"file"` // the YAML file it lives in, relative to the models directory
	Runtime     string   `json:"runtime"`
	GGUF        string   `json:"gguf"`
	Flags       []Flag   `json:"flags"`
	Raw         bool     `json:"raw"`
	Cmd         string   `json:"cmd"`
	TTL         *int     `json:"ttl"`
	Aliases     []string `json:"aliases"`
	Description string   `json:"description"`
	Unlisted    bool     `json:"unlisted"`
	Comment     string   `json:"comment"` // the file's leading comment
	// HF names the GGUF and mmproj on Hugging Face; nil for local files.
	HF *HFSource `json:"hf"`
}

// Flag is one argument of the runtime. A nil Value is a switch.
type Flag struct {
	Name  string  `json:"name"`
	Value *string `json:"value"`
}

var macroToken = regexp.MustCompile(`^\$\{([A-Za-z0-9_.-]+)\}$`)

// parseCmd splits a cmd in infermux-ui's form. ok is false for anything else.
// The arguments are llama-swap's own split, so the table shows what the
// runtime is started with.
func parseCmd(cmd string) (runtime, gguf string, flags []Flag, ok bool) {
	for _, line := range strings.Split(cmd, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			return "", "", nil, false // a comment would be lost
		}
	}
	args, err := config.SanitizeCommand(cmd)
	if err != nil {
		return "", "", nil, false
	}
	m := macroToken.FindStringSubmatch(args[0])
	if m == nil {
		return "", "", nil, false
	}
	runtime = m[1]
	sawPort := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || isNumber(arg) {
			return "", "", nil, false // a positional argument
		}
		var value *string
		if i+1 < len(args) && (!strings.HasPrefix(args[i+1], "-") || isNumber(args[i+1])) {
			v := args[i+1]
			value = &v
			i++
		}
		switch arg {
		case "--port":
			if value == nil || *value != "${PORT}" {
				return "", "", nil, false
			}
			sawPort = true
		case "--model", "-m":
			if value == nil || gguf != "" {
				return "", "", nil, false
			}
			gguf = *value
		default:
			flags = append(flags, Flag{Name: arg, Value: value})
		}
	}
	if !sawPort || gguf == "" {
		return "", "", nil, false
	}
	return runtime, gguf, flags, true
}

func sameFlags(a, b []Flag) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || (a[i].Value == nil) != (b[i].Value == nil) ||
			(a[i].Value != nil && *a[i].Value != *b[i].Value) {
			return false
		}
	}
	return true
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// renderCmd is parseCmd's inverse.
func renderCmd(runtime, gguf string, flags []Flag) string {
	var b strings.Builder
	fmt.Fprintf(&b, "${%s}\n  --port ${PORT}\n  --model %s\n", runtime, quote(gguf))
	for _, f := range flags {
		b.WriteString("  " + f.Name)
		if f.Value != nil {
			b.WriteString(" " + quote(*f.Value))
		}
		b.WriteString("\n")
	}
	return b.String()
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_./:=,+@%${}-]+$`)

func quote(s string) string {
	if s != "" && plainArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// checkFlags refuses what renderCmd could not write back as the same flags.
func checkFlags(flags []Flag) error {
	for _, f := range flags {
		// A name is written unquoted, so it has to be one quote leaves bare.
		if !strings.HasPrefix(f.Name, "-") || !plainArg.MatchString(f.Name) || isNumber(f.Name) {
			return fmt.Errorf("%q is not a flag name", f.Name)
		}
		switch f.Name {
		case "--port", "--model", "-m":
			return fmt.Errorf("%s is set by the model's runtime and GGUF fields", f.Name)
		}
		if f.Value != nil && strings.HasPrefix(*f.Value, "-") && !isNumber(*f.Value) {
			return fmt.Errorf("the value of %s starts with '-' and would read as a flag", f.Name)
		}
	}
	return nil
}

// modelFile is one YAML file of the models directory.
type modelFile struct {
	name string
	doc  *yaml.Node
}

func readModelFiles(dir string) ([]modelFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []modelFile
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		files = append(files, modelFile{name: e.Name(), doc: &doc})
	}
	return files, nil
}

// root is the document's top mapping, created if the file is empty.
func (f modelFile) root() *yaml.Node {
	if f.doc.Kind == 0 {
		f.doc.Kind = yaml.DocumentNode
	}
	if len(f.doc.Content) == 0 {
		f.doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	return f.doc.Content[0]
}

func (f modelFile) models() *yaml.Node {
	return mappingValue(f.root(), "models", true)
}

func mappingValue(m *yaml.Node, key string, create bool) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	if !create {
		return nil
	}
	v := &yaml.Node{Kind: yaml.MappingNode}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
	return v
}

func setValue(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v.HeadComment, v.LineComment = m.Content[i+1].HeadComment, m.Content[i+1].LineComment
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
}

func deleteKey(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

// leadingComment is the comment at the top of a file. yaml.v3 hangs it on
// the document, the top mapping or the first key, depending on blank lines.
func leadingComment(doc *yaml.Node) string {
	parts := []string{doc.HeadComment}
	if len(doc.Content) > 0 {
		root := doc.Content[0]
		parts = append(parts, root.HeadComment)
		if len(root.Content) > 0 {
			parts = append(parts, root.Content[0].HeadComment)
		}
	}
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

func (f modelFile) bytes() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(f.doc); err != nil {
		return nil, err
	}
	enc.Close()
	return buf.Bytes(), nil
}

func listModels(files []modelFile) []Model {
	var out []Model
	for _, f := range files {
		models := mappingValue(f.root(), "models", false)
		if models == nil || models.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(models.Content); i += 2 {
			out = append(out, decodeModel(f, models.Content[i].Value, models.Content[i+1]))
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func decodeModel(f modelFile, name string, n *yaml.Node) Model {
	var raw struct {
		Cmd         string   `yaml:"cmd"`
		TTL         *int     `yaml:"ttl"`
		Aliases     []string `yaml:"aliases"`
		Description string   `yaml:"description"`
		Unlisted    bool     `yaml:"unlisted"`
		Metadata    struct {
			HF *HFSource `yaml:"hf"`
		} `yaml:"metadata"`
	}
	n.Decode(&raw)
	m := Model{
		Name: name, File: f.name, Cmd: raw.Cmd, TTL: raw.TTL, Aliases: raw.Aliases,
		Description: raw.Description, Unlisted: raw.Unlisted,
		Comment: leadingComment(f.doc), HF: raw.Metadata.HF,
	}
	if m.Aliases == nil {
		m.Aliases = []string{}
	}
	runtime, gguf, flags, ok := parseCmd(raw.Cmd)
	if !ok {
		m.Raw = true
		m.Flags = []Flag{}
		return m
	}
	m.Runtime, m.GGUF, m.Flags = runtime, gguf, flags
	if m.Flags == nil {
		m.Flags = []Flag{}
	}
	return m
}

// encodeModel writes m into its mapping node, keeping every key the UI does
// not edit (env, filters, metadata, ...) and their comments.
func encodeModel(n *yaml.Node, m Model) error {
	if n.Kind != yaml.MappingNode {
		*n = yaml.Node{Kind: yaml.MappingNode}
	}
	cmd := m.Cmd
	if !m.Raw {
		if m.Runtime == "" || m.GGUF == "" {
			return fmt.Errorf("a model needs a runtime and a GGUF")
		}
		if !filepath.IsAbs(m.GGUF) {
			return fmt.Errorf("the GGUF must be an absolute path")
		}
		if err := checkFlags(m.Flags); err != nil {
			return err
		}
		cmd = renderCmd(m.Runtime, m.GGUF, m.Flags)
		// The last word is llama-swap's: a value it would split differently,
		// such as one with a line starting with '#', is refused rather than
		// written as something else.
		runtime, gguf, flags, ok := parseCmd(cmd)
		if !ok || runtime != m.Runtime || gguf != m.GGUF || !sameFlags(flags, m.Flags) {
			return fmt.Errorf("llama-swap would read this command differently than the table shows; edit it as text")
		}
	}
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("a model needs a cmd")
	}
	setValue(n, "cmd", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: cmd, Style: yaml.LiteralStyle})
	if mappingValue(n, "proxy", false) == nil {
		setValue(n, "proxy", &yaml.Node{Kind: yaml.ScalarNode, Value: "http://127.0.0.1:${PORT}"})
	}
	if m.TTL != nil {
		setValue(n, "ttl", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(*m.TTL)})
	} else {
		deleteKey(n, "ttl")
	}
	for _, a := range m.Aliases {
		if err := checkName(a); err != nil {
			return fmt.Errorf("alias %w", err)
		}
	}
	if len(m.Aliases) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, a := range m.Aliases {
			seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: a})
		}
		setValue(n, "aliases", seq)
	} else {
		deleteKey(n, "aliases")
	}
	if m.Description != "" {
		desc := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: m.Description}
		if strings.Contains(m.Description, "\n") {
			// yaml.v3 writes a multi-line string as a literal block, which
			// drops a leading newline. Quoted, every character survives.
			desc.Style = yaml.DoubleQuotedStyle
		}
		setValue(n, "description", desc)
	} else {
		deleteKey(n, "description")
	}
	if m.Unlisted {
		setValue(n, "unlisted", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	} else {
		deleteKey(n, "unlisted")
	}
	// metadata.hf only; any other metadata is the user's and stays.
	if m.HF != nil {
		hf := &yaml.Node{}
		if err := hf.Encode(m.HF); err != nil {
			return err
		}
		setValue(mappingValue(n, "metadata", true), "hf", hf)
	} else if meta := mappingValue(n, "metadata", false); meta != nil {
		deleteKey(meta, "hf")
		if len(meta.Content) == 0 {
			deleteKey(n, "metadata")
		}
	}
	return nil
}

// fromHF points the command at the files m.HF names, under downloads' Dir.
func fromHF(m *Model, downloads *Downloads) error {
	if m.HF.Model == "" {
		return fmt.Errorf("a Hugging Face source needs its model file")
	}
	for _, src := range m.HF.sources() {
		if err := checkHF(src); err != nil {
			return err
		}
	}
	if m.Raw {
		return fmt.Errorf("a command edited as text cannot take its files from Hugging Face")
	}
	m.GGUF = downloads.Path(m.HF.Model)
	if m.HF.MMProj == "" {
		return nil
	}
	mmproj := downloads.Path(m.HF.MMProj)
	for i, f := range m.Flags {
		if f.Name == "--mmproj" || f.Name == "-mm" {
			m.Flags[i].Value = &mmproj
			return nil
		}
	}
	m.Flags = append(m.Flags, Flag{Name: "--mmproj", Value: &mmproj})
	return nil
}

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]*$`)

func checkName(name string) error {
	if !modelName.MatchString(name) {
		return fmt.Errorf("%q: a model name is letters, digits and . _ : + -", name)
	}
	return nil
}
