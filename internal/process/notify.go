package process

// InferMux: a backend that speaks systemd's sd_notify protocol says when it is
// ready, instead of InferMux polling its health endpoint (0019). This file is
// the part every platform builds; the socket is in notify_linux.go.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// ReadinessNotify is the value of a model's metadata.readiness that opts it
// in: the backend gets NOTIFY_SOCKET and sends READY=1 once it can serve.
const ReadinessNotify = "notify"

// Readiness is how a model's start becomes ready: "notify" by READY=1,
// "start" at once (checkEndpoint: none), or "health" by polling
// checkEndpoint until it answers 200.
func Readiness(conf config.ModelConfig) string {
	if want, _ := notifyWanted(conf); want {
		return ReadinessNotify
	}
	if strings.TrimSpace(conf.CheckEndpoint) == "none" {
		return "start"
	}
	return "health"
}

// notifyWanted reads metadata.readiness. Any value but "notify" is an error,
// so a misspelt opt-in fails the start rather than falling back to polling
// without a word.
func notifyWanted(conf config.ModelConfig) (bool, error) {
	v, ok := conf.Metadata["readiness"]
	if !ok {
		return false, nil
	}
	if s, _ := v.(string); s == ReadinessNotify {
		return true, nil
	}
	return false, fmt.Errorf("metadata.readiness is %v; the only value is %q", v, ReadinessNotify)
}

// notifyMax is the longest datagram read. systemd's own limit is the same.
const notifyMax = 4096

// notifyMessage is what one datagram said. The message is newline-separated
// KEY=VALUE assignments (sd_notify(3)).
type notifyMessage struct {
	ready  bool
	status string // STATUS=, "" when absent
	errno  string // ERRNO=, "" when absent
	extend string // EXTEND_TIMEOUT_USEC=, read only to say it is ignored
}

func parseNotify(b []byte) notifyMessage {
	var m notifyMessage
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		key, value, ok := bytes.Cut(line, []byte("="))
		if !ok {
			continue
		}
		switch string(key) {
		case "READY":
			m.ready = string(value) == "1"
		case "STATUS":
			m.status = string(value)
		case "ERRNO":
			m.errno = string(value)
		case "EXTEND_TIMEOUT_USEC":
			m.extend = string(value)
		}
	}
	return m
}

// errExitedBeforeReady is a process that ended before READY=1. doStart treats
// it as a premature exit: there is nothing left to kill.
var errExitedBeforeReady = errors.New("upstream command exited prematurely, before READY=1")

// lastWords is what the backend last said with STATUS= and ERRNO=, for the
// error of a start that failed.
type lastWords struct{ status, errno string }

func (l lastWords) String() string {
	var parts []string
	if l.status != "" {
		parts = append(parts, fmt.Sprintf("last STATUS=%q", l.status))
	}
	if l.errno != "" {
		parts = append(parts, "ERRNO="+l.errno)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// checkOnce asks checkEndpoint once, after READY=1, and wants 200. A backend
// that says READY=1 before it can answer has broken the contract, and the
// start fails with that said rather than falling back to polling, which would
// bring back the delay notify exists to remove.
//
// The request goes through a copy of the model's proxy whose ErrorHandler
// keeps the error, which the proxy's own logs only at debug for a health
// check, so a check that never got an answer says why.
func (p *ProcessCommand) checkOnce(ctx context.Context, proxy *httputil.ReverseProxy) error {
	endpoint := strings.TrimSpace(p.config.CheckEndpoint)
	checkCtx := context.WithValue(ctx, healthCheckKey{}, true)
	req, err := http.NewRequestWithContext(checkCtx, "GET", endpoint, nil)
	if err != nil {
		return fmt.Errorf("READY=1, but checkEndpoint %q: %w", endpoint, err)
	}
	var failed error
	check := *proxy
	check.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		failed = err
		w.WriteHeader(http.StatusBadGateway)
	}
	rr := httptest.NewRecorder()
	check.ServeHTTP(rr, req)
	if failed != nil {
		return fmt.Errorf("READY=1, but GET %s%s failed: %w", p.config.Proxy, endpoint, failed)
	}
	if rr.Code != http.StatusOK {
		return fmt.Errorf("READY=1, but %s%s answered %d", p.config.Proxy, endpoint, rr.Code)
	}
	return nil
}

// awaitNotify waits for READY=1 from the process or one of its descendants,
// then, unless checkEndpoint is none, checks the endpoint once. It returns
// when READY=1 arrived. The timeout is the health check's.
func (p *ProcessCommand) awaitNotify(ctx context.Context, n *notifySocket, pid int, cmdDone <-chan struct{}, timeout time.Duration, proxy *httputil.ReverseProxy) (time.Time, error) {
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	events := n.serve(pid)
	var last lastWords
	for {
		select {
		case <-ctx.Done():
			return time.Time{}, ErrStartAborted
		case <-cmdDone:
			return time.Time{}, fmt.Errorf("%w%v", errExitedBeforeReady, last)
		case <-timer.C:
			return time.Time{}, fmt.Errorf("no READY=1 within %v%v", timeout, last)
		case e := <-events:
			if e.rejected != "" {
				p.proxyLogger.Warnf("<%s> notify: ignored a message: %s", p.id, e.rejected)
				continue
			}
			m := e.msg
			if m.status != "" {
				last.status = m.status
				p.proxyLogger.Debugf("<%s> notify: STATUS=%s", p.id, m.status)
			}
			if m.errno != "" {
				last.errno = m.errno
			}
			if m.extend != "" {
				p.proxyLogger.Debugf("<%s> notify: EXTEND_TIMEOUT_USEC=%s ignored; the timeout is healthCheckTimeout", p.id, m.extend)
			}
			if !m.ready {
				continue
			}
			p.proxyLogger.Infof("<%s> READY=1 from pid %d", p.id, e.pid)
			if strings.TrimSpace(p.config.CheckEndpoint) == "none" {
				return e.at, nil
			}
			checkCtx, cancel := context.WithDeadline(ctx, deadline)
			err := p.checkOnce(checkCtx, proxy)
			cancel()
			if err != nil && ctx.Err() != nil {
				return time.Time{}, ErrStartAborted
			}
			return e.at, err
		}
	}
}

// notifyEvent is one datagram, or why it was not taken.
type notifyEvent struct {
	at       time.Time // when it was read
	pid      int       // the sender, from SCM_CREDENTIALS
	msg      notifyMessage
	rejected string // why it was ignored; "" when taken
}
