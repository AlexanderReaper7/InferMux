#!/bin/sh
# The end-to-end runs: an isolated daemon and UI, driven by Playwright.
# run.py is the UI's pages on 5101/5110, config.py the Config tab's editor on
# 5121/5130; `e2e/run.sh config.py` runs one. Run `nix build` first;
# INFERMUX_BIN points elsewhere.
cd "$(dirname "$0")/.." || exit 1
BR=$(nix build --no-link --print-out-paths nixpkgs#playwright-driver.browsers) || exit 1
TOOLS=$(nix build --no-link --print-out-paths nixpkgs#sops nixpkgs#age | sed 's|$|/bin|' | paste -sd:) || exit 1
PY=$(nix build --no-link --print-out-paths --impure --expr 'with import <nixpkgs> {}; python3.withPackages (p: [ p.playwright ])') || exit 1
# The runs' wardens yield the card at 100% of other work, and run.py then
# fails at "pause by hand", so a run while a game holds it would fail for a
# reason that is not the code's (sol, review 15). Any of five samples, half a
# second apart, at 90% or more refuses the run: 90, not 100, since this figure
# and the wardens' are read at different moments.
if command -v nvidia-smi >/dev/null; then
  for i in 1 2 3 4 5; do
    load=$(nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits | sort -n | tail -1)
    case $load in ''|*[!0-9]*) echo "e2e/run.sh: nvidia-smi read the card's load as '$load'" >&2; exit 1 ;; esac
    if [ "$load" -ge 90 ]; then
      echo "e2e/run.sh: the card is at $load%, and the runs' wardens yield at 100%; run it when the card is free" >&2
      exit 1
    fi
    if [ $i -lt 5 ]; then sleep 0.5; fi
  done
fi
failed=0
for script in ${@:-run.py config.py}; do
  PYTHONDONTWRITEBYTECODE=1 PATH=$TOOLS:$PATH PLAYWRIGHT_BROWSERS_PATH=$BR PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=true timeout 600 "$PY/bin/python3" "e2e/$script" || failed=1
done
exit $failed
