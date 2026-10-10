# 0023. A batch request is a job that InferMux pauses mid-inference and resumes

- Date: 2026-10-10
- Status: proposed; nothing built
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
- **A tool call is finished before a pause.** Continuing inside half-written tool-call JSON may not parse.
- **OpenAI-protocol batch clients keep today's behaviour,** refused with 503 and cancelled on a yield. InferMux enforces both, so neither depends on the client.
- **Performance figures (0014) leave out paused time,** so a paused job's time to its first token is not counted as slow.

## Measured 2026-10-10

On strix's CPU, llama.cpp v0.6.0's server, Qwythos-9B (a Qwen3.5 hybrid with Gated DeltaNet layers), one slot, 8192 context, under a load average of about 51 from a Chromium build. The script is `~/Projects/scratch/imx-pause/pause.py`, the logs beside it.

- **A cancelled generation keeps its tokens in the slot.** The stream was cut after 30 deltas; the slot released with 144 tokens, the 114 of the prompt and the 30 generated, and the save wrote all 144.
- **The partial reply tokenizes back to exactly the tokens generated.** The continuation, the messages plus the 169 characters of reasoning with `continue_final_message: "reasoning_content"`, matched the restored slot at `f_sim_best = 1.000, f_keep = 1.000`, and the text went on mid-sentence ("I need to make" then " sure I stay true to ..."). The request needs `add_generation_prompt: false`; llama-server refuses both set.
- **The restored cache saved nothing.** The continuation after the restore reported `cache_n 0, prompt_n 144`, the same as from an erased slot. With every token matching, the server has to evaluate the last one again for its logits, which means removing it from the cache, and a recurrent layer cannot remove one token. So it processed the whole prompt again. All of the user's chat models are Qwen3.5 or 3.6 hybrids.
- **Size and time:** 54 MB for 144 tokens, most of it the recurrent state, which does not grow with the context. The save took 43 ms and the restore 11 ms, but the save waited 10 s for the cancelled generation to stop under that load.

## Calls the agent made while building, for the user to review

- **Pause and resume carry the text only; no KV file in the first build.** The measurement above shows the restore buys nothing on the models that are paused. Dropped with it: `--slot-save-path`, the tmpfs under `/run/infermux`, and deleting KV files under the RAM floor. On `SIGTERM` the jobs' text is all that is written. A plain transformer would gain from the file; it can be added for one when one is paused.

## Consequences

- Once Episteme sends jobs, the warden stops pushing to consumers. `consumers` in `warden.yaml`, and CLAUDE.md's rules "A pause is a message, not a lease" and "An announcement must be idempotent at the other end", retire with it, and so does 0001's residual risk: a held job leaves no state at the consumer.
- Episteme changes its client to the job envelope. That is work in Episteme's repository.
- A crash, an OOM kill, a `SIGKILL` or a power cut loses the jobs in memory, since only a shutdown signal writes them. The consumer finds its job id unknown and submits the request again. A job's text up to the crash is lost with it.
- Resuming at another place needs only the lanes, which `failover.yaml` provides today, so this does not have to wait for 0022.
