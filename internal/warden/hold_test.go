package warden

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func holdHarness(t *testing.T) *harness {
	t.Helper()
	h, _, _ := fitHarness(t, func() (Resources, error) {
		r := quiet()
		r.VRAMFreeMB = ip(8000)
		return r, nil
	})
	h.w.SetPlacement(Placement{
		"reranker":     {Devices: []string{DeviceGPU}, VRAMMB: 3100},
		"chat":         {Devices: []string{DeviceGPU}},
		"embed":        {Devices: []string{DeviceCPU}},
		"comfyui_auto": {Devices: []string{DeviceGPU}},
	})
	h.w.SetComfyUIModel("comfyui_auto")
	return h
}

// fitsAtOnce is Fits for a start that must not wait for the slot.
func fitsAtOnce(t *testing.T, w *Warden, model string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- w.Fits(model) }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("%s waited for the slot", model)
		return nil
	}
}

func isHeld(t *testing.T, err error, model string) {
	t.Helper()
	var held HeldError
	if !errors.As(err, &held) {
		t.Fatalf("%s under a hold: %v", model, err)
	}
	if held.StatusCode() != http.StatusServiceUnavailable || held.Header().Get("Retry-After") != "300" || held.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%s: %d %v", model, held.StatusCode(), held.Header())
	}
	if body := string(held.Body()); !strings.Contains(body, `"type":"gpu_held"`) || !strings.Contains(body, model+" not started: the GPU is held") {
		t.Fatalf("%s: %s", model, body)
	}
}

// While a hold is open every start on the GPU is refused with 503 gpu_held
// and the GPU's resume window, a model with no need, ComfyUI and a model the
// placement does not name included; a model on the CPU alone starts. A
// refusal takes no slot (0024, 5).
func TestAHoldRefusesEveryStartOnTheGPU(t *testing.T) {
	h := holdHarness(t)
	n, release := h.w.Hold()
	if n != 1 || h.w.State().Holds != 1 {
		t.Fatalf("holds %d, state %d", n, h.w.State().Holds)
	}
	for _, model := range []string{"reranker", "chat", "comfyui_auto", "unplaced"} {
		isHeld(t, fitsAtOnce(t, h.w, model), model)
	}
	if err := fitsAtOnce(t, h.w, "embed"); err != nil {
		t.Fatalf("a model on the CPU under a hold: %v", err)
	}
	release()
	if h.w.State().Holds != 0 {
		t.Fatalf("holds %d after the release", h.w.State().Holds)
	}
	for _, model := range []string{"chat", "comfyui_auto"} {
		if err := fitsAtOnce(t, h.w, model); err != nil {
			t.Fatalf("%s after the hold: %v", model, err)
		}
	}
}

// Holds stack: the card stays held until the last one ends.
func TestHoldsStack(t *testing.T) {
	h := holdHarness(t)
	_, first := h.w.Hold()
	n, second := h.w.Hold()
	if n != 2 {
		t.Fatalf("holds %d", n)
	}
	first()
	isHeld(t, fitsAtOnce(t, h.w, "chat"), "chat")
	second()
	if err := fitsAtOnce(t, h.w, "chat"); err != nil {
		t.Fatalf("after both holds: %v", err)
	}
}

// A start that waits for the slot when a hold begins is refused once the
// slot is free, and a start whose card reading a hold began during is
// refused and frees the slot.
func TestAHoldThatBeginsDuringAStartRefusesIt(t *testing.T) {
	h := holdHarness(t)
	if err := fitsAtOnce(t, h.w, "chat"); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan error, 1)
	go func() { waiting <- h.w.Fits("reranker") }()
	select {
	case err := <-waiting:
		t.Fatalf("the second start did not wait for the slot: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	_, release := h.w.Hold()
	h.w.StartEnded("chat")
	isHeld(t, <-waiting, "reranker")
	release()

	reading := h.w.freshProbe
	h.w.freshProbe = func() (Resources, error) {
		_, release = h.w.Hold()
		return reading()
	}
	isHeld(t, fitsAtOnce(t, h.w, "reranker"), "reranker")
	release()
	h.w.freshProbe = reading
	if err := fitsAtOnce(t, h.w, "chat"); err != nil {
		t.Fatalf("the refused start kept the slot: %v", err)
	}
}

// The hold's endpoint counts a hold from its answer's headers until the
// client closes the connection, or the server's stop closes it. Only the
// control socket's handler has it.
func TestAHoldLastsAsLongAsItsConnection(t *testing.T) {
	h := holdHarness(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: h.w.LocalControl()}
	go server.Serve(ln)
	defer server.Close()
	open := func(ctx context.Context) *http.Response {
		t.Helper()
		// A hold whose answer never comes fails the test rather than hangs it.
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		t.Cleanup(cancel)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ln.Addr().String()+"/warden/hold", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if resp.StatusCode != http.StatusOK || err != nil || line != `{"holds":1}`+"\n" {
			t.Fatalf("%d %q %v", resp.StatusCode, line, err)
		}
		return resp
	}
	until := func(holds int) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); h.w.State().Holds != holds; {
			if time.Now().After(deadline) {
				t.Fatalf("holds %d, want %d", h.w.State().Holds, holds)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	resp := open(ctx)
	until(1)
	isHeld(t, fitsAtOnce(t, h.w, "chat"), "chat")
	cancel()
	resp.Body.Close()
	until(0)
	if err := fitsAtOnce(t, h.w, "chat"); err != nil {
		t.Fatalf("after the connection closed: %v", err)
	}
	h.w.StartEnded("chat")

	resp = open(context.Background())
	defer resp.Body.Close()
	until(1)
	server.Close()
	until(0)

	// The keyed side has no hold.
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/warden/hold", nil).WithContext(ctx)
	req.Header.Set("X-InferMux", "test")
	rec := httptest.NewRecorder()
	h.wrap(http.NotFoundHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || ctx.Err() != nil {
		t.Fatalf("POST /warden/hold with a key: %d, %v", rec.Code, ctx.Err())
	}
}
