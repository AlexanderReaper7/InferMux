# InferMux

One endpoint for the local models, which gives the GPU back when something else needs it, and never kills the prompt the user just sent.

InferMux is [llama-swap](https://github.com/mostlygeek/llama-swap) with a GPU warden built in. llama-swap starts one model server per request on demand and swaps them one at a time, behind OpenAI and Anthropic compatible APIs. The warden watches the card through NVML. When a game or a ComfyUI job needs it, the warden unloads the model, cancels batch work, tells its consumers to pause, and frees an idle ComfyUI. Every client has a key, and the key says whether its requests are batch or the user's own. The user's own are let through, and the model is not unloaded under them. Two hosts each running InferMux serve each other's models, so one URL reaches all of them ([0006](docs/decisions/0006-two-hosts-keys-and-the-cloud.md)).

It was Episteme's `hostagent/` until 2026-09-13 ([0001](docs/decisions/0001-the-warden-decides-and-says-so.md)), a Windows program until 2026-09-28 ([0002](docs/decisions/0002-linux-nvml-router-unload-comfyui.md)), and llama-warden, a Python sidecar to llama.cpp's router, until 2026-10-03 ([0004](docs/decisions/0004-infermux-request-priority.md)).

## What is InferMux's and what is llama-swap's

InferMux is a fork of llama-swap that merges each upstream release rather than vendoring it, so most of the tree and most of the history are upstream's. InferMux's own code is `infermux.go`, `internal/warden/`, `internal/remote/`, `internal/failover/`, `internal/catalog/`, `internal/stats/`, `internal/stream/`, `internal/adapter/`, `internal/muxui/`, `internal/server/warden.go`, `cmd/infermux-ui/`, `cmd/infermux-adapter/`, `webui/`, `nix/`, `e2e/` and `docs/decisions/`. [CLAUDE.md](CLAUDE.md) has the full map.

The Go module path is still `github.com/mostlygeek/llama-swap` for the same reason. Renaming it would rewrite every upstream import, and every later merge would conflict on all of them.

## Run it

On NixOS, through the flake's module:

```nix
# flake inputs
infermux = {
  url = "git+https://github.com/AlexanderReaper7/InferMux";
  inputs.nixpkgs.follows = "nixpkgs";
};

# a NixOS module
imports = [ inputs.infermux.nixosModules.default ];
services.infermux = {
  enable = true;
  listen = "127.0.0.1:5001";
  settings.models.qwen.cmd = "llama-server --port \${PORT} --model /srv/models/qwen.gguf";
  warden = {
    comfyui_url = "http://127.0.0.1:8188";
    keys_file = "/etc/infermux/keys.yaml";
    consumers = [ { name = "episteme"; url = "http://127.0.0.1:8200"; } ];
  };
};
```

From a checkout:

```sh
nix develop -c go run . -config config.yaml -warden-config warden.example.yaml -listen 127.0.0.1:5001
journalctl -u infermux -f              # the service's log
```

## Configure it

Two files. `-config` is llama-swap's own: models, groups, TTLs. See [config.example.yaml](config.example.yaml). `-warden-config` is the warden's: see [warden.example.yaml](warden.example.yaml), where every key is optional and documented. Without `-warden-config` InferMux is plain llama-swap.

`-config-dir` adds every `*.yaml` in a directory to `-config`, one model per file. InferMux watches all three. A change to the warden's file applies at once; a change to llama-swap's config reloads it once no interactive request is in flight, because a reload stops every model ([0005](docs/decisions/0005-web-ui-and-config-outside-nix.md)).

## The web UI

`infermux-ui` is a separate binary that edits those files, shows the verdict, and has the controls. With the module:

```nix
services.infermux = {
  configDir = "/home/alice/nixcfg/infermux";   # models/*.yaml and warden.yaml, outside the store
  settings.macros.llama-server = "${pkgs.llama-cpp}/bin/llama-server --host 127.0.0.1";
  ui = { enable = true; user = "alice"; ggufDirs = [ "/srv/models" ]; };
};
```

Then open http://127.0.0.1:5010. Its llama-swap tab frames llama-swap's own UI from infermux-ui's second listener, `ui.swapListen` (:5011), which passes everything to the daemon with the UI's key. A model's command is edited as runtime, GGUF and flags when it has the form `${runtime} --port ${PORT} --model <file> <flags>`, and as text otherwise. Every save is validated with llama-swap's loader first. The Changes tab commits the files to the repository they live in, when asked; it never pushes. On a host with no desktop session to show gpg's pinentry, `ui.commitPassphrase` makes Commit ask for the GPG key's passphrase and sign in gpg's loopback mode ([0008](docs/decisions/0008-the-zbox-has-its-own-ui.md)).

Both answer on loopback names only, plus the warden file's `trusted_hosts`. To reach them from another device, publish them with `tailscale serve --bg --https=5010 http://127.0.0.1:5010` (and 5001 and 5011) and add the node's tailnet name to `trusted_hosts`.

With `ui.kvKernels` set, the UI marks a model whose K-V cache pair has no compiled FlashAttention kernel, which llama.cpp runs by converting the cache to f16 on every decode step. `ui.prebuild` adds a "Build now" button that runs `nix build` on that installable as the user, ahead of the switch that puts the new kernels in use.

The Keys tab makes, edits and revokes keys. With `ui.keySecrets`, a new key's plaintext goes to that sops file, so the tab can show it again; keys.yaml gets only its SHA-256. The sops file opens with the user's age identity at `ui.ageIdentity`, encrypted with `age -p`; the tab asks for its passphrase for Make key (after the first), Show and Revoke, and forgets it when the tab closes. `ui.daemonKeyFile` is the UI's own key for the daemon. The browser needs none for the UI or the llama-swap tab. Without `ui.swapListen` the tab frames the daemon, which asks with a Basic prompt (any user name, the key as password).

A models file may hold llama-swap `peers:` instead, such as OpenRouter with `apiKey: ${env.OPENROUTER_API_KEY}`. The Models tab edits each peer's model list, and the proxy and key stay as written. The check fills every `${env.NAME}` with a placeholder, because the daemon's environment is not the UI's. A models file that is a symlink is written through, so two hosts' directories can share one peers file, and Changes shows and commits the file it points to.

The Models tab also edits `routing.yaml` in the models directory as text: llama-swap's `routing.router`, groups or a matrix, saying which models may run at once. A save is checked like a model's, and a group member that names no model is refused (0006, 9).

With `ui.hfDir`, a model can take its files from Hugging Face: `org/repo/file.gguf` for the GGUF and for its mmproj, kept in the model file as `metadata.hf`. Saving writes the paths under `ui.hfDir` into `--model` and `--mmproj` and downloads from the repository's `main`, again on a later save only if main has changed; the Models tab shows the progress, and an interrupted download resumes. `ui.hfTokenFile` is a token for gated and private repos ([0012](docs/decisions/0012-a-model-names-its-gguf-on-hugging-face.md)).

The Performance tab shows each host's last 500 requests for the models it serves, kept in memory since the daemon started, and per model the median and 95th percentile of time to the first token, the prefill and decode rates, the prompt cache's share and drafts accepted. The Models table shows the median time to the first token and decode rate beside each model ([0014](docs/decisions/0014-performance-measured-at-the-front-door.md)).

From a checkout, `cd webui && npm install && npm run dev` serves the UI on :5173 against an `infermux-ui` on :5010.

## Talk to it

Loopback :5001. With a `keys_file`, every request but `/health` needs a key, as `Authorization: Bearer <key>`, `x-api-key: <key>`, Basic's password, or, for a browser's WebSocket, the subprotocol `openai-insecure-api-key.<key>`. No key or an unknown one gets 401; a model outside the key's `allow` list gets 403, and `/v1/models` leaves it out. See [keys.example.yaml](keys.example.yaml).

```sh
alias imx='curl -H "Authorization: Bearer $INFERMUX_KEY"'
imx 127.0.0.1:5001/v1/models           # llama-swap: every configured model
imx 127.0.0.1:5001/running             # llama-swap: which are up
imx 127.0.0.1:5001/warden/verdict      # what it decided, who has heard it, what is in flight
imx 127.0.0.1:5001/warden/resources    # a fresh NVML probe, on purpose
imx '127.0.0.1:5001/warden/requests?hosts=all'   # the last requests and their timings, every host's

# controls; a write needs the X-InferMux header
imx -XPOST -H 'X-InferMux: cli' 127.0.0.1:5001/warden/manual -d '{"action":"pause"}'   # or resume, auto
imx -XPOST -H 'X-InferMux: cli' 127.0.0.1:5001/warden/unload         # 409 while a request of yours runs
imx -XPOST -H 'X-InferMux: cli' 127.0.0.1:5001/warden/forgive        # drop an owed unload
imx -XPOST -H 'X-InferMux: cli' 127.0.0.1:5001/warden/cancel-batch
imx -XPOST -H 'X-InferMux: cli' 127.0.0.1:5001/warden/comfyui/free
```

A manual pause or resume holds until the warden's own verdict changes, then the warden takes over again. A browser POST from another origin, or to a name that is neither loopback nor in `trusted_hosts`, gets 403.

A key with `class: batch` makes its requests batch. While the verdict is pause such a request gets:

```
HTTP/1.1 503 Service Unavailable
Retry-After: 300
{"error":{"type":"gpu_yielded","message":"batch requests wait while the GPU is yielded: ComfyUI has 1 job queued"}}
```

## Streams

A WebSocket that names its model in the query reaches that model's server at the path and query it was sent with: `/v1/realtime?model=x` (OpenAI's realtime), `/v1/listen?model=x` (Deepgram's), `/asr?model=x` (WhisperLiveKit's). `/upstream/<model>/<path>` reaches any path of the model's server as before. Every frame passes through unchanged, binary ones included; InferMux does not translate between protocols. A chunked reply, such as `/v1/audio/speech`, and SSE pass through chunk by chunk.

