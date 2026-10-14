package failover

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/remote"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
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
	h.Set(map[string][]Place{
		"embed": {{Place: "zbox"}, {Place: "reaperboi"}},
		"three": {{Place: "zbox"}, {Place: "reaperboi/three-gpu"}, {Place: "reaperboi/three-cpu"}},
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
			where := last
			if !strings.Contains(where, "/") {
				where = "reaperboi/" + where
			}
			if got := rw.Header().Values(PlaceHeader); fmt.Sprint(got) != fmt.Sprint([]string{where}) {
				t.Fatalf("%s %v, want %s", PlaceHeader, got, where)
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

// didNotStart is llama-swap's answer to a model that would not start, as the
// deployed binary sent it on 2026-10-14 for a command that exits at once.
const didNotStart = `{"src":"llama-swap","error":{"message":"unspecific error: upstream command exited prematurely","type":"server_error","param":null,"code":"internal_error"}}`

// A model that does not start fails over: the host that runs it marks
// llama-swap's 500, since the model never had the request (Mark). A 500
// without the mark is the client's, in llama-swap's words too, since
// retrying it elsewhere would run the request twice (0016, 3).
func TestAModelThatDoesNotStartFailsOver(t *testing.T) {
	own := `{"error":{"code":500,"message":"failed to decode image","type":"server_error"}}`
	for _, tc := range []struct {
		name     string
		marked   bool
		body     string
		both     bool
		attempts []string
		code     int
	}{
		{"marked", true, didNotStart, false, []string{"zbox/embed", "embed"}, 200},
		{"llama-swap's words without the mark", false, didNotStart, false, []string{"zbox/embed"}, 500},
		{"a model's own 500", false, own, false, []string{"zbox/embed"}, 500},
		{"the last place's is the client's", true, didNotStart, true, []string{"zbox/embed", "embed"}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts []string
			next := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				model, _ := swaputil.ExtractModel(r)
				attempts = append(attempts, model)
				if model == "zbox/embed" || tc.both {
					if tc.marked {
						rw.Header().Set(NotStartedHeader, "true")
					}
					rw.Header().Set("Content-Type", "application/json")
					rw.WriteHeader(http.StatusInternalServerError)
					io.WriteString(rw, tc.body)
					return
				}
				io.WriteString(rw, "from "+model)
			})
			rw := post(t, newHandler(next), "embed", nil)
			if fmt.Sprint(attempts) != fmt.Sprint(tc.attempts) || rw.Code != tc.code {
				t.Fatalf("attempts %v status %d, want %v and %d", attempts, rw.Code, tc.attempts, tc.code)
			}
			if tc.code == 500 && rw.Body.String() != tc.body {
				t.Fatalf("the 500 reached the client as %q", rw.Body.String())
			}
			if tc.code == 200 && (rw.Body.String() != "from embed" || rw.Header().Get(NotStartedHeader) != "") {
				t.Fatalf("the dropped attempt reached the client: %v %q", rw.Header(), rw.Body.String())
			}
		})
	}
}

// Mark tells llama-swap's own 500 from a model's by whether llama-swap sent
// the request to an upstream, and puts the X-InferMux- headers back as they
// were before llama-swap ran, whatever the model sent.
func TestMarkSeesWhetherAModelHadTheRequest(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(NotStartedHeader, "true")
		w.Header()["X-INFERMUX-BUSY"] = []string{"capacity"}
		w.Header().Set(PlaceHeader, "elsewhere/embed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, didNotStart)
	}))
	defer model.Close()
	target, _ := url.Parse(model.URL)
	// As llama-swap's process proxy: a ReverseProxy on the request's context.
	proxy := httputil.NewSingleHostReverseProxy(target)
	llamaSwap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/started" {
			proxy.ServeHTTP(w, r)
			return
		}
		swaputil.SendError(w, r, errors.New("upstream command exited prematurely"))
	})
	for _, c := range []struct {
		path   string
		marked bool
	}{{"/v1/started", false}, {"/v1/dies", true}} {
		rw := httptest.NewRecorder()
		rw.Header().Set(PlaceHeader, "zbox/embed")
		Mark(llamaSwap).ServeHTTP(rw, httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(`{"model":"embed"}`)))
		res := rw.Result()
		if res.StatusCode != 500 || (res.Header.Get(NotStartedHeader) != "") != c.marked {
			t.Errorf("%s: %d %v, want 500 marked %v", c.path, res.StatusCode, res.Header, c.marked)
		}
		if res.Header.Get(BusyHeader) != "" || fmt.Sprint(res.Header.Values(PlaceHeader)) != "[zbox/embed]" {
			t.Errorf("%s: InferMux's headers are %v", c.path, res.Header)
		}
	}
}

