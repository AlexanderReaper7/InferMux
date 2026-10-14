# 0023. A batch request is a job that InferMux pauses mid-inference and resumes

- Date: 2026-10-10
- Status: built 2026-10-11 (`internal/jobs`), running on both hosts since 2026-10-14 (`/warden/jobs` answers); Episteme does not send jobs yet
- Would replace [0001](0001-the-warden-decides-and-says-so.md)'s pause and resume pushed to consumers, for first-party consumers. Replaces item 4 of [0022](0022-one-signed-config-placed-by-device.md)'s "The warden covers every device". Keeps [0004](0004-infermux-request-priority.md): the user's own request is never killed, and now never paused.

## Context

Since 0001 the warden pushes `pause` and `resume` to its consumers, and a consumer may or may not respect it. InferMux enforces only the edges: a batch request is refused with 503 during a pause and cancelled on a yield, and the work it had done is lost. 0001 states the residual risk: a pause is a message, not a lease, so a warden that dies while a consumer is paused leaves it paused.

The user, 2026-10-10: pause at the request level instead, which may mean a protocol of our own. Keep the OpenAI-compatible protocol and have first-party consumers use ours. Pause a request mid-inference rather than wait for it to finish, and keep its state on the SSD or in RAM.

## What llama.cpp already does

Read in v0.6.0's server, the version strix runs (`tools/server/`):

- `POST /slots/{id}?action=save` writes a slot's tokens and KV cache to a file under `--slot-save-path`, and `action=restore` loads it. A save is deferred while the slot is generating (`server-context.cpp:2758`), so a pause stops the generation first and saves after.
- `continue_final_message` (`server-common.cpp:1364`) continues an unfinished final assistant message, in its reasoning (`"reasoning_content"`) or its content (`"content"`).

So a pause stops the generation, keeps the text streamed so far, saves the slot and unloads. A resume loads the model, restores the slot, and sends the original messages plus the partial assistant message with `continue_final_message`. The restored cache covers that prefix, and the client receives the rest of the same response.

The text is the state and the KV file is a cache of it. Without the file the same continuation works and processes the prompt again first.

## Decision

The user's choices, 2026-10-10:

