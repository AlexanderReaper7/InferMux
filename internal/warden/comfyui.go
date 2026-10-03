package warden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ComfyUI keeps its models on the GPU after a job and never releases them on
// its own, so an idle ComfyUI holds 6 to 12 GB until something says otherwise.
// The warden watches its queue for two reasons (0002):
//
//   - A queued or running job is contention the moment it is seen. Utilization
//     alone would notice it one tick later, after ComfyUI had started loading
//     weights into a card the models still occupied.
//   - After ComfyUIIdleSeconds with an empty queue the warden POSTs /free,
//     which unloads its models and empties torch's cache.

// comfyUIQueueDepth is running plus pending jobs, or nil when ComfyUI does not
// answer. Nil is no opinion, not an empty queue: a ComfyUI that is down is not
// working, but one too busy to answer may be.
func comfyUIQueueDepth(url string) *int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var body struct {
		Running []json.RawMessage `json:"queue_running"`
		Pending []json.RawMessage `json:"queue_pending"`
	}
	if err := getJSON(ctx, strings.TrimRight(url, "/")+"/queue", &body); err != nil {
		return nil
	}
	n := len(body.Running) + len(body.Pending)
	return &n
}

// comfyUIFree unloads every model and releases the cached allocations.
// Idempotent: a ComfyUI with nothing loaded answers the same.
func comfyUIFree(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := postJSON(ctx, strings.TrimRight(url, "/")+"/free",
		map[string]bool{"unload_models": true, "free_memory": true})
	return err
}

func getJSON(ctx context.Context, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// postJSON returns the answer as JSON when it is JSON, and as {"body": ...}
// when it is not. Anything but a 2xx is an error.
func postJSON(ctx context.Context, url string, payload any) (any, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		return map[string]any{"body": truncate(string(body), 500)}, nil
	}
	return parsed, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