// The mark crosses hosts: the zbox marks its llama-swap's 500, and the host
// the client asked fails over on it through the remote router.
func TestAModelThatDoesNotStartOnAnotherHostFailsOver(t *testing.T) {
	dies := Mark(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		swaputil.SendError(w, r, errors.New("upstream command exited prematurely"))
	}))
	zbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"embed","meta":{"llamaswap":{"type":"model"}}}]}`))
			return
		}
		dies.ServeHTTP(w, r)
	}))
	defer zbox.Close()
	local := &places{}
	router, err := remote.New([]remote.Host{{Name: "zbox", URL: zbox.URL}}, local, func(model string) bool { return model == "embed" }, "", remoteLog{})
	if err != nil {
		t.Fatal(err)
	}
	router.Poll("zbox")
	rw := post(t, newHandler(router), "embed", nil)
	if rw.Code != 200 || rw.Body.String() != "from embed" || fmt.Sprint(local.attempts) != "[embed]" {
		t.Fatalf("status %d body %q local %v, want the local model's answer", rw.Code, rw.Body.String(), local.attempts)
	}
}

// A model's own 500 at a place that is not the last reaches the client as
// the model writes it, as any other answer does: its status as soon as
// net/http sends it, here once the body passes the server's buffer, and its
// headers as they were at WriteHeader (0016).
func TestAModelsOwn500IsNotHeld(t *testing.T) {
	seen := make(chan struct{})
	var early atomic.Bool
	next := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		model, _ := swaputil.ExtractModel(r)
		if model != "zbox/embed" {
			io.WriteString(rw, "from "+model)
			return
		}
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Header().Set("X-Late", "true")
		rw.Write(bytes.Repeat([]byte("x"), 16<<10))
		select {
		case <-seen:
			early.Store(true)
		case <-time.After(3 * time.Second):
		}
		io.WriteString(rw, "end")
	})
	front := httptest.NewServer(newHandler(next))
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/embeddings", "application/json", strings.NewReader(`{"model":"embed","input":["a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	close(seen)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 500 || len(raw) != 16<<10+3 {
		t.Fatalf("status %d, %d bytes", resp.StatusCode, len(raw))
	}
	if !early.Load() {
		t.Error("the client got the status only after the attempt returned")
	}
	if resp.Header.Get("X-Late") != "" {
		t.Error("a header set after WriteHeader reached the client")
	}
}

