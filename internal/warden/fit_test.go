package warden

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The reranker under Warframe (2026-10-15): the processes NVML lists hold
// 7,000 MB, and the card's own figure has 62 MB free, since the desktop's
// dma-bufs are in no process's figure.
func underAGame() Resources {
	r := quiet()
	r.VRAMUsedMB, r.VRAMTotalMB, r.VRAMFreeMB = 10178, 10240, ip(62)
	r.Processes = []Process{
		{Name: "Warframe.x64.exe", VRAMMB: 6900},
		{Name: "Xwayland", VRAMMB: 100, Desktop: true},
	}
	return r
}

func fitHarness(t *testing.T, reading func() (Resources, error)) (*harness, *warnings, *int) {
	t.Helper()
	h := newHarness(t, nil)
	log := &warnings{}
	h.w.log = log
	h.w.SetPlacement(Placement{
		"reranker": {Devices: []string{DeviceGPU}, VRAMMB: 3100},
		"chat":     {Devices: []string{DeviceGPU}},
		"embed":    {Devices: []string{DeviceCPU}, VRAMMB: 3100},
	})
	h.w.probe = func() (Resources, error) { t.Fatal("the loop's probe was read"); return Resources{}, nil }
	reads := new(int)
	h.w.freshProbe = func() (Resources, error) { *reads++; return reading() }
	return h, log, reads
}

// A start on the GPU whose need the card's free figure does not cover is
// refused with a 503 llama-swap writes whole, however little the processes
// NVML lists hold, and the log names what held the card.
func TestAStartTheCardHasNoRoomForIsRefused(t *testing.T) {
	h, log, _ := fitHarness(t, func() (Resources, error) { return underAGame(), nil })
	err := h.w.Fits("reranker")
	var room NoRoomError
	if !errors.As(err, &room) {
		t.Fatalf("started: %v", err)
	}
	if room.StatusCode() != http.StatusServiceUnavailable || room.Header().Get("Retry-After") != strconv.Itoa(DefaultConfig().Policy.ResumeQuietSeconds) || room.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", room.StatusCode(), room.Header())
	}
	if body := string(room.Body()); !strings.Contains(body, `"type":"gpu_no_room"`) || !strings.Contains(body, "reranker needs 3100 MB of VRAM and the card has 62 MB free") {
		t.Fatalf("body %s", body)
	}
	want := "<reranker> not started: it needs 3100 MB of VRAM and the card has 62 MB free, 10178 of 10240 MB in use: Warframe.x64.exe 6900 MB (foreign), Xwayland 100 MB (desktop)"
	if len(log.lines) != 1 || log.lines[0] != want {
		t.Fatalf("logged %q\nwant %q", log.lines, want)
	}
}

// Exactly the need fits.
func TestAStartTheCardHasRoomForStarts(t *testing.T) {
	r := underAGame()
	r.VRAMFreeMB = ip(3100)
	h, log, _ := fitHarness(t, func() (Resources, error) { return r, nil })
	if err := h.w.Fits("reranker"); err != nil || len(log.lines) != 0 {
		t.Fatalf("%v %q", err, log.lines)
	}
}

// A model with no need, one not on the GPU, and a model outside the one
// config start without a reading.
func TestOnlyANeedOnTheGPUReadsTheCard(t *testing.T) {
	h, _, reads := fitHarness(t, func() (Resources, error) { return underAGame(), nil })
	for _, model := range []string{"chat", "embed", "unplaced"} {
		if err := h.w.Fits(model); err != nil {
			t.Errorf("%s: %v", model, err)
		}
		h.w.StartEnded(model)
	}
	h.w.SetPlacement(nil)
	if err := h.w.Fits("reranker"); err != nil {
		t.Errorf("outside the one config: %v", err)
	}
	if *reads != 0 {
		t.Fatalf("the card was read %d times", *reads)
	}
}

// A card that cannot be read is not evidence of contention: the start goes
// ahead, and the log says so.
func TestACardThatCannotBeReadLetsTheStartThrough(t *testing.T) {
	h, log, _ := fitHarness(t, func() (Resources, error) { return Resources{}, errors.New("NVML: unknown error") })
	if err := h.w.Fits("reranker"); err != nil {
		t.Fatal(err)
	}
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "could not be read, so it starts: NVML: unknown error") {
		t.Fatalf("logged %q", log.lines)
	}
}

