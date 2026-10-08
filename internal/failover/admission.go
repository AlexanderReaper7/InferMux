package failover

import (
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// These hints apply only to the request carrying them. They never change
// the destination's policy or bypass its authentication and GPU warden.
const (
	MaxInflightHeader = "X-InferMux-Max-Inflight"
	OnlyIfIdleHeader  = "X-InferMux-Only-If-Idle"
	BusyHeader        = "X-InferMux-Busy"
)

// Admission counts requests on the host that serves them, including direct
// clients and aliases. Model returns a canonical name, whether it uses this
// host's GPU, and whether this host serves it. Remote forwarding is excluded.
type Admission struct {
	Model     func(*http.Request) (name string, gpu, served bool)
	mu        sync.Mutex
	active    map[string]int
	gpuActive map[string]int
}

// Wrap belongs after authentication and the warden's request gate. The busy
// check and reservation share one lock, so concurrent arrivals cannot both
// claim the last slot. A reservation lasts until the response completes.
func (a *Admission) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !inference(r) {
			next.ServeHTTP(rw, r)
			return
		}
		name, gpu, served := a.Model(r)
		if !served {
			next.ServeHTTP(rw, r)
			return
		}
		limit := 0
		if raw := r.Header.Get(MaxInflightHeader); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 0 {
				swaputil.SendResponse(rw, r, http.StatusBadRequest, "invalid overflow request limit")
				return
			}
		}
		idle := r.Header.Get(OnlyIfIdleHeader) == "true"
		if busy := a.begin(name, gpu, limit, idle); busy != "" {
			rw.Header().Set(BusyHeader, busy)
			rw.Header().Set("Retry-After", "1")
			swaputil.SendResponse(rw, r, http.StatusServiceUnavailable, "destination is busy")
			return
		}
		defer a.end(name, gpu)
		// The model backend is unaware of InferMux's routing hints.
		r = r.Clone(r.Context())
		r.Header.Del(MaxInflightHeader)
		r.Header.Del(OnlyIfIdleHeader)
		next.ServeHTTP(rw, r)
	})
}

func (a *Admission) begin(name string, gpu bool, limit int, idle bool) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gpu && idle {
		for other, count := range a.gpuActive {
			if other != name && count > 0 {
				return "other-model"
			}
		}
	}
	if limit > 0 && a.active[name] >= limit {
		return "capacity"
	}
	if a.active == nil {
		a.active = map[string]int{}
		a.gpuActive = map[string]int{}
	}
	a.active[name]++
	if gpu {
		a.gpuActive[name]++
	}
	return ""
}

func (a *Admission) end(name string, gpu bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active[name]--
	if a.active[name] == 0 {
		delete(a.active, name)
	}
	if gpu {
		a.gpuActive[name]--
		if a.gpuActive[name] == 0 {
			delete(a.gpuActive, name)
		}
	}
}

func inference(r *http.Request) bool {
	return (r.Method == http.MethodPost || (r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket"))) && !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/warden/")
}
