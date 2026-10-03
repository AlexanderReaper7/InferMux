package warden

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// The decision: is somebody else's work on this GPU at stake. Pure functions
// over a measurement and the previous verdict, so the whole table is testable
// without a GPU, a model, or anything to announce to.
//
// The rule, as the user set it: other work takes priority, but only where the
// models would noticeably degrade it. This governs on resource contention, not
// on whether somebody is at the keyboard. A game left running while the user is
// away is still contention; a reader typing an email is not.
//
// Three signals, each used only where it is valid:
//
//   - ComfyUIJobs, running plus pending. A queued job is contention before it
//     has drawn any power, so the models unload before ComfyUI loads much (0002).
//   - ForeignGPUPercent, per-process utilization minus our processes and the
//     desktop's. Attribution makes it truthful while we generate too.
//   - Free VRAM, read only while no model is loaded, where the whole figure is
//     by definition somebody else's (0002).

// Verdict is what the warden currently believes, and since when.
//
// ContendedAt is when contention was last observed, which is what the resume
// window is measured from. A window anchored to the start of the pause would
// elapse while the game was still running, and the first dip after that would
// resume into a lull between two loading screens.
type Verdict struct {
	Yielded     bool       `json:"yielded"`
	Reason      string     `json:"reason"`
	Since       *time.Time `json:"since"`
	ContendedAt *time.Time `json:"contended_at"`
	SampledAt   *time.Time `json:"sampled_at"`
}

func (v Verdict) Action() string {
	if v.Yielded {
		return "pause"
	}
	return "resume"
}

func initialVerdict() Verdict {
	return Verdict{Reason: "no measurement yet"}
}

// IsContended says whether someone else's work is at stake, and why. The why is
// carried into the log, /warden/verdict and every announcement: a pipeline that
// stopped on its own must be able to say what it saw, or the feature is
// indistinguishable from a bug.
func IsContended(r Resources, modelsLoaded bool, p Policy) (bool, string) {
	if r.ComfyUIJobs != nil && *r.ComfyUIJobs > 0 {
		jobs := *r.ComfyUIJobs
		plural := "s"
		if jobs == 1 {
			plural = ""
		}
		return true, fmt.Sprintf("ComfyUI has %d job%s queued", jobs, plural)
	}

	if r.ForeignGPUPercent != nil && *r.ForeignGPUPercent >= p.GPUBusyPercent {
		detail := ""
		if len(r.Culprits) > 0 {
			detail = " (" + strings.Join(r.Culprits, ", ") + ")"
		}
		return true, fmt.Sprintf("foreign GPU load %.0f%% >= %.0f%%%s",
			math.Round(*r.ForeignGPUPercent), p.GPUBusyPercent, detail)
	}

	if !modelsLoaded && r.VRAMFreeMB != nil && *r.VRAMFreeMB < p.MinFreeVRAMMB {
		return true, fmt.Sprintf("only %d MB VRAM free, need %d MB to load a model",
			*r.VRAMFreeMB, p.MinFreeVRAMMB)
	}

	return false, "GPU is free"
}

// Decide is one tick. Compare the result with the previous verdict to know
// whether anything has to be announced.
//
// Asymmetric by design: yield the moment contention appears, return only after
// the GPU has been quiet for ResumeQuietSeconds. Restarting a 20 GB model load
// during a lull between two loading screens is worse than waiting.
func Decide(previous Verdict, r Resources, now time.Time, modelsLoaded bool, p Policy) Verdict {
	contended, why := IsContended(r, modelsLoaded, p)

	if contended {
		if previous.Yielded {
			// Still busy: re-stamp the observation without moving Since, which
			// is what the announcement reports as the start.
			next := previous
			next.Reason, next.ContendedAt, next.SampledAt = why, &now, &now
			return next
		}
		return Verdict{Yielded: true, Reason: why, Since: &now, ContendedAt: &now, SampledAt: &now}
	}

	if !previous.Yielded {
		next := previous
		next.Reason, next.SampledAt = why, &now
		return next
	}

	if previous.ContendedAt != nil {
		quiet := now.Sub(*previous.ContendedAt).Seconds()
		if quiet < float64(p.ResumeQuietSeconds) {
			next := previous
			next.Reason = fmt.Sprintf("%s, but only for %.0fs of %ds", why, quiet, p.ResumeQuietSeconds)
			next.SampledAt = &now
			return next
		}
	}
	// No observation to measure from counts as long enough, since the
	// alternative is a pause that can never lift.
	return Verdict{Yielded: false, Reason: why, Since: &now, SampledAt: &now}
}

// ComfyIdle is when ComfyUI was last seen working, and whether it has been told
// to drop its models since. BusyAt starts at the warden's own start, so a
// warden that restarts beside an idle ComfyUI still frees it, one window later.
type ComfyIdle struct {
	BusyAt time.Time `json:"busy_at"`
	Freed  bool      `json:"freed"`
}

// ComfyUIFreeDue is one tick of ComfyUI's idle clock. It returns the state to
// keep if a /free is sent and lands, and whether to send one now.
//
// Once per idle stretch: after a free nothing more is sent until a job has been
// seen, because a free also empties the cache ComfyUI would reuse for the next
// job. An unreadable queue (nil) moves nothing, the same no-opinion rule as a
// failed probe.
func ComfyUIFreeDue(previous ComfyIdle, jobs *int, now time.Time, p Policy) (ComfyIdle, bool) {
	if jobs == nil {
		return previous, false
	}
	if *jobs > 0 {
		return ComfyIdle{BusyAt: now}, false
	}
	if previous.Freed {
		return previous, false
	}
	if now.Sub(previous.BusyAt).Seconds() < float64(p.ComfyUIIdleSeconds) {
		return previous, false
	}
	return ComfyIdle{BusyAt: previous.BusyAt, Freed: true}, true
}
