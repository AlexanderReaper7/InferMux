# 0029. A group stays loaded together on a device that swaps

- Date: 2026-10-16
- Status: accepted; built 2026-10-16, not deployed; the agent's model is in the signed config, its group is not yet (Verification, below)
- Amends [0022](0022-one-signed-config-placed-by-device.md), item 7 (a device swaps one model at a time, or runs several at once) and the derivation's groups with `exclusive: false`, and [0028](0028-a-start-on-the-gpu-needs-room-on-the-card.md) (two starts at once on one GPU). Keeps 0028's check and 0002's free VRAM rule for the verdict.

## Context

SemSearch's deep search runs an agent, Qwen3.5-4B Q4_K_M, about 3.5 to 4.5 GB, whose every turn runs a hister search, and hister reranks with jina-reranker-v3.5 on `strix/gpu` (`vram_mb: 3000`). strix's GPU swapped one model at a time (0022, 7), so each turn's rerank stopped the agent and the next turn stopped the reranker. The user decided on 2026-10-16, in SemSearch's deep search thread, that the two share the card (SemSearch's `docs/deep-search.md`, "What the user decided (2026-10-16)"), and picked from that thread's options "a group of models that stay loaded together on a swapping device. Starting one member doesn't stop the other, and any model outside the group stops both. 0028's room check still applies." Rejected there: a second concurrent device on the same card, where a big chat model would answer 503 until the pair's TTL ran out, and semsearch running its own llama-server, which the warden would not see.

Neither `occupies` nor a placement group could do it. `occupies` only tells the warden which devices count a model (0022, the warden section, 3), and a placement group is a label for lanes (0022, 3). Each device was one llama-swap group, and a group either swaps all its members or none.

llama-swap 262, the version merged, has a second router, `matrix` (`internal/router/matrix.go`, upstream's guide `docs/kb/guides/routing/groups-and-matrix.md`). It takes the sets of models allowed to run at once, starts only the model asked for, and stops the running models outside the cheapest set that has it.

## Decision

The agent offered the options in this thread on 2026-10-16, and the user took the recommended one in each:

1. **Each host's part uses llama-swap's matrix router, on every host.** Rejected: InferMux's own swapper, a new router type that needs a hook in upstream's `server.go`, a merge conflict each release; keeping the group router with the GPU's models in two exclusive groups and the CPU's persistent, which holds only while a host has one GPU; the matrix only on a host with a group, which keeps the zbox's file as it was but means two derivations to test.
2. **The group is a named map on the device**, `together`, in the device's own block:

    ```yaml
    hosts:
      strix:
        devices:
          gpu:
            kind: gpu
            together:
              deep-search: [Qwen3.5-4B-Q4_K_M, jina-reranker-v3.5]
    ```

    The name is the matrix set's, which llama-swap's log line shows (`matrix: model=... set=deep-search`). Rejected: the device naming a placement group, which ties a lane's label to a swap unit; unnamed lists.
3. **Starts on the GPU take turns.** The fit check (0028) holds the GPU's one start slot from its check until llama-swap's state change out of `starting`, so a second start waits, then reads a card the first model is on. Without it two members asked for at once both read the same free figure, both pass, and one fails in CUDA seconds later, the failure 0028 was made to end. Rejected: counting the `vram_mb` of a model still starting as held, which counts twice what it has already allocated and can refuse a start that fits; leaving it, as 0028 had for a GPU that runs several at once.

What follows from the router and is not new: each member keeps its own `ttl`, so one unloads and the other stays; a member that does not fit beside its loaded sibling gets 503 `gpu_no_room` and the sibling stays; a yield unloads the device's models, both members, after interactive quiet (0004); the verdict reads free VRAM only while no GPU model is loaded (0002).

The agent's calls:

- **One set for each way of choosing what runs on every device at once.** On a device that swaps, that is one of its groups or one of its models outside a group; on a concurrent device, all its models in every set. A set is named by the choices on the devices that swap, joined by `+`, or `all` when every device is concurrent. llama-swap's compiler does not expand an expression, but InferMux writes each set out so that the set has the group's name; strix has one device that swaps, so the sets are as many as its GPU's choices.
- **A name llama-swap's expressions cannot hold**, `<model>@<device>` (0022, a model on two devices), is written as a variable `v1`, `v2`, ..., skipping any name a local model has, since a variable wins over a model of the same name.
- **A group needs two members or more, each a model in `models/` and named once, and a group's name is not a model's.** A group on a concurrent device is refused: it runs its models together already. A member that no lane places on the device is a warning, not an error, since removing a model's places is how it is turned off (0022, "A model placed nowhere is a warning"). A group left with one member placed is no set of its own, and that member swaps like any other model there.
- **A model may be in two groups on one device.** The matrix allows it: starting a model in both keeps what the cheaper set keeps.
- **Every start on the GPU takes the slot, with `vram_mb` or without,** since a start with no need still allocates VRAM that the next start's reading must hold. A model on the CPU never takes it. A refused start frees it at once. Outside the one config every local model is on the GPU and takes it too.
- **A start waits for the slot 60 s at most**, then goes ahead with a WARN. A start always ends in a state change, llama-swap's event bus delivers every one, and llama-swap's own health check limit ends a start that hangs (600 s on both hosts). The limit is for an end never heard, such as one after the daemon's stop has unsubscribed, where every later start on the GPU would otherwise wait for good. The daemon test showed that case hang until the limit.
- **The build's check without the environment (0022, 11) takes a model it leaves out out of the matrix's sets**, since llama-swap checks each name in a set at load and a group's members it did not; a set left empty goes, and the router with the last one.

## Consequences

- strix's derived file changes on the next deploy from `groups:` to `routing.router.use: matrix`, and so does the zbox's. Under the matrix a device's model never stops another device's, as `exclusive: false` had it.
- `only_if_idle` (0021) still refuses a GPU that is serving another model's request, and a group member's sibling counts as another model, though no swap would follow. No group member has an `only_if_idle` place.
- A second start on the GPU waits for the first, up to the first's load time: the reranker's cold start was 5.5 s on 2026-10-16 (0028, Verification). Only group members start at once on a GPU that swaps, since llama-swap queues a start that would stop one in progress.
- The card holds both members, 7.4 GB by their `vram_mb` (the agent's 4400 since revision 11 of 2026-10-17, the reranker's 3000), against strix's 10,240 MB less the desktop. That leaves the desktop 2.8 GB; at 03:30 CEST on 2026-10-17 it held 4.4 GB, and then the second member to start gets `gpu_no_room` (0028) and the first stays. While both are loaded, a game has less room, and the warden yields and unloads them as for any model.
- The agent's model is in the signed config since revision 9 of 2026-10-17, with its `vram_mb` since revision 10, 4400 since revision 11. Its group is not, since a daemon without 0029 refuses `together` (`field together not found in type placement.Device`, the UI's check on strix that night).

## Verification

Done, by tests (`go test`, no GPU):

- `internal/placement/together_test.go`: under the derived matrix, llama-swap's own solver starts the reranker beside the loaded agent and the agent beside the reranker stopping nothing, a chat model stops both, a member stops a chat model, a CPU model stops nothing on the GPU, and `embed@gpu`, a second device's name, through a variable; the set it logs is `deep-search`; a group with one member placed swaps, with the warning; the six refusals; a member left out by the build's check is taken out of the sets, and the router with the last one. `TestTodayStrix` checks the same of today's config without a group.
- `internal/warden/fit_test.go`: a second start on the GPU waits for the first's end and then reads the card with the first in it, so the agent's 3,100 MB no longer fit in what the reranker left; a start with no need holds the slot; a refused start frees it; the end of a start that does not hold it frees nothing; a CPU model never waits; an end never heard holds it for the limit only, and the late end then frees nothing.
- `infermux_together_test.go`: the daemon with a signed config whose GPU has the group, on the part it derives, through a real llama-swap and the event wiring. The reranker asked for while the agent starts runs its command only once the agent is ready; both answer 200 and are ready together; a chat model then runs alone.

Eight mutations each failed them: the slot's wait removed (the warden's tests and the daemon's), a refused start keeping the slot, any end freeing it, the group ignored by the derivation, the event wiring removed (which hung the daemon test until the limit was added), the matrix pruning removed, the warning removed, and a group on a concurrent device allowed.

Watched on strix, 2026-10-17, under revision 10 (`vram_mb: 4200`) and the daemon without 0029: with SemSearch's eval holding 4,030 MB in its own server, an unmarked request for the agent answered 503 `gpu_no_room`, "needs 4200 MB of VRAM and the card has 1741 MB free", in 0.008 s, and nothing started (0028's check on the new model).

Not yet: deployed, a group in the signed config, the agent's model on the card beside the reranker, and the WARN of a start that waited out the limit.