A session counts as an interactive request in flight while text or binary frames cross, and for 10 s after the last one. After that it counts only through its last data frame, as a finished request does, so a tab left open does not keep the models on the card. A ping counts for nothing. Before the warden unloads the models, or cancels a batch session, it closes each session with 1013 (Try Again Later) and the reason; a config reload closes them with 1012 (Service Restart). A client should reconnect when it has something to send: a new session loads its model. `/warden/verdict` shows an open session in `in_flight` with `session.moving` and `session.idle_seconds`, and `/warden/requests` has one row per session once it ends ([0018](docs/decisions/0018-streams-pass-through-and-a-session-is-interactive-while-data-moves.md)).

## What a client learns about a model

Each local model in `/v1/models` has `meta.infermux`: `context_window`, `input_modalities`, `reasoning_efforts` and `default_effort`, derived from its command and its GGUF's chat template, never declared. Codex's own request, which carries `client_version`, gets the same models as a Codex catalog when the module's `codexPrompt` is set. [0007](docs/decisions/0007-model-settings-derived.md). A cloud peer's models get the same fields from the peer's own `/v1/models` in OpenRouter's format, read at most once an hour ([0010](docs/decisions/0010-a-peers-facts-from-its-own-list.md)).

## A client that cannot send a key

`infermux-adapter -listen <addr> -target <url> -key-file <file>` forwards every request to the target with the key from the file, replacing whatever credentials the client sent; the request's path is appended to the target's. `-route /ping=/health` sends one path to another path on the target's host. The NixOS module runs one per entry in `services.infermux.adapters`. Immich's ML URL is the reason: Immich reaches `/upstream/immich-ml/` through one, and its 30 s health check goes to `/health`, so it never starts the model ([0013](docs/decisions/0013-immich-ml-under-the-zbox-through-an-adapter.md)).

