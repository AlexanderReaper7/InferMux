# 0013. Immich ML runs under the zbox's InferMux, and an adapter gives Immich its key

- Date: 2026-10-03
- Status: accepted
- Rule: A client that cannot send a key reaches InferMux through `infermux-adapter`, one instance per client, which forwards to a `-target` with the client's key from `-key-file` in place of any credentials it sent. `-route /from=/to` sends one path elsewhere on the target's host. Immich's ML service is a local model, `immich-ml`, on the zbox; its health check goes to the daemon's `/health` and never starts it.
- Builds 0006, 11, and closes its two open items about Immich.

## Context

0006, 11 put Immich's machine-learning service under the zbox's InferMux, so the GTX 1060 has one scheduler: natively from nixpkgs at the server's tag, reached as `/upstream/immich-ml/predict`, its requests interactive. Two things were left open there. Immich's ML URL takes no headers, so it cannot send a key, and 0006, 4 refuses a keyless request. And `internal/server/inflight.go` tracks `/upstream/` only for inference paths, which `/predict` is not.

Until now Immich ML ran as the `-cuda` container in the zbox's immich stack, always up, unloading its models after 120 s idle.

## Decision

1. **An adapter, a Go binary in this repository** (the user's choice, 2026-10-03, over a key in the URL, an allow-list of keyless addresses, or a proxy in the compose stack). `cmd/infermux-adapter` is a reverse proxy configured by flags; the NixOS module runs one `infermux-adapter-<name>` unit per entry in `services.infermux.adapters`, with the key passed by `LoadCredential`. On the zbox it listens on the immich stack's bridge gateway, `172.20.0.1:3003`, which the compose file pins together with the bridge name `br-immich`, and only that interface's firewall opens the port. Anything on that network can use the key, and the key may use only `immich-ml`.
2. **Immich's `/ping` goes to the daemon's `/health`** (the user's choice, over turning Immich's availability checks off or the adapter answering by itself). The Immich server pings its ML URL every 30 s. Through InferMux that would start the model and keep it loaded for good, which defeats running it on demand. Health then means "InferMux answers", not "the ML process is up"; with one URL Immich tries it either way (`machine-learning.repository.ts`). Turning the checks off would have needed no code, but lives in Immich's database, out of Git, and a reset would bring the pings back unnoticed.
3. **The model.** `hosts/zbox/infermux/models/immich-ml.yaml` in nixcfg: the nixpkgs recipe at v3.2.4, onnxruntime built with CUDA 12.9 for capability 6.1 (plus 7.5, without which its CUDA provider fails to load), and cuDNN pinned to 9.10.2, the last release with Pascal kernels and what Immich's own image pins (nixcfg `packages/immich-machine-learning`). InferMux's `ttl: 300` stops the whole process after five idle minutes, so Immich's own unload is off. The models it downloads stay in the daemon's cache directory.
4. **The never-kill rule needs nothing new for `/predict`.** The tracker in `inflight.go` feeds llama-swap's in-flight panel and its Cancel button, nothing else. What could stop a running request all count it: a swap waits for the router's granted requests, which `/upstream/` goes through; the TTL waits for the process's own count of requests through `ServeHTTP`; and the warden tracks every POST as interactive, so its unload waits for `interactive_recent_seconds` of quiet (0004). `/predict` does not show in llama-swap's panel.

## Consequences

- The first request after five idle minutes waits for Python, CUDA and the model to load before Immich gets an answer. Immich retries a failed job later rather than losing it.
- A swap between `immich-ml` and a second local model on the zbox would wait for in-flight `/predict` requests, by point 4. There is no second local model yet.
- The immich key has a copy in `secrets/infermux.yaml` only, not in the Keys tab's file, like the codex key: the agent cannot write the user's passphrase-protected file. Show in the Keys tab does not find it.
- The old container's `model-cache` volume stays on the zbox until removed by hand.
