package warden

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
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
