package warden

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The /warden/* controls through Wrap, as infermux-ui and curl reach them.

func control(handler http.Handler, path, body string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("X-InferMux", "test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestManualOverHTTP(t *testing.T) {
	h := newHarness(t, nil)
	h.w.Tick()
	handler := h.wrap(http.NotFoundHandler())
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"action":"pause"}`, 200},
		{`{"action":"sideways"}`, http.StatusConflict},
		{`{`, http.StatusBadRequest},
		{`{"action":"auto"}`, 200},
	} {
		if code, out := control(handler, "/warden/manual", c.body); code != c.want {
			t.Errorf("%s: %d %v", c.body, code, out)
		}
	}
	if code, _ := control(handler, "/warden/manual", `{"action":"pause"}`); code != 200 || !h.w.State().Verdict.Yielded {
		t.Fatalf("a pause by hand did not yield")
	}
	cfg := h.w.config()
	cfg.Policy.Enabled = false
	h.w.Reload(cfg)
	h.w.Tick()
	if code, out := control(handler, "/warden/manual", `{"action":"pause"}`); code != http.StatusConflict {
		t.Fatalf("a manual verdict while disabled: %d %v", code, out)
	}
}

func TestUnloadAndForgiveOverHTTP(t *testing.T) {
	h := newHarness(t, nil)
	h.w.Tick()
	inside, release := make(chan struct{}), make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			close(inside)
			<-release
		}
	}))
	done := make(chan struct{})
	go func() { post(handler, "/v1/chat/completions", nil); close(done) }()
	<-inside
	if code, out := control(handler, "/warden/unload", ""); code != http.StatusConflict || h.models.unloads != 0 {
		t.Fatalf("unload under an interactive request: %d %v, unloads %d", code, out, h.models.unloads)
	}
	h.reading = busy(60)
	h.w.Tick() // owed, deferred under the request
	if code, out := control(handler, "/warden/forgive", ""); code != 200 || out["was_owed"] != true {
		t.Fatalf("forgive: %d %v", code, out)
	}
	if code, out := control(handler, "/warden/forgive", ""); code != 200 || out["was_owed"] != false {
		t.Fatalf("forgive twice: %d %v", code, out)
	}
	close(release)
	<-done
	h.at(time.Hour).w.Tick()
	if h.models.unloads != 0 {
		t.Fatal("a forgiven unload was paid")
	}
	if code, out := control(handler, "/warden/unload", ""); code != 200 || h.models.unloads != 1 {
		t.Fatalf("unload when quiet: %d %v", code, out)
	}
}

func TestCancelBatchOverHTTPLeavesInteractiveAlone(t *testing.T) {
	h := newHarness(t, nil)
	h.w.Tick()
	started := make(chan string, 2)
	ended := make(chan string, 2)
	release := make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			return
		}
		who := r.Header.Get("Authorization")
		started <- who
		select {
		case <-r.Context().Done():
			ended <- who + " cancelled"
		case <-release:
			ended <- who + " finished"
		}
	}))
	go post(handler, "/v1/chat/completions", map[string]string{"Authorization": "Bearer batch-key"})
	go post(handler, "/v1/chat/completions", map[string]string{"Authorization": "Bearer my-key"})
	<-started
	<-started
	if code, out := control(handler, "/warden/cancel-batch", ""); code != 200 || out["cancelled"] != float64(1) {
		t.Fatalf("cancel-batch: %d %v", code, out)
	}
	if got := <-ended; got != "Bearer batch-key cancelled" {
		t.Fatalf("first to end: %s", got)
	}
	close(release)
	if got := <-ended; got != "Bearer my-key finished" {
		t.Fatalf("the interactive request: %s", got)
	}
}

func TestFreeComfyUIOverHTTP(t *testing.T) {
	h := newHarness(t, nil)
	handler := h.wrap(http.NotFoundHandler())
	if code, _ := control(handler, "/warden/comfyui/free", ""); code != http.StatusBadGateway {
		t.Fatalf("free with no ComfyUI: %d", code)
	}
	var body map[string]bool
	fail := true
	comfy := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/free" {
			http.NotFound(rw, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		if fail {
			http.Error(rw, "out of memory", 500)
		}
	}))
	defer comfy.Close()
	cfg := h.w.config()
	cfg.ComfyUIURL = comfy.URL + "/"
	h.w.Reload(cfg)
	if code, out := control(handler, "/warden/comfyui/free", ""); code != http.StatusBadGateway || !strings.Contains(out["detail"].(string), "500") {
		t.Fatalf("a failing ComfyUI: %d %v", code, out)
	}
	if h.w.State().ComfyUI.Freed {
		t.Fatal("marked freed after a failure")
	}
	fail = false
	if code, out := control(handler, "/warden/comfyui/free", ""); code != 200 || !h.w.State().ComfyUI.Freed {
		t.Fatalf("free: %d %v", code, out)
	}
	if !body["unload_models"] || !body["free_memory"] {
		t.Fatalf("ComfyUI was sent %v", body)
	}
}

func TestComfyUIQueueDepth(t *testing.T) {
	answer, status := `{"queue_running":[[1]],"queue_pending":[[2],[3]]}`, 200
	comfy := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/queue" {
			http.NotFound(rw, r)
			return
		}
		rw.WriteHeader(status)
		rw.Write([]byte(answer))
	}))
	defer comfy.Close()
	if n := comfyUIQueueDepth(comfy.URL); n == nil || *n != 3 {
		t.Fatalf("depth %v", n)
	}
	answer = `{}`
	if n := comfyUIQueueDepth(comfy.URL); n == nil || *n != 0 {
		t.Fatalf("an empty queue: %v", n)
	}
	for _, c := range []struct {
		answer string
		status int
	}{{`not json`, 200}, {`{}`, 503}} {
		answer, status = c.answer, c.status
		if n := comfyUIQueueDepth(comfy.URL); n != nil {
			t.Fatalf("%q %d read as %d jobs, not as no answer", c.answer, c.status, *n)
		}
	}
	comfy.Close()
	if n := comfyUIQueueDepth(comfy.URL); n != nil {
		t.Fatal("a ComfyUI that is down read as a queue")
	}
}

func TestAPanickingTickDoesNotStopTheLoop(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Policy.PollSeconds = 1 })
	calls := make(chan struct{}, 10)
	h.w.probe = func() (Resources, error) {
		calls <- struct{}{}
		if len(calls) == 1 {
			panic("probe exploded")
		}
		return quiet(), nil
	}
	h.w.Start()
	defer h.w.Stop()
	deadline := time.After(5 * time.Second)
	for seen := 0; seen < 2; {
		select {
		case <-calls:
			seen++
		case <-deadline:
			t.Fatalf("the loop stopped after a panic, %d ticks", seen)
		}
	}
}
