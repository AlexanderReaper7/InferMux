package placement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Without env, ${env.VAR} reads as a placeholder in the part and in the base
// config alike; with it, an unset one fails as in llama-swap (0022, 11).
func TestLoadsWithReadsEnvAsAPlaceholderOnlyWithoutEnv(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(base, []byte("macros:\n  runtime: run --key ${env.INFERMUX_TEST_UNSET_BASE}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	part := &Part{LlamaSwap: []byte("models:\n  m:\n    cmd: ${runtime} --port ${PORT}\n")}
	if err := part.LoadsWith(base, false); err != nil {
		t.Fatalf("without env: %v", err)
	}
	if err := part.LoadsWith(base, true); err == nil || !strings.Contains(err.Error(), "INFERMUX_TEST_UNSET_BASE") {
		t.Fatalf("with env: %v", err)
	}
	part.LlamaSwap = []byte("models:\n  m:\n    cmd: ${runtime} --port ${PORT} --key ${env.INFERMUX_TEST_UNSET_PART}\n")
	if err := part.LoadsWith(base, false); err != nil {
		t.Fatalf("without env, in the part: %v", err)
	}
	part.LlamaSwap = []byte("models:\n  m:\n    cmd: ${elsewhere} --port ${PORT}\n")
	if err := part.LoadsWith(base, false); err == nil || !strings.Contains(err.Error(), "unknown macro '${elsewhere}'") {
		t.Fatalf("an unknown macro: %v", err)
	}
}
