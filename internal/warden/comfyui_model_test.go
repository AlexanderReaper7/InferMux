package warden

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// comfyHarness is a warden under the one config on host "this", whose GPU
// runs qwen and ComfyUI as the model comfyui_auto (0030). ComfyUI's queue,
// its /free and its clear are counted, and both probes read h.reading.
type comfyHarness struct {
	*harness
	jobs   *int
	frees  int
	clears int
}

func newComfyHarness(t *testing.T) *comfyHarness {
	t.Helper()
	h := &comfyHarness{harness: newHarness(t, func(c *Config) {
		c.Host = "this"
		c.ComfyUIURL = "http://comfy"
		c.ComfyUIUnit = "comfyui.service"
	}), jobs: ip(0)}
	h.models.running = map[string]string{"qwen": "ready", "comfyui_auto": "ready"}
	h.w.SetPlacement(Placement{"qwen": {Devices: []string{DeviceGPU}}, "chat": {Devices: []string{DeviceGPU}}, "comfyui_auto": {Devices: []string{DeviceGPU}}})
	h.w.SetComfyUIModel("comfyui_auto")
	h.w.cpuProbe = func() (CPUResources, error) { return CPUResources{}, errNoCPU }
	h.w.comfyQueue = func() *int { return h.jobs }
	h.w.comfyFree = func() error { h.frees++; return nil }
	h.w.comfyClear = func() error { h.clears++; return nil }
	return h
}

var errNoCPU = &probeFailure{"no CPU reading in this test"}

type probeFailure struct{ s string }

func (e *probeFailure) Error() string { return e.s }

func shortComfyWait(t *testing.T) {
	t.Helper()
	wait, poll := comfyFreeWait, comfyFreePoll
	comfyFreeWait, comfyFreePoll = 200*time.Millisecond, time.Millisecond
	t.Cleanup(func() { comfyFreeWait, comfyFreePoll = wait, poll })
}

// Another model's start on the GPU frees an idle ComfyUI and waits for the
// card to show its VRAM back, since /free answers before ComfyUI acts on it.
func TestAStartOnTheGPUFreesAnIdleComfyUI(t *testing.T) {
	h := newComfyHarness(t)
	shortComfyWait(t)
	reads := 0
	h.w.freshProbe = func() (Resources, error) {
		reads++
		r := quiet()
		r.ComfyUIVRAMMB = 6000
		if h.frees > 0 && reads > 3 {
			r.ComfyUIVRAMMB = 216
		}
		return r, nil
	}
	if err := h.w.Fits("chat"); err != nil {
		t.Fatal(err)
	}
	if h.frees != 1 || reads != 4 || !h.w.State().ComfyUI.Freed {
		t.Fatalf("frees %d, card read %d times, freed %v", h.frees, reads, h.w.State().ComfyUI.Freed)
	}
	h.w.StartEnded("chat")

	// It also counts as given back once it stops shrinking above the floor.
	h.frees, reads = 0, 0
	h.w.freshProbe = func() (Resources, error) {
		reads++
		r := quiet()
		r.ComfyUIVRAMMB = 6000
		if h.frees > 0 {
			r.ComfyUIVRAMMB = 900
		}
		return r, nil
	}
	start := time.Now()
	if err := h.w.Fits("chat"); err != nil {
		t.Fatal(err)
	}
	if h.frees != 1 || reads != 3 || time.Since(start) >= comfyFreeWait {
		t.Fatalf("frees %d, card read %d times in %s", h.frees, reads, time.Since(start))
	}
}

// A ComfyUI with a job, or with nothing loaded, is not sent /free, and
// ComfyUI's own start frees nothing and takes no start slot.
func TestAStartLeavesABusyOrEmptyComfyUIAlone(t *testing.T) {
	h := newComfyHarness(t)
	shortComfyWait(t)
	held := 6000
	h.w.freshProbe = func() (Resources, error) { r := quiet(); r.ComfyUIVRAMMB = held; return r, nil }
	*h.jobs = 1
	h.w.Fits("chat")
	h.w.StartEnded("chat")
	*h.jobs, held = 0, 216
	h.w.Fits("chat")
	if h.frees != 0 {
		t.Fatalf("frees %d", h.frees)
	}
	held = 6000
	h.w.gpuStart, h.w.gpuStartAt = "chat", time.Now()
	done := make(chan error, 1)
	go func() { done <- h.w.Fits("comfyui_auto") }()
	select {
	case err := <-done:
		if err != nil || h.frees != 0 {
			t.Fatalf("%v, frees %d", err, h.frees)
		}
	case <-time.After(time.Second):
		t.Fatal("ComfyUI's start waited for the GPU's start slot")
	}
}

