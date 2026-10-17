package warden

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// The fit check (the user's decision 17 of 2026-10-15). A model with a VRAM
// need, vram_mb in its model file, starts on the GPU only while the card has
// that much free. Without it, a start into a card a game holds runs until
// CUDA cannot allocate: the reranker, 3.1 GB once serving, failed after 4.9
// to 8.8 s with 62 MiB free while Warframe held 9,009 to 9,176 MB of 10,240.
// The figure is the card's own, NVML's device memory, not a sum of its
// process list, which misses about 2.2 GB of the desktop's dma-bufs and
// would let through starts that then fail. The user accepted that an
// interactive request is refused here at once, an exception to 0004.

// Fits refuses the start of a local model on the GPU whose VRAM need is more
// than the card has free, read fresh at the start. The gate asks it after a
// swap has stopped the models it evicts, so their VRAM counts as free. A
// model with no need, or not on the GPU, fits, and so does one when the card
// cannot be read: a reading not taken is not evidence of contention.
//
// A start on the GPU first takes the card's one start slot, and waits while
// another model's start holds it (0029). Two members of a group kept loaded
// together can start at once, and both would read the card before either
// had allocated. The slot is held until StartEnded, so the second reads a
// figure the first's VRAM is in.
//
// ComfyUI's own start takes no slot and is not checked: it loads no weights
// until a job asks for them, and each other model's start sends it /free
// once the slot is taken (0030).
func (w *Warden) Fits(model string) error {
	w.mu.Lock()
	if !slices.Contains(w.devicesLocked(model), DeviceGPU) || model == w.comfyModel {
		w.mu.Unlock()
		return nil
	}
	if w.gpuStart != "" {
		w.log.Infof("<%s> waits for <%s> to finish starting on the GPU, so its reading of the card includes that model", model, w.gpuStart)
	}
	// The limit runs from when the holder took the slot, so a start that
	// went ahead without an end is waited for in turn by the next.
	for w.gpuStart != "" {
		left := time.Until(w.gpuStartAt.Add(gpuStartWait))
		if left <= 0 {
			w.log.Warnf("<%s> waited for <%s>'s start on the GPU, which has held the slot for %s, and goes ahead without its end", model, w.gpuStart, gpuStartWait)
			break
		}
		timer := time.AfterFunc(left, func() {
			w.mu.Lock()
			w.gpuStartDone.Broadcast()
			w.mu.Unlock()
		})
		w.gpuStartDone.Wait()
		timer.Stop()
	}
	w.gpuStart, w.gpuStartAt = model, time.Now()
	w.mu.Unlock()
	w.freeComfyUIFor(model)
	w.mu.Lock()
	need := w.placement[model].VRAMMB
	failed, held := w.failedStarts[model]
	if held && !w.now().Before(failed.at.Add(failedStartHold)) {
		delete(w.failedStarts, model)
		held = false
	}
	probe, retry, now := w.freshProbe, w.cfg.Policy.ResumeQuietSeconds, w.now()
	w.mu.Unlock()
	if need == 0 && !held {
		return nil
	}
	res, err := probe()
	if err == nil && res.VRAMFreeMB == nil {
		err = fmt.Errorf("no free figure")
	}
	if err != nil {
		if need > 0 {
			w.log.Warnf("<%s> needs %d MB of VRAM and the card could not be read, so it starts: %v", model, need, err)
		} else {
			w.log.Warnf("<%s>'s last start on the GPU stopped before it was ready and the card could not be read, so it starts: %v", model, err)
		}
		return nil
	}
	free := *res.VRAMFreeMB
	// A start that stopped before it was ready, most often for want of
	// VRAM, fails again while the card has no more room than it had then.
	// Without this, each request for a model the card cannot hold started
	// it, and Octen's starts failed about once a second (2026-10-17).
	if held && free <= failed.freeMB {
		w.StartEnded(model)
		w.log.Warnf("<%s> not started: its last start on the GPU stopped before it was ready with %d MB free, and the card has %d MB free, %s", model, failed.freeMB, free, holders(res))
		left := int(failed.at.Add(failedStartHold).Sub(now).Seconds() + 0.999)
		return StartFailedError{Model: model, FailedMB: failed.freeMB, FreeMB: free, RetryAfter: max(left, 1)}
	}
	if free < need {
		w.StartEnded(model)
		w.log.Warnf("<%s> not started: it needs %d MB of VRAM and the card has %d MB free, %s", model, need, free, holders(res))
		return NoRoomError{Model: model, NeedMB: need, FreeMB: free, RetryAfter: retry}
	}
	return nil
}

// StartEnded is told that a start the gate let through has ended, the model
// ready or stopped, and frees the GPU's start slot when that model holds it.
// infermux.go calls it on llama-swap's state change out of starting, which
// follows every start the gate passes.
func (w *Warden) StartEnded(model string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.gpuStart == model {
		w.gpuStart = ""
		w.gpuStartDone.Broadcast()
	}
}

// gpuStartWait is the longest one start holds the GPU's start slot against
// the others. The end of a start always frees it, so this is only for an end that is never
// heard, such as one after the daemon's stop has unsubscribed: then every
// later start on the GPU would wait for good.
var gpuStartWait = 60 * time.Second

// NoRoomError is a start refused for want of VRAM, with the answer the gate
// writes. llama-swap writes it whole to whoever asked for the start: it has
// the methods of llama-swap's HTTPError. A 503, so failover tries the
// model's next place.
type NoRoomError struct {
	Model      string
	NeedMB     int
	FreeMB     int
	RetryAfter int
}

func (e NoRoomError) Error() string {
	return fmt.Sprintf("%s needs %d MB of VRAM and the card has %d MB free", e.Model, e.NeedMB, e.FreeMB)
}

func (e NoRoomError) StatusCode() int { return http.StatusServiceUnavailable }

func (e NoRoomError) Header() http.Header {
	h := http.Header{"Content-Type": {"application/json"}}
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	return h
}

func (e NoRoomError) Body() []byte { return refusalBody("gpu_no_room", e.Error()) }

// failedStartHold is how long a start that stopped before it was ready keeps
// the next start of that model on the GPU from going ahead, while the card
// has no more free than it had then.
const failedStartHold = 60 * time.Second

// failedStart is the card's free figure when a model's start on the GPU
// stopped before it was ready, and when.
type failedStart struct {
	freeMB int
	at     time.Time
}

// StartFailedError is a start refused because the model's last start on the
// GPU stopped before it was ready with as much free as the card has now. A
// 503, so failover tries the model's next place, as for NoRoomError.
type StartFailedError struct {
	Model      string
	FailedMB   int
	FreeMB     int
	RetryAfter int
}

func (e StartFailedError) Error() string {
	return fmt.Sprintf("%s stopped before it was ready with %d MB free, and the card has %d MB free", e.Model, e.FailedMB, e.FreeMB)
}

func (e StartFailedError) StatusCode() int { return http.StatusServiceUnavailable }

func (e StartFailedError) Header() http.Header {
	return http.Header{"Content-Type": {"application/json"}, "Retry-After": {strconv.Itoa(e.RetryAfter)}}
}

func (e StartFailedError) Body() []byte { return refusalBody("gpu_start_failed", e.Error()) }
