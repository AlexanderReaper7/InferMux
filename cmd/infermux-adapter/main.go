// infermux-adapter puts an InferMux key on the requests of a client that
// cannot send one, such as Immich's ML URL (0013). One instance per client.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/adapter"
)

// routes is -route, repeatable: /from=/to.
type routes map[string]string

func (r routes) String() string { return fmt.Sprint(map[string]string(r)) }

func (r routes) Set(v string) error {
	from, to, ok := strings.Cut(v, "=")
	if !ok || !strings.HasPrefix(from, "/") || !strings.HasPrefix(to, "/") {
		return fmt.Errorf("%q: a route is /path=/path", v)
	}
	r[from] = to
	return nil
}

func main() {
	listen := flag.String("listen", "", "address to listen on, where the client can reach it")
	target := flag.String("target", "", "where requests go, e.g. http://127.0.0.1:5001/upstream/immich-ml")
	keyFile := flag.String("key-file", "", "a file holding the client's InferMux key")
	rs := routes{}
	flag.Var(rs, "route", "/from=/to: a request for /from goes to /to on the target's host instead; repeatable")
	flag.Parse()

	if *listen == "" || *target == "" || *keyFile == "" {
		slog.Error("-listen, -target and -key-file are required")
		os.Exit(2)
	}
	u, err := url.Parse(*target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		slog.Error("bad -target", "target", *target, "error", err)
		os.Exit(2)
	}
	raw, err := os.ReadFile(*keyFile)
	key := strings.TrimSpace(string(raw))
	if err != nil || key == "" {
		slog.Error("bad -key-file", "error", err)
		os.Exit(2)
	}
	slog.Info("forwarding", "listen", *listen, "target", u.String(), "routes", rs.String())
	if err := http.ListenAndServe(*listen, adapter.New(u, key, rs)); err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
}
