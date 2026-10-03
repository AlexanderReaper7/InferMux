# InferMux

One endpoint for the local models, which gives the GPU back when something else needs it, and never kills the prompt the user just sent.

InferMux is [llama-swap](https://github.com/mostlygeek/llama-swap) with a GPU warden built in. llama-swap starts one model server per request on demand and swaps them one at a time, behind OpenAI and Anthropic compatible APIs. The warden watches the card through NVML. When a game or a ComfyUI job needs it, the warden unloads the model, cancels batch work, tells its consumers to pause, and frees an idle ComfyUI. Every client has a key, and the key says whether its requests are batch or the user's own. The user's own are let through, and the model is not unloaded under them. Two hosts each running InferMux serve each other's models, so one URL reaches all of them ([0006](docs/decisions/0006-two-hosts-keys-and-the-cloud.md)).

It was Episteme's `hostagent/` until 2026-09-13 ([0001](docs/decisions/0001-the-warden-decides-and-says-so.md)), a Windows program until 2026-09-28 ([0002](docs/decisions/0002-linux-nvml-router-unload-comfyui.md)), and llama-warden, a Python sidecar to llama.cpp's router, until 2026-10-03 ([0004](docs/decisions/0004-infermux-request-priority.md)).

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

Then open http://127.0.0.1:5010. Its llama-swap tab frames llama-swap's own UI from the daemon's port. A model's command is edited as runtime, GGUF and flags when it has the form `${runtime} --port ${PORT} --model <file> <flags>`, and as text otherwise. Every save is validated with llama-swap's loader first. The Changes tab commits the files to the repository they live in, when asked; it never pushes.

Both answer on loopback names only, plus the warden file's `trusted_hosts`. To reach them from another device, publish them with `tailscale serve --bg --https=5010 http://127.0.0.1:5010` (and 5001) and add the node's tailnet name to `trusted_hosts`.

With `ui.kvKernels` set, the UI marks a model whose K-V cache pair has no compiled FlashAttention kernel, which llama.cpp runs by converting the cache to f16 on every decode step. `ui.prebuild` adds a "Build now" button that runs `nix build` on that installable as the user, ahead of the switch that puts the new kernels in use.

The Keys tab makes, edits and revokes keys. With `ui.keySecrets`, a new key's plaintext goes to that sops file, so the tab can show it again; keys.yaml gets only its SHA-256. `ui.daemonKeyFile` is the UI's own key for the daemon. The browser needs none for the UI; it does for the llama-swap tab, which asks with a Basic prompt (any user name, the key as password).

From a checkout, `cd webui && npm install && npm run dev` serves the UI on :5173 against an `infermux-ui` on :5010.

## Talk to it

Loopback :5001. With a `keys_file`, every request but `/health` needs a key, as `Authorization: Bearer <key>`, `x-api-key: <key>`, Basic's password, or, for a browser's WebSocket, the subprotocol `openai-insecure-api-key.<key>`. No key or an unknown one gets 401; a model outside the key's `allow` list gets 403. See [keys.example.yaml](keys.example.yaml).

```sh
alias imx='curl -H "Authorization: Bearer $INFERMUX_KEY"'
imx 127.0.0.1:5001/v1/models           # llama-swap: every configured model
imx 127.0.0.1:5001/running             # llama-swap: which are up
imx 127.0.0.1:5001/warden/verdict      # what it decided, who has heard it, what is in flight
imx 127.0.0.1:5001/warden/resources    # a fresh NVML probe, on purpose

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

## The other host

`remotes` in warden.yaml lists the other InferMux hosts, each with the file holding this host's key for it. InferMux reads each one's `/v1/models` every 30 s and lists its models as `<host>/<model>`. A request for one is forwarded with the client's own key, so the other host applies that key's class and allow list. A host that stops answering keeps its models listed and fails a request for them at once with 502. Off the machine, traffic goes over HTTPS with `tailscale serve`.

## What it does on a yield

1. Cancels every batch request in flight.
2. Unloads every model, once. If an interactive request is in flight or ended less than `interactive_recent_seconds` ago, the unload waits until it has been quiet that long. A resume forgives an unload still owed.
3. Sends one POST to every configured consumer, repeated every five minutes until it lands:

```json
{"action": "pause", "reason": "ComfyUI has 1 job queued",
 "since": "2026-09-28T18:04:11+00:00", "warden": "infermux"}
```

What a consumer does about that is the consumer's business. **The endpoint must be idempotent**: re-announcing `pause` to an already-paused consumer must not re-stamp when the pause began.

Nothing expires. A warden that dies while a consumer is paused leaves it paused, and that trade is argued in [0001](docs/decisions/0001-the-warden-decides-and-says-so.md#push-not-a-lease).

## Develop

```sh
nix develop -c go test ./internal/warden/ ./internal/remote/ ./internal/muxui/
nix develop -c go test -short ./internal/server/ .
nix build                              # runs the tests as part of the build
```

The warden is `internal/warden/` and `infermux.go`, the routing to the other host `internal/remote/`, the UI is `internal/muxui/`, `cmd/infermux-ui/` and `webui/`; the rest is upstream llama-swap, merged rather than vendored. [CLAUDE.md](CLAUDE.md) has the map and how to merge a new llama-swap release.

## License

MIT, as llama-swap ([LICENSE.md](LICENSE.md)).
