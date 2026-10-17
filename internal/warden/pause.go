package warden

import (
	"context"
	"time"
)

// A job's request (0023) is not cancelled by a yield. The warden tells the
// job instead, and the job stops its own request where it can continue it
// later. Everything else about the request is gated as any other.

// Pause is what the warden tells a job whose batch request it would cancel.
type Pause struct {
	// Device is the device that yielded, "" for a cancel by hand, "off"
	// when the local models went off (0025), and "held" for an unload by
	// hand or by infermux-hold (0024, 4).
	Device string
	Reason string
	// Hold is how long a cancel by hand keeps the job back: the GPU's
	// resume_quiet_seconds, what Retry-After tells any other client. A yield
	// holds nothing, since another place may take the job at once.
	Hold time.Duration
}

type pauserKey struct{}

// WithPauser marks a request as a job's: a yield or a cancel by hand calls
// pause rather than cancelling it.
func WithPauser(ctx context.Context, pause func(Pause)) context.Context {
	return context.WithValue(ctx, pauserKey{}, pause)
}

// PauserOf is the pause WithPauser set, nil for any other request.
func PauserOf(ctx context.Context) func(Pause) {
	pause, _ := ctx.Value(pauserKey{}).(func(Pause))
	return pause
}

// OnResume calls f each time a device's batch work may run again. Call it
// before Start.
func (w *Warden) OnResume(f func()) {
	w.traffic.mu.Lock()
	defer w.traffic.mu.Unlock()
	w.traffic.resumed = append(w.traffic.resumed, f)
}

// ReportJobs adds the jobs to /warden/verdict. The warden only shows them.
func (w *Warden) ReportJobs(state func() any) {
	w.mu.Lock()
	w.jobs = state
	w.mu.Unlock()
}

// KeysRequired says whether a request needs a key, which a job read back
// after a restart waits for, since its key was not written to disk.
func (w *Warden) KeysRequired() bool {
	w.traffic.mu.Lock()
	defer w.traffic.mu.Unlock()
	return w.traffic.keys != nil
}
