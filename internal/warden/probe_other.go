//go:build !linux

package warden

import "errors"

// The probe reads NVML and /proc, so it is Linux only (0002). Elsewhere the
// warden still classifies requests, and the probe always fails, which leaves
// the verdict alone.
func newProbe(Config) func() (Resources, error) {
	return func() (Resources, error) { return Resources{}, errors.New("the GPU probe is Linux only") }
}
