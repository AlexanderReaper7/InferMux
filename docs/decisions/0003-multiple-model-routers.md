# Multiple model routers

Date: 2026-10-02
Status: superseded by [0004](0004-infermux-request-priority.md). llama-swap runs both runtimes as one router, one model at a time.

The user chose a separate PrismML server for Bonsai 2 instead of replacing the mainline llama.cpp server, then chose extending the warden instead of making the two chat services mutually exclusive.

`agent.additional_router_urls` adds routers to the existing `router_url`. Any router holding a model suppresses the low-free-VRAM condition. A yield unloads every router once, including when another router is unavailable. `our_units` must include each server's systemd unit so Bonsai inference does not count as foreign GPU work. `/status` preserves `router_models` for the primary and adds `additional_router_models` for the others.

This coordinates yielding to foreign GPU work. It does not schedule requests between model servers or prevent simultaneous loads. Each router's fit pass still uses the currently available VRAM. The existing transition-only unload rule stays in force.
