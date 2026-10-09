package failover

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func testAdmission() *Admission {
	return &Admission{Model: func(r *http.Request) (string, bool, bool) {
		model, _ := swaputil.ExtractModel(r)
		if model == "alias" {
			model = "embed"
		}
		return model, model != "cpu", model != "remote"
	}}
}

func TestAdmission_DirectRequestsAndAliasesCount(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a := testAdmission()
	next := a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(200)
	}))
	go func() { defer close(done); post(t, next, "alias", nil) }()
	<-started
	rw := post(t, next, "embed", http.Header{MaxInflightHeader: {"1"}})
	close(release)
	<-done
	if rw.Code != 503 || rw.Header().Get(BusyHeader) != "capacity" {
		t.Fatalf("status %d headers %v: direct alias did not occupy capacity", rw.Code, rw.Header())
	}
	if got := a.begin("embed", true, 1, false); got != "" {
		t.Fatalf("slot leaked: %s", got)
	}
	a.end("embed", true)
}

func TestAdmission_OnlyIfIdleCountsOtherGPUWork(t *testing.T) {
	a := testAdmission()
	if busy := a.begin("chat", true, 0, false); busy != "" {
		t.Fatal(busy)
	}
	if busy := a.begin("embed", true, 1, true); busy != "other-model" {
		t.Fatalf("busy=%q", busy)
	}
	if busy := a.begin("cpu", false, 0, true); busy != "" {
		t.Fatalf("CPU blocked by GPU work: %s", busy)
	}
	a.end("cpu", false)
	a.end("chat", true)
	// A loaded but idle model does not enter this request counter.
	if busy := a.begin("embed", true, 2, true); busy != "" {
		t.Fatal(busy)
	}
	if busy := a.begin("embed", true, 2, true); busy != "" {
		t.Fatalf("same-model work blocked: %s", busy)
	}
	a.end("embed", true)
	a.end("embed", true)
}

func TestAdmission_ConcurrentRequestsReserveAtomically(t *testing.T) {
	a := testAdmission()
	var accepted atomic.Int32
	start, release := make(chan struct{}), make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(32)
	done.Add(32)
	for range 32 {
		go func() {
			defer done.Done()
			<-start
			busy := a.begin("embed", true, 1, false)
			if busy == "" {
				accepted.Add(1)
			}
			ready.Done()
			<-release
			if busy == "" {
				a.end("embed", true)
			}
		}()
	}
	close(start)
	ready.Wait()
	close(release)
	done.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d simultaneous requests, want 1", accepted.Load())
	}
}

func TestAdmission_HintsStopAtServingHost(t *testing.T) {
	a := testAdmission()
	var got http.Header
	next := a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	headers := http.Header{MaxInflightHeader: {"1"}, OnlyIfIdleHeader: {"true"}}
	post(t, next, "remote", headers)
	if got.Get(MaxInflightHeader) != "1" || got.Get(OnlyIfIdleHeader) != "true" {
		t.Fatalf("remote lost hints: %v", got)
	}
	post(t, next, "embed", headers)
	if got.Get(MaxInflightHeader) != "" || got.Get(OnlyIfIdleHeader) != "" {
		t.Fatalf("backend got hints: %v", got)
	}
	for _, limit := range []string{"-1", "abc"} {
		if rw := post(t, next, "embed", http.Header{MaxInflightHeader: {limit}}); rw.Code != 400 {
			t.Fatalf("limit %q status %d", limit, rw.Code)
		}
	}
	// Management calls must not enter model admission.
	r := httptest.NewRequest("POST", "/api/models/unload", strings.NewReader(`{"model":"embed"}`))
	r.Header.Set(MaxInflightHeader, "abc")
	rw := httptest.NewRecorder()
	next.ServeHTTP(rw, r)
	if rw.Code != 200 {
		t.Fatalf("management call gated: %d", rw.Code)
	}
}

func TestAdmission_PanicReleasesReservation(t *testing.T) {
	a := testAdmission()
	next := a.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("backend failed") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("backend did not panic")
			}
		}()
		post(t, next, "embed", nil)
	}()
	if busy := a.begin("embed", true, 1, false); busy != "" {
		t.Fatalf("reservation leaked: %s", busy)
	}
	a.end("embed", true)
}

// A game that made the warden yield must not get a model loaded under it by
// overflow, interactive or not (0021). Directly addressed work is the gate's.
func TestAdmission_OnlyIfIdleRefusedWhileYielded(t *testing.T) {
	a := testAdmission()
	yielded := true
	a.Yielded = func() bool { return yielded }
	if busy := a.begin("embed", true, 1, true); busy != "yielded" {
		t.Fatalf("busy=%q, want yielded", busy)
	}
	if busy := a.begin("cpu", false, 0, true); busy != "" {
		t.Fatalf("CPU refused by the GPU's yield: %s", busy)
	}
	a.end("cpu", false)
	if busy := a.begin("embed", true, 1, false); busy != "" {
		t.Fatalf("a request without only_if_idle was refused: %s", busy)
	}
	a.end("embed", true)
	yielded = false
	if busy := a.begin("embed", true, 1, true); busy != "" {
		t.Fatalf("refused after resume: %s", busy)
	}
	a.end("embed", true)
}
