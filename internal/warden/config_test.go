package warden

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheExampleFileLoadsWithEveryKeyRead(t *testing.T) {
	cfg, err := LoadConfig("../../warden.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ComfyUIURL == "" || len(cfg.OurUnits) != 1 || len(cfg.DesktopProcesses) != 2 ||
		len(cfg.BatchAPIKeys) != 1 || len(cfg.Consumers) != 1 {
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
