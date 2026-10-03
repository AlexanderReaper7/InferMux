# 0010. A cloud peer's facts come from the peer's own model list; T3's OpenRouter instance folds into Local Codex

- Date: 2026-10-03
- Status: accepted
- Rule: `/v1/models` carries `meta.infermux` on a peer's model too, read from the peer's own `<proxy>/v1/models` in OpenRouter's format (`context_length`, `architecture.input_modalities`, `supported_parameters`), at most once an hour per peer, again after a minute when a read fails, keeping the last good list. A peer whose list is not in that format, or needs a key to read, gets no facts.
- Extends 0007 (derived, not declared) to the peers of 0006.

## Context

0006 put OpenRouter behind InferMux as a llama-swap peer, and 0007's Codex catalog already listed its models, so T3's Local Codex instance showed `openrouter/...` beside the local models. With no facts: no context window, so Codex fell back to its own guess, no reasoning levels, and text only, Gemma included. The opencode plugin skipped them for the same reason.

T3 also had an OpenRouter instance of its own, reaching OpenRouter directly with a static Codex catalog in `agents/t3/codex/openrouter.models.json`. That catalog existed because the instance's key came from bw-app-gate, and without one every five-minute health probe would have opened an approval dialog. The plan was to point that instance at InferMux with its own key, which would have listed the same models in two instances.

## Decision

The user, 2026-10-03, over keeping the instance pointed at InferMux, and over folding it without facts: "Fold, InferMux fills facts". The OpenRouter instance, its catalog and its generator go; Local Codex is the one instance.

The facts follow what the static catalog had: the context length, image input when the model takes images, and the efforts low, medium and high with medium the default when OpenRouter lists `reasoning_effort`. OpenRouter maps the effort onto each provider's own setting. The list is read without a key: OpenRouter's is public, and sending the peer's key to a URL InferMux builds itself would be a second place the key travels.

## Consequences

- The first `/v1/models` after a start, and the first after an hour, waits for OpenRouter's list, up to 10 s.
- A peer that is not OpenRouter-compatible is listed without facts, as before.
- The opencode plugin lists peer models from these facts and does not ask them for `/props`.
