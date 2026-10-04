package warden

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheExampleFileLoadsWithEveryKeyRead(t *testing.T) {
	cfg, err := LoadConfig("../../warden.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ComfyUIURL == "" || len(cfg.OurUnits) != 1 || len(cfg.DesktopProcesses) != 2 ||
		cfg.Host != "reaperboi" || cfg.Keys == nil || len(cfg.Keys.Keys) != 2 || len(cfg.Remotes) != 1 || len(cfg.Consumers) != 1 {
		t.Fatalf("a key was not read: %+v", cfg)
	}
	if cfg.Policy != DefaultConfig().Policy {
		t.Fatalf("the example's policy is not the defaults: %+v", cfg.Policy)
	}
	if cfg.Consumers[0].AnnouncePath != "/api/pipeline/announce" || cfg.Consumers[0].TimeoutSeconds != 10 {
		t.Fatalf("consumer defaults: %+v", cfg.Consumers[0])
	}
}

func TestAPartialFileKeepsTheOtherDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.yaml")
	os.WriteFile(path, []byte("policy:\n  gpu_busy_percent: 40\n"), 0o644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultConfig().Policy
	want.GPUBusyPercent = 40
	if cfg.Policy != want {
		t.Fatalf("got %+v", cfg.Policy)
	}
}

func TestAConsumerWithoutAURLIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.yaml")
	os.WriteFile(path, []byte("consumers:\n  - name: x\n"), 0o644)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("accepted")
	}
}

func TestTheOldBatchKeysAreRefusedNotDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.yaml")
	os.WriteFile(path, []byte("batch_api_keys: [episteme-batch]\n"), 0o644)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "keys.yaml") {
		t.Fatalf("got %v", err)
	}
}

func TestAKeysFileThatDoesNotCheckOutIsRefusedWhole(t *testing.T) {
	good := HashKey("k")
	for name, body := range map[string]string{
		"short hash":    "keys:\n  a: {sha256: abc, class: batch}\n",
		"upper hex":     "keys:\n  a: {sha256: " + strings.ToUpper(good) + ", class: batch}\n",
		"no class":      "keys:\n  a: {sha256: " + good + "}\n",
		"unknown class": "keys:\n  a: {sha256: " + good + ", class: urgent}\n",
		"the same key":  "keys:\n  a: {sha256: " + good + ", class: batch}\n  b: {sha256: " + good + ", class: interactive}\n",
		"bad pattern":   "keys:\n  a: {sha256: " + good + ", class: batch, allow: [\"[\"]}\n",
		"empty pattern": "keys:\n  a: {sha256: " + good + ", class: batch, allow: [\"\"]}\n",
		"bad name":      "keys:\n  \"a b\": {sha256: " + good + ", class: batch}\n",
		"not yaml":      "keys: [\n",
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "keys.yaml"), []byte(body), 0o644)
		os.WriteFile(filepath.Join(dir, "w.yaml"), []byte("keys_file: keys.yaml\n"), 0o644)
		if _, err := LoadConfig(filepath.Join(dir, "w.yaml")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestARemoteNamedTwiceOrAfterThisHostIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"twice":     "remotes:\n  - {name: zbox, url: https://a}\n  - {name: zbox, url: https://b}\n",
		"this host": "host: zbox\nremotes:\n  - {name: zbox, url: https://a}\n",
		"no url":    "remotes:\n  - {name: zbox}\n",
	} {
		path := filepath.Join(t.TempDir(), "w.yaml")
		os.WriteFile(path, []byte(body), 0o644)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// 0002: ComfyUI is contention, never ours. With its unit named, a file that
// also lists it as ours is refused, by the daemon and by the UI's save alike.
func TestComfyUIsUnitInOurUnitsIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.yaml")
	os.WriteFile(path, []byte("comfyui_unit: comfyui.service\nour_units: [llama-embed.service, comfyui.service]\n"), 0o644)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "0002") {
		t.Fatalf("accepted, or refused without the reason: %v", err)
	}
	os.WriteFile(path, []byte("comfyui_unit: comfyui.service\nour_units: [llama-embed.service]\n"), 0o644)
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
}
