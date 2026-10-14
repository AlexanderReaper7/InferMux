package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/warden"
)

// quietVRAMMB is the free VRAM the tests' GPU reports, a number no card has.
const quietVRAMMB = 12345

// Every warden here reads a GPU and a CPU that nothing else uses, never the
// host's: a game on strix's card would yield the warden and cancel the
// tests' batch work before the test began (sol, review 12, M2).
func TestMain(m *testing.M) {
	probes = warden.Probes{
		GPU: func() (warden.Resources, error) {
			zero, free := 0.0, quietVRAMMB
			return warden.Resources{ForeignGPUPercent: &zero, GPUPercent: &zero, VRAMFreeMB: &free, VRAMTotalMB: quietVRAMMB, SampledAt: time.Now()}, nil
		},
		CPU: func() (warden.CPUResources, error) {
			zero := 0.0
			return warden.CPUResources{ForeignPercent: &zero, OurPercent: &zero, Cores: 1, RAMAvailableMB: 1 << 20, RAMTotalMB: 1 << 20, SampledAt: time.Now()}, nil
		},
	}
	os.Exit(m.Run())
}

// The daemon a test starts reads the tests' GPU, not the host's.
func TestTheTestsReadNoHostProbe(t *testing.T) {
	d := newDrainDaemon(t, time.Second)
	resp := d.do(t, "GET", "/warden/resources", "me-key", "")
	var r warden.Resources
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.VRAMFreeMB == nil || *r.VRAMFreeMB != quietVRAMMB {
		t.Fatalf("/warden/resources: %d %v %+v", resp.StatusCode, err, r)
	}
}
