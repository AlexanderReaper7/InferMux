package warden

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// The warden on every device (0022). Outside the one config every local
// model is on the GPU and the warden is what it always was. Under it, each
// local model is on the devices its placement occupies, the CPU has a verdict
// of its own, and a yield refuses, cancels and unloads only the models on the
// device that yielded.

// Placement is each local model's place here, by its llama-swap name.
type Placement map[string]Placed

// Placed is a local model's devices, DeviceGPU, DeviceCPU, or both, and the
// VRAM it needs to start on the GPU, 0 unchecked (Fits). A model on a GPU
// other than NVML's first is on none, since nothing measures it.
type Placed struct {
	Devices []string
	VRAMMB  int
}

// SetPlacement hands the warden the one config's placement. From then on the
// CPU is measured, and a model the placement does not name is on every
// device, so it is gated by both.
func (w *Warden) SetPlacement(p Placement) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.placement == nil && p != nil {
		w.cpu.verdict = initialVerdict()
	}
	w.placement = p
}

// devicesLocked is the devices a local model is on.
func (w *Warden) devicesLocked(model string) []string {
	if w.placement == nil {
		return []string{DeviceGPU}
	}
	if p, ok := w.placement[model]; ok {
		return p.Devices
	}
	return []string{DeviceGPU, DeviceCPU}
}

func (w *Warden) devices(model string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.devicesLocked(model)
}

// local is the llama-swap name of a model Qualify named <host>/<model>.
func (w *Warden) local(qualified string) (string, bool) {
	w.mu.Lock()
	host := w.cfg.Host
	w.mu.Unlock()
	return strings.CutPrefix(qualified, host+"/")
}

