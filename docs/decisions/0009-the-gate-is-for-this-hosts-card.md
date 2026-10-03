# 0009. The gate is for this host's card

- Date: 2026-10-03
- Status: accepted
- Rule: Only a request for one of this host's own models enters the warden's gate: refused or cancelled when batch during a pause, and counted toward the interactive quiet that an owed unload waits for. A request for another host's model or a cloud peer's still needs a key and passes the allow list, then goes straight through. A request naming no model InferMux knows is gated.
- Narrows 0004, which gated every inference request.

## Context

0004 put every POST outside the management API through one gate, written when every model sat on this host's card. 0006 added the other host's models and OpenRouter as a peer, and the gate went on counting them.

Adding the OpenRouter peer made that visible. A game starts and the verdict yields; the user keeps working in T3 on `openrouter/...`. Each of those requests set the interactive clock, so the unload owed from the yield waited on traffic that never touched the card, and the local model stayed in VRAM through the game, the case the project exists to prevent. A batch request to a cloud model was refused during a pause, though it needs no GPU. The zbox counted a `reaperboi/*` request it only forwards, and reaperboi's warden counted it again, rightly, for its own card.

## Decision

The user, 2026-10-03, over keeping the gate on everything: "Local models only". The warden's `Models.Qualify` reports whether the model is local, so the warden does not read locality out of a name that a peer could share. An `/upstream/<peer>/<model>/...` path is qualified with its peer too, which fixes the allow list for it: it had been qualified as `<this host>/<model>`.

## Consequences

- `/warden/verdict`'s in-flight list shows only this host's requests.
- The other host's warden still gates what lands on its card: the forwarded request carries the client's own key and class (0006).
