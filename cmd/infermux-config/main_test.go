package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/configstore"
)

// verify with -host and -config loads that host's part with its base config,
// as llama-swap loads both at start, which is the build's check (0022, 11):
// a model of a's on a macro a's Nix dropped fails a's build and not b's.
func TestVerifyLoadsTheHostsPartWithItsBaseConfig(t *testing.T) {
	dir := t.TempDir()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, "keys")
	os.WriteFile(keys, []byte(configstore.PublicKeyString(pub)+"\n"), 0o600)
	b, err := configstore.Sign(map[string][]byte{
		"infermux.yaml": []byte("hosts:\n  a:\n    url: https://a.example\n    devices:\n      gpu: {kind: gpu}\n  b:\n    url: https://b.example\n    devices:\n      gpu: {kind: gpu}\n" +
			"clouds:\n  c:\n    url: https://c.example\n    api_key: ${env.INFERMUX_TEST_UNSET_VERIFY}\n    models: [x]\n" +
			"balance:\n  m: [a/gpu]\n  n: [b/gpu]\n"),
		"models/m.yaml": []byte("cmd: ${dropped} --port ${PORT}\n"),
		"models/n.yaml": []byte("cmd: ${runtime} --port ${PORT}\n"),
	}, key, "a", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config")
	if err := configstore.WriteBundle(config, b); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, "base.yaml")
	os.WriteFile(base, []byte("macros:\n  runtime: run\n"), 0o600)

	if err := verify([]string{"-trusted-keys", keys, config}); err != nil {
		t.Fatalf("without -config: %v", err)
	}
	// The cloud reads the environment, which the build has not: left out.
	if err := verify([]string{"-trusted-keys", keys, "-host", "b", "-config", base, config}); err != nil {
		t.Fatalf("b's part: %v", err)
	}
	err = verify([]string{"-trusted-keys", keys, "-host", "a", "-config", base, config})
	if err == nil || !strings.Contains(err.Error(), "unknown macro '${dropped}'") {
		t.Fatalf("a's part, on a macro its base config lacks: %v", err)
	}
	if err := verify([]string{"-trusted-keys", keys, "-host", "b", config}); err == nil || !strings.HasPrefix(err.Error(), "verify -trusted-keys") {
		t.Fatalf("-host without -config: %v", err)
	}
}
