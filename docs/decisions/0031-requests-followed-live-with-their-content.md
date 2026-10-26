# 0031. Requests followed live with their content, in InferMux's own handler

- Date: 2026-10-19
- Amended on 2026-10-27 (item 6): Clear content wipes every body this host holds.
- Status: accepted; built and tested 2026-10-19, deployed with InferMux a7ee4dc on strix at 21:31 and the zbox at 23:05 CET on 2026-10-26 (Verification).
- Keeps 0004 (no upstream file changes), 0006 (behind the same key check as the rest of `/warden/`), 0009 (only this host's models) and 0014 (the same place in the chain as the stats).

## Context

The user, 2026-10-19: "i want to watch requests in realtime, with thier full content."

What llama-swap 262 already had:

- Captures. `internal/server/metrics_middleware.go` buffers each request's body and headers and copies the reply, and stores the pair, zstd-compressed, in a ring of `captureBuffer` MB (5 by default, `internal/config/load.go`) once the reply has ended. `GET /api/captures/{id}` and the Activity page read it. A reply is readable only after its last byte: minutes, for an agent's long turn.
- The in-flight list. `/api/events` sends each running request every 250 ms with its model, path, headers, `resp_bytes` and `elapsed_ms`. It carries no body.

The agent offered two ways: extend upstream's in-flight tracker and Activity page, or a handler of InferMux's own with a page in `webui/`. The first edits `metrics_middleware.go`, `inflight.go` and llama-swap's UI, a merge conflict every release unless upstream takes it.

## Decision

The user, 2026-10-19: "2, add active request tab." InferMux's own handler, and a tab in infermux-ui.

1. **`internal/live` sits beside the stats recorder**, inside the warden's key check: `w.Wrap(board.Wrap(recorder.Wrap(...)))`. It sees the same requests the stats record: a POST for a model this host serves, ComfyUI's excepted. A request for another host's model is shown on that host.
2. **`GET /warden/live` is server-sent events**: a `snapshot` of every entry on connect, then `start` (the request, its body included), `head` (status and type), `chunk` (the reply's next text, as written) and `end`. A follower more than 4096 events behind is cut rather than waited for; its EventSource reconnects and gets a new snapshot. A request never waits for a follower.
3. **The last 20 finished requests stay on the board** beside the running ones, so a request shorter than the page's reconnect is still there to read.
4. **4 MB of each body is kept**, request and reply apart, and a body that is not text is listed by type and size only: audio, images and multipart uploads. A character split between two writes is sent whole with the second.
5. **The Active tab** lists the requests newest first and shows one: the request's messages and the reply's reasoning, text and tool calls, read from Chat Completions, the Responses API, Anthropic's Messages and llama-server's `/completion` (`webui/src/lib/livetext.ts`), or either body raw.

6. **Clear content wipes every request and reply body this host holds in memory.** The user, 2026-10-27: "add a clear button to wipe anything stored about the content itself beyond anonymised metrics like size, time." `POST /warden/live/wipe`, behind the key check and `X-InferMux` like the other `/warden/` writes, is the Active tab's Clear content. It empties the board's bodies, finished and running. A running request keeps none of the reply it writes after the wipe, and its client still gets the whole reply. It also clears llama-swap's capture ring through `Server.ClearCaptures` (`internal/server/warden.go`). Each entry keeps its time, model, key name, path, content types, byte counts, status and duration. A `wiped` event blanks what every following page kept, and a follower that connects later gets the last wipe after its snapshot, so a page that was reconnecting drops its copies too.
   - Nothing else holds content. The stats (0014) and llama-swap's activity log, the one table on disk, keep sizes, tokens and timings. On 2026-10-26 the journal of 21:40 to 21:45 held 537 lines and not one word of a chat sent then. llama-server writes a prompt only under `--verbose`, and no model in the signed config has it.
   - Not wiped, by the agent's choice: a job's text (0023), the job's own state for as long as it runs and for 10 minutes after it ends, for its client to fetch; and a request running at the click, which llama-swap captures once it ends. The wipe is this host's only; the zbox has its own.

Who may follow it is the agent's choice, not yet the user's: any key, as for every other `GET /warden/` and as llama-swap's `/api/captures` already allows. A key that can read this reads every client's prompts.

Rejected: extending llama-swap's in-flight tracker (the merge cost above); a WebSocket through `internal/stream` (the page only reads, and EventSource reconnects on its own); reading the bodies from llama-swap's captures (only after the end).

## Verification

- 2026-10-19, tests: `internal/live` (a streamed reply's chunk is seen while the backend still holds the rest; finished entries kept to `Keep`; only this host's model POSTs; binary bodies sized, not kept, and still reaching the backend whole; bodies cut at the limit; a split UTF-8 character; a follower that reads nothing does not hold a request), `TestLiveShowsARequestWhileItRuns` through the daemon's own chain with llama-swap, and `webui/test/livetext.test.ts`.
- 2026-10-19, `e2e/run.sh run.py` against the nix-built binaries, headless Chromium at 2560x1300: the Active tab showed a streamed chat to the fake model with its key and its message `hi`, read `w0 w1` and `streaming` while the client had not finished, and ended with all 40 tokens and 200.
- 2026-10-26, `e2e/run.sh` against the nix-built binaries: 75 and 41 checks passed, none failed, the Active tab's included. The check older than 0024, 4 that had Unload all refused while a reply ran now has it cut the interactive stream mid-reply and stop the model.
- 2026-10-26 21:42 CET, the deployed daemon on strix: `curl -N` on infermux-ui's `/daemon/warden/live` while a streamed chat for `Qwen3.5-4B-Q4_K_M` with 40 tokens ran through the same proxy. The follower got a `snapshot`, a `start` with the request's body, a `head`, 41 `chunk` events and an `end` with 200 and 12601 bytes in 6.7 s, the model's start included.
- 2026-10-27, the wipe, tests: `TestLive_AWipeDropsEveryBodyAndKeepsTheSizes` (a finished and a running request lose their bodies and keep sizes and status, the running one's later reply is counted and not kept while its client gets all of it, a follower hears `wiped`, a later follower gets it after its snapshot, a request after the wipe is kept) and `TestAWipeDropsTheBoardsBodiesAndLlamaSwapsCaptures` through the daemon's chain (the capture readable, then 404, the activity row kept with `has_capture` false, 403 without `X-InferMux`). Three mutations each failed them: no `Clear` of the captures, a wiped running request still kept, the finished ones not wiped. `e2e/run.sh run.py`, 77 checks passed: Clear content left both panes reading Cleared., the row its 200 and size, and the daemon's board no body.
- Not yet watched: the tab in the user's browser, and a reply with reasoning or tool calls.
