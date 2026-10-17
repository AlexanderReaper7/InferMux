# InferMux

One endpoint for the local models, which gives the GPU back when something else needs it, and never kills the prompt the user just sent.

InferMux is [llama-swap](https://github.com/mostlygeek/llama-swap) with a GPU warden built in. llama-swap starts one model server per request on demand and swaps them one at a time, behind OpenAI and Anthropic compatible APIs. The warden watches the card through NVML. When a game or a ComfyUI job needs it, the warden unloads the model, cancels batch work, tells its consumers to pause, and frees an idle ComfyUI. ComfyUI itself runs as one of the models, kept up beside the others and freed before each of them starts ([0030](docs/decisions/0030-comfyui-runs-under-infermux.md)). Every client has a key, and the key says whether its requests are batch or the user's own. The user's own are let through, and the model is not unloaded under them. Two hosts each running InferMux serve each other's models, so one URL reaches all of them ([0006](docs/decisions/0006-two-hosts-keys-and-the-cloud.md)).

It was Episteme's `hostagent/` until 2026-09-13 ([0001](docs/decisions/0001-the-warden-decides-and-says-so.md)), a Windows program until 2026-09-28 ([0002](docs/decisions/0002-linux-nvml-router-unload-comfyui.md)), and llama-warden, a Python sidecar to llama.cpp's router, until 2026-10-03 ([0004](docs/decisions/0004-infermux-request-priority.md)).

## What is InferMux's and what is llama-swap's

InferMux is a fork of llama-swap that merges each upstream release rather than vendoring it, so most of the tree and most of the history are upstream's. InferMux's own code is `infermux.go`, `internal/warden/`, `internal/remote/`, `internal/failover/`, `internal/catalog/`, `internal/stats/`, `internal/stream/`, `internal/placement/`, `internal/configstore/`, `internal/cpurun/`, `internal/fetch/`, `internal/adapter/`, `internal/muxui/`, `internal/server/warden.go`, `infermux_config.go`, `cmd/infermux-ui/`, `cmd/infermux-adapter/`, `cmd/infermux-config/`, `cmd/infermux-cpu/`, `webui/`, `nix/`, `e2e/` and `docs/decisions/`. [AGENTS.md](AGENTS.md) has the full map.

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

`infermux-ui` is a separate binary that edits the one config, warden.yaml and the keys, shows the verdict, and has the controls. It needs the one config (see [One config for every host](#one-config-for-every-host)), and the module refuses `ui.enable` without `signedConfig.seed` and `ui.configKey`. With the module:

```nix
services.infermux = {
  configDir = "/home/alice/nixcfg/infermux";   # warden.yaml, outside the store
  signedConfig = {
    trustedKeys = [ "q6NnXhYc3E5gq0n2m8l0Z8m0g6Yy3eJmWm4mVq3Zr2Q= the user's config key" ];
    seed = "/home/alice/nixcfg/infermux/config";
  };
  settings.macros.llama-server = "${pkgs.llama-cpp}/bin/llama-server --host 127.0.0.1";
  ui = {
    enable = true;
    user = "alice";
    configKey = "/home/alice/.config/infermux/config-key.age";
    ggufDirs = [ "/srv/models" ];
  };
};
```

Then open http://127.0.0.1:5010. Its llama-swap tab frames llama-swap's own UI from infermux-ui's second listener, `ui.swapListen` (:5011), which passes everything to the daemon with the UI's key. A save of warden.yaml is validated with the daemon's loader first. The Changes tab commits the files to the repository they live in, when asked; it never pushes. On a host with no desktop session to show gpg's pinentry, `ui.commitPassphrase` makes Commit ask for the GPG key's passphrase and sign in gpg's loopback mode ([0008](docs/decisions/0008-the-zbox-has-its-own-ui.md)).

Both answer on loopback names only, plus the warden file's `trusted_hosts`. To reach them from another device, publish them with `tailscale serve --bg --https=5010 http://127.0.0.1:5010` (and 5001 and 5011) and add the node's tailnet name to `trusted_hosts`.

With `ui.kvKernels` set, the Config tab marks a model whose K-V cache pair has no compiled FlashAttention kernel, which llama.cpp runs by converting the cache to f16 on every decode step. `ui.prebuild` lets the UI run `nix build` on that installable as the user, ahead of the switch that puts the new kernels in use; its "Build now" button was on the model form, and no tab shows it since the form was removed ([TODO.md](TODO.md)).

The Keys tab makes, edits and revokes keys. With `ui.keySecrets`, a new key's plaintext goes to that sops file, so the tab can show it again; keys.yaml gets only its SHA-256. The sops file opens with the user's age identity at `ui.ageIdentity`, encrypted with `age -p`; the tab asks for its passphrase for Make key (after the first), Show and Revoke, and forgets it when the tab closes. `ui.daemonKeyFile` is the UI's own key for the daemon. The browser needs none for the UI or the llama-swap tab. Without `ui.swapListen` the tab frames the daemon, which asks with a Basic prompt (any user name, the key as password).

The Config tab's "Model from Hugging Face" reads a file's SHA-256 and size for `files:` from its LFS ETag, given `org/repo/file.gguf`. `ui.hfTokenFile` is a token for gated and private repos. The UI downloads nothing; a daemon fetches a missing file into `signedConfig.fetchDirs` (0022, 9).

The Performance tab shows each host's last 500 requests for the models it serves, kept in memory since the daemon started, and per model the median and 95th percentile of time to the first token, the prefill and decode rates, the prompt cache's share and drafts accepted ([0014](docs/decisions/0014-performance-measured-at-the-front-door.md)).

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

With `-control-socket` (the module's `controlGroup`), the verdict, the probe and the controls above also answer on a Unix socket without a key, for the members of `-control-group` (0024). `infermux-hold` uses it to clear the card before a game reads its VRAM budget:

```sh
infermux-hold -- gamescope -W 3840 -H 2160 -f -- %command%   # a Steam launch option
curl --unix-socket /run/infermux/control.sock http://infermux/warden/verdict
```

It pauses by hand, unloads, and waits up to `-timeout` (30 s) for InferMux to hold no VRAM; if the card is still not clear, typically because one of your requests is running, it refuses to start the command and says why in a notification. When the command exits it sets `auto`.

A key with `class: batch` makes its requests batch. While the verdict is pause such a request gets:

```
HTTP/1.1 503 Service Unavailable
Retry-After: 300
{"error":{"type":"gpu_yielded","message":"batch requests wait while the GPU is yielded: ComfyUI has 1 job queued"}}
```

## ComfyUI

ComfyUI runs as the model `comfyui_auto`, llama-swap's ComfyUI endpoint at `/comfyui/`, and keeps managing its own models ([0030](docs/decisions/0030-comfyui-runs-under-infermux.md)). With `beside: [comfyui_auto]` on its device in the one config it stays up whatever the device swaps:

- Before any other model starts on the GPU, an idle ComfyUI gets `/free`, and the start waits up to 10 s for NVML to show ComfyUI's VRAM back (`comfyui_unit`). A render in progress is left alone.
- A job through InferMux (`POST /comfyui/prompt` or `/comfyui/api/prompt`) yields the card and unloads the GPU's other models before it reaches ComfyUI, unless an interactive request is in flight on one.
- A yield never stops ComfyUI. While other work contends it gets `/free` as soon as its queue is empty, otherwise after `comfyui_idle_seconds`.
- A priority process, `/warden/unload` and `infermux-hold` clear its queue, interrupt its job and stop it. The off file stops it at once.

`-comfyui-listen 127.0.0.1:8188` (the module's `comfyuiListen`) serves ComfyUI at that address's root with no key, for `tailscale serve` to put a browser on. Every path there is ComfyUI's, so it reaches nothing else; the browser's name goes in `trusted_hosts`. On :5001, `/comfyui/` needs a key like any model.

## Streams

A WebSocket that names its model in the query reaches that model's server at the path and query it was sent with: `/v1/realtime?model=x` (OpenAI's realtime), `/v1/listen?model=x` (Deepgram's), `/asr?model=x` (WhisperLiveKit's). `/upstream/<model>/<path>` reaches any path of the model's server as before. Every frame passes through unchanged, binary ones included; InferMux does not translate between protocols. A chunked reply, such as `/v1/audio/speech`, and SSE pass through chunk by chunk.

A session counts as an interactive request in flight while text or binary frames cross, and for 10 s after the last one. After that it counts only through its last data frame, as a finished request does, so a tab left open does not keep the models on the card. A ping counts for nothing. Before the warden unloads the models, or cancels a batch session, it closes each session with 1013 (Try Again Later) and the reason; a config reload closes them with 1012 (Service Restart). A client should reconnect when it has something to send: a new session loads its model. `/warden/verdict` shows an open session in `in_flight` with `session.moving` and `session.idle_seconds`, and `/warden/requests` has one row per session once it ends ([0018](docs/decisions/0018-streams-pass-through-and-a-session-is-interactive-while-data-moves.md)).

## What a client learns about a model

Each local model in `/v1/models` has `meta.infermux`: `context_window`, `input_modalities`, `reasoning_efforts` and `default_effort`, derived from its command and its GGUF's chat template, never declared. Codex's own request, which carries `client_version`, gets the same models as a Codex catalog when the module's `codexPrompt` is set. [0007](docs/decisions/0007-model-settings-derived.md). A cloud peer's models get the same fields from the peer's own `/v1/models` in OpenRouter's format, read at most once an hour ([0010](docs/decisions/0010-a-peers-facts-from-its-own-list.md)).

## A client that cannot send a key

`infermux-adapter -listen <addr> -target <url> -key-file <file>` forwards every request to the target with the key from the file, replacing whatever credentials the client sent; the request's path is appended to the target's. `-route /ping=/health` sends one path to another path on the target's host. The NixOS module runs one per entry in `services.infermux.adapters`. Immich's ML URL is the reason: Immich reaches `/upstream/immich-ml/` through one, and its 30 s health check goes to `/health`, so it never starts the model ([0013](docs/decisions/0013-immich-ml-under-the-zbox-through-an-adapter.md)).

## The other host

Under the one config the other InferMux hosts are `infermux.yaml`'s `hosts`, and warden.yaml's `remote_key_file` holds this host's key for them. Without it, `remotes` in warden.yaml lists them, each with the file holding this host's key for it. That key's `allow` decides which of the other host's models this one learns. InferMux reads each one's `/v1/models` every 30 s and lists its models as `<host>/<model>`. A request for one is forwarded with the client's own key, so the other host applies that key's class and allow list. A host that stops answering keeps its models listed and fails a request for them at once with 502. Off the machine, traffic goes over HTTPS with `tailscale serve`.

Under the one config a model's `balance` and `failover` lanes decide where its requests go, with the place settings below. Without it, `failover_file` names a reloadable YAML file mapping any model to destinations in preference order. Bare names such as `[zbox, strix]` switch only on 502, 503 or 504. A destination can set `max_inflight` to send excess concurrent requests to the next host, `only_if_idle` to skip a GPU running a different model request, and `batch_only` to restrict a destination to batch keys. The destination counts and reserves requests atomically, including direct traffic and aliases. An idle loaded model may be swapped. Both hosts need overflow support. See [failover.example.yaml](failover.example.yaml) and [routing capacity](docs/kb/guides/routing/capacity-and-queues.md). Outside the one config, CPU models should be peers of independent CPU-only services so the GPU warden never unloads or gates them; under it, the warden gates each model by its own devices ([0019](docs/decisions/0019-model-overflow-uses-destination-capacity.md)).

## What it does on a yield

1. Cancels every batch request in flight, and closes every batch session with 1013. A job's request is paused instead (see Jobs).
2. Unloads every model but ComfyUI, once (see ComfyUI). If an interactive request is in flight or ended less than `interactive_recent_seconds` ago, the unload waits until it has been quiet that long; a session is in flight while it moves data. The sessions still open are closed with 1013 first. A resume forgives an unload still owed.
3. Sends one POST to every configured consumer, repeated every five minutes until it lands:

```json
{"action": "pause", "reason": "ComfyUI has 1 job queued",
 "since": "2026-09-28T18:04:11+00:00", "warden": "infermux"}
```

What a consumer does about that is the consumer's business. **The endpoint must be idempotent**: re-announcing `pause` to an already-paused consumer must not re-stamp when the pause began.

Nothing expires. A warden that dies while a consumer is paused leaves it paused, and that trade is argued in [0001](docs/decisions/0001-the-warden-decides-and-says-so.md#push-not-a-lease).

Under the one config the GPU and the CPU each have a verdict, and a yield refuses, cancels and unloads only the models on the device that yielded. A model's devices are the device its place names plus its `occupies`. The CPU yields when CPU time outside InferMux's cgroups reaches `cpu.busy_percent` of every core over `cpu.window_seconds`, while a `cpu.priority_processes` name runs, or when MemAvailable falls under `cpu.min_available_ram_mb`. A busy CPU refuses and cancels batch work on its models and unloads nothing. RAM under the floor unloads them once, after `interactive_recent_seconds` of quiet on the CPU's own models. A refusal names the device: `gpu_yielded` or `cpu_yielded`, with that device's `resume_quiet_seconds` as Retry-After. Consumers hear `pause` while either device is yielded, with the GPU's reason when both are, and a pause handed from one device to the other keeps its `since`. Pause and Resume by hand stay the GPU's, so a hand resume while the CPU is busy leaves the consumers paused. Status shows the CPU's card: the verdict, the foreign and own CPU time, RAM, and the busiest processes outside InferMux. The GPU warden reads NVML's first card only, so a model on another CUDA index is on no measured device, and the config check warns about it.

## Off, for a safety stop

With `-off-file` (the module's `offFile`), this host's local models are off while that file exists, and its text says why ([0025](docs/decisions/0025-a-safety-stop-takes-the-local-models-off.md)). InferMux reads it before its first request and then every second. While off:

- Every request for a local model, interactive or batch, gets 503 with `{"error":{"type":"off",...}}` and `resume_quiet_seconds` as Retry-After. Failover takes it to the model's next place.
- The ones in flight are cancelled, the user's own included, and a job's are paused and sent on through failover at once.
- Every model stops at once, without waiting for interactive quiet, and none starts: each start reads the file itself and is refused with the same 503, whoever asked for it, a GET to `/upstream/<model>/` and preload included. A start that began before the file stops at the next read.
- Another host's model and a cloud's pass, and so does a job following one.

Consumers hear nothing of it. `/warden/verdict` shows it as `off`, and Status as a notice with the file's text and since when. The zbox's quiet-hours controller writes the file at 22:00 and on a thermal pause, and removes it when inference resumes.

## Jobs

A first-party batch client hands InferMux a request as a job rather than sending it, and a yield then pauses it mid-reply instead of losing it ([0023](docs/decisions/0023-a-batch-request-is-a-job-paused-mid-inference.md)). The job keeps the text sent so far. When a place in the model's lanes takes it again, here after the resume or another host at once, InferMux sends the request again with that text as a prefilled assistant message (`continue_final_message`) and `max_tokens` cut by the tokens already sent. The client hears one reply.

```sh
imx -XPOST -H 'X-InferMux: cli' 127.0.0.1:5001/warden/jobs \
  -d '{"path": "/v1/chat/completions", "body": {"model": "qwen", "messages": [...]}}'   # 201, {"id": "job_...", ...}
imx -N 127.0.0.1:5001/warden/jobs/job_.../events     # SSE; ?after=<seq> or Last-Event-ID to reconnect
imx 127.0.0.1:5001/warden/jobs                       # this key's jobs
imx -XDELETE -H 'X-InferMux: cli' 127.0.0.1:5001/warden/jobs/job_...
```

An event is `state` (queued, running with the place, paused with the reason, done, failed with the answer, cancelled), `chunk` (an OpenAI stream chunk, unchanged), or `response` (the whole answer of an embedding or rerank). Chat and completions always stream. A tool call is sent only whole, so a pause inside one drops it and the model writes it again. An embedding or rerank is never stopped inside: a yield lets it finish, and a job is refused before it starts like any batch request. A job refused by every place waits for a device here to resume, and asks again on its own at its Retry-After, at most 30 s apart, one job per model and key at a time. Another host's yield cuts the stream, and the job continues from what it had after 2 s. Three failures in a row without a new token fail it.

A job belongs to the key that made it. It lives in memory and is kept 10 minutes after it ends. On SIGTERM InferMux first drains (below), then writes the unfinished ones, never their keys, to `-jobs-file` or `$STATE_DIRECTORY/jobs.json`, and reads them back and deletes the file at start. A job read back waits for its key's next request to `/warden/jobs`. Status counts the jobs and says why the paused ones are.

## A deploy's stop

On SIGTERM InferMux answers every new request 503 with `"type":"draining"` and `Retry-After: 5`, and gives the ones running up to 40 s to finish (0026). A job still running then is paused and written, and the rest are cut. With the off file there it drains too: off has refused and cancelled the local requests already. The unit is `KillMode=mixed` and `TimeoutStopSec=90`, so the SIGTERM is the daemon's alone and a stop can take up to 85 s.

## One config for every host

With `signedConfig.trustedKeys` set, one signed directory decides every host's models and places, instead of `models/`, `remotes` and `failover_file` ([0022](docs/decisions/0022-one-signed-config-placed-by-device.md)). `infermux.yaml` names the hosts and their devices, the clouds whose models every host offers as `<cloud>/<model>`, the `balance` lane each model runs in, the `failover` lane it falls back to when every balance place is down or refusing, and groups of models. `models/<name>.yaml` is one llama-swap model entry plus `files:`, each weight's SHA-256, and optionally `vram_mb`, the VRAM the model holds once serving: a start on the GPU then answers 503 `gpu_no_room` at once, for every key, while NVML's own free figure for the card is less ([0028](docs/decisions/0028-a-start-on-the-gpu-needs-room-on-the-card.md)). A device that swaps may name groups under `together`, such as `deep-search: [Qwen3.5-4B-Q4_K_M, jina-reranker-v3.5]`: starting one member stops no other member, any other model on the device stops them all, and starts on the GPU take turns so each reads the card with the last one on it ([0029](docs/decisions/0029-a-group-stays-loaded-together-on-a-device-that-swaps.md)). Each daemon derives its own part, keeps the versions under its state directory and pushes a new one to the others. warden.yaml then names this host as `host` and its key for the others as `remote_key_file`.

```sh
infermux-config keygen -out ~/.config/infermux/config-key.age   # prints the public key for trustedKeys
infermux-config sign -key ~/.config/infermux/config-key.age -host strix ~/nixcfg/infermux/config
```

```nix
services.infermux = {
  signedConfig = {
    trustedKeys = [ "q6NnXhYc3E5gq0n2m8l0Z8m0g6Yy3eJmWm4mVq3Zr2Q= the user's config key" ];
    seed = "/home/alice/nixcfg/infermux/config";   # nixcfg's copy, used when newer than the daemon's
    fetchDirs = [ "/srv/models" ];                  # where a model's missing file may be fetched to
    buildCheck = { config = ./infermux/config; host = "strix"; };   # the same copy, from the flake
  };
  environmentFile = "/run/secrets/rendered/infermux.env";   # the clouds' keys; the daemon's and the UI check's
  ui.configKey = "/home/alice/.config/infermux/config-key.age";
};
```

With `buildCheck`, the build fails when this host's part of that copy does not load with `settings`, as llama-swap loads the derived file with `-config` at start. The build has no environment, so it leaves out what reads `${env.VAR}` and what a YAML alias ties to it. It checks the copy, not a newer version the daemon keeps (0022, 11). At start, before llama-swap loads, the daemon moves a derived file that does not load with `-config` to `infermux.yaml.refused`, where llama-swap exited and restarted without end. It then runs no model of its own until a config derives, and `/warden/config/state` shows the kept version that did not as `rejected`, `from: start`.

The UI's Config tab edits the YAML with completion and hover from the schema, checks the draft with the daemons' own loader as it is typed, and shows where each model would run. It loads this host's part with this host's runtimes, so a model that names a runtime the host lacks fails the check and Publish. It loads with the daemon's `environmentFile` too, so a cloud whose `${env.VAR}` the file does not set fails the check, and an error that would quote a key's value shows `${env.VAR}` instead (0022, 11). The UI's user must be able to read the file. The UI reads it in a child process of its own, so the UI's environment, and git's and nix's, never hold the keys. Publish asks for the key's passphrase, signs the draft and hands it to this host's daemon. With `ui.configPassphraseCommand`, an executable that prints the passphrase, such as `secret-tool lookup service infermux-config` against the user's keyring, Publish asks for none, and whoever reaches that host's UI can sign a config every host runs. Once the daemon runs it, the UI writes nixcfg's copy, which Changes commits. A draft made from an older version than the one running gets a side-by-side comparison instead: build on the running version, or publish over it, which every host then shows as a conflict until it is dismissed.

## Develop

```sh
nix develop -c go test ./internal/... -short
(cd webui && bun test)
nix build                              # runs the tests as part of the build
e2e/run.sh                             # after nix build: both UIs end to end in a headless browser
```

[AGENTS.md](AGENTS.md) has the map of the code and how to merge a new llama-swap release.

## License

InferMux is licensed under the GNU Affero General Public License, version 3 only ([LICENSE](LICENSE)). Copyright (c) 2026 Alexander Öberg.

It includes llama-swap, Copyright (c) 2024 Benson Wong, under the MIT license ([LICENSE.md](LICENSE.md)). The upstream code stays available under MIT.
