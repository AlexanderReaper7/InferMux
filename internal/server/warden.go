package server

import "sort"

// InferMux: the warden's handle on this server's local models. A file of its
// own, so a merge from upstream never touches it and a rename upstream fails
// the build here rather than at run time.

// RunningModelStates is every local model that is not stopped, with its state.
func (s *Server) RunningModelStates() map[string]string {
	states := map[string]string{}
	for id, state := range s.local.RunningModels() {
		states[id] = string(state)
	}
	return states
}

// UnloadAllModels stops every local model and returns the ones that were
// running. It blocks until they have stopped.
func (s *Server) UnloadAllModels() []string {
	running := make([]string, 0)
	for id := range s.local.RunningModels() {
		running = append(running, id)
	}
	sort.Strings(running)
	if len(running) > 0 {
		s.local.Unload(0, running...)
	}
	return running
}
