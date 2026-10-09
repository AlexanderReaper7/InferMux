# 0021. Overflow never takes a yielded card

- Date: 2026-10-09
- Status: accepted
- Amends [0019](0019-model-overflow-uses-destination-capacity.md). Keeps [0017](0017-a-priority-process-takes-the-card.md).

On 2026-10-09 at 13:08 the warden yielded strix's card to Warframe and unloaded the 35B. At 13:10 `semsearch-index-code.timer` started, the zbox answered 503 at `max_inflight: 1`, and `failover.yaml` overflowed the embedder to strix's `only_if_idle` place. `only_if_idle` only looked for other InferMux model requests, not for a game on the card. The `semsearch` key is `class: interactive`, so the warden's pause did not refuse it either. Bulk indexing loaded 5.8 GB under the game, and the timer would repeat it every 15 minutes while the zbox was busy.

The user was offered three fixes and chose both halves:

1. A batch key for indexing. Indexing is refused during a pause and overflows to `embed-cpu`, while queries keep the interactive key. This needs the client to send a different key for indexing, which hister's `document_api_key` does.
2. `only_if_idle` also respects the warden's yield. `Admission.begin` asks `Warden.Yielded()` and refuses an only-if-idle request for this host's GPU, whatever its key, with busy reason `yielded`. Failover moves on to the next place.

Either half alone leaves a gap. The key alone fixes SemSearch but not the next interactive client that bulk-loads through overflow. The admission check alone covers every client, but an indexer with an interactive key still competes with queries at the zbox.

The cost of 2 is accepted: while a game holds the card, an interactive embedding query gets no GPU fallback on strix and waits at the zbox or fails. Directly addressed requests are untouched; they remain the warden gate's decision (0017). Only overflow, which by definition had somewhere else to go, is refused.

Verified: `TestAdmission_OnlyIfIdleRefusedWhileYielded`, and `TestProxy_OverflowSkipsYieldedGPU`, which failed before the fix with `after pause got 200 "gpu"`.
