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
//	/daemon/...  the daemon, with X-InferMux and the UI's own key added
//	/            the web app
//
// swapPort is SwapHandler's port, which the llama-swap tab frames; empty
// frames the daemon itself, which asks for a key.
func Handler(store *Store, build *Builder, daemon *url.URL, daemonKey, swapPort string) http.Handler {
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
		peers, err := store.Peers()
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]any{
			"models":     models,
			"peers":      peers,
			"runtimes":   runtimes,
			"warden":     wcfg,
			"kv_kernels": store.KVKernels,
			"hf":         store.HF != nil,
			// The browser reaches llama-swap's own UI on this port, on whatever
			// host it reached infermux-ui: loopback, or the tailnet name.
			"daemon_port": daemon.Port(),
			"swap_port":   swapPort,
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
	mux.HandleFunc("GET /api/downloads", func(rw http.ResponseWriter, r *http.Request) {
		if store.HF == nil {
			reply(rw, []Download{})
			return
		}
		reply(rw, store.HF.State())
	})
	mux.HandleFunc("PUT /api/peers/{name}", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Models []string `json:"models"`
		}
		if !decode(rw, r, &body) {
			return
		}
		if err := store.SavePeerModels(r.PathValue("name"), body.Models); err != nil {
			fail(rw, err)
			return
		}
		reply(rw, body)
	})
	mux.HandleFunc("GET /api/routing", func(rw http.ResponseWriter, r *http.Request) {
		text, err := store.Routing()
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]string{"file": RoutingFile, "text": text})
	})
	mux.HandleFunc("PUT /api/routing", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if !decode(rw, r, &body) {
			return
		}
		if err := store.SaveRouting(body.Text); err != nil {
			fail(rw, err)
			return
		}
		reply(rw, body)
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
	mux.HandleFunc("GET /api/keys", func(rw http.ResponseWriter, r *http.Request) {
		st, err := store.Keys()
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, st)
	})
	mux.HandleFunc("POST /api/keys/{name}", func(rw http.ResponseWriter, r *http.Request) {
		var body keyRequest
		if !decode(rw, r, &body) {
			return
		}
		key, err := store.CreateKey(r.PathValue("name"), body.Key, body.Passphrase)
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]string{"key": key})
	})
	mux.HandleFunc("PUT /api/keys/{name}", func(rw http.ResponseWriter, r *http.Request) {
		var k warden.Key
		if !decode(rw, r, &k) {
			return
		}
		if err := store.UpdateKey(r.PathValue("name"), k); err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]bool{"saved": true})
	})
	mux.HandleFunc("DELETE /api/keys/{name}", func(rw http.ResponseWriter, r *http.Request) {
		var body passphrase
		if !decode(rw, r, &body) {
			return
		}
		if err := store.DeleteKey(r.PathValue("name"), body.Passphrase); err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]bool{"deleted": true})
	})
	// A POST, though it changes nothing: a write needs X-InferMux from the
	// UI's own origin, and a key should need no less.
	mux.HandleFunc("POST /api/keys/{name}/reveal", func(rw http.ResponseWriter, r *http.Request) {
		var body passphrase
		if !decode(rw, r, &body) {
			return
		}
		key, err := store.RevealKey(r.PathValue("name"), body.Passphrase)
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]string{"key": key})
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
			Message    string `json:"message"`
			Passphrase string `json:"passphrase"`
		}
		if !decode(rw, r, &body) {
			return
		}
		hash, err := store.Commit(body.Message, body.Passphrase)
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]string{"commit": hash})
	})

	mux.Handle("/daemon/", daemonProxy(daemon, daemonKey))

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
// dropped: the request is now infermux-ui's, which marks it with X-InferMux
// and sends its own key (0006, 4). The browser brings no key to the UI.
func daemonProxy(daemon *url.URL, key string) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(daemon)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/daemon")
			pr.Out.URL.RawPath = ""
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
			pr.Out.Header.Set("X-InferMux", "infermux-ui")
			for _, h := range []string{"Authorization", "X-Api-Key"} {
				pr.Out.Header.Del(h)
			}
			if key != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+key)
			}
		},
		FlushInterval: -1, // llama-swap's /api/events is a stream
	}
}

// SwapHandler is llama-swap's own UI without a key prompt: every request
// goes to the daemon with the UI's key, as /daemon/ does. It is a listener of
// its own because that UI fetches absolute paths, /api/ among them, which
// are this UI's. The browser gets the daemon's guard here, since the daemon
// sees only this process: a known Host, and a write or WebSocket from this
// origin only. No X-InferMux is added, so /warden/ writes stay refused, as
// they are to a browser on the daemon.
func SwapHandler(store *Store, daemon *url.URL, key string) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(daemon)
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
			for _, h := range []string{"Authorization", "X-Api-Key"} {
				pr.Out.Header.Del(h)
			}
			if key != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+key)
			}
		},
		FlushInterval: -1,
	}
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		trusted := store.TrustedHosts()
		if !warden.KnownHost(r.Host, trusted) {
			http.Error(rw, "infermux-ui answers on a loopback name or a trusted host only", http.StatusMisdirectedRequest)
			return
		}
		write := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if write || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			if refused := warden.SameOrigin(r, trusted); refused != "" {
				http.Error(rw, refused, http.StatusForbidden)
				return
			}
		}
		proxy.ServeHTTP(rw, r)
	})
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

// passphrase is the age identity's passphrase, sent with an operation that
// opens the sops file and dropped when it returns.
type passphrase struct {
	Passphrase string `json:"passphrase"`
}

type keyRequest struct {
	warden.Key
	passphrase
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
