package warden

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// The warden on every device (0022): under the one config a yield refuses,
// cancels and unloads only the models on the device that yielded.

// placed is a harness under the one config: qwen on the GPU, embed on the
// CPU, big on both, and a CPU reading that is quiet until a test moves it.
func placed(t *testing.T) (*harness, *CPUResources) {
	t.Helper()
	h := newHarness(t, func(c *Config) { c.Host = "this"; c.CPU.ResumeQuietSeconds = 120 })
	h.models.running = map[string]string{"qwen": "ready", "embed": "ready", "big": "ready"}
	reading := &CPUResources{ForeignPercent: f64(5), RAMAvailableMB: 60000, RAMTotalMB: 96000}
	var probeErr error
	h.w.cpuProbe = func() (CPUResources, error) { return *reading, probeErr }
	h.w.SetPlacement(Placement{"qwen": {DeviceGPU}, "embed": {DeviceCPU}, "big": {DeviceGPU, DeviceCPU}})
	return h, reading
}

func send(handler http.Handler, model, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestABusyCPURefusesBatchOnItsModelsOnly(t *testing.T) {
	h, cpu := placed(t)
	cpu.ForeignPercent = f64(80)
	cpu.Culprits = []string{"rustc"}
	h.w.Tick()
	if v := h.w.State().CPU.Verdict; !v.Yielded || !strings.Contains(v.Reason, "rustc") {
		t.Fatalf("CPU verdict %+v", v)
	}
	if h.w.State().Verdict.Yielded {
		t.Fatal("the GPU yielded for the CPU's load")
	}
	served := 0
	handler := h.w.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ }))
	for model, want := range map[string]int{"qwen": 200, "embed": 503, "big": 503, "nobody-knows": 503} {
		rec := send(handler, model, "batch-key")
		if rec.Code != want {
			t.Errorf("batch %s: %d, want %d: %s", model, rec.Code, want, rec.Body)
		}
		if want == 503 && (!strings.Contains(rec.Body.String(), "cpu_yielded") || rec.Header().Get("Retry-After") != "120") {
			t.Errorf("batch %s refused as %s, Retry-After %s", model, rec.Body, rec.Header().Get("Retry-After"))
		}
	}
	if rec := send(handler, "embed", "my-key"); rec.Code != 200 {
		t.Fatalf("the user's own request on the busy CPU: %d", rec.Code)
	}
	if served != 2 {
		t.Fatalf("served %d", served)
	}
	if h.models.unloads != 0 {
		t.Fatal("a busy CPU unloaded its models; only RAM under the floor does")
	}
}

func TestACPUYieldCancelsBatchOnTheCPUOnly(t *testing.T) {
	h, cpu := placed(t)
	var onCPU, onGPU, onBoth string
	h.w.traffic.begin(Batch, "/v1/embeddings", []string{DeviceCPU}, func(d, _ string) { onCPU = d }, nil)
	h.w.traffic.begin(Batch, "/v1/chat/completions", []string{DeviceGPU}, func(d, _ string) { onGPU = d }, nil)
	h.w.traffic.begin(Batch, "/v1/chat/completions", []string{DeviceGPU, DeviceCPU}, func(d, _ string) { onBoth = d }, nil)
	cpu.ForeignPercent = f64(80)
	h.w.Tick()
	if onCPU != DeviceCPU || onBoth != DeviceCPU || onGPU != "" {
		t.Fatalf("cancelled: cpu %q gpu %q both %q", onCPU, onGPU, onBoth)
	}
}

