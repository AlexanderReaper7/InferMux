package swaputil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// marker is a recorder of the kind MarkClientClosed is given.
type marker struct {
	*httptest.ResponseRecorder
	status  int
	started bool
}

func (m *marker) WriteHeader(code int) {
	m.status, m.started = code, true
	m.ResponseRecorder.WriteHeader(code)
}

func (m *marker) MarkStatus(code int) { m.status = code }
func (m *marker) WroteHeader() bool   { return m.started }

// A context the warden cancelled with an HTTPError is answered with it, a
// client that hung up is still recorded as 499, and a response that started
// keeps its status (0024, 4).
func TestACancelWithAnHTTPErrorIsAnswered(t *testing.T) {
	request := func(cause error) *http.Request {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		return WithClientContext(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx))
	}

	m := &marker{ResponseRecorder: httptest.NewRecorder()}
	if !MarkClientClosed(m, request(ConcurrencyLimitError{RetryAfter: 300})) {
		t.Error("an answered cancel was not reported")
	}
	if m.status != http.StatusTooManyRequests || m.Header().Get("Retry-After") != "300" || m.Body.Len() == 0 {
		t.Errorf("answered with %d, Retry-After %q: %q", m.status, m.Header().Get("Retry-After"), m.Body.String())
	}

	m = &marker{ResponseRecorder: httptest.NewRecorder()}
	if !MarkClientClosed(m, request(context.Canceled)) || m.status != StatusClientClosedRequest || m.started {
		t.Errorf("a client that hung up: %d, written %v", m.status, m.started)
	}

	m = &marker{ResponseRecorder: httptest.NewRecorder()}
	m.WriteHeader(http.StatusOK)
	if MarkClientClosed(m, request(ConcurrencyLimitError{})) || m.status != http.StatusOK || m.Body.Len() != 0 {
		t.Errorf("a started response: %d, %q", m.status, m.Body.String())
	}

	m = &marker{ResponseRecorder: httptest.NewRecorder()}
	if MarkClientClosed(m, WithClientContext(httptest.NewRequest(http.MethodPost, "/", nil))) || m.started {
		t.Error("a live request was answered")
	}
}
