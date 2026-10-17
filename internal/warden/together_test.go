package warden

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// The deep search agent and the reranker as a together group (0029), the
// pair's 7.4 GB beside a desktop that leaves 6,000 MB free with neither
// loaded, so 3,000 MB with the reranker, and chat, a model outside it.
func groupHarness(t *testing.T, free func(running map[string]string) int) (*harness, *warnings, *int) {
	t.Helper()
	h, log, reads := fitHarness(t, nil)
	h.w.SetPlacement(Placement{
		"agent":    {Devices: []string{DeviceGPU}, VRAMMB: 4400, Together: []string{"reranker"}},
		"reranker": {Devices: []string{DeviceGPU}, VRAMMB: 3000, Together: []string{"agent"}},
		"chat":     {Devices: []string{DeviceGPU}, VRAMMB: 4400},
	})
	h.models.running = map[string]string{"reranker": "ready"}
	h.w.freshProbe = func() (Resources, error) {
		*reads++
		r := quiet()
		r.VRAMFreeMB = ip(free(h.models.Running()))
		return r, nil
	}
	return h, log, reads
}

func besideTheDesktop(running map[string]string) int {
	if _, ok := running["reranker"]; ok {
		return 3000
	}
	return 6000
}

// A member that does not fit beside its idle sibling stops it, reads the
// card once more, and starts.
func TestAGroupMemberThatDoesNotFitStopsItsIdleSibling(t *testing.T) {
	h, log, reads := groupHarness(t, besideTheDesktop)
	if err := h.w.Fits("agent"); err != nil {
		t.Fatalf("refused: %v", err)
	}
	if !slices.Equal(h.models.unloaded, []string{"reranker"}) || *reads != 2 || len(log.lines) != 0 {
		t.Fatalf("unloaded %v, %d readings, logged %q", h.models.unloaded, *reads, log.lines)
	}
}

// A sibling with a request, or an open WebSocket, is kept, and the start is
// refused as before (0004).
func TestABusySiblingStaysLoadedAndTheStartIsRefused(t *testing.T) {
	h, _, reads := groupHarness(t, besideTheDesktop)
	h.w.traffic.begin("reranker", Batch, "/v1/rerank", gpu, func(string, string) {}, nil)
	var room NoRoomError
	if err := h.w.Fits("agent"); !errors.As(err, &room) || room.FreeMB != 3000 {
		t.Fatalf("started: %v", err)
	}
	if h.models.unloads != 0 || *reads != 1 {
		t.Fatalf("%d unloads, %d readings", h.models.unloads, *reads)
	}
}

// Under a game, stopping the sibling would not make room, so it stays.
func TestASiblingWhoseStopCannotMakeRoomStaysLoaded(t *testing.T) {
	h, _, _ := groupHarness(t, func(map[string]string) int { return 62 })
	var room NoRoomError
	if err := h.w.Fits("agent"); !errors.As(err, &room) {
		t.Fatalf("started: %v", err)
	}
	if h.models.unloads != 0 {
		t.Fatalf("unloaded %v", h.models.unloaded)
	}
}

// A model outside the group stops no member here: the matrix router has
// stopped the group before the gate is asked.
func TestAModelOutsideTheGroupStopsNoMember(t *testing.T) {
	h, _, _ := groupHarness(t, besideTheDesktop)
	var room NoRoomError
	if err := h.w.Fits("chat"); !errors.As(err, &room) {
		t.Fatalf("started: %v", err)
	}
	if h.models.unloads != 0 {
		t.Fatalf("unloaded %v", h.models.unloaded)
	}
}

// slowUnload holds each Unload until let is closed.
type slowUnload struct {
	*fakeModels
	in  chan struct{}
	let chan struct{}
}

func (m slowUnload) Unload(ids []string) []string {
	close(m.in)
	<-m.let
	return m.fakeModels.Unload(ids)
}

// A request for the sibling that arrives while it is being stopped waits
// for the stop, so the stop kills no request of its, and then goes ahead.
func TestARequestForASiblingBeingStoppedWaitsForTheStop(t *testing.T) {
	h, _, _ := groupHarness(t, besideTheDesktop)
	slow := slowUnload{h.models, make(chan struct{}), make(chan struct{})}
	h.w.models = slow
	fits := make(chan error)
	go func() { fits <- h.w.Fits("agent") }()
	<-slow.in
	begun := make(chan uint64)
	go func() {
		id, _ := h.w.traffic.begin("reranker", Interactive, "/v1/rerank", gpu, func(string, string) {}, nil)
		begun <- id
	}()
	select {
	case <-begun:
		t.Fatal("the request began while its model was being stopped")
	case <-time.After(100 * time.Millisecond):
	}
	close(slow.let)
	if err := <-fits; err != nil {
		t.Fatalf("refused: %v", err)
	}
	select {
	case id := <-begun:
		h.w.traffic.end(id)
	case <-time.After(5 * time.Second):
		t.Fatal("the request still waits after the stop")
	}
}