func TestRAMUnderTheFloorUnloadsTheCPUsModelsOnceInteractiveIsQuiet(t *testing.T) {
	h, cpu := placed(t)
	// The user is chatting on the GPU: that does not hold the CPU's unload.
	h.w.traffic.begin(Interactive, "/v1/chat/completions", []string{DeviceGPU}, func(string, string) {}, nil)
	id, _ := h.w.traffic.begin(Interactive, "/v1/embeddings", []string{DeviceCPU}, func(string, string) {}, nil)
	cpu.RAMAvailableMB = 2000
	h.w.Tick()
	s := h.w.State()
	if !s.CPU.Verdict.Yielded || !s.CPU.Verdict.RAMShort || !s.CPU.PendingUnload || h.models.unloads != 0 {
		t.Fatalf("under a request on the CPU: %+v, unloads %d", s.CPU, h.models.unloads)
	}
	h.w.traffic.end(id)
	h.at(5 * time.Second).w.Tick()
	if h.models.unloads != 0 {
		t.Fatal("unloaded within interactive_recent_seconds of the CPU's last request")
	}
	h.at(11 * time.Minute).w.Tick()
	if h.models.unloads != 1 || !slices.Equal(h.models.unloaded, []string{"big", "embed"}) {
		t.Fatalf("unloaded %d times: %v", h.models.unloads, h.models.unloaded)
	}
	if _, ok := h.models.running["qwen"]; !ok {
		t.Fatal("the GPU's model went with the CPU's")
	}
	// Still short: the unload was owed once.
	h.models.running["embed"] = "ready"
	h.at(12 * time.Minute).w.Tick()
	if h.models.unloads != 1 {
		t.Fatalf("unloaded again in the same shortage: %d", h.models.unloads)
	}
}

func TestRAMShortAgainDuringTheQuietOwesAnotherUnload(t *testing.T) {
	h, cpu := placed(t)
	cpu.RAMAvailableMB = 2000
	h.w.Tick()
	cpu.RAMAvailableMB = 60000
	h.at(5 * time.Second).w.Tick()
	if v := h.w.State().CPU.Verdict; !v.Yielded || v.RAMShort {
		t.Fatalf("RAM back, within the quiet: %+v", v)
	}
	h.models.running["embed"] = "ready"
	cpu.RAMAvailableMB = 2000
	h.at(10 * time.Second).w.Tick()
	if h.models.unloads != 2 {
		t.Fatalf("unloaded %d times", h.models.unloads)
	}
}

func TestAGPUYieldUnloadsOnlyTheGPUsModels(t *testing.T) {
	h, _ := placed(t)
	h.reading = busy(60, "blender")
	h.w.Tick()
	if !slices.Equal(h.models.unloaded, []string{"big", "qwen"}) {
		t.Fatalf("unloaded %v", h.models.unloaded)
	}
	if _, ok := h.models.running["embed"]; !ok {
		t.Fatal("the CPU's model went with the GPU's")
	}
}

func TestFreeVRAMIsReadWhileOnlyTheCPUsModelsRun(t *testing.T) {
	h, _ := placed(t)
	h.models.running = map[string]string{"embed": "ready"}
	h.reading = Resources{ForeignGPUPercent: f64(0), VRAMFreeMB: ip(1000)}
	if v := h.w.Tick(); !v.Yielded || !strings.Contains(v.Reason, "VRAM") {
		t.Fatalf("a CPU model hid low free VRAM: %+v", v)
	}
}

func TestOverflowAsksTheDevicesOfTheModel(t *testing.T) {
	h, cpu := placed(t)
	cpu.ForeignPercent = f64(80)
	h.w.Tick()
	for name, want := range map[string]bool{"this/embed": true, "this/big": true, "this/qwen": false, "zbox/embed": false, "openrouter/x": false} {
		if got := h.w.YieldedFor(name); got != want {
			t.Errorf("YieldedFor(%s) = %v", name, got)
		}
	}
	if !h.w.OnGPU("this/big") || h.w.OnGPU("this/embed") || h.w.OnGPU("zbox/qwen") {
		t.Fatal("OnGPU")
	}
}

