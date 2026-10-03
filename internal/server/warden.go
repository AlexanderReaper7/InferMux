package server

import (
	"sort"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

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

// QualifyModel is a model name as a key's allow list sees it (0006, 4): a
// local model or alias as its real ID with peer "", a peer's as its peer ID
// and the peer's model name. False for a name this server does not know.
func (s *Server) QualifyModel(name string) (peer, model string, ok bool) {
	if real, found := s.cfg.RealModelName(name); found {
		return "", real, true
	}
	if peerID, peerModel, found := s.cfg.ResolvePeerModel(name); found {
		return peerID, peerModel, true
	}
	return "", "", false
}

// UpstreamModel is the model an /upstream/<model>/... path names.
func (s *Server) UpstreamModel(path string) (string, bool) {
	_, real, _, found := swaputil.FindModelInPath(s.cfg, path)
	return real, found
}

// ModelCommand is a local model's command, macros expanded, split as
// llama-swap runs it. internal/catalog derives the model's settings from it.
func (s *Server) ModelCommand(id string) ([]string, bool) {
	mc, ok := s.cfg.Models[id]
	if !ok {
		return nil, false
	}
	args, err := mc.SanitizedCommand()
	return args, err == nil
}
