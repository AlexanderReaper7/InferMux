package placement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Without env the part loads without its models and clouds that read the
// environment, whatever type the field they sit in has; with it, as in
// llama-swap (0022, 11).
func TestLoadsWithLeavesWhatReadsTheEnvironmentToTheDaemon(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(base, []byte("macros:\n  runtime: run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	part := &Part{LlamaSwap: []byte("models:\n  m:\n    cmd: ${runtime} --port ${PORT}\n    ttl: ${env.INFERMUX_TEST_TTL}\n" +
		"  n:\n    cmd: ${runtime} --port ${PORT}\n" +
		"groups:\n  gpu:\n    members: [m, n]\n" +
		"peers:\n  c:\n    proxy: https://c.example\n    apiKey: ${env.INFERMUX_TEST_UNSET_PART}\n    models: [x]\n")}
	if err := part.LoadsWith(base, false); err != nil {
		t.Fatalf("without env: %v", err)
	}
	t.Setenv("INFERMUX_TEST_TTL", "30")
	t.Setenv("INFERMUX_TEST_UNSET_PART", "k")
	if err := part.LoadsWith(base, true); err != nil {
		t.Fatalf("with env: %v", err)
	}
	t.Setenv("INFERMUX_TEST_TTL", "soon")
	if err := part.LoadsWith(base, true); err == nil {
		t.Fatal("with env, a ttl that is no number loaded")
	}

	// A name reads it too: llama-swap substitutes in keys as in values.
	part.LlamaSwap = []byte("models:\n  n:\n    cmd: ${runtime} --port ${PORT}\ngroups:\n  ${env.INFERMUX_TEST_UNSET_GROUP}:\n    members: [n]\n")
	if err := part.LoadsWith(base, false); err != nil {
		t.Fatalf("a group named from the environment: %v", err)
	}

	// What reads no environment is still loaded.
	part.LlamaSwap = []byte("models:\n  m:\n    cmd: ${elsewhere} --port ${PORT}\n  n:\n    cmd: ${runtime} --port ${PORT}\n    ttl: ${env.INFERMUX_TEST_TTL}\n")
	if err := part.LoadsWith(base, false); err == nil || !strings.Contains(err.Error(), "unknown macro '${elsewhere}'") {
		t.Fatalf("an unknown macro: %v", err)
	}
	// A base config that reads the environment leaves the whole load to the
	// daemon; one in a comment is not read.
	if err := os.WriteFile(base, []byte("# ${env.INFERMUX_TEST_TTL}\nmacros:\n  runtime: run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := part.LoadsWith(base, false); err == nil {
		t.Fatal("a comment in the base config stopped the load")
	}
	if err := os.WriteFile(base, []byte("macros:\n  runtime: run --key ${env.INFERMUX_TEST_UNSET_BASE}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := part.LoadsWith(base, false); err != nil {
		t.Fatalf("a base config that reads the environment: %v", err)
	}
}
