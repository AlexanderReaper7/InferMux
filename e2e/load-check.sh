#!/bin/sh
# e2e/run.sh's check of the card before a run. e2e/loadcheck_test.go runs it
# with a fake nvidia-smi.
# The runs' wardens yield the card at 100% of other work, and run.py then
# fails at "pause by hand", so a run while a game holds it would fail for a
# reason that is not the code's (sol, review 15). A card at 90% or more in any
# of five samples, half a second apart, refuses the run: 90, not 100, since
# this figure and the wardens' are read at different moments. So do a failed
# nvidia-smi and a card's reading that is not a number, each card's checked
# on its own, so that one nvidia-smi cannot read does not pass beside one it
# can (sol, review 18, M2).
if command -v nvidia-smi >/dev/null; then
  for i in 1 2 3 4 5; do
    loads=$(nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits) || {
      echo "e2e/run.sh: nvidia-smi failed to read the cards' load" >&2
      exit 1
    }
    while IFS= read -r load; do
      case $load in ''|*[!0-9]*) echo "e2e/run.sh: nvidia-smi read a card's load as '$load'" >&2; exit 1 ;; esac
      if [ "$load" -ge 90 ]; then
        echo "e2e/run.sh: a card is at $load%, and the runs' wardens yield at 100%; run it when the card is free" >&2
        exit 1
      fi
    done <<EOF
$loads
EOF
    if [ $i -lt 5 ]; then sleep 0.5; fi
  done
fi
