//go:build linux

package warden

import (
	"errors"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// What NVML answers on a host with no GPU is the GPU absent, and anything
// else a probe that failed (0032). These call no NVML function: an error's
// string comes from go-nvml's own table while the library is not loaded.
func TestNoDriverNoLibraryAndNoDeviceAreTheGPUAbsent(t *testing.T) {
	for _, c := range []struct {
		name   string
		err    error
		absent bool
	}{
		{"driver not loaded", initError(nvml.ERROR_DRIVER_NOT_LOADED), true},
		{"no library", initError(nvml.ERROR_LIBRARY_NOT_FOUND), true},
		{"no permission", initError(nvml.ERROR_NO_PERMISSION), false},
		{"unknown", initError(nvml.ERROR_UNKNOWN), false},
		{"no device at the index", deviceError(0, nvml.ERROR_INVALID_ARGUMENT), true},
		{"device not found", deviceError(0, nvml.ERROR_NOT_FOUND), true},
		{"device lost", deviceError(0, nvml.ERROR_GPU_IS_LOST), false},
	} {
		if got := errors.As(c.err, new(GPUAbsent)); got != c.absent {
			t.Errorf("%s: %v absent %v, want %v", c.name, c.err, got, c.absent)
		}
	}
	if got := initError(nvml.ERROR_LIBRARY_NOT_FOUND).Error(); got != "nvml init: ERROR_LIBRARY_NOT_FOUND" {
		t.Errorf("detail %q", got)
	}
}
