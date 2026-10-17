package warden

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"
)

// ComfyUI as one of InferMux's models (0030). llama-swap runs it as
// comfyui_auto at /comfyui/, beside whatever else the GPU swaps, and
// ComfyUI keeps managing its own models. What the warden adds:
//
//   - Another model's start on the GPU first sends it /free, when its queue
//     is empty, and waits for its VRAM to come back (freeComfyUIFor).
//   - A job sent through InferMux yields the card before ComfyUI loads
//     anything, and unloads the GPU's other models unless an interactive
//     request is in flight on one (beforeComfyUIJob).
//   - Its queue and its cache are contention, as they were (0002), but a
//     yield never stops it. A priority process (0017) and an unload by hand
//     or by infermux-hold (0024) do, after its queue is cleared and its job
//     interrupted (stopComfyUI), and the off file stops it at once (0025).
//   - Its requests are not interactive activity on the GPU (flight.comfy),
//     and its pages and websocket pass untracked.

// SetComfyUIModel names the llama-swap model that runs ComfyUI, upstream's
// comfyui_auto, "" when ComfyUI is not one of the models. main names it,
// since the warden imports nothing from llama-swap.
func (w *Warden) SetComfyUIModel(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.comfyModel = id
}

type comfyUIListener struct{}

// FromComfyUIListener marks a request that came in on the ComfyUI listener,
// which takes no key: the user's decision of 2026-10-17 that the browser
// opens https://comfyui.java-tint.ts.net with no login (0030). Wrap gives it
// an interactive key allowed only ComfyUI's model, named comfyui-listener.
// The listener is on loopback, where Tailscale's serve reaches it.
func FromComfyUIListener(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), comfyUIListener{}, true))
}

// comfyUIListenerKey is the key of a request FromComfyUIListener marked.
func (w *Warden) comfyUIListenerKey() namedKey {
	w.mu.Lock()
	defer w.mu.Unlock()
	allow := ModelList{}
	if w.comfyModel != "" {
		allow = ModelList{w.cfg.Host + "/" + w.comfyModel}
	}
	return namedKey{name: "comfyui-listener", Key: Key{Class: Interactive, Allow: allow}}
}

// isComfyUI is a request for ComfyUI's endpoint, while ComfyUI is a model.
func (w *Warden) isComfyUI(r *http.Request) bool {
	w.mu.Lock()
	on := w.comfyModel != ""
	w.mu.Unlock()
	return on && (r.URL.Path == "/comfyui" || strings.HasPrefix(r.URL.Path, "/comfyui/"))
}

// isComfyUIJob is a request that queues a ComfyUI job. The frontend sends
// /api/prompt, a script /prompt; both reach the same handler.
func isComfyUIJob(r *http.Request) bool {
	return r.Method == http.MethodPost && (r.URL.Path == "/comfyui/prompt" || r.URL.Path == "/comfyui/api/prompt")
}

// othersOn is the running models on device other than ComfyUI, sorted.
func (w *Warden) othersOn(device string) []string {
	w.mu.Lock()
	comfy := w.comfyModel
	w.mu.Unlock()
	return slices.DeleteFunc(w.runningOn(device), func(id string) bool { return id == comfy })
}

// comfyUIRunning is whether ComfyUI is a model and not stopped.
func (w *Warden) comfyUIRunning() (string, bool) {
	w.mu.Lock()
	comfy := w.comfyModel
	w.mu.Unlock()
	if comfy == "" {
		return "", false
	}
	_, running := w.models.Running()[comfy]
	return comfy, running
}

// beforeComfyUIJob runs before a job reaches ComfyUI. The job counts as
// queued at once, and the warden ticks, so the yield pauses the consumers
// and cancels the batch work before ComfyUI loads anything; 0002's race was
// the 5 s between a job and the tick that saw it. Then the GPU's other
// models stop: a render is the user's statement that the card is wanted, as
// a game launch is (0024), so interactive_recent_seconds does not hold the
// unload back. An interactive request in flight on one does (0004), and the
// render goes into what is left. done ends the job's count once ComfyUI has
// answered, when its own queue shows it.
func (w *Warden) beforeComfyUIJob() (done func()) {
	w.mu.Lock()
	w.comfyPending++
	w.comfy = ComfyIdle{BusyAt: w.now()}
	enabled := w.cfg.Policy.Enabled
	w.mu.Unlock()
	done = func() {
		w.mu.Lock()
		w.comfyPending--
		w.mu.Unlock()
	}
	if !enabled {
		return done
	}
	w.Tick()
	others := w.othersOn(DeviceGPU)
	if len(others) == 0 {
		return done
	}
	if recent, _ := w.traffic.interactiveRecent(0, DeviceGPU); recent {
		w.log.Infof("A ComfyUI job starts beside %s: an interactive request is in flight on the GPU", strings.Join(others, ", "))
		return done
	}
	w.closeSessions(DeviceGPU, "infermux: a ComfyUI job needs the GPU")
	unloaded := w.models.Unload(others)
	w.mu.Lock()
	w.gpuUnload.pending, w.gpuUnload.last = false, unloaded
	w.mu.Unlock()
	w.log.Infof("Models on the GPU unloaded for a ComfyUI job: %s", orNone(strings.Join(unloaded, ", ")))
	return done
}

