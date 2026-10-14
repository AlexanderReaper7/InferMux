# 0016. A model fails over between places, in a file of its own

- Date: 2026-10-06
- Status: accepted; built, tested and running 2026-10-06. Failing over from an offline host was watched; a 503 or 504 was not
- Amended by [0022](0022-one-signed-config-placed-by-device.md) on 2026-10-14: the places are the one config's lanes, not `failover.yaml` (the rule, items 2 and 4).
- Rule: a model's places, in the order tried, are its `balance` lane in the one config and then its `failover` lane (0022). A place is a host's device, `<host>/<device>`. A request for a listed model goes through the whole handler chain once per place, renamed for it, until one answers something other than 502, 503 or 504. The last place's answer is the client's whatever it is. A new config's lanes apply at once.
- Keeps 0006, 2 (an unqualified name that is local stays local unless listed here, and a forwarded request is served where it lands) and 0009 (the gate is for this host's card).

## Context

The semantic search project (Semantic-Search, 2026-10-06) moves the embedder, Octen-Embedding-4B Q8_0, from reaperboi's CPU to the zbox's GTX 1060. Measured that day: a query embedding is 0.04 s there against 2.7–6.6 s on reaperboi's CPU while it is loaded, and eight 919-token chunks take 12 s against 47–49 s. The card holds it at ubatch 2048 in 5655 MiB, so it takes turns with Immich ML (0013).

The user wanted a fallback on reaperboi's GPU while the zbox cannot serve, and chose InferMux for it, so every client gets it rather than only hister. Episteme names the same model.

## Decision

The user, 2026-10-06, each asked in turn.

1. **Failover lives in InferMux.** Rejected: a second endpoint in hister's fork, which only hister would have. Rejected: switching by hand.
2. **Routing read by InferMux, not llama-swap, apart from the model files.** The user proposed a file of its own over metadata in the model's file; since 0022 it is the one config's `balance` and `failover` lanes. A llama-swap file reloads llama-swap, which stops every model (0005); the lanes apply at once, like the warden's file. It also keeps routing between hosts in one place rather than spread over model files. The cost is that a model's file does not say it fails over.
3. **502, 503 and 504 fail over; nothing else does.** 502 is the remote router's answer for an offline host and for one that did not answer (0006, 3), and llama-swap's for a model that would not start; 503 is a warden refusing (a yield, or batch during a pause); 504 a timeout. A 4xx or a 500 would be the same at the next place.
4. **A place is a host's device.** The user wanted a CPU fallback possible and not added to the embedder; 0019 added it for batch keys, and since 0022 it is `strix/cpu` in Octen's `failover` lane, the same model definition on another device. A llama-swap peer, which is what a cloud is, is not a place, because it is not a host.

How it is built, the agent's choice: the failover handler is the outermost one. Each attempt is a whole request through the warden, renamed `zbox/<model>` or to the local name, so the warden checks the key's allow list for that name and gates a local attempt on this card like any local request. A failover decided inside the remote router would have served the local model past the warden. The body is read once and given to each attempt. An attempt's headers are held until its status is known, so a dropped answer leaves nothing on the client's; from the first other status everything passes straight through, a stream chunk by chunk. A request carrying the hop header, and an `/upstream/` path, are passed on untouched.

## Consequences

- A failed-over request pays the first place's failure: at once for a host the router knows is offline, up to the dial timeout (5 s) for one that went away since its last poll.
- Each attempt is a request of its own to the warden and the timing (0014): a failover shows as the failed attempt on the host it tried, then the one that answered.
- The fallback on reaperboi's card is a local model like any other: with no groups it takes the card's one slot, evicting a chat model or the reranker.
- The UI edits the lanes in the Config tab, with the rest of the one config (0022, 9).