// A job sent through InferMux yields the card and stops the GPU's other
// models before it reaches ComfyUI, which may not be running yet. A chat
// that ended a minute ago does not hold the unload back.
func TestAComfyUIJobYieldsAndUnloadsBeforeItReachesComfyUI(t *testing.T) {
	h := newComfyHarness(t)
	h.jobs = nil
	id, _ := h.w.traffic.begin(Interactive, "/v1/chat/completions", gpu, func(string, string) {}, nil)
	h.w.traffic.end(id)
	h.at(time.Minute)
	var yielded bool
	var running map[string]string
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		yielded, running = h.w.Yielded(), h.models.Running()
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/comfyui/api/prompt", strings.NewReader(`{"prompt":{}}`)))
	if !yielded || running["qwen"] != "" || running["comfyui_auto"] == "" {
		t.Fatalf("ComfyUI got the job with the verdict yielded %v and %v running", yielded, running)
	}
	if !strings.Contains(h.w.State().Verdict.Reason, "ComfyUI has 1 job") || h.w.State().PendingUnload {
		t.Fatalf("verdict %+v", h.w.State())
	}
}

// An interactive request in flight on the GPU keeps its model loaded under a
// ComfyUI job (0004).
func TestAComfyUIJobLeavesAModelWithARequestInFlight(t *testing.T) {
	h := newComfyHarness(t)
	h.w.traffic.begin(Interactive, "/v1/chat/completions", gpu, func(string, string) {}, nil)
	handler := h.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/comfyui/prompt", strings.NewReader(`{}`)))
	if h.models.Running()["qwen"] == "" {
		t.Fatal("unloaded under an interactive request")
	}
}

// On a host with no comfyui_auto, such as the zbox, a job for /comfyui/
// reaches llama-swap's 404 and stops no model on the way.
func TestAComfyUIJobOnAHostWithoutComfyUIStopsNothing(t *testing.T) {
	h := newComfyHarness(t)
	h.models.noComfyUI = true
	h.models.running = map[string]string{"qwen": "ready"}
	reached := false
	handler := h.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/comfyui/prompt", strings.NewReader(`{}`)))
	if !reached || h.models.Running()["qwen"] == "" || h.w.Yielded() {
		t.Fatalf("reached %v, running %v, yielded %v", reached, h.models.Running(), h.w.Yielded())
	}
}

// A yield unloads the GPU's other models and never ComfyUI, and ComfyUI's
// requests in flight do not defer it, being no interactive activity there.
func TestAYieldNeverStopsComfyUIAndItsRequestsDoNotDeferIt(t *testing.T) {
	h := newComfyHarness(t)
	h.w.traffic.beginComfyUI(Interactive, "/comfyui/upload/image", gpu, func(string, string) {})
	h.reading = busy(60, "blender")
	h.w.Tick()
	running := h.models.Running()
	if running["qwen"] != "" || running["comfyui_auto"] == "" {
		t.Fatalf("running %v", running)
	}
}

// The free VRAM rule reads ComfyUI as no loaded model, so its cache after a
// render pauses the consumers until /free, as before it was a model (0002).
func TestComfyUIIsNoLoadedModelForTheFreeVRAMRule(t *testing.T) {
	h := newComfyHarness(t)
	h.models.running = map[string]string{"comfyui_auto": "ready"}
	h.reading.VRAMFreeMB = ip(500)
	if v := h.w.Tick(); !v.Yielded || !strings.Contains(v.Reason, "only 500 MB VRAM free") {
		t.Fatalf("verdict %+v", v)
	}
}

// Others' work frees ComfyUI as soon as its queue is empty, not after the
// idle window; its own load and cache are no one else's work.
func TestOthersWorkFreesComfyUIOnceItsQueueIsEmpty(t *testing.T) {
	h := newComfyHarness(t)
	h.models.running = map[string]string{"comfyui_auto": "ready"}
	h.reading = busy(60, "python3")
	h.reading.ComfyUIGPUPercent, h.reading.ComfyUIVRAMMB, h.reading.VRAMFreeMB = 60, 7000, ip(1000)
	h.w.Tick()
	if h.frees != 0 {
		t.Fatal("freed for ComfyUI's own load and cache")
	}
	*h.jobs = 1
	h.reading = busy(70, "Warframe.x64.exe")
	h.at(5 * time.Second).w.Tick()
	if h.frees != 0 {
		t.Fatal("freed under a render")
	}
	*h.jobs = 0
	h.at(10 * time.Second).w.Tick()
	h.at(15 * time.Second).w.Tick()
	if h.frees != 1 {
		t.Fatalf("frees %d once the render ended under a game", h.frees)
	}
	if h.models.Running()["comfyui_auto"] == "" || h.clears != 0 {
		t.Fatal("a game seen by its load stopped ComfyUI")
	}
}

