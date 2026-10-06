package failover

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/remote"
)

type nopLog struct{}

func (nopLog) Warnf(string, ...any) {}

// places answers each model name with its status, records what arrived, and
// sets a header of its own on every answer, so a test sees whether a dropped
// attempt's headers reached the client.
type places struct {
	mu       sync.Mutex
	status   map[string]int
	attempts []string
	bodies   []map[string]any
}

func (p *places) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(raw, &body)
	model, _ := body["model"].(string)
	p.mu.Lock()
	p.attempts = append(p.attempts, model)
	p.bodies = append(p.bodies, body)
	status := p.status[model]
	p.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	rw.Header().Add("X-Answered-By", model)
	rw.WriteHeader(status)
	fmt.Fprintf(rw, "from %s", model)
}

func post(t *testing.T, h http.Handler, model string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"input":["a","b"],"encoding_format":"float"}`, model)
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		for _, value := range v {
			r.Header.Add(k, value)
		}
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	return rw
}

func newHandler(next http.Handler) *Handler {
	h := New(next, "reaperboi", nopLog{})
	h.Set(map[string][]string{
		"embed": {"zbox", "reaperboi"},
		"three": {"zbox", "reaperboi/three-gpu", "reaperboi/three-cpu"},
	})
	return h
}

func TestFirstPlaceThatAnswers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   map[string]int
		attempts []string
		code     int
	}{
		{"the first answers", nil, []string{"zbox/embed"}, 200},
		{"502 fails over", map[string]int{"zbox/embed": 502}, []string{"zbox/embed", "embed"}, 200},
		{"503 fails over", map[string]int{"zbox/embed": 503}, []string{"zbox/embed", "embed"}, 200},
		{"504 fails over", map[string]int{"zbox/embed": 504}, []string{"zbox/embed", "embed"}, 200},
		{"500 is the client's", map[string]int{"zbox/embed": 500}, []string{"zbox/embed"}, 500},
		{"404 is the client's", map[string]int{"zbox/embed": 404}, []string{"zbox/embed"}, 404},
		{"401 is the client's", map[string]int{"zbox/embed": 401}, []string{"zbox/embed"}, 401},
		{"403 is the client's", map[string]int{"zbox/embed": 403}, []string{"zbox/embed"}, 403},
		{"the last place's 503 is the client's", map[string]int{"zbox/embed": 503, "embed": 503}, []string{"zbox/embed", "embed"}, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &places{status: tc.status}
			rw := post(t, newHandler(p), "embed", nil)
			if fmt.Sprint(p.attempts) != fmt.Sprint(tc.attempts) {
				t.Fatalf("attempts %v, want %v", p.attempts, tc.attempts)
			}
			if rw.Code != tc.code {
				t.Fatalf("status %d, want %d", rw.Code, tc.code)
			}
			last := tc.attempts[len(tc.attempts)-1]
			if got := rw.Body.String(); got != "from "+last {
				t.Fatalf("body %q, want only the answer from %s", got, last)
			}
			if got := rw.Header().Values("X-Answered-By"); fmt.Sprint(got) != fmt.Sprint([]string{last}) {
				t.Fatalf("X-Answered-By %v: a dropped attempt's headers reached the client", got)
			}
		})
	}
}

func TestPlacesNameOtherModels(t *testing.T) {
	p := &places{status: map[string]int{"zbox/three": 502, "three-gpu": 503}}
	rw := post(t, newHandler(p), "three", nil)
	want := []string{"zbox/three", "three-gpu", "three-cpu"}
	if fmt.Sprint(p.attempts) != fmt.Sprint(want) || rw.Code != 200 {
		t.Fatalf("attempts %v status %d, want %v and 200", p.attempts, rw.Code, want)
	}
}

func TestEveryAttemptGetsTheWholeBody(t *testing.T) {
	p := &places{status: map[string]int{"zbox/embed": 502}}
	post(t, newHandler(p), "embed", nil)
	for i, body := range p.bodies {
		if fmt.Sprint(body["input"]) != "[a b]" || body["encoding_format"] != "float" {
			t.Fatalf("attempt %d got %v", i+1, body)
		}
	}
}

func TestOnlyListedModelsFailOver(t *testing.T) {
	p := &places{status: map[string]int{"other": 503}}
	rw := post(t, newHandler(p), "other", nil)
	if fmt.Sprint(p.attempts) != "[other]" || rw.Code != 503 {
		t.Fatalf("attempts %v status %d, want one untouched attempt answered 503", p.attempts, rw.Code)
	}
}

func TestAForwardedRequestIsServedWhereItLands(t *testing.T) {
	p := &places{status: map[string]int{"embed": 503}}
	rw := post(t, newHandler(p), "embed", http.Header{remote.HopHeader: {"1"}})
	if fmt.Sprint(p.attempts) != "[embed]" || rw.Code != 503 {
		t.Fatalf("attempts %v status %d, want the request as it came", p.attempts, rw.Code)
	}
}

func TestAStreamPassesThrough(t *testing.T) {
	next := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(rw, "data: %d\n\n", i)
			rw.(http.Flusher).Flush()
		}
	})
	rw := post(t, newHandler(next), "embed", nil)
	if !rw.Flushed || rw.Body.String() != "data: 0\n\ndata: 1\n\ndata: 2\n\n" {
		t.Fatalf("flushed %v body %q", rw.Flushed, rw.Body.String())
	}
}

// The zbox is off: the remote router answers 502 and the local model serves.
func TestAnOfflineHostFailsOverToTheLocalModel(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	local := &places{}
	router, err := remote.New([]remote.Host{{Name: "zbox", URL: url, Key: "host-key"}}, local,
		func(model string) bool { return model == "embed" }, "", remoteLog{})
	if err != nil {
		t.Fatal(err)
	}
	router.Poll("zbox")
	rw := post(t, newHandler(router), "embed", nil)
	if rw.Code != 200 || rw.Body.String() != "from embed" {
		t.Fatalf("status %d body %q, want the local model's answer", rw.Code, rw.Body.String())
	}
	if fmt.Sprint(local.attempts) != "[embed]" {
		t.Fatalf("local attempts %v", local.attempts)
	}
}

type remoteLog struct{}

func (remoteLog) Infof(string, ...any) {}
func (remoteLog) Warnf(string, ...any) {}