// YieldedFor says whether a device the model is on is yielded. Overflow asks
// it before it puts a model on a place marked only_if_idle (0021); another
// host's model and a peer's are on no device here.
func (w *Warden) YieldedFor(qualified string) bool {
	model, ok := w.local(qualified)
	if !ok {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, d := range w.devicesLocked(model) {
		if w.verdictLocked(d).Yielded {
			return true
		}
	}
	return false
}

// OnGPU says whether a local model occupies this host's GPU, where one model
// at a time is the rule overflow keeps (0021).
func (w *Warden) OnGPU(qualified string) bool {
	model, ok := w.local(qualified)
	return ok && slices.Contains(w.devices(model), DeviceGPU)
}

func (w *Warden) verdictLocked(device string) Verdict {
	if device == DeviceCPU {
		return w.cpu.verdict
	}
	return w.verdict
}

func (w *Warden) unloadLocked(device string) *unloadState {
	if device == DeviceCPU {
		return &w.cpu.unload
	}
	return &w.gpuUnload
}

// unloadState is what one device owes: the unload due since its yield, and
// the last one paid.
type unloadState struct {
	pending     bool
	deferLogged bool
	last        []string
}

// cpuDevice is the CPU's side of the warden, used under the one config.
type cpuDevice struct {
	verdict    Verdict
	resources  *CPUResources
	probeError *string
	unload     unloadState
}

// CPUState is the CPU in /warden/verdict.
type CPUState struct {
	Verdict       Verdict       `json:"verdict"`
	Policy        CPUPolicy     `json:"policy"`
	Resources     *CPUResources `json:"resources"`
	ProbeError    *string       `json:"probe_error"`
	PendingUnload bool          `json:"pending_unload"`
	LastUnload    []string      `json:"last_unload"`
}

// cpuTick measures the CPU and acts on its verdict. A failed reading leaves
// the verdict alone, as the GPU's does.
func (w *Warden) cpuTick(cfg Config) {
	w.mu.Lock()
	on, probe := w.placement != nil, w.cpuProbe
	w.mu.Unlock()
	if !on {
		return
	}
	res, err := probe()
	if err != nil {
		detail := err.Error()
		w.mu.Lock()
		w.cpu.probeError = &detail
		w.mu.Unlock()
		w.log.Warnf("Warden CPU probe failed, verdict unchanged: %s", detail)
		return
	}
	now := w.now()
	w.mu.Lock()
	w.cpu.probeError, w.cpu.resources = nil, &res
	before := w.cpu.verdict
	after := decide(before, IsCPUContended(res, cfg.CPU), now, cfg.CPU.ResumeQuietSeconds)
	w.cpu.verdict = after
	w.mu.Unlock()
	w.actCPU(before, after)
}

// actCPU carries out a change of the CPU's verdict. A busy CPU refuses and
// cancels its batch work and unloads nothing; RAM under the floor owes one
// unload of the CPU's models each time it falls there (0022, 1).
func (w *Warden) actCPU(before, after Verdict) {
	if before.Yielded != after.Yielded {
		w.log.Infof("CPU verdict: %s - %s", strings.ToUpper(after.Action()), after.Reason)
	}
	switch {
	case after.Yielded && !before.Yielded:
		if n := w.traffic.pause(DeviceCPU, after.Reason); n > 0 {
			w.log.Infof("Cancelled %d batch request(s) on the CPU", n)
		}
	case !after.Yielded && before.Yielded:
		w.traffic.resume(DeviceCPU)
		w.mu.Lock()
		w.cpu.unload.pending = false
		w.mu.Unlock()
	}
	if after.RAMShort && !before.RAMShort {
		w.mu.Lock()
		w.cpu.unload.pending, w.cpu.unload.deferLogged = true, false
		w.mu.Unlock()
	}
	w.settle(DeviceCPU)
}

// settle pays a device's owed unload unless the user is in the middle of an
// interactive session on it. For a priority process on the GPU only a
// request in flight counts as that (0017).
func (w *Warden) settle(device string) {
	w.mu.Lock()
	u := w.unloadLocked(device)
	pending, v := u.pending, w.verdictLocked(device)
	w.mu.Unlock()
	if !pending {
		return
	}
	window := time.Duration(w.config().Policy.InteractiveRecentSeconds) * time.Second
	if v.Priority && device == DeviceGPU {
		window = 0
	}
	if recent, last := w.traffic.interactiveRecent(window, device); recent {
		w.mu.Lock()
		logged := u.deferLogged
		u.deferLogged = true
		w.mu.Unlock()
		if !logged {
			w.log.Infof("Unload on %s deferred: interactive request %s", deviceName(device), describeLast(last, w.now()))
		}
		return
	}
	w.closeSessions(device, "infermux: "+deviceName(device)+" was yielded: "+v.Reason)
	unloaded := w.unloadOn(device)
	w.mu.Lock()
	u.pending, u.last = false, unloaded
	w.mu.Unlock()
	w.log.Infof("Models on %s unloaded: %s", deviceName(device), orNone(strings.Join(unloaded, ", ")))
}

// unloadOn stops the running models on device. Outside the one config that
// is every model, as it always was.
func (w *Warden) unloadOn(device string) []string {
	w.mu.Lock()
	whole := w.placement == nil
	w.mu.Unlock()
	if whole {
		return w.models.UnloadAll()
	}
	return w.models.Unload(w.runningOn(device))
}

// runningOn is the running models on device, sorted.
func (w *Warden) runningOn(device string) []string {
	running := w.models.Running()
	w.mu.Lock()
	defer w.mu.Unlock()
	var on []string
	for id := range running {
		if slices.Contains(w.devicesLocked(id), device) {
			on = append(on, id)
		}
	}
	sort.Strings(on)
	return on
}

// disabledCPU drops the CPU's verdict when the warden is disabled, and lets
// its batch work through again.
func (w *Warden) disabledCPU() {
	w.mu.Lock()
	was := w.cpu.verdict.Yielded
	w.cpu.verdict = Verdict{Reason: "the warden is disabled"}
	w.cpu.unload.pending = false
	w.mu.Unlock()
	if was {
		w.traffic.resume(DeviceCPU)
		w.log.Infof("CPU verdict: RESUME - the warden is disabled")
		w.announce()
	}
}
