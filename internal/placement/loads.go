package placement

import (
	"os"
	"path/filepath"
	"regexp"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// envMacro is llama-swap's ${env.VAR} (internal/config/macros.go).
var envMacro = regexp.MustCompile(`\$\{env\.([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// LoadsWith loads the part's file with base, llama-swap's -config, as
// llama-swap does once the file is in -config-dir. A macro the host's Nix
// does not define, such as a runtime only another host has, fails here
// and not in llama-swap's reload (0022, 11). With env false each
// ${env.VAR} reads as a placeholder, for a caller without the daemon's
// environment, which holds the clouds' keys.
func (p *Part) LoadsWith(base string, env bool) error {
	dir, err := os.MkdirTemp("", "infermux-part-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	part := p.LlamaSwap
	if !env {
		part = placeholders(part)
		if base != "" {
			raw, err := os.ReadFile(base)
			if err != nil {
				return err
			}
			base = filepath.Join(dir, "base.yaml")
			if err := os.WriteFile(base, placeholders(raw), 0o600); err != nil {
				return err
			}
		}
	}
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

func placeholders(raw []byte) []byte {
	return envMacro.ReplaceAll(raw, []byte("env-$1"))
}
