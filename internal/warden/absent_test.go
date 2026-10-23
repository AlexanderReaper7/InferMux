package warden

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// lines records the warden's log, each line with its level.
type lines struct{ got []string }

func (l *lines) Infof(format string, args ...any) {
	l.got = append(l.got, "INFO "+fmt.Sprintf(format, args...))
}

func (l *lines) Warnf(format string, args ...any) {
	l.got = append(l.got, "WARN "+fmt.Sprintf(format, args...))
}

func (l *lines) count(prefix string) int {
	n := 0
	for _, line := range l.got {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// What NVML answers in strix's rtx3080-passthrough boot, the 3080 bound to
// vfio-pci and nvidia blacklisted (2026-10-23).
var driverNotLoaded = GPUAbsent{Detail: "nvml init: Driver Not Loaded"}

// absentHarness is strix under the one config: the embedder on the GPU with
// its CPU place beside it, a chat model on the GPU with no need, and the
// loop's probe and the fresh one both reading what h.reading and h.probeErr
// say. The CPU's reading is a fixed quiet one.
func absentHarness(t *testing.T) (*harness, *lines, *int) {
	t.Helper()
	h := newHarness(t, func(c *Config) { c.Host = "this" })
	log := &lines{}
	h.w.log = log
	// A quiet CPU, not this host's: under a placement the loop measures it too.
	h.w.cpuProbe = func() (CPUResources, error) {
		return CPUResources{ForeignPercent: f64(5), RAMAvailableMB: 60000, RAMTotalMB: 96000}, nil
	}
	h.w.SetPlacement(Placement{
		"embed":     {Devices: []string{DeviceGPU}, VRAMMB: 5900},
		"chat":      {Devices: []string{DeviceGPU, DeviceCPU}},
		"embed@cpu": {Devices: []string{DeviceCPU}},
	})
	fresh := new(int)
	h.w.freshProbe = func() (Resources, error) { *fresh++; return h.reading, h.probeErr }
	return h, log, fresh
}

// Absence is a state: 36 polls of a host with no driver loaded, three
// minutes of the boot that logged a WARN every 5 s, log one INFO line and no
// WARN, the GPU's verdict resumes and says why, and /warden/verdict carries
// the reason with no probe error. The card's arrival is one INFO line too.
func TestAnAbsentGPUIsLoggedOnceAndShownAsAbsent(t *testing.T) {
	h, log, _ := absentHarness(t)
	h.probeErr = driverNotLoaded
	for range 36 {
		h.w.Tick()
		h.clock = h.clock.Add(5 * time.Second)
	}
	if log.count("WARN") != 0 || log.count("INFO GPU absent: nvml init: Driver Not Loaded") != 1 {
		t.Fatalf("logged %q", log.got)
	}
	s := h.w.State()
	if s.Verdict.Yielded || !strings.Contains(s.Verdict.Reason, "no GPU on this host: nvml init: Driver Not Loaded") {
		t.Fatalf("verdict %+v", s.Verdict)
	}
	if s.GPUAbsent == nil || *s.GPUAbsent != "nvml init: Driver Not Loaded" || s.ProbeError != nil || s.Resources != nil {
		t.Fatalf("absent %v, probe error %v, resources %v", s.GPUAbsent, s.ProbeError, s.Resources)
	}

	h.probeErr = nil
	h.reading = quiet()
	h.reading.VRAMTotalMB = 10240
	for range 3 {
		h.w.Tick()
		h.clock = h.clock.Add(5 * time.Second)
	}
	if n := log.count("INFO GPU present: 10240 MB, 8000 MB free"); n != 1 || log.count("WARN") != 0 {
		t.Fatalf("logged %q", log.got)
	}
	if s := h.w.State(); s.GPUAbsent != nil || s.Resources == nil {
		t.Fatalf("absent %v, resources %v", s.GPUAbsent, s.Resources)
	}
}

// A GPU that disappears while the warden is yielded resumes by the usual
// rule, after resume_quiet_seconds: nothing on the host contends for a card
// it does not have, and no model can run there.
func TestAYieldEndsWhileTheGPUIsAbsent(t *testing.T) {
	h, _, _ := absentHarness(t)
	h.reading = busy(80, "blender")
	h.w.Tick()
	if !h.w.State().Verdict.Yielded {
		t.Fatal("did not yield")
	}
	h.probeErr = driverNotLoaded
	h.clock = h.clock.Add(5 * time.Second)
	if h.w.Tick().Yielded != true {
		t.Fatal("resumed before the quiet window")
	}
	h.clock = h.clock.Add(time.Duration(DefaultConfig().Policy.ResumeQuietSeconds) * time.Second)
	if v := h.w.Tick(); v.Yielded || !strings.Contains(v.Reason, "no GPU") {
		t.Fatalf("verdict %+v", v)
	}
}

// A probe that fails for another reason still leaves the verdict alone, and
// warns once per failure rather than every poll; the recovery is logged.
func TestAFailedProbeWarnsOncePerFailure(t *testing.T) {
	h, log, _ := absentHarness(t)
	h.probeErr = errors.New("nvml init: Unknown Error")
	for range 10 {
		h.w.Tick()
	}
	if log.count("WARN Warden probe failed, verdict unchanged: nvml init: Unknown Error") != 1 || len(log.got) != 1 {
		t.Fatalf("logged %q", log.got)
	}
	if s := h.w.State(); s.ProbeError == nil || s.GPUAbsent != nil {
		t.Fatalf("probe error %v, absent %v", s.ProbeError, s.GPUAbsent)
	}
	h.probeErr = nil
	h.w.Tick()
	h.probeErr = errors.New("nvml memory: GPU is lost")
	h.w.Tick()
	h.w.Tick()
	want := []string{
		"WARN Warden probe failed, verdict unchanged: nvml init: Unknown Error",
		"INFO Warden probe reads the card again after: nvml init: Unknown Error",
		"WARN Warden probe failed, verdict unchanged: nvml memory: GPU is lost",
	}
	if fmt.Sprint(log.got) != fmt.Sprint(want) {
		t.Fatalf("logged %q\nwant %q", log.got, want)
	}
}

// A start of a model the one config places on the GPU is refused at once
// while the host has none: a 503 failover moves on from, marked absent, and
// one INFO line per model per absence however many starts are refused. The
// CPU's place starts, and so does a model the placement does not name.
func TestAStartOnAnAbsentGPUIsRefused(t *testing.T) {
	h, log, fresh := absentHarness(t)
	h.probeErr = driverNotLoaded
	h.w.Tick()
	for range 5 {
		err := h.w.Fits("embed")
		var absent GPUAbsentError
		if !errors.As(err, &absent) {
			t.Fatalf("started: %v", err)
		}
		if absent.StatusCode() != http.StatusServiceUnavailable || absent.Header().Get("Retry-After") != "30" || absent.Header().Get(AbsentHeader) != DeviceGPU {
			t.Fatalf("%d %v", absent.StatusCode(), absent.Header())
		}
		if body := string(absent.Body()); !strings.Contains(body, `"type":"gpu_absent"`) || !strings.Contains(body, "embed is placed on the GPU and this host has none now: nvml init: Driver Not Loaded") {
			t.Fatalf("body %s", body)
		}
	}
	if err := h.w.Fits("chat"); !errors.As(err, new(GPUAbsentError)) {
		t.Fatalf("chat, with no need, started: %v", err)
	}
	for _, model := range []string{"embed@cpu", "unplaced"} {
		if err := h.w.Fits(model); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		h.w.StartEnded(model)
	}
	if n := log.count("INFO <embed> not started: the GPU is absent"); n != 1 || log.count("INFO <chat> not started") != 1 || log.count("WARN") != 0 {
		t.Fatalf("logged %q", log.got)
	}
	// Within one poll of the loop's reading, the card is not read again.
	if *fresh != 0 {
		t.Fatalf("the card was read %d times", *fresh)
	}
	// The slot is free: a hold or an absence never keeps it.
	h.w.mu.Lock()
	slot := h.w.gpuStart
	h.w.mu.Unlock()
	if slot != "" {
		t.Fatalf("%s holds the start slot", slot)
	}
}

// The card handed to the host at runtime, `svm gpu host` (2026-10-22), is
// used at the next start once the absence is a poll old, without waiting for
// the loop or a restart, and with the policy off, which reads the card only
// at a start.
func TestACardHandedToTheHostIsUsedAtTheNextStart(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		h, log, fresh := absentHarness(t)
		h.w.cfg.Policy.Enabled = enabled
		h.probeErr = driverNotLoaded
		h.w.Tick()
		if err := h.w.Fits("embed"); !errors.As(err, new(GPUAbsentError)) {
			t.Fatalf("enabled %v: started: %v", enabled, err)
		}
		h.probeErr = nil
		h.reading = quiet()
		h.clock = h.clock.Add(time.Second)
		if err := h.w.Fits("embed"); !errors.As(err, new(GPUAbsentError)) {
			t.Fatalf("enabled %v: read again within a poll: %v", enabled, err)
		}
		h.clock = h.clock.Add(5 * time.Second)
		if err := h.w.Fits("embed"); err != nil {
			t.Fatalf("enabled %v: %v", enabled, err)
		}
		h.w.StartEnded("embed")
		if log.count("INFO GPU present") != 1 || log.count("WARN") != 0 {
			t.Fatalf("enabled %v: logged %q", enabled, log.got)
		}
		if *fresh == 0 {
			t.Fatalf("enabled %v: the card was not read at the start", enabled)
		}
	}
}

// With the loop not yet run, or the policy off, the start gate's own reading
// finds the card absent and refuses the start, rather than letting it
// through as a card it could not read.
func TestTheStartGatesReadingFindsTheGPUAbsent(t *testing.T) {
	h, log, _ := absentHarness(t)
	h.w.cfg.Policy.Enabled = false
	h.probeErr = driverNotLoaded
	if err := h.w.Fits("embed"); !errors.As(err, new(GPUAbsentError)) {
		t.Fatalf("started: %v", err)
	}
	if log.count("WARN") != 0 || log.count("INFO GPU absent") != 1 {
		t.Fatalf("logged %q", log.got)
	}
}

// Outside the one config every model is on the GPU by default, and a host
// without a GPU runs them as it did.
func TestOutsideTheOneConfigAnAbsentGPURefusesNothing(t *testing.T) {
	h, _, _ := absentHarness(t)
	h.w.SetPlacement(nil)
	h.probeErr = driverNotLoaded
	h.w.Tick()
	if err := h.w.Fits("embed"); err != nil {
		t.Fatal(err)
	}
}

// The request gate refuses a request for a model on the absent GPU before
// llama-swap, with the same answer, and passes the CPU's place.
func TestTheRequestGateRefusesAModelOnAnAbsentGPU(t *testing.T) {
	h, _, _ := absentHarness(t)
	h.probeErr = driverNotLoaded
	h.w.Tick()
	reached := []string{}
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.URL.Path)
		rw.WriteHeader(http.StatusOK)
	}))
	rec := send(handler, "embed", "batch-key")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get(AbsentHeader) != DeviceGPU || !strings.Contains(rec.Body.String(), `"type":"gpu_absent"`) {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := send(handler, "embed@cpu", "batch-key"); rec.Code != http.StatusOK {
		t.Fatalf("the CPU's place: %d %s", rec.Code, rec.Body)
	}
	if len(reached) != 1 {
		t.Fatalf("reached llama-swap %d times", len(reached))
	}
}
