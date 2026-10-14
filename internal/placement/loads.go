package placement

import (
	"os"
	"path/filepath"
	"regexp"

	"github.com/mostlygeek/llama-swap/internal/config"
	"gopkg.in/yaml.v3"
)

// envMacro is llama-swap's ${env.VAR} (internal/config/macros.go).
var envMacro = regexp.MustCompile(`\$\{env\.([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// LoadsWith loads the part's file with base, llama-swap's -config, as
// llama-swap does once the file is in -config-dir. A macro the host's Nix
// does not define, such as a runtime only another host has, fails here
// and not in llama-swap's reload (0022, 11).
//
// With env false the caller has not the daemon's environment, which holds
// the clouds' keys. llama-swap pastes each ${env.VAR} into the text before
// it parses, so no stand-in fits every field it can sit in. The part is
// loaded without its models, groups and clouds that read the environment, and
// not at all when base reads it: the daemon loads those before it keeps the
// config.
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
		return envMacro.Match(raw)
	}
	return nodeReadsEnv(&doc)
}

func nodeReadsEnv(n *yaml.Node) bool {
	if envMacro.MatchString(n.Value) {
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
// environment. Derive writes nothing else at the top.
func withoutEnv(part []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(part, &doc); err != nil || len(doc.Content) == 0 {
		return part, nil // llama-swap's load says what is wrong
	}
	top := doc.Content[0]
	for i := 1; i < len(top.Content); i += 2 {
		entries := top.Content[i]
		if entries.Kind != yaml.MappingNode {
			continue
		}
		var kept []*yaml.Node
		for j := 0; j+1 < len(entries.Content); j += 2 {
			if !nodeReadsEnv(entries.Content[j]) && !nodeReadsEnv(entries.Content[j+1]) {
				kept = append(kept, entries.Content[j], entries.Content[j+1])
			}
		}
		entries.Content = kept
	}
	return yaml.Marshal(&doc)
}
