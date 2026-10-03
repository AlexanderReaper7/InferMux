package muxui

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/warden"
)

// dist is the built web app (webui/). The Nix build copies it in; a checkout
// has only .keep, and serves a page that says so.
//
//go:embed all:dist
var dist embed.FS

// Handler is infermux-ui's whole HTTP side:
//
//	/api/...     the files, through Store
//	/daemon/...  the daemon at Daemon, with X-InferMux added
//	/            the web app
func Handler(store *Store, build *Builder, daemon *url.URL) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/state", func(rw http.ResponseWriter, r *http.Request) {
		models, err := store.Models()
		if err != nil {
			fail(rw, err)
			return
		}
		runtimes, err := store.Runtimes()
		if err != nil {
			fail(rw, err)
			return
		}
		wcfg, err := store.Warden()
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]any{
			"models":     models,
			"runtimes":   runtimes,
			"warden":     wcfg,
			"kv_kernels": store.KVKernels,
			"paths": map[string]any{
				"models_dir": store.ModelsDir, "warden_file": store.WardenFile,
				"base_config": store.BaseConfig, "gguf_dirs": store.GGUFDirs,
			},
		})
	})
	mux.HandleFunc("POST /api/models", func(rw http.ResponseWriter, r *http.Request) {
		var m Model
		if !decode(rw, r, &m) {
			return
		}
		if err := store.SaveModel("", m); err != nil {
			fail(rw, err)
			return
		}
		build.Outdated()
		reply(rw, m)
	})
	mux.HandleFunc("PUT /api/models/{name}", func(rw http.ResponseWriter, r *http.Request) {
		var m Model
		if !decode(rw, r, &m) {
			return
		}
		if err := store.SaveModel(r.PathValue("name"), m); err != nil {
			fail(rw, err)
			return
		}
		build.Outdated()
		reply(rw, m)
	})
	mux.HandleFunc("DELETE /api/models/{name}", func(rw http.ResponseWriter, r *http.Request) {
		if err := store.DeleteModel(r.PathValue("name")); err != nil {
			fail(rw, err)
			return
		}
		build.Outdated()
		reply(rw, map[string]bool{"deleted": true})
	})
	mux.HandleFunc("GET /api/gguf", func(rw http.ResponseWriter, r *http.Request) {
		files, err := store.GGUFs()
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, files)
	})
	mux.HandleFunc("PUT /api/warden", func(rw http.ResponseWriter, r *http.Request) {
		cfg := warden.DefaultConfig()
		if !decode(rw, r, &cfg) {
			return
		}
		if err := store.SaveWarden(cfg); err != nil {
			fail(rw, err)
			return
		}
		reply(rw, cfg)
	})
	mux.HandleFunc("GET /api/build", func(rw http.ResponseWriter, r *http.Request) {
		reply(rw, build.State())
	})
	mux.HandleFunc("POST /api/build", func(rw http.ResponseWriter, r *http.Request) {
		if err := build.Start(); err != nil {
			writeJSON(rw, http.StatusConflict, map[string]string{"detail": err.Error()})
			return
		}
		reply(rw, build.State())
	})
	mux.HandleFunc("GET /api/git", func(rw http.ResponseWriter, r *http.Request) {
		st, err := store.Git()
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, st)
	})
	mux.HandleFunc("POST /api/git/commit", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Message string `json:"message"`
		}
		if !decode(rw, r, &body) {
			return
		}
		hash, err := store.Commit(body.Message)
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]string{"commit": hash})
	})

	mux.Handle("/daemon/", daemonProxy(daemon))

	app, _ := fs.Sub(dist, "dist")
	files := http.FileServerFS(app)
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(rw, "not found", http.StatusNotFound)
			return
		}
		// A client-side route is the app's index.
		if _, err := fs.Stat(app, strings.TrimPrefix(r.URL.Path, "/")); r.URL.Path != "/" && err != nil {
			r.URL.Path = "/"
		}
		if _, err := fs.Stat(app, "index.html"); err != nil {
			http.Error(rw, "infermux-ui was built without its web app (webui/): build it with nix build", http.StatusNotFound)
			return
		}
		files.ServeHTTP(rw, r)
	})

	return guardUI(store, mux)
}

// daemonProxy passes /daemon/<path> to the daemon. The browser's Origin is
// dropped: the request is now infermux-ui's, which marks it with X-InferMux.
func daemonProxy(daemon *url.URL) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(daemon)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/daemon")
			pr.Out.URL.RawPath = ""
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
			pr.Out.Header.Set("X-InferMux", "infermux-ui")
		},
		FlushInterval: -1, // llama-swap's /api/events is a stream
	}
}

// guardUI answers only on a loopback name or one of the warden file's
// trusted_hosts, which a page rebinding its own DNS name to 127.0.0.1 cannot
// use, and takes a write only from its own origin with X-InferMux, which no
// other page can add without a refused preflight.
func guardUI(store *Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		trusted := store.TrustedHosts()
		if !warden.KnownHost(r.Host, trusted) {
			http.Error(rw, "infermux-ui answers on a loopback name or a trusted host only", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if refused := warden.SameOrigin(r, trusted); refused != "" {
				http.Error(rw, refused, http.StatusForbidden)
				return
			}
			if r.Header.Get("X-InferMux") == "" {
				http.Error(rw, "writes need the X-InferMux header", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(rw, r)
	})
}

func decode(rw http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return false
	}
	return true
}

func reply(rw http.ResponseWriter, v any) { writeJSON(rw, http.StatusOK, v) }

func fail(rw http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	if errors.Is(err, ErrNotFound) {
		status = http.StatusNotFound
	}
	writeJSON(rw, status, map[string]string{"detail": err.Error()})
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(v)
}
