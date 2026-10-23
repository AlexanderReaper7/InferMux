package warden

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"
)

// The GPU may be absent (0032). In strix's rtx3080-passthrough boot the 3080
// is bound to vfio-pci and the nvidia modules are blacklisted, so NVML
// answers ERROR_DRIVER_NOT_LOADED until the user hands the card to the host
// with `svm gpu host`, which loads the driver at runtime. Absent is a state:
// the warden logs each change of it once, at info, shows it in
// /warden/verdict, resumes the GPU's verdict, since nothing can contend for a
// card the host does not have, and refuses a start of a model the one config
// places on the GPU, so its other places take the request. Before this a
// failed probe was logged every 5 s, and llama-server started with 99 GPU
// layers found no CUDA device and ran the GPU's place on the CPU threads in
// the daemon's own cgroup, beside the CPU place's copy (2026-10-23).

// AbsentHeader marks a refusal for a GPU the host does not have, so failover
// logs the hop to the model's next place as the routing it is rather than a
// failure. internal/failover reads the same name.
const AbsentHeader = "X-InferMux-Absent"

// absentRetrySeconds is the Retry-After of a refusal while the GPU is absent.
// The card comes back only by the user's hand, and the warden reads it every
// poll, so a client that waits this long finds it if it has come.
const absentRetrySeconds = 30

// gpuState is what the last reading of the card found.
type gpuState int

const (
	gpuUnknown gpuState = iota
	gpuPresent
	gpuAbsent
)

// gpuSeen is the card's state, the absence's detail, and when a reading last
// set it. refusedLogged is each model whose refusal this absence has logged.
type gpuSeen struct {
	state         gpuState
	detail        string
	at            time.Time
	refusedLogged map[string]bool
}

// read takes a reading through probe and notes what it says about the card.
// Every reading of the GPU goes through here, so a fresh one at a start sees
// the card arrive as soon as the loop's does.
func (w *Warden) read(probe func() (Resources, error)) (Resources, error) {
	res, err := probe()
	w.noteGPU(res, err)
	return res, err
}

// noteGPU logs the card's arrival and its absence once each. A reading that
// failed for another reason says nothing either way.
func (w *Warden) noteGPU(res Resources, err error) {
	var absent GPUAbsent
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	switch {
	case errors.As(err, &absent):
		if w.gpu.state != gpuAbsent {
			w.log.Infof("GPU absent: %s. A start of a model placed on it is refused until it appears", absent.Detail)
			w.gpu.refusedLogged = map[string]bool{}
		}
		w.gpu.state, w.gpu.detail, w.gpu.at = gpuAbsent, absent.Detail, now
	case err == nil:
		if w.gpu.state == gpuAbsent {
			free := "free not read"
			if res.VRAMFreeMB != nil {
				free = fmt.Sprintf("%d MB free", *res.VRAMFreeMB)
			}
			w.log.Infof("GPU present: %d MB, %s. Its models start on it again", res.VRAMTotalMB, free)
		}
		w.gpu.state, w.gpu.detail, w.gpu.at = gpuPresent, "", now
	default:
		w.gpu.state, w.gpu.detail, w.gpu.at = gpuUnknown, "", now
	}
}

// absentFor is the refusal of a start of model while the GPU is absent, nil
// when the card is present, unread, or the model is not placed on it. Only
// the one config places a model on a GPU the host says it has; outside it
// every model is on the GPU by default, and a host without one runs them
// as it did. A model the placement does not name is on every device and is
// not refused either. An absence older than one poll is read again first,
// so a card handed to the host is used at the next start, whether or not
// the loop measures.
func (w *Warden) absentFor(model string) error {
	w.mu.Lock()
	placed, ok := w.placement[model]
	if !ok || !slices.Contains(placed.Devices, DeviceGPU) || w.gpu.state != gpuAbsent {
		w.mu.Unlock()
		return nil
	}
	stale := w.now().Sub(w.gpu.at) >= max(time.Second, time.Duration(w.cfg.Policy.PollSeconds*float64(time.Second)))
	probe := w.freshProbe
	w.mu.Unlock()
	if stale {
		w.read(probe)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.gpu.state != gpuAbsent {
		return nil
	}
	if !w.gpu.refusedLogged[model] {
		w.gpu.refusedLogged[model] = true
		w.log.Infof("<%s> not started: the GPU is absent (%s); its other places take its requests", model, w.gpu.detail)
	}
	return GPUAbsentError{Model: model, Detail: w.gpu.detail, RetryAfter: absentRetrySeconds}
}

// absentDetailLocked is the absence's detail for /warden/verdict, nil while the
// card is present or unread.
func (w *Warden) absentDetailLocked() *string {
	if w.gpu.state != gpuAbsent {
		return nil
	}
	detail := w.gpu.detail
	return &detail
}

// GPUAbsentError is a start on the GPU refused because the host has no GPU,
// a 503 so failover tries the model's next place at once (0016). It has the
// methods of llama-swap's HTTPError, for a refusal at the start gate, and
// the request gate writes the same answer.
type GPUAbsentError struct {
	Model      string
	Detail     string
	RetryAfter int
}

func (e GPUAbsentError) Error() string {
	return fmt.Sprintf("%s is placed on the GPU and this host has none now: %s", e.Model, e.Detail)
}

func (e GPUAbsentError) StatusCode() int { return http.StatusServiceUnavailable }

func (e GPUAbsentError) Header() http.Header {
	h := refusalHeader(e.RetryAfter)
	h.Set(AbsentHeader, DeviceGPU)
	return h
}

func (e GPUAbsentError) Body() []byte { return refusalBody("gpu_absent", e.Error()) }

func writeAbsent(rw http.ResponseWriter, e GPUAbsentError) {
	for k, v := range e.Header() {
		rw.Header()[k] = v
	}
	rw.WriteHeader(e.StatusCode())
	rw.Write(e.Body())
}
