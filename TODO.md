# TODO

- **Build now has no button.** Its panel was on the model form, removed 2026-10-14. `ui.prebuild`, `/api/build` and `webui/src/routes/BuildPanel.svelte` remain. Mount the panel in the Config tab, or remove all three.
- **0022's review section is out of date in one line.** "The model form, Peers, Routing and Downloads are hidden under the one config, not removed" stopped being true on 2026-10-14, when they were removed. The section is the user's review.
- **Fetching is off on both hosts.** `signedConfig.fetchDirs` is empty, because every file the config names is present on the hosts that run it. Turning it on needs `/srv/models` writable through `signedConfig.fetchGroup` (0022, "Fetching a missing file").
- **`nix develop -c go test -short .` hangs** in `TestProxy_AJobPausedByTheWardenGoesOnAfterTheResume` (`infermux_jobs_test.go:142`). The job still reads `queued` after the backend flushed three chunks, and the `t.Fatalf` then waits forever for the backend handler at line 53. It hangs the same way at `b8e18a7`, before the phase 8 work.
