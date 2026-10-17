package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/warden"
)

// ComfyUI as one of InferMux's models (0030). llama-swap serves it as
// comfyui_auto at /comfyui/. The ComfyUI listener serves that endpoint at its
// own root, so the browser at https://comfyui.java-tint.ts.net reaches it
// through Tailscale's serve with the paths ComfyUI's frontend asks for, and
// without a key: the user's decision of 2026-10-17, no login.

var flagComfyUIListen = flag.String("comfyui-listen", "", "InferMux: a loopback address that serves ComfyUI's /comfyui/ at its root with no key, for Tailscale's serve (0030)")

// serveComfyUI listens on addr, which must be a loopback address, and serves
// h there under /comfyui until the returned stop is called.
func serveComfyUI(addr string, h http.Handler) (stop func(), err error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("%s is not a loopback address, and the ComfyUI listener takes no key", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: comfyUIRoot(h)}
	go server.Serve(ln)
	return func() { server.Close() }, nil
}

// comfyUIRoot puts /comfyui in front of every path, so the listener reaches
// ComfyUI and nothing else, and marks the request for the warden, which gives
// it a key allowed only ComfyUI's model.
func comfyUIRoot(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		r = warden.FromComfyUIListener(r)
		r.URL.Path = "/comfyui" + r.URL.Path
		if r.URL.RawPath != "" {
			r.URL.RawPath = "/comfyui" + r.URL.RawPath
		}
		h.ServeHTTP(rw, r)
	})
}

// isComfyUIPath is a request for llama-swap's ComfyUI endpoint.
func isComfyUIPath(r *http.Request) bool {
	return r.URL.Path == "/comfyui" || strings.HasPrefix(r.URL.Path, "/comfyui/")
}

// qualifyComfyUI names ComfyUI's model when this host's llama-swap serves it,
// and is not known otherwise: llama-swap's endpoint answers 404 for a
// comfyui_auto it does not run itself, a peer's or an alias.
func (a activeModels) qualifyComfyUI() (string, bool, bool) {
	peer, model, ok := a.server().QualifyModel(config.ComfyUIModelID)
	if !ok || peer != "" || model != config.ComfyUIModelID {
		return "", false, false
	}
	return a.host + "/" + model, true, true
}