// The verdict lists each model's devices and nothing else, as the UI's
// Status reads them: the need stays the gate's.
func TestTheVerdictListsEachModelsDevicesOnly(t *testing.T) {
	h, _, _ := fitHarness(t, func() (Resources, error) { return quiet(), nil })
	raw, err := json.Marshal(h.w.State())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Placement map[string][]string `json:"placement"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v in %s", err, raw)
	}
	if s := fmt.Sprint(got.Placement); s != "map[chat:[gpu] embed:[cpu] reranker:[gpu]]" {
		t.Fatalf("placement %s in %s", s, raw)
	}
	h.w.SetPlacement(nil)
	if raw, _ := json.Marshal(h.w.State()); !strings.Contains(string(raw), `"placement":null`) {
		t.Fatalf("outside the one config: %s", raw)
	}
}

// Two members of a group kept loaded together start at once (0029). The
// second start waits until the first has ended, so its reading is taken with
// the first model's VRAM in it: here the reranker took 4,000 of the 6,000 MB
// that were free, and the agent's 3,100 no longer fit.
func TestASecondStartOnTheGPUReadsTheCardAfterTheFirstHasStarted(t *testing.T) {
	var mu sync.Mutex
	free := 6000
	h, log, _ := fitHarness(t, func() (Resources, error) {
		mu.Lock()
		defer mu.Unlock()
		r := quiet()
		r.VRAMFreeMB = ip(free)
		return r, nil
	})
	h.w.SetPlacement(Placement{
		"reranker": {Devices: []string{DeviceGPU}, VRAMMB: 3000},
		"agent":    {Devices: []string{DeviceGPU}, VRAMMB: 3100},
	})
	if err := h.w.Fits("reranker"); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() { second <- h.w.Fits("agent") }()
	select {
	case err := <-second:
		t.Fatalf("the second start did not wait for the first: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	mu.Lock()
	free = 2000
	mu.Unlock()
	h.w.StartEnded("reranker")
	var room NoRoomError
	if err := <-second; !errors.As(err, &room) || room.FreeMB != 2000 {
		t.Fatalf("the second start read the card before the first had started: %v", err)
	}
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "<agent> not started") {
		t.Fatalf("logged %q", log.lines)
	}
}

// The slot is the GPU's. A start with no need still holds it, since its VRAM
// is in the next start's reading; a start refused for room frees it at once;
// the end of a start that does not hold it frees nothing; and a model on the
// CPU never waits for it.
func TestOnlyAStartOnTheGPUHoldsTheSlot(t *testing.T) {
	h, _, _ := fitHarness(t, func() (Resources, error) { return underAGame(), nil })
	returns := func(model string) error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- h.w.Fits(model) }()
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			t.Fatalf("%s waited for the slot", model)
			return nil
		}
	}
	waits := func(model string) chan error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- h.w.Fits(model) }()
		select {
		case err := <-done:
			t.Fatalf("%s did not wait for the slot: %v", model, err)
		case <-time.After(100 * time.Millisecond):
		}
		return done
	}
	if err := returns("chat"); err != nil {
		t.Fatal(err)
	}
	if err := returns("embed"); err != nil {
		t.Fatalf("a model on the CPU: %v", err)
	}
	pending := waits("reranker")
	h.w.StartEnded("embed")
	h.w.StartEnded("unplaced")
	select {
	case err := <-pending:
		t.Fatalf("the end of a start that did not hold the slot freed it: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	h.w.StartEnded("chat")
	var room NoRoomError
	if err := <-pending; !errors.As(err, &room) {
		t.Fatalf("under a game the reranker started: %v", err)
	}
	if err := returns("chat"); err != nil {
		t.Fatalf("after a refusal the slot was still held: %v", err)
	}
}

// A start whose end is never heard holds the slot for gpuStartWait at most,
// then the next start goes ahead and says so.
func TestAStartWhoseEndIsNeverHeardHoldsTheSlotForAWhileOnly(t *testing.T) {
	was := gpuStartWait
	gpuStartWait = 200 * time.Millisecond
	t.Cleanup(func() { gpuStartWait = was })
	r := underAGame()
	r.VRAMFreeMB = ip(8000)
	h, log, _ := fitHarness(t, func() (Resources, error) { return r, nil })
	if err := h.w.Fits("chat"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := h.w.Fits("reranker"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took < gpuStartWait {
		t.Fatalf("the second start waited %s", took)
	}
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "<reranker> waited 200ms for <chat>'s start on the GPU to end, and goes ahead") {
		t.Fatalf("logged %q", log.lines)
	}
	// The slot is the second start's now: the first's late end frees nothing.
	h.w.StartEnded("chat")
	h.w.mu.Lock()
	holder := h.w.gpuStart
	h.w.mu.Unlock()
	if holder != "reranker" {
		t.Fatalf("the slot is %q's", holder)
	}
}
