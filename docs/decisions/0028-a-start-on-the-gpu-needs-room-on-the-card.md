# 0028. A start on the GPU needs room on the card

- Date: 2026-10-15
- Status: accepted; built 2026-10-15, running on both hosts; in the signed config `drama-3` has `vram_mb: 6200` since revision 5 and `jina-reranker-v3.5` 3000 since revision 7, both of 2026-10-16 (Verification, below)
- Amended on 2026-10-16 by the user: starts on the GPU take turns, so a second one reads the card once the first has ended ([0029](0029-a-group-stays-loaded-together-on-a-device-that-swaps.md)).
- Amended on 2026-10-17 by the agent, on the aide's request to stop a start loop: a start on the GPU that stopped before it was ready holds the model's next start there for 60 s while the card has no more free than it had then (item 6). `Octen-Embedding-4B.Q8_0` has `vram_mb: 5900` since revision 12.
- Amends [0004](0004-infermux-request-priority.md): a third exception to "the user's own request is never killed". Keeps [0025](0025-a-safety-stop-takes-the-local-models-off.md)'s start gate, which this check is part of, [0016](0016-a-model-fails-over-between-places.md) (a 503 fails over) and [0002](0002-linux-nvml-router-unload-comfyui.md)'s free VRAM rule for the verdict.

## Context

strix's reranker, jina-reranker-v3.5 on `strix/gpu` only, has 1.14 GB of weights and held about 3.1 GB once serving when measured on 2026-10-06, 2.8 GB at most on 2026-10-16 (its model file in nixcfg). The agent's report on it for decision 17 found the starts that failed under Warframe: other work held 9,009 to 9,176 MB of the card's 10,240 MB, and the reranker's server ran 4.9 to 8.8 s before CUDA could not allocate, with 62 MiB free. hister falls back to its unreranked order when the reranker does not answer, so each of those starts cost its search the wait and nothing else.

NVML's process list does not show all of what holds the card. By the user's figure, about 2.2 GB of dma-bufs is in no process's number, so a sum of the list reads more free than the card has.

## Decision

The agent offered options for the reranker. Option 2 was a fit check: a VRAM need on a model, and a start on a GPU answered with 503 at once, for every key, while other work leaves less than that. The user, 2026-10-15, relayed by the coordinating session: "17: option 2, the fit check. It has to read the card's own free VRAM figure, not a sum of per-process numbers, which miss the roughly 2.2 GB of dma-bufs and would let starts through that then fail. The user accepts that some interactive requests get an immediate 503 (0004)."

1. **A model file may say `vram_mb`**: the VRAM the model holds once serving, in MB, a whole number above 0. It is InferMux's and signed with the config. Derivation removes it from the entry llama-swap loads, and each instance of the model on this host carries it to the warden. The check warns about a model with `vram_mb` and no place on a GPU, since only a start there reads it.
2. **The check is part of the start gate** (0025, item 7), after the off file, so it covers every start of a local model on the GPU NVML reads: a request with any key or none, a GET to `/upstream/`, and a swap. A model with no `vram_mb`, or not on that GPU, is not checked and the card is not read.
3. **The figure is the card's own**: NVML's device memory, total less used, read fresh by the warden's second probe, which does not move the loop's utilization window (`internal/warden/fit.go`). 0002's "free VRAM is only read while no GPU model is loaded" is about the verdict and stays. At the gate a swap has already stopped the models it evicts: llama-swap's swap waits for each stop, and a stop answers once the process has exited. So their VRAM counts as free, and a model still loaded on a GPU that runs several at once counts as held, which it is.
4. **A start with less free than the need is refused at once**: 503, `"type":"gpu_no_room"`, "<model> needs N MB of VRAM and the card has F MB free", and the GPU's `resume_quiet_seconds` as Retry-After. llama-swap writes it whole to whoever asked, and the model stays stopped. Failover tries the model's next place (0016). The reranker has none, so hister gets the 503. A WARN names the need, the free figure and what held the card, the line the failed start left before.
5. **This is a third exception to 0004**, the user's: an interactive request is refused at once. The start it replaces failed after 4.9 to 8.8 s.
6. **A start that failed is not run again into the same card.** When a start on the GPU stops before it is ready, the warden's fresh probe for the WARN of 0016, 3 also records the card's free figure. For 60 s from then, the model's next start on the GPU is refused at once, for every key, while the card has no more free than that: 503, `"type":"gpu_start_failed"`, "<model> stopped before it was ready with F MB free, and the card has G MB free", with the hold's remaining seconds as Retry-After, and a WARN naming what holds the card. Failover tries the next place (0016). More free than at the failure, the end of the 60 s, or a start of the model that becomes ready ends the hold. It works with no `vram_mb` and with one set too low. The agent's call of 2026-10-17, from the aide's request: Octen on strix, with no `vram_mb`, failed its start beside a 4.5 GB desktop with 5,682 MB free about once a second from 01:46 to 11:00 CEST, about 1,780 failed starts, each a request to the zbox's balance place that found it at its limit. A start that failed for a reason the free figure does not show, such as a bad flag, is tried once a minute.

