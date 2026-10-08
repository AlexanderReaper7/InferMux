---
title: Routing capacity and request queues
summary: Configure concurrencyLimit and globalConcurrencyLimit, and understand queued work while a model is loading or busy.
category: guides
tags: [routing, queue, capacity, concurrency, concurrency-limit, max-concurrent-requests, global-concurrency-limit, rate-limit]
config_keys: [routing, models.*.concurrencyLimit, globalConcurrencyLimit]
updated: 2026-10-08
---

# Routing capacity and request queues

There is no `models.*.maxConcurrentRequests` setting. The real per-model key
is `models.*.concurrencyLimit`, which limits active parallel requests.

The router serializes model swaps and queues requests until the selected model
is ready. Avoid using many simultaneous client retries as a capacity control:
they create more queued work. Choose a routing policy that fits the models that
may coexist, then set client timeouts high enough for the queue and load time.

Inspect the Activity view and logs when latency grows. A queue that never drains
usually means the command, proxy, or health check is wrong; see
`guides/model-runtime/troubleshooting-model-wont-load`.

## Global concurrency limit

`globalConcurrencyLimit` is a top-level setting, separate from
`models.*.concurrencyLimit`. It caps the number of inference requests served
at once across every model combined, using a single shared semaphore:

```yaml
globalConcurrencyLimit: 8
```

The default is `0`, meaning no limit — the semaphore is not even added to the
request chain, so a default config pays no cost for the feature. Once the
limit is reached, further requests are rejected immediately with an HTTP 429
response rather than queued; there is no wait for a slot to free up. Clients
should treat a 429 as a signal to back off and retry, the same way they handle
a per-model concurrency limit rejection.

Use this to protect shared hardware (CPU, disk, network) from being
overwhelmed by traffic spread across many different models, which a per-model
`concurrencyLimit` cannot do since it only counts requests to one model at a
time.

## InferMux overflow between hosts

In InferMux, `failover_file: failover.yaml` in the separate warden configuration enables routing between hosts. This is distinct from llama-swap's `concurrencyLimit`. In `failover.yaml`, any model may list destinations with optional overflow limits:

```yaml
Octen-Embedding-4B.Q8_0:
  - place: zbox
    max_inflight: 1
  - place: strix
    max_inflight: 1
    only_if_idle: true
  - place: strix/embed-cpu/Octen-Embedding-4B.Q8_0
    batch_only: true
```

Each host must exist in the warden's `host` or `remotes`. `host/model` can select a different model name, including a peer on that host. The CPU example requires a peer named `embed-cpu` pointing at an independent CPU-only embedding server. GPU model groups must not manage that service.

`max_inflight` counts active requests at the destination, including direct clients and aliases. Loading and queued requests also count. At the limit, the destination returns 503 before sending the request to the model; InferMux tries the next place. Set each GPU's limit to its useful request concurrency. `0` or omitted imposes no overflow limit. Both hosts must run a version with overflow support; an older host ignores the limit and keeps queueing.

`only_if_idle: true` skips a GPU while a different model request is active. It allows swapping a different model that is loaded but has no active request. `batch_only: true` uses the client's authenticated key class. A client cannot enable CPU fallback by adding a class header. Without a keys file, requests are interactive.

In this example the second concurrent embedding request uses strix. A batch request uses the CPU if both GPUs are occupied. An interactive request skips the CPU and queues on the first saturated GPU. No fallback bypasses authentication, model allow lists or the GPU warden. An unbounded final destination accepts queued overflow; if every destination is unavailable, the final failure reaches the client.

Bare lists such as `embed: [zbox, strix]` retain failure-only routing. Changes to the routing file apply without unloading models. Requests balance as whole requests; one embedding batch is not divided between hosts.
