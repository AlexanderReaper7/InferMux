package scheduler

// InferMux: a WebSocket session counts in the scheduler only while it moves
// data (0018, 10). An idle one holds no concurrency slot and does not keep
// its model from being swapped out; it is still in reserved and inFlight,
// and is subtracted where they are compared. A file of its own, so a merge
// from upstream touches only the hook lines in fifo.go that call it.

// SessionEffects is implemented by an Effects that knows about sessions.
type SessionEffects interface {
	// IdleSessions is how many of modelID's requests in flight are sessions
	// that moved no data within stream.ActiveWindow.
	IdleSessions(modelID string) int
}

// SessionScheduler is implemented by a Scheduler that follows sessions.
type SessionScheduler interface {
	// OnSessionIdle handles a session on modelID that stopped moving data:
	// a request queued behind it may now go ahead.
	OnSessionIdle(modelID string)
}

var _ SessionScheduler = (*FIFO)(nil)

// idle is how many of modelID's requests in flight are idle sessions.
func (s *FIFO) idle(modelID string) int {
	if e, ok := s.effects.(SessionEffects); ok {
		return e.IdleSessions(modelID)
	}
	return 0
}

// busyInFlight is inFlight without the idle sessions: the requests a stop
// would cut off. It is inFlight itself when no session is idle.
func (s *FIFO) busyInFlight() map[string]int {
	var busy map[string]int
	for id, n := range s.inFlight {
		idle := s.idle(id)
		if idle == 0 {
			continue
		}
		if busy == nil {
			busy = make(map[string]int, len(s.inFlight))
			for k, v := range s.inFlight {
				busy[k] = v
			}
		}
		busy[id] = max(n-idle, 0)
	}
	if busy == nil {
		return s.inFlight
	}
	return busy
}

// OnSessionIdle implements SessionScheduler.
func (s *FIFO) OnSessionIdle(modelID string) {
	s.logger.Debugf("%s: a session on %s is idle", s.name, modelID)
	s.drainQueue()
}
