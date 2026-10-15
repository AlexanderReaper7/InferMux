package placement

import (
	"os"
	"path/filepath"
	"regexp"

	"github.com/mostlygeek/llama-swap/internal/config"
	"gopkg.in/yaml.v3"
)

// EnvMacro is llama-swap's ${env.VAR} (internal/config/macros.go).
var EnvMacro = regexp.MustCompile(`\$\{env\.([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// LoadsWith loads the part's file with base, llama-swap's -config, as
// llama-swap does once the file is in -config-dir. A macro the host's Nix
// does not define, such as a runtime only another host has, fails here
// and not in llama-swap's reload (0022, 11).
//
// With env false the caller has not the daemon's environment, which holds
// the clouds' keys. llama-swap pastes each ${env.VAR} into the text before
// it parses, so no stand-in fits every field it can sit in. The part is
// loaded without its models, groups and clouds that read the environment,
// and without each that an alias ties to one left out, and not at all when
// base reads it: the daemon loads those before it keeps the config.
func (p *Part) LoadsWith(base string, env bool) error {
	part := p.LlamaSwap
	if !env {
		if base != "" {
			raw, err := os.ReadFile(base)
			if err != nil {
				return err
			}
			if readsEnv(raw) {
				return nil
			}
		}
		var err error
		if part, err = withoutEnv(part); err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp("", "infermux-part-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	derived := filepath.Join(dir, "derived")
	if err := os.Mkdir(derived, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(derived, "infermux.yaml"), part, 0o600); err != nil {
		return err
	}
	_, err = config.LoadConfigSources(base, derived)
	return err
}

// readsEnv is whether raw names ${env.VAR} outside a comment, where
// llama-swap substitutes it.
func readsEnv(raw []byte) bool {
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil {
		// llama-swap then substitutes in the text as it stands.
		return EnvMacro.Match(raw)
	}
	return nodeReadsEnv(&doc)
}

func nodeReadsEnv(n *yaml.Node) bool {
	if EnvMacro.MatchString(n.Value) {
		return true
	}
	for _, c := range n.Content {
		if nodeReadsEnv(c) {
			return true
		}
	}
	return false
}

// withoutEnv is the part without each model, group and cloud that reads the
// environment, and without each entry with an alias to a node in one left
// out, until none is left. Derive writes nothing else at the top. What it
// keeps are the part's own nodes in their order, and every alias in them
// names a node that stays, so they load as they do in the daemon. No alias
// is followed, so no anchor can make it loop (sol, review 14).
func withoutEnv(part []byte) ([]byte, error) {
	var doc yaml.Node
	if yaml.Unmarshal(part, &doc) != nil || len(doc.Content) == 0 {
		return part, nil // llama-swap's load says what is wrong
	}
	type entry struct{ key, value *yaml.Node }
	var entries []entry
	top := doc.Content[0]
	for i := 1; i < len(top.Content); i += 2 {
		if s := top.Content[i]; s.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(s.Content); j += 2 {
				entries = append(entries, entry{s.Content[j], s.Content[j+1]})
			}
		}
	}
	out := map[*yaml.Node]bool{}
	for _, e := range entries {
		if nodeReadsEnv(e.key) || nodeReadsEnv(e.value) {
			out[e.key] = true
		}
	}
	for changed := true; changed; {
		changed = false
		gone := map[*yaml.Node]bool{}
		for _, e := range entries {
			if out[e.key] {
				within(e.key, gone)
				within(e.value, gone)
			}
		}
		for _, e := range entries {
			if !out[e.key] && (namesAny(e.key, gone) || namesAny(e.value, gone)) {
				out[e.key] = true
				changed = true
			}
		}
	}
	for i := 1; i < len(top.Content); i += 2 {
		if s := top.Content[i]; s.Kind == yaml.MappingNode {
			var kept []*yaml.Node
			for j := 0; j+1 < len(s.Content); j += 2 {
				if !out[s.Content[j]] {
					kept = append(kept, s.Content[j], s.Content[j+1])
				}
			}
			s.Content = kept
		}
	}
	return yaml.Marshal(&doc)
}

// within adds n and every node under it to set, aliases not followed.
func within(n *yaml.Node, set map[*yaml.Node]bool) {
	if set[n] {
		return
	}
	set[n] = true
	for _, c := range n.Content {
		within(c, set)
	}
}

// namesAny is whether an alias under n names a node in set.
func namesAny(n *yaml.Node, set map[*yaml.Node]bool) bool {
	if n.Kind == yaml.AliasNode {
		return set[n.Alias]
	}
	for _, c := range n.Content {
		if namesAny(c, set) {
			return true
		}
	}
	return false
}
