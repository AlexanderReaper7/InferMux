package process

import "sync/atomic"

// InferMux: every start of a local model passes one gate, in the process's own
// run loop, so a safety stop refuses it whoever asked: a request, a GET to
// /upstream/, preload, or a swap queued before the stop (0025). The one hook
// is in run(); a refused start leaves the process stopped and sends nothing.

var startGate atomic.Pointer[func(id string) error]

// SetStartGate makes each process ask gate before it starts and answer the
// start with gate's error instead. nil removes the gate. Set it before the
// server exists: preload starts as the server is made.
func SetStartGate(gate func(id string) error) {
	if gate == nil {
		startGate.Store(nil)
		return
	}
	startGate.Store(&gate)
}

func gateStart(id string) error {
	if gate := startGate.Load(); gate != nil {
		return (*gate)(id)
	}
	return nil
}
