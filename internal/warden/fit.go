package warden

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
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
// A group member that does not fit beside its loaded siblings stops them and
// reads the card once more (0029): the pair that fits on a quiet card does
// not fit beside every desktop, and the one asked for wins. A sibling with a
// request in flight, or an open WebSocket, is kept and the start refused, so
// no request is killed for room (0004), and so are siblings whose vram_mb
// with the free figure falls short of the need. Any model outside the group stops
// the whole group, as the matrix router has it.
func (w *Warden) Fits(model string) error {
	w.mu.Lock()
	if !slices.Contains(w.devicesLocked(model), DeviceGPU) {
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
	need := w.placement[model].VRAMMB
	probe, retry := w.freshProbe, w.cfg.Policy.ResumeQuietSeconds
	w.mu.Unlock()
	if need == 0 {
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
		if stopped := w.evictTogether(model, need, free); len(stopped) > 0 {
			res, err = probe()
			if err == nil && res.VRAMFreeMB == nil {
				err = fmt.Errorf("no free figure")
			}
			if err != nil {
				w.log.Warnf("<%s> needs %d MB of VRAM and the card could not be read after its group made room, so it starts: %v", model, need, err)
				return nil
			}
		}
		if free := *res.VRAMFreeMB; free < need {
			w.StartEnded(model)
			w.log.Warnf("<%s> not started: it needs %d MB of VRAM and the card has %d MB free, %s", model, need, free, holders(res))
			return NoRoomError{Model: model, NeedMB: need, FreeMB: free, RetryAfter: retry}
		}
	}
	return nil
}

// evictTogether stops model's loaded siblings in its together groups, for a
// start that needs more VRAM than is free, and returns them. It stops none
// while one of them has a request: claim holds back a request that arrives
// meanwhile until they have stopped.
func (w *Warden) evictTogether(model string, need, free int) []string {
	w.mu.Lock()
	placement := w.placement
	w.mu.Unlock()
	together := placement[model].Together
	if len(together) == 0 {
		return nil
	}
	running := w.models.Running()
	var loaded []string
	// held is what the loaded siblings need, and known whether each has a
	// figure: a stop that cannot make room, such as under a game, stops none.
	held, known := 0, true
	for _, s := range together {
		if _, ok := running[s]; ok {
			loaded = append(loaded, s)
			held += placement[s].VRAMMB
			known = known && placement[s].VRAMMB > 0
		}
	}
	if len(loaded) == 0 {
		return nil
	}
	if known && free+held < need {
		w.log.Infof("<%s> needs %d MB of VRAM and the card has %d MB free, and stopping its group's %s would free about %d MB, so they stay loaded", model, need, free, strings.Join(loaded, ", "), held)
		return nil
	}
	busy, release := w.traffic.claim(loaded)
	if busy != "" {
		w.log.Infof("<%s> needs %d MB of VRAM and the card has %d MB free, and <%s> of its group stays loaded: it has a request", model, need, free, busy)
		return nil
	}
	defer release()
	stopped := w.models.Unload(loaded)
	w.log.Infof("<%s> needs %d MB of VRAM and the card has %d MB free, so its group's %s stopped to make room", model, need, free, strings.Join(stopped, ", "))
	return stopped
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
