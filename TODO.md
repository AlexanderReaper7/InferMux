# TODO

- **Build now has no button.** Its panel was on the model form, removed 2026-10-14. `ui.prebuild`, `/api/build` and `webui/src/routes/BuildPanel.svelte` remain. Mount the panel in the Config tab, or remove all three.
- **0022's review section is out of date in one line.** "The model form, Peers, Routing and Downloads are hidden under the one config, not removed" stopped being true on 2026-10-14, when they were removed. The section is the user's review.
- **Fetching is off on both hosts.** `signedConfig.fetchDirs` is empty, because every file the config names is present on the hosts that run it. Turning it on needs `/srv/models` writable through `signedConfig.fetchGroup` (0022, "Fetching a missing file").
