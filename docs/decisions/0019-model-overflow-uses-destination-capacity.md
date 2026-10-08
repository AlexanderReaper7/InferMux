# 0019. Model overflow uses the destination's request count

- Date: 2026-10-08
- Status: accepted; deployed on both hosts 2026-10-08. Live GPU overflow and the independent CPU backend verified; CPU batch selection verified through the full handler tests
- Extends [0016](0016-a-model-fails-over-between-places.md). Keeps [0009](0009-the-gate-is-for-this-hosts-card.md) and [0011](0011-the-cpu-embedder-is-a-peer.md).

The zbox's embedding backlog queues behind a healthy GPU, so failure-only routing never uses the RTX 3080 while the zbox answers successfully. The user requested load balancing configurable for any model and chose per-destination limits in the existing `failover.yaml`, rather than least-busy routing.

A destination can be a bare string or a mapping with `place`, `max_inflight`, `only_if_idle` and `batch_only`. Bare strings keep failure-only routing. The destination counts requests and reserves capacity under one lock, including direct clients and canonical model aliases. This avoids polling stale load or counting only requests from one front door. Authenticated requests carry admission hints to that destination; hints affect only their own admission and grant no access. Each attempt still passes authentication, the allow list and the local GPU warden. Hosts without this implementation ignore the hints, so both GPU hosts must be updated.

The embedding policy is zbox at one request, strix at one request only when no different GPU model request is active, then the CPU for batch keys only. The user explicitly allowed swapping a different loaded model once it has no active request. A CPU service stays independent of GPU model groups, exposed as a peer of strix. Its requests neither occupy GPU capacity nor enter the GPU warden's local-model gate.

Interactive requests skip batch-only places. If all eligible GPUs are occupied, they queue at the first destination that refused for model capacity, with its limit removed for that attempt. A GPU occupied by another model remains skipped. No accepted request is cancelled to make room. An unbounded final place queues overflow for eligible clients.

Limits measure whole requests, not tokens or individual entries in an embedding batch. They distribute concurrent work; they cannot split a single request or accelerate a serial client. WebSocket reservations last for the session, while the warden's separate moving-data rules remain unchanged.
