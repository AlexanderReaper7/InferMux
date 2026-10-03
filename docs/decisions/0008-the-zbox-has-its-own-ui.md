# 0008. The zbox has its own UI and nixcfg checkout; Commit can take the GPG passphrase

- Date: 2026-10-03
- Status: accepted
- Rule: Each host with InferMux runs infermux-ui on its own checkout of nixcfg, reached at its own tailnet name on :5010. The zbox's files are `hosts/zbox/infermux/` in that checkout; keys.yaml is the shared one, read from the checkout. A host with no desktop session sets `ui.commitPassphrase`, and Commit there takes the GPG key's passphrase from the browser.
- Replaces 0006's item 7 (the zbox's UI as an agent the desktop's UI sends files to).

## Context

The user, 2026-10-03: "give the zbox the ui too. so if reaperboi goes down, its still accessible." Under 0006, 7, the desktop's UI held every host's files and a zbox edit needed the desktop on. Until then the zbox had no UI at all, its warden settings were Nix attributes in `hosts/zbox/default.nix`, and its keys.yaml was the store copy from the last deploy.

## Decision

Asked and answered in one session, 2026-10-03.

1. **A full UI with its own checkout** (rejected: a UI that only shows the verdict and controls the daemon; a full UI with the Keys tab, which needs the user's age identity on a second, always-on machine). The zbox's UI has no `keySecrets`: its Keys tab makes and revokes keys in keys.yaml, but a key made there shows its plaintext once and keeps it nowhere, and Show does nothing. Costs: commits land on two clones, which the user pushes by hand (the UI never pushes, 0005), and the zbox's config is files in the checkout, not Nix.
2. **Each host at its own name** (`https://<host>.tail.ts.net:5010`, and :5001 for the daemon). Rejected: one Tailscale Service name for both hosts. Tailscale gives each client a fixed pseudorandom preference among a service's hosts within a region and moves it only when that host is unavailable; there is no "prefer reaperboi" setting. A client sent to the zbox would see reaperboi's models as `reaperboi/<model>` and route through the zbox's dual-core i3, and behind one UI name a browser would land on either host's files.
3. **The zbox's files are `hosts/zbox/infermux/`** (`warden.yaml`, `models/`), beside its host config (rejected: moving reaperboi's to `modules/nixos/llm/infermux/<host>/` for symmetry). `keys_file` points at the shared `modules/nixos/llm/infermux/keys.yaml`. The daemon's sandbox mounts only `configDir`, so the zbox binds the whole checkout read-only (rejected: a `readOnlyPaths` module option naming only the keys directory). The cost is a daemon that can read every file in the repository, including the encrypted secrets.
4. **keys.yaml from the checkout,** so a key made or revoked on reaperboi reaches the zbox with a pull, not a deploy. Replaces the deploy-time copy in 0006's Open section.
5. **A deploy key with write access** to nixcfg, made on the zbox (rejected: the user's own GitHub login there, which reaches every repository; pulling from reaperboi, which fails in exactly the case this is for).
6. **A timer fast-forwards the zbox's checkout** (`git pull --ff-only`, rejected: by hand). It stops when the zbox has commits of its own that are not pushed. Cost: files change under an open editor.
7. **The GPG key is copied to the zbox, and Commit takes its passphrase from the browser** (rejected: an SSH signing key made on the zbox, which needs no passphrase; unsigned commits there). Every nixcfg commit is signed with the user's passphrase-protected GPG key, and a host with no desktop session has nowhere to show a pinentry, so `ui.commitPassphrase` adds a passphrase field to Commit. infermux-ui hands it to gpg in loopback mode on a pipe at fd 3, which git passes on to its `gpg.program`; that program is infermux-ui itself, which execs gpg with `--pinentry-mode loopback --passphrase-fd 3`. The passphrase is never in an argument, the environment or a file. gpg-agent caches it for its own TTL, as after a pinentry. Costs: the key that signs everything is on a second machine, and the export to it asks for the passphrase in a pinentry on the desktop once.