The agent's calls:

- A card that cannot be read lets the start through, with a WARN. A reading not taken is never evidence of contention, as for the verdict, and the start then does what it did before this check.
- Exactly the need fits.
- The check runs with `policy.enabled: false` too, which measures on request only, since a start is a request.
- The fit check's WARN is the one InferMux adds. A refused start leaves three WARN lines, as sol's review 15 recorded: the fit check's, then llama-swap's own `group: starting <model> failed: ...` and `non-200 response, recording partial metrics: status=503`. The two are upstream's and stay (0004). When the refusal fails over to another place, failover logs its own WARN too (0019, kept by the user on 2026-10-15).

## Consequences

- The number is the user's to measure and keep: one too low lets a start through that then fails, one too high refuses a start that would fit. The agent picked each as the highest peak measured, of the process's own figure and the rise in the card's used figure, plus about 100 MB, rounded up to the next 100: `drama-3`'s 6,060 MB to 6200, the reranker's 2,853 MiB at its server's 120k-character limit to 3000 (nixcfg's model files, 2026-10-16). The reranker's 3.1 GB of 2026-10-06 was before its server ran flash-attn.
- Not checked: a preload as the server is made, before the warden exists, which no host's config uses today; and a model on a GPU other than NVML's first (0022).
- Item 6 refuses an interactive request for up to 60 s after a failed start, which extends item 5's exception. A start that failed only because the driver had not yet freed a stopped model's VRAM is held although the next would succeed; the probe after the failure reads the freed figure, so the hold lasts the full 60 s. Not measured.
- Two starts at once on one GPU take turns (0029): the check holds the GPU's start slot until the model's start ends, so the second reads the card with the first on it.
- The driver may report a stopped process's VRAM as used for a moment after it exits. Not measured. A refusal right after a swap would show it: the WARN would list the evicted model among what held the card.

## Verification

Done, by tests (`go test`, no GPU):

- `internal/warden/fit_test.go`: a start the card's free figure does not cover is refused with the 503, its Retry-After and body, and the WARN, while the listed processes hold too little to refuse it; exactly the need fits; a model with no need, one on the CPU, one the placement does not name and every model outside the one config start without a reading; a card that cannot be read lets the start through; `/warden/verdict`'s `placement` still lists each model's devices and no need.
- `internal/placement`: `vram_mb` reaches each instance's `Local`, not llama-swap's entry; 0 and 3.1 are refused; a model with no GPU place is warned about; the editor's schema has it.
- `infermux_fit_test.go`: the daemon under the one config, a signed model with `vram_mb: 3100`, and a card with 62 MB free. An unmarked `/v1/rerank` gets the 503 `gpu_no_room` with Retry-After at once, and the model's command never runs. With 4,000 MB free, the same request starts it.

Nineteen mutations each failed them (`docs/verification.md`).

Watched on strix, 2026-10-16, after revision 7: an unmarked `/v1/rerank` started the reranker with 6,502 MiB free and answered 200 in 5.5 s cold, 0.54 s warm. With the reranker unloaded and a stand-in CUDA process holding 4,500 MiB, the same request answered 503 `gpu_no_room` with `Retry-After: 300` in 0.20 s, "needs 3000 MB of VRAM and the card has 1720 MB free", the model never started, and the WARN listed the stand-in first. Once it exited, the request answered 200 again in 5.5 s. Not yet: a refusal under a real game, and hister's own search answered with its unreranked order.
