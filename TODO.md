# TODO

- **Build now has no button.** Its panel was on the model form, removed 2026-10-14. `ui.prebuild`, `/api/build` and `webui/src/routes/BuildPanel.svelte` remain. Mount the panel in the Config tab, or remove all three.
- **0022's review section is out of date in one line.** "The model form, Peers, Routing and Downloads are hidden under the one config, not removed" stopped being true on 2026-10-14, when they were removed. The section is the user's review.
- **Fetching is off on both hosts.** `signedConfig.fetchDirs` is empty, because every file the config names is present on the hosts that run it. Turning it on needs `/srv/models` writable through `signedConfig.fetchGroup` (0022, "Fetching a missing file").
- **llama-swap's writers drop the status after a 1xx.** `inflightResponseWriter`, `responseBodyCopier`, `statusRecorder` and `loadingWriter` keep the first `WriteHeader`, still in v264, so a model's unasked 1xx hides its status. Mark removes `Expect` so llama-server sends none (0016). Not reported upstream yet; the patch that made the root test pass is four early returns for a 1xx other than 101.