1. **First-party consumers use a job envelope of our own.** A job wraps any OpenAI request body (chat, completions, embeddings). Creating one returns an id. Its event stream carries state changes (queued, running at a place, paused with the reason, done) and the OpenAI chunks unchanged, each with a sequence number. A client reconnects after any number and cancels by id. Rejected: OpenAI's Responses background mode (`background`, `stream`, resume with `starting_after`, cancel), a standard with SDK support but no embeddings, which are most of the batch work, and chat clients would have to move to the Responses format; holding the OpenAI request open with keep-alives while paused, which breaks on a client timeout, a dropped connection or a daemon restart, and a client cannot tell paused from stuck.
2. **Only batch work is paused.** The user's own requests are never paused, as 0004 never kills them. A yield pauses batch jobs at once; interactive requests run on, and the unload waits for them as today. Rejected: pausing the user's own request too when a priority process such as OBS needs the card, which 0017 makes wait for that request.
3. **A paused job resumes at the first place in its lanes that can take it** (0022's `balance`, then `failover`). The saved KV file is used on the same place; anywhere else the text is replayed. Rejected: the same place only, which holds a job for a whole game session.
4. **A job lives in memory and goes to disk only when it has to, on a shutdown signal.** On `SIGTERM` (a deploy, a `systemctl restart`, a switch) the daemon pauses every batch job, writes the jobs and their KV files under `/var/lib/infermux`, and exits; on start it reads them back and deletes them. A config reload does not restart the daemon (0005, 4), so its jobs stay in memory: the reload pauses batch jobs, and waits only for interactive requests. The user's first answer was to write every job through to `/var/lib/infermux` as it changed; changed the same day: "lets not unnecessarily write to disk, only write state to disk when its needed, like when receiving a shutdown signal." Rejected: writing through on every change; memory only, which a deploy would empty.

## Proposed by the agent, agreed by the user the same day

- *Dropped after the measurement below: the restored cache saves nothing on these models.* **A paused job's KV cache goes to tmpfs, which is RAM.** llama-server saves a slot only to a file under `--slot-save-path`, and the model process that holds the cache is about to stop, so the save is needed; the disk is not. The path is the daemon's `RuntimeDirectory`, `/run/infermux`, on strix a 23 GB tmpfs. It reaches the SSD only with the jobs, at a shutdown signal. The file is a cache of the text, so under 0022's RAM floor the warden deletes KV files before it unloads a model, and those jobs replay their text instead. (First proposed as a file on the SSD, left to the page cache; replaced by item 4.)
- **An embedding request is held before it starts, never paused inside.** It is one forward pass, seconds long.
- *Replaced while building, see the calls below: llama-server will not continue a message that has `tool_calls`, so a tool call is held back and sent whole, and a pause drops it.* **A tool call is finished before a pause.** Continuing inside half-written tool-call JSON may not parse.
- **OpenAI-protocol batch clients keep today's behaviour,** refused with 503 and cancelled on a yield. InferMux enforces both, so neither depends on the client.
- **Performance figures (0014) leave out paused time,** so a paused job's time to its first token is not counted as slow.

## Measured 2026-10-10

On strix's CPU, llama.cpp v0.6.0's server, Qwythos-9B (a Qwen3.5 hybrid with Gated DeltaNet layers), one slot, 8192 context, under a load average of about 51 from a Chromium build. The script is `~/Projects/scratch/imx-pause/pause.py`, the logs beside it.

- **A cancelled generation keeps its tokens in the slot.** The stream was cut after 30 deltas; the slot released with 144 tokens, the 114 of the prompt and the 30 generated, and the save wrote all 144.
- **The partial reply tokenizes back to exactly the tokens generated.** The continuation, the messages plus the 169 characters of reasoning with `continue_final_message: "reasoning_content"`, matched the restored slot at `f_sim_best = 1.000, f_keep = 1.000`, and the text went on mid-sentence ("I need to make" then " sure I stay true to ..."). The request needs `add_generation_prompt: false`; llama-server refuses both set.
- **The restored cache saved nothing.** The continuation after the restore reported `cache_n 0, prompt_n 144`, the same as from an erased slot. With every token matching, the server has to evaluate the last one again for its logits, which means removing it from the cache, and a recurrent layer cannot remove one token. So it processed the whole prompt again. All of the user's chat models are Qwen3.5 or 3.6 hybrids.
- **Size and time:** 54 MB for 144 tokens, most of it the recurrent state, which does not grow with the context. The save took 43 ms and the restore 11 ms, but the save waited 10 s for the cancelled generation to stop under that load.

## Measured 2026-10-11

llama.cpp v0.6.0's server on strix's CPU, Qwen3.5-0.8B-Q8_0 and then Qwen3.5-4B-Q4_K_M, temperature 0. The scripts are in `~/Projects/scratch/imx-jobs/`.

- **A cut in the content continues to the same text.** The 0.8B without thinking, ten rivers one line each, cut after 10 tokens: 273 more, and the 283 joined equal the uncut reply. The 4B with thinking, "What is 17 times 23?", cut in the content: 399 tokens and 8 more against 409 uncut, the same text.
- **A cut inside a `json_schema` reply continues to valid JSON.** Cut after 8 deltas at `{\n  "rivers": [`; the joined text parses and equals the uncut one.
- **A cut in the reasoning continues to a different but coherent reasoning.** The 0.8B cut after 10 reasoning deltas: different reasoning, the same answer. No text repeats or goes missing at the cut. The continuation evaluates the text so far as one prompt batch, and that gives slightly different numbers from generating it token by token, so a near tie can go the other way at temperature 0.
- **The assistant message needs `content`.** One with only `reasoning_content` got 400 "Assistant message must contain either 'content' or 'tool_calls'!". `content: ""` is accepted.
- **A streamed continuation sends only the new text**, never the prefix again.
- **llama-server will not continue a message that has `tool_calls`.**
- **Through InferMux** (`live.py`, the nix-built daemon in front of the 4B, paused by `/warden/manual`; uncut: 946 characters of reasoning, then "17 times 23 equals 391."):
  - Paused at content chunk 5: the joined reasoning and content equal the uncut ones. Three attempts: the one paused, one refused while the hand pause held, and the one after `auto`.
  - Paused at reasoning chunk 40 (131 characters in): the reasoning is identical up to character 456, then takes another path to the same content.
  - SIGTERM at reasoning chunk 60 (182 characters in): exit 0, `jobs.json` 20 316 bytes, mode 0600, without the key. After the restart the job was paused "InferMux restarted; waiting for its client to come back with its key", went on when the client asked for it, and ended "17 times 23 is 391." The reasoning differs from character 193, at about the cut.

## Measured 2026-10-14

OpenRouter, the one config's only cloud, through strix. The nemotron stream is `internal/jobs/testdata/openrouter-nemotron.sse`.

- **OpenRouter streams the reasoning as `reasoning`**, with the same text again in `reasoning_details`; `nvidia/nemotron-3-ultra-550b-a55b:free` and `openrouter/free` (routed to `dots-studio/dots-3-note-preview:free`) both did. Both llama-server builds the config runs, llama.cpp 0.6.0 and PrismML's fork for Bonsai, have only `reasoning_content`. A job reads either name, llama-server's first.
- **No free model tested continued a prefill as llama-server does**, though OpenRouter documents assistant prefill without naming a model that lacks it. Each accepted the continuation with 200. In the reasoning (`continue_final_message: "reasoning_content"`, or `"reasoning"` with the reasoning under that name), `nemotron-3-ultra` and `openrouter/free` both thought again from the start. In the content, `nemotron-3-ultra`, served by Nvidia, reads the partial reply as an earlier turn, thinks again, and then chooses: after "17 × 3 =" it answered "340 + 51 = **391**.", and after "1, 2, 3, 4, 5, 6, 7," in a count to 20 it went on from 8 in five calls of five, with and without the job's `continue_final_message`, its reasoning saying it continues "from where the previous response left off". `openrouter/free`, routed to `nvidia/nemotron-3.5-lightning:free`, thought again until `max_tokens`. On the count, `liquid/lfm-2.5-2.6b:free` and `cohere/north-mini-code:free` started again from 1, and Gemma 4's and Poolside's free models answered 429. None of the 14 free models is Anthropic's, whose API documents prefill, and the account, on the free tier with no credits, cannot call one.

## Calls the agent made while building, for the user to review

- **Pause and resume carry the text only; no KV file in the first build.** The measurement above shows the restore buys nothing on the models that are paused. Dropped with it: `--slot-save-path`, the tmpfs under `/run/infermux`, and deleting KV files under the RAM floor. On `SIGTERM` the jobs' text is all that is written. A plain transformer would gain from the file; it can be added for one when one is paused.
- **A tool call is held, not continued.** From the first `tool_calls` delta to `finish_reason` the chunks are kept back and then sent together. A pause in between drops them, and the next attempt starts from the text before the call. llama-server would not continue the call (above).
- **`/warden/jobs` sits behind the key check** like any `/warden/*` route, writes need `X-InferMux` as the other writes do, and a job belongs to the key that made it: another key gets 404 for it.
- **An attempt is an in-process request through the failover handler**, with the client's `Authorization` and `X-Api-Key`, so the lanes, the gate and the stats see it as any request. `warden.WithPauser` on its context makes a yield or a cancel by hand call the job instead of cancelling the request.
- **The key is never written to disk.** A job read back after a restart waits, with no limit, until its client's next `/warden/jobs` request brings the key. Without `keys_file` it goes on at once.
- **A cancel by hand holds a job for the GPU's `resume_quiet_seconds`**, what `Retry-After` tells any other client, and a resume does not cut the hold short. A yield holds nothing, since another place may take the job.
- **The continuation:** the assistant message always carries `content`, `""` if nothing was said yet. A client's own trailing assistant message is merged with the reply so far. `max_tokens`, `max_completion_tokens` and `n_predict` go down by one per text chunk received, to at least 1.
- **A `json_schema` reply is continued like any other**, on the one measurement above.
- **A chat or completion attempt is always streamed**, whatever the client asked for. The events carry the chunks either way.
- **Refused at creation:** `n` other than 1, a completion's prompt that is not one string, chat without messages, no model, and any path other than chat, completions, `/v1/embeddings`, `/v1/rerank`, `/v1/reranking` and `/rerank`.
- **An embedding or rerank is never paused inside**; its whole answer arrives as one `response` event.
- **Refused jobs line up per model and key.** The first asks again at the smaller of `Retry-After` and 30 s; the others wait behind it. The line goes when one is admitted, on the warden's resume (`OnResume`), and when the one config's places change.
- **A 5xx or a broken stream backs off 2 s, then 4 s,** and the job fails after three in a row with no new text. A 4xx fails it at once. Another host's yield reaches a job as a broken stream.
- **Every answer through failover names its place** in `X-InferMux-Place` (`host/model`), which the job shows as `running at`.
- **A second hook in `llama-swap.go`:** `startWarden` returns a stop, called first in the SIGINT and SIGTERM branch. It waits up to 10 s for the attempts to stop. That wait comes out of upstream's 35 s backstop, so in the worst case llama-swap's own drain gets 25 s of its 30.
- **`jobs.json` is `-jobs-file` or `$STATE_DIRECTORY/jobs.json`** (`/var/lib/infermux` under the module), mode 0600, written to a temporary file and renamed. A file that cannot be read is moved to `jobs.json.unreadable`. With neither set, the shutdown logs how many jobs are lost.
- **A finished job stays 10 minutes** for a client that reconnects. **At 10 000 unfinished jobs a new one gets 429.**

## Smells left in the first build

- A job's events have no byte limit. A long reply is kept chunk by chunk until the job has been finished for 10 minutes.
- The verdict's `cancelled_batch` counts a paused job's attempt as cancelled.
- The `usage` chunk a job relays covers its last attempt only.
- A job continued at a cloud place repeats itself at the models measured on 2026-10-14: none continued a prefill as llama-server does, so a client hears the cut reply, then new reasoning, and then in the content a continuation or a new reply, as the model chooses.
- Each pause logs Go's "suppressing panic for copyResponse error in test". An in-process request has no `http.ServerContextKey`, so `httputil.ReverseProxy` returns where a served request would panic with `http.ErrAbortHandler`. Setting the key would make the attempt take the panic path, but `stats.Recorder.Wrap` adds its row after `next` returns rather than in a defer, so the paused attempt would vanish from `/warden/requests`. That is already true of every request whose client disconnects mid-stream. The real fix is both: the recorder records in a defer, and the attempt carries the key.

## Consequences

- Once Episteme sends jobs, the warden stops pushing to consumers. `consumers` in `warden.yaml`, and CLAUDE.md's rules "A pause is a message, not a lease" and "An announcement must be idempotent at the other end", retire with it, and so does 0001's residual risk: a held job leaves no state at the consumer.
- Episteme changes its client to the job envelope. That is work in Episteme's repository.
- A crash, an OOM kill, a `SIGKILL` or a power cut loses the jobs in memory, since only a shutdown signal writes them. The consumer finds its job id unknown and submits the request again. A job's text up to the crash is lost with it.
- Resuming at another place needs only the lanes, which the one config's `balance` and `failover` provide (0022).