// comfyIdleMB is the most ComfyUI holds with nothing loaded: its CUDA
// context, 216 MiB in NVML after a /free on strix (2026-10-17).
const comfyIdleMB = 512

// comfyFreeWait is how long a start waits for ComfyUI's VRAM after /free,
// and comfyFreePoll how often it reads the card meanwhile.
var (
	comfyFreeWait = 10 * time.Second
	comfyFreePoll = 250 * time.Millisecond
)

// freeComfyUIFor sends ComfyUI /free before model starts on the GPU, when
// its queue is empty and it holds more than comfyIdleMB (0030). /free only
// sets a flag that ComfyUI's worker acts on, so its answer says nothing: the
// card is read until ComfyUI's processes hold no more than comfyIdleMB, or
// hold what they held at the last reading after giving some back, for up to
// comfyFreeWait. With a job queued or running, ComfyUI is left alone and the
// model starts into what is left. Without comfyui_unit nothing can read its
// VRAM, so /free goes out and nothing is waited for.
func (w *Warden) freeComfyUIFor(model string) {
	w.mu.Lock()
	queue, free, probe, unit := w.comfyQueue, w.comfyFree, w.freshProbe, w.cfg.ComfyUIUnit
	w.mu.Unlock()
	if queue == nil {
		return
	}
	jobs := queue()
	if jobs == nil {
		return
	}
	if *jobs > 0 {
		w.log.Infof("<%s> starts beside ComfyUI's %d job(s), into what ComfyUI leaves of the card", model, *jobs)
		return
	}
	held := 0
	if unit != "" {
		res, err := probe()
		if err != nil || res.ComfyUIVRAMMB <= comfyIdleMB {
			return
		}
		held = res.ComfyUIVRAMMB
	}
	if err := free(); err != nil {
		w.log.Warnf("ComfyUI did not take /free before <%s> started: %v", model, err)
		return
	}
	w.mu.Lock()
	w.comfy.Freed = true
	w.mu.Unlock()
	if unit == "" {
		w.log.Infof("ComfyUI sent /free before <%s> started", model)
		return
	}
	started, last := time.Now(), held
	for time.Since(started) < comfyFreeWait {
		time.Sleep(comfyFreePoll)
		res, err := probe()
		if err != nil {
			break
		}
		now := res.ComfyUIVRAMMB
		if now <= comfyIdleMB || (now < held && now == last) {
			w.log.Infof("ComfyUI gave back %d of %d MB for <%s> in %.1fs", held-now, held, model, time.Since(started).Seconds())
			return
		}
		last = now
	}
	w.log.Warnf("ComfyUI still held %d of %d MB %.0fs after /free; <%s> starts into what is left", last, held, comfyFreeWait.Seconds(), model)
}

// stopComfyUI clears ComfyUI's queue, interrupts its job and stops it, for
// a priority process (0017). The game wins over a render; a yield for load
// alone never does this, since NVML's time-share reads about 4x high at idle
// clocks (the user, 2026-10-17).
func (w *Warden) stopComfyUI(why string) {
	comfy, running := w.comfyUIRunning()
	if !running {
		return
	}
	w.clearComfyUI()
	stopped := w.models.Unload([]string{comfy})
	w.log.Infof("ComfyUI stopped, %s: %s", why, orNone(strings.Join(stopped, ", ")))
}

// clearComfyUI empties ComfyUI's queue and interrupts its job, before it is
// stopped. A ComfyUI that does not answer is stopped all the same.
func (w *Warden) clearComfyUI() {
	if _, running := w.comfyUIRunning(); !running {
		return
	}
	w.mu.Lock()
	clear := w.comfyClear
	w.mu.Unlock()
	if clear == nil {
		return
	}
	if err := clear(); err != nil {
		w.log.Warnf("ComfyUI's queue was not cleared before its stop: %v", err)
	}
}