// A priority process clears ComfyUI's queue, interrupts its job and stops it
// (0017, 0030); the game wins over a render.
func TestAPriorityProcessStopsComfyUI(t *testing.T) {
	h := newComfyHarness(t)
	*h.jobs = 1
	h.reading = obsWithCUDA()
	h.w.Tick()
	if h.clears != 1 || h.models.Running()["comfyui_auto"] != "" {
		t.Fatalf("clears %d, running %v", h.clears, h.models.Running())
	}
}

// An unload by hand clears ComfyUI's queue before it stops it with the rest.
func TestAnUnloadByHandClearsComfyUIFirst(t *testing.T) {
	h := newComfyHarness(t)
	unloaded := h.w.UnloadNow()
	if h.clears != 1 || !slices.Contains(unloaded, "comfyui_auto") {
		t.Fatalf("%v, clears %d", unloaded, h.clears)
	}
}

// A config reload stops ComfyUI too, so it waits for its queue to empty.
func TestAReloadWaitsForComfyUIsJobs(t *testing.T) {
	h := newComfyHarness(t)
	*h.jobs = 1
	// A ComfyUI that is not one of the running models is not stopped by a
	// reload, which does not wait for its render.
	h.models.running = map[string]string{"qwen": "ready"}
	h.w.Tick()
	ran := false
	h.w.WhenNoInteractive("reload", func() { ran = true })
	if !ran {
		t.Fatal("the reload waited for the jobs of a ComfyUI it does not stop")
	}
	h.models.running = map[string]string{"qwen": "ready", "comfyui_auto": "ready"}
	h.w.Tick()
	ran = false
	h.w.WhenNoInteractive("reload", func() { ran = true })
	if ran {
		t.Fatal("reloaded under a ComfyUI job")
	}
	*h.jobs = 0
	h.at(5 * time.Second).w.Tick()
	if !ran {
		t.Fatal("the reload did not run once the queue was empty")
	}

	// A batch key's job on its way to ComfyUI is no interactive request in
	// flight, and its queue does not show it yet.
	done := h.w.beforeComfyUIJob()
	ran = false
	h.w.WhenNoInteractive("reload", func() { ran = true })
	if ran {
		t.Fatal("reloaded under a job on its way to ComfyUI")
	}
	done()
	h.w.Tick()
	if !ran {
		t.Fatal("the reload did not run once the job reached ComfyUI")
	}
}

// ComfyUI's pages need a key that may use it, and pass untracked, its
// websocket too; the listener takes no key and reaches ComfyUI only.
func TestComfyUIsKeysAndTheListener(t *testing.T) {
	h := newComfyHarness(t)
	h.w.cfg.Keys.Keys["immich"] = Key{SHA256: HashKey("immich-key"), Class: Interactive, Allow: ModelList{"this/clip"}}
	h.w.traffic.setKeys(h.w.cfg.Keys)
	reached := 0
	handler := h.w.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		reached++
		if n := len(h.w.State().Traffic.InFlight); r.Method == http.MethodGet && n != 0 {
			t.Errorf("%s %s is tracked", r.Method, r.URL.Path)
		}
	}))
	send := func(r *http.Request) int {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Code
	}
	page := httptest.NewRequest(http.MethodGet, "/comfyui/", nil)
	if code := send(page); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	page.Header.Set("Authorization", "Bearer immich-key")
	if code := send(page); code != http.StatusForbidden {
		t.Fatalf("a key not allowed ComfyUI: %d", code)
	}
	page.Header.Set("Authorization", "Bearer my-key")
	ws := httptest.NewRequest(http.MethodGet, "/comfyui/ws", nil)
	ws.Header.Set("Authorization", "Bearer my-key")
	ws.Header.Set("Connection", "Upgrade")
	ws.Header.Set("Upgrade", "websocket")
	if send(page) != http.StatusOK || send(ws) != http.StatusOK || reached != 2 {
		t.Fatalf("reached %d", reached)
	}
	job := FromComfyUIListener(httptest.NewRequest(http.MethodPost, "/comfyui/api/prompt", strings.NewReader(`{}`)))
	if code := send(job); code != http.StatusOK || reached != 3 {
		t.Fatalf("the listener's job: %d", code)
	}
	chat := FromComfyUIListener(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`)))
	if code := send(chat); code != http.StatusForbidden || reached != 3 {
		t.Fatalf("the listener reached qwen: %d", code)
	}
}