func TestAFailedCPUProbeLeavesItsVerdictAlone(t *testing.T) {
	h, cpu := placed(t)
	cpu.ForeignPercent = f64(80)
	h.w.Tick()
	h.w.cpuProbe = func() (CPUResources, error) { return CPUResources{}, errors.New("cgroup gone") }
	h.at(time.Hour).w.Tick()
	s := h.w.State().CPU
	if !s.Verdict.Yielded || s.ProbeError == nil {
		t.Fatalf("%+v", s)
	}
}

func TestTheCPUResumesAfterItsOwnQuiet(t *testing.T) {
	h, cpu := placed(t)
	cfg := h.w.config()
	cfg.CPU.ResumeQuietSeconds = 60
	h.w.Reload(cfg)
	cpu.ForeignPercent = f64(80)
	h.w.Tick()
	cpu.ForeignPercent = f64(5)
	h.at(30 * time.Second).w.Tick()
	if !h.w.State().CPU.Verdict.Yielded {
		t.Fatal("resumed before its quiet")
	}
	h.at(61 * time.Second).w.Tick()
	if h.w.State().CPU.Verdict.Yielded || len(h.w.traffic.state().PausedDevices) != 0 {
		t.Fatalf("still yielded: %+v", h.w.State().CPU.Verdict)
	}
}

func TestDisablingTheWardenLetsTheCPUsBatchThrough(t *testing.T) {
	h, cpu := placed(t)
	cpu.ForeignPercent = f64(80)
	h.w.Tick()
	cfg := h.w.config()
	cfg.Policy.Enabled = false
	h.w.Reload(cfg)
	h.w.Tick()
	if h.w.State().CPU.Verdict.Yielded || len(h.w.traffic.state().PausedDevices) != 0 {
		t.Fatal("the CPU stayed yielded with the warden disabled")
	}
}

func TestOutsideTheOneConfigTheCPUIsNotMeasured(t *testing.T) {
	h := newHarness(t, nil)
	h.w.cpuProbe = func() (CPUResources, error) { t.Fatal("measured"); return CPUResources{}, nil }
	h.w.Tick()
	if h.w.State().CPU != nil {
		t.Fatal("a CPU state outside the one config")
	}
}

// warnings records the warden's warnings.
type warnings struct {
	nopLog
	lines []string
}

func (l *warnings) Warnf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// A model that stops before it is ready leaves a line naming what held the
// card at that moment, read fresh, and a model on the CPU alone leaves none.
func TestAModelThatDoesNotStartLogsWhatHeldTheCard(t *testing.T) {
	h := newHarness(t, nil)
	log := &warnings{}
	h.w.log = log
	h.w.SetPlacement(Placement{"qwen": {DeviceGPU}, "embed": {DeviceCPU}})
	h.w.probe = func() (Resources, error) { return quiet(), nil } // the loop's, not read here
	h.w.freshProbe = func() (Resources, error) {
		r := quiet()
		r.VRAMUsedMB, r.VRAMTotalMB = 9300, 10240
		r.Processes = []Process{
			{Name: "Xwayland", VRAMMB: 310, Desktop: true},
			{Name: "nvidia-smi", VRAMMB: 0},
			{Name: "python3", Unit: "comfyui.service", VRAMMB: 8800},
			{Name: "llama-server", Unit: "infermux.service", VRAMMB: 190, Ours: true},
		}
		return r, nil
	}
	h.w.NotStarted("embed")
	h.w.NotStarted("qwen")
	want := "<qwen> stopped before it was ready; the card had 9300 of 10240 MB in use: python3 8800 MB (comfyui.service, foreign), Xwayland 310 MB (desktop), llama-server 190 MB (infermux.service, ours)"
	if len(log.lines) != 1 || log.lines[0] != want {
		t.Fatalf("logged %q\nwant %q", log.lines, want)
	}

	log.lines = nil
	h.w.freshProbe = func() (Resources, error) { return Resources{}, errors.New("NVML: unknown error") }
	h.w.NotStarted("qwen")
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "could not be read: NVML: unknown error") {
		t.Fatalf("logged %q", log.lines)
	}
}
