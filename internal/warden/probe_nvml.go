//go:build linux

package warden

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// nvmlProbe samples one device's processes since the previous call. Stateful
// only in lastSeen: NVML returns the utilization samples newer than the
// timestamp it is given, so each call asks for what happened since the last.
type nvmlProbe struct {
	mu          sync.Mutex
	device      int
	attr        Attribution
	lastSeen    uint64
	initialised bool
}

func newProbe(cfg Config) func() (Resources, error) {
	p := &nvmlProbe{attr: newAttribution(cfg, "/proc")}
	return p.sample
}

func (p *nvmlProbe) sample() (Resources, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// libnvidia-ml.so.1 is dlopened here, on first use, so a host without the
	// driver still runs InferMux and only the probe fails. A failed init is
	// tried again next call: at boot the driver may not be up yet, and a
	// passthrough card bound to vfio-pci gets its driver only when the user
	// hands it to the host (0032).
	if !p.initialised {
		if ret := nvml.Init(); ret != nvml.SUCCESS {
			return Resources{}, initError(ret)
		}
		p.initialised = true
	}
	device, ret := nvml.DeviceGetHandleByIndex(p.device)
	if ret != nvml.SUCCESS {
		// The card or its driver went away under an initialised NVML. Shut
		// it down, so the next call initialises again and reads what is
		// there then.
		p.reset()
		return Resources{}, deviceError(p.device, ret)
	}
	memory, ret := device.GetMemoryInfo()
	if ret != nvml.SUCCESS {
		if gone(ret) {
			p.reset()
		}
		return Resources{}, fmt.Errorf("nvml memory: %s", nvml.ErrorString(ret))
	}
	var gpuPercent *float64
	if rates, ret := device.GetUtilizationRates(); ret == nvml.SUCCESS {
		g := float64(rates.Gpu)
		gpuPercent = &g
	}

	// A process in both lists (compute and graphics) reports the same figure
	// in each.
	vram := map[uint32]int{}
	compute := map[uint32]bool{}
	for i, list := range []func() ([]nvml.ProcessInfo, nvml.Return){
		device.GetComputeRunningProcesses,
		device.GetGraphicsRunningProcesses,
	} {
		rows, ret := list()
		if ret != nvml.SUCCESS {
			return Resources{}, fmt.Errorf("nvml processes: %s", nvml.ErrorString(ret))
		}
		for _, row := range rows {
			vram[row.Pid] = max(vram[row.Pid], int(row.UsedGpuMemory>>20))
			if i == 0 {
				compute[row.Pid] = true
			}
		}
	}

	now := uint64(time.Now().UnixMicro())
	since := p.lastSeen
	if since == 0 {
		since = now - 1_000_000
	}
	p.lastSeen = now
	util := map[uint32]float64{}
	samples, ret := device.GetProcessUtilization(since)
	switch ret {
	case nvml.SUCCESS:
		for _, s := range samples {
			util[s.Pid] = max(util[s.Pid], float64(s.SmUtil))
		}
	case nvml.ERROR_NOT_FOUND:
		// No process had a sample in the window: an idle card, not a failure.
	default:
		return Resources{}, fmt.Errorf("nvml process utilization: %s", nvml.ErrorString(ret))
	}

	return Summarise(vram, util, compute, int(memory.Used>>20), int(memory.Total>>20), gpuPercent, p.attr), nil
}

func (p *nvmlProbe) reset() {
	nvml.Shutdown()
	p.initialised, p.lastSeen = false, 0
}

// initError is a failed nvml.Init. No driver loaded and no NVIDIA library
// are the GPU absent, a state, not a failure (0032): in the rtx3080-passthrough
// boot the card is bound to vfio-pci and nvidia is blacklisted, so NVML
// answers ERROR_DRIVER_NOT_LOADED on every call until the card is handed to
// the host. Anything else is a probe that failed.
func initError(ret nvml.Return) error {
	detail := "nvml init: " + nvml.ErrorString(ret)
	switch ret {
	case nvml.ERROR_DRIVER_NOT_LOADED, nvml.ERROR_LIBRARY_NOT_FOUND:
		return GPUAbsent{Detail: detail}
	}
	return errors.New(detail)
}

// deviceError is a failed handle for the device the probe reads. No device
// at that index is the GPU absent; a card that fell off the bus is a failure.
func deviceError(index int, ret nvml.Return) error {
	detail := fmt.Sprintf("nvml device %d: %s", index, nvml.ErrorString(ret))
	switch ret {
	case nvml.ERROR_INVALID_ARGUMENT, nvml.ERROR_NOT_FOUND, nvml.ERROR_DRIVER_NOT_LOADED:
		return GPUAbsent{Detail: detail}
	}
	return errors.New(detail)
}

// gone is an answer after which NVML has to be initialised again.
func gone(ret nvml.Return) bool {
	switch ret {
	case nvml.ERROR_UNINITIALIZED, nvml.ERROR_DRIVER_NOT_LOADED, nvml.ERROR_GPU_IS_LOST, nvml.ERROR_INVALID_ARGUMENT, nvml.ERROR_NOT_FOUND:
		return true
	}
	return false
}
