# 0014. Each host times the requests it serves, in memory, and the UI shows both hosts

- Date: 2026-10-04
- Status: accepted
- Rule: `internal/stats` records every POST for a model this host serves, a local one or a cloud peer's, as it passes through InferMux: time to the first generated token as the client sees it, and llama-server's own `timings`. The last 500 stay in the daemon's memory. `GET /warden/requests` is this host's, and `?hosts=all` adds every other host's, asked for with this host's key at that moment.

## Context

The user asked for tok/s, time to the first token and the prefill rate in the UI. llama-swap already has an activity log (`/api/metrics/activity`), but it records no time to the first token, drops llama-server's `prompt_ms`, and builds its entries inside upstream's proxy code, so extending it would be an upstream edit we pay at every merge (0004).

## Decision

1. **A new tab and two columns in the Models table** (the user's choice, 2026-10-04). The Performance tab has, per host, each model's median and 95th percentile and the recent requests. The Models table shows this host's median TTFT and decode rate beside each model.
2. **TTFT is split** (the user's choice). TTFT runs from the request's arrival to the first event that carries text, reasoning or a tool call, so it includes a model load, a swap and the queue. Prefill is llama-server's `prompt_ms`. *Wait* is TTFT minus prefill, which is the time a request spent on anything but its prompt. A reply that is not streamed has no first token, only its total.
3. **Memory only** (the user's choice, over a file in the cache directory). A daemon restart or a deploy empties it. A llama-swap reload does not, because the recorder sits outside the replaced server, in `infermux.go`.
4. **Both hosts** (the user's choice). Each host records what it serves, so a request from the zbox for `reaperboi/<model>` is recorded on reaperboi, once. The tab asks its own daemon for `?hosts=all`, which reads every other host's `/warden/requests` with the host key (0006, 4) and a 5 s limit. A host that does not answer is shown with its error.
5. **The rates.** When llama-server sends `timings`, the prefill and decode rates are its own, the prompt is `prompt_n` plus `cache_n`, and drafts accepted come from `draft_n_accepted`. A cloud peer sends none, so its decode rate is the output tokens after the first, over the time after the first token, measured here and marked with `*`.
6. **What is counted.** A request the warden refuses (401, 403, 503) never reaches the recorder, so it says nothing about the model. A failed one that reached the model is listed and counted as failed, but its numbers stay out of the summary. The summary's cache share and draft acceptance are ratios of sums, so a long request weighs what it cost.

7. **Bytes, to decide compression on data** (the user's choice, 2026-10-06). Each request records its request and reply body in bytes, uncompressed and without headers. Nothing between the hosts compresses today. Over the tailnet 1 MB took 75 ms to the zbox and 28 ms through this host's own `tailscale serve`, against seconds of prefill for a prompt that size, so compression was not worth building on estimates. If the agents' turns between hosts turn out large, these numbers say so.

## Consequences

- Any key can read `/warden/requests`, as it can `/warden/verdict`. The list names the key behind each request and its token counts, never its content.
- A reply that is not streamed and is larger than 8 MB, such as a long embedding batch, is listed without its timings.
- History ends at the last daemon restart. If trends across days are wanted, that is a store on disk, which point 3 declined for now.
