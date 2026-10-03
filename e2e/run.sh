#!/bin/sh
# The end-to-end run: an isolated daemon and UI on 5101/5110, driven by
# Playwright. Run `nix build` first; INFERMUX_BIN points elsewhere.
cd "$(dirname "$0")/.." || exit 1
BR=$(nix build --no-link --print-out-paths nixpkgs#playwright-driver.browsers) || exit 1
PY=$(nix build --no-link --print-out-paths --impure --expr 'with import <nixpkgs> {}; python3.withPackages (p: [ p.playwright ])') || exit 1
PLAYWRIGHT_BROWSERS_PATH=$BR PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=true exec timeout 600 "$PY/bin/python3" e2e/run.py