// A trailer set after the body passes through Mark and a place that is not
// the last, as net/http sends it.
func TestATrailerPassesThrough(t *testing.T) {
	next := Mark(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Trailer", "X-Done")
		io.WriteString(rw, "body")
		rw.Header().Set("X-Done", "yes")
	}))
	front := httptest.NewServer(newHandler(next))
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/embeddings", "application/json", strings.NewReader(`{"model":"embed","input":["a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if string(raw) != "body" || resp.Trailer.Get("X-Done") != "yes" {
		t.Fatalf("body %q trailer %v", raw, resp.Trailer)
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

func TestProxy_LoadBalanceAcrossServingHosts(t *testing.T) {
	// Traffic arriving directly on zbox must count against requests forwarded
	// from this front door. The real remote router carries the reservation hints.
	zboxAdmission, localAdmission := testAdmission(), testAdmission()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	zboxModel := zboxAdmission.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hold") == "true" {
			close(started)
			<-release
		}
		w.Header().Set("X-Answered-By", "zbox")
		w.Write([]byte("zbox"))
	}))
	zbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"embed","meta":{"llamaswap":{"type":"model"}}}]}`))
			return
		}
		zboxModel.ServeHTTP(w, r)
	}))
	defer zbox.Close()
	go func() { defer close(done); post(t, zboxModel, "embed", http.Header{"X-Hold": {"true"}}) }()
	<-started
	defer func() { close(release); <-done }()
	local := localAdmission.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model, _ := swaputil.ExtractModel(r)
		w.Header().Set("X-Answered-By", model)
		w.Write([]byte(model))
	}))
	router, err := remote.New([]remote.Host{{Name: "zbox", URL: zbox.URL}}, local, func(string) bool { return true }, "", remoteLog{})
	if err != nil {
		t.Fatal(err)
	}
	router.Poll("zbox")
	h := New(router, "strix", nopLog{})
	h.Batch = func(r *http.Request) bool { return r.Header.Get("X-Test-Batch") == "true" }
	h.Set(map[string][]Place{"embed": {
		{Place: "zbox", MaxInflight: 1},
		{Place: "strix", MaxInflight: 1, OnlyIfIdle: true},
		{Place: "strix/cpu", BatchOnly: true},
	}})
	if rw := post(t, h, "embed", nil); rw.Code != 200 || rw.Body.String() != "embed" || rw.Header().Get(BusyHeader) != "" {
		t.Fatalf("zbox overflow did not reach local GPU cleanly: %d %v %q", rw.Code, rw.Header(), rw.Body.String())
	}
	for _, active := range []string{"chat", "embed"} {
		if busy := localAdmission.begin(active, true, 0, false); busy != "" {
			t.Fatal(busy)
		}
		if rw := post(t, h, "embed", http.Header{"X-Test-Batch": {"true"}}); rw.Code != 200 || rw.Body.String() != "cpu" {
			t.Fatalf("batch overflow while %s active: %d %q", active, rw.Code, rw.Body.String())
		}
		// The primary is still processing the direct request. The unbounded
		// retry reaches it, while the model backend owns its actual queue.
		if rw := post(t, h, "embed", nil); rw.Code != 200 || rw.Body.String() != "zbox" {
			t.Fatalf("interactive used CPU or displaced %s: %d %q", active, rw.Code, rw.Body.String())
		}
		localAdmission.end(active, true)
	}
	// Reloading the route applies immediately and keeps occupancy intact.
	h.Set(map[string][]Place{"embed": {{Place: "zbox", MaxInflight: 2}, {Place: "strix"}}})
	if rw := post(t, h, "embed", nil); rw.Code != 200 || rw.Body.String() != "zbox" {
		t.Fatalf("new limit not applied: %d %q", rw.Code, rw.Body.String())
	}
}

// busyPlaces answers like places, except that a model in busy answers 503
// with the BusyHeader, as Admission does at a place's limit.
type busyPlaces struct {
	places
	busy map[string]string
}

func (p *busyPlaces) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	model, _ := swaputil.ExtractModel(r)
	if why := p.busy[model]; why != "" && r.Header.Get(MaxInflightHeader) != "" {
		p.mu.Lock()
		p.attempts = append(p.attempts, model+" busy")
		p.mu.Unlock()
		rw.Header().Set(BusyHeader, why)
		rw.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	p.places.ServeHTTP(rw, r)
}

// The failover lane is tried only when every balance place is down or
// refusing, never because one is at its limit (0022, 2).
func TestTheFailoverLane(t *testing.T) {
	lanes := map[string][]Place{"m": {
		{Place: "zbox", MaxInflight: 1},
		{Place: "strix", MaxInflight: 1},
		{Place: "strix/m@cpu", Failover: true},
	}}
	for _, tc := range []struct {
		name     string
		status   map[string]int
		busy     map[string]string
		attempts []string
		code     int
		place    string
	}{
		{"balance serves", nil, nil, []string{"zbox/m"}, 200, "zbox/m"},
		{"both down", map[string]int{"zbox/m": 502, "m": 503}, nil, []string{"zbox/m", "m", "m@cpu"}, 200, "strix/m@cpu"},
		{"one at its limit waits there", map[string]int{"m": 502}, map[string]string{"zbox/m": "capacity"}, []string{"zbox/m busy", "m", "zbox/m"}, 200, "zbox/m"},
		{"a yielded card is refusing", map[string]int{"zbox/m": 502}, map[string]string{"m": "yielded"}, []string{"zbox/m", "m busy", "m@cpu"}, 200, "strix/m@cpu"},
		{"the failover place's failure is the client's", map[string]int{"zbox/m": 502, "m": 502, "m@cpu": 503}, nil, []string{"zbox/m", "m", "m@cpu"}, 503, "strix/m@cpu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &busyPlaces{places: places{status: tc.status}, busy: tc.busy}
			h := New(p, "strix", nopLog{})
			h.Set(lanes)
			rw := post(t, h, "m", nil)
			if fmt.Sprint(p.attempts) != fmt.Sprint(tc.attempts) || rw.Code != tc.code {
				t.Fatalf("attempts %v status %d, want %v and %d", p.attempts, rw.Code, tc.attempts, tc.code)
			}
			if got := rw.Header().Values(PlaceHeader); fmt.Sprint(got) != fmt.Sprint([]string{tc.place}) {
				t.Fatalf("%s %v, want %s", PlaceHeader, got, tc.place)
			}
		})
	}
	// A batch-only failover place is no place for an interactive request.
	p := &places{status: map[string]int{"zbox/m": 502, "m": 502}}
	h := New(p, "strix", nopLog{})
	h.Batch = func(*http.Request) bool { return false }
	h.Set(map[string][]Place{"m": {{Place: "zbox"}, {Place: "strix"}, {Place: "strix/m@cpu", Failover: true, BatchOnly: true}}})
	if rw := post(t, h, "m", nil); rw.Code != 502 || fmt.Sprint(p.attempts) != "[zbox/m m]" {
		t.Fatalf("attempts %v status %d", p.attempts, rw.Code)
	}
}