## The other host

`remotes` in warden.yaml lists the other InferMux hosts, each with the file holding this host's key for it. That key's `allow` decides which of the other host's models this one learns. InferMux reads each one's `/v1/models` every 30 s and lists its models as `<host>/<model>`. A request for one is forwarded with the client's own key, so the other host applies that key's class and allow list. A host that stops answering keeps its models listed and fails a request for them at once with 502. Off the machine, traffic goes over HTTPS with `tailscale serve`.

## What it does on a yield

1. Cancels every batch request in flight, and closes every batch session with 1013.
2. Unloads every model, once. If an interactive request is in flight or ended less than `interactive_recent_seconds` ago, the unload waits until it has been quiet that long; a session is in flight while it moves data. The sessions still open are closed with 1013 first. A resume forgives an unload still owed.
3. Sends one POST to every configured consumer, repeated every five minutes until it lands:

```json
{"action": "pause", "reason": "ComfyUI has 1 job queued",
 "since": "2026-09-28T18:04:11+00:00", "warden": "infermux"}
```

What a consumer does about that is the consumer's business. **The endpoint must be idempotent**: re-announcing `pause` to an already-paused consumer must not re-stamp when the pause began.

Nothing expires. A warden that dies while a consumer is paused leaves it paused, and that trade is argued in [0001](docs/decisions/0001-the-warden-decides-and-says-so.md#push-not-a-lease).

## Develop

```sh
nix develop -c go test ./internal/warden/ ./internal/remote/ ./internal/catalog/ ./internal/muxui/ ./internal/adapter/ ./internal/stats/
nix develop -c go test -short ./internal/server/ .
nix build                              # runs the tests as part of the build
```

[CLAUDE.md](CLAUDE.md) has the map of the code and how to merge a new llama-swap release.

## License

InferMux is licensed under the GNU Affero General Public License, version 3 only ([LICENSE](LICENSE)). Copyright (c) 2026 Alexander Öberg.

It includes llama-swap, Copyright (c) 2024 Benson Wong, under the MIT license ([LICENSE.md](LICENSE.md)). The upstream code stays available under MIT.
