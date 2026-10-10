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

## Proposed by the agent, not yet chosen

- **A paused job's KV cache goes to tmpfs, which is RAM.** llama-server saves a slot only to a file under `--slot-save-path`, and the model process that holds the cache is about to stop, so the save is needed; the disk is not. The path is the daemon's `RuntimeDirectory`, `/run/infermux`, on strix a 23 GB tmpfs. It reaches the SSD only with the jobs, at a shutdown signal. The file is a cache of the text, so under 0022's RAM floor the warden deletes KV files before it unloads a model, and those jobs replay their text instead. (First proposed as a file on the SSD, left to the page cache; replaced by item 4.)
- **An embedding request is held before it starts, never paused inside.** It is one forward pass, seconds long.
- **A tool call is finished before a pause.** Continuing inside half-written tool-call JSON may not parse.
- **OpenAI-protocol batch clients keep today's behaviour,** refused with 503 and cancelled on a yield. InferMux enforces both, so neither depends on the client.
- **Performance figures (0014) leave out paused time,** so a paused job's time to its first token is not counted as slow.

## To measure before building

- Whether a cancelled generation leaves its generated tokens in the slot, so the save holds the prompt and the partial reply. Not read in the source yet.
- Whether the partial reply, rendered back through the chat template, tokenizes to exactly the tokens that were generated. On a plain transformer a mismatch costs the tokens after it. Qwen3.6 has linear-attention layers that cannot roll back, so there one mismatched token means processing the whole prompt again.
- The save and restore time and file size per model, at the context lengths Episteme sends. The size decides how many paused jobs `/run` holds; the time, whether a shutdown fits in `infermux.service`'s 90 s `TimeoutStopSec`.

## Consequences

- Once Episteme sends jobs, the warden stops pushing to consumers. `consumers` in `warden.yaml`, and CLAUDE.md's rules "A pause is a message, not a lease" and "An announcement must be idempotent at the other end", retire with it, and so does 0001's residual risk: a held job leaves no state at the consumer.
- Episteme changes its client to the job envelope. That is work in Episteme's repository.
- A crash, an OOM kill, a `SIGKILL` or a power cut loses the jobs in memory, since only a shutdown signal writes them. The consumer finds its job id unknown and submits the request again. A job's text up to the crash is lost with it.
- Resuming at another place needs only the lanes, which `failover.yaml` provides today, so this does not have to wait for 0022.
