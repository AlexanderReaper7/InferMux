package warden

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
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
func (w *Warden) Fits(model string) error {
	w.mu.Lock()
	need := w.placement[model].VRAMMB
	gpu := slices.Contains(w.devicesLocked(model), DeviceGPU)
	probe, retry := w.freshProbe, w.cfg.Policy.ResumeQuietSeconds
	w.mu.Unlock()
	if need == 0 || !gpu {
		return nil
	}
	res, err := probe()
	if err == nil && res.VRAMFreeMB == nil {
		err = fmt.Errorf("no free figure")
	}
	if err != nil {
		w.log.Warnf("<%s> needs %d MB of VRAM and the card could not be read, so it starts: %v", model, need, err)
		return nil
	}
	if free := *res.VRAMFreeMB; free < need {
		w.log.Warnf("<%s> not started: it needs %d MB of VRAM and the card has %d MB free, %s", model, need, free, holders(res))
		return NoRoomError{Model: model, NeedMB: need, FreeMB: free, RetryAfter: retry}
	}
	return nil
}

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
