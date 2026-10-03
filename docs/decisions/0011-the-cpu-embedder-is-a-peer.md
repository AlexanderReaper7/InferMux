# 0011. The CPU embedder is a peer of its own unit

- Date: 2026-10-03
- Status: accepted
- Rule: A model that never touches the card runs as its own systemd unit and is a llama-swap peer in a model file (`proxy: http://127.0.0.1:<port>`), not a local model. The warden sees only local models (0009), so it never stops, counts or gates it.
- Replaces 0006, 10 (the embedder moves under InferMux, kept by `never_unload` in warden.yaml).

## Context

0006, 10 planned to move reaperboi's CPU embedder, `llama-embed.service` on :5002, under InferMux as a model, with a `never_unload` list in warden.yaml. Three of the warden's rules treat every running local model as the card's tenant, and `never_unload` switches off only one:

1. A yield unloads every model (0002).
2. Free VRAM is read only while no model is loaded (0002). An always-running embedder would end that reading for good.
3. During a pause a batch request to a local model gets 503 (0004). Episteme's worker could not embed during any pause, though embedding needs no GPU.

## Decision

The user, 2026-10-03, after these were laid out: "A peer of its own unit". `embed.yaml` in reaperboi's models directory lists `Octen-Embedding-4B.Q8_0` under the peer `embed`. The unit keeps its own sandbox and its `--device none`, and stays in `our_units`.

Rejected:

- **An `off_card` list in warden.yaml** that exempts a model from all three rules. Three warden changes, and a GPU model listed there by mistake would hide from the warden.
- **Measuring per model process in NVML** for rules 1 and 2. It needs a model-to-PID mapping through llama-swap, and the gate decides before the process exists, so it still needs a declaration.
- **`never_unload` alone**, as 0006 wrote it: the costs in 2 and 3 above.

## Consequences

- The embedder's flags stay in Nix (`modules/nixos/llm.nix` in nixcfg); the UI lists the peer but does not edit its command.
- It is named `embed/<model>`; the bare name resolves while no other peer has it.
- llama-server checks no key, so the client's key travels to it and is ignored. Only InferMux's check counts.
- 0010's peer facts read `http://127.0.0.1:5002/v1/models` hourly and find no OpenRouter fields, so the embedder has no `meta.infermux`.
- 0006, 9 (groups) was planned for the embedder and has no other use yet.
