#!/usr/bin/env bash
# The model-swap measurements of docs/research/2026-10-17-vram-swap.md: load
# times cold and warm, Octen and Qwen3.5-4B resident, under CUDA unified
# memory, and under unified memory with a Vulkan stand-in for a game holding
# the rest of the card, and llama-server's --sleep-idle-seconds.
# Run only while InferMux holds no GPU model and no game runs. Throwaway.
set -u
cd "$(dirname "$0")"
OUT=${OUT:-$HOME/Projects/scratch/vram-research/out/swapbench-$(date +%H%M)}
mkdir -p "$OUT"
LS=/nix/store/gswi9631bxfj4wspihh2p104mvrf6xpc-llama-cpp-0.6.0/bin/llama-server
VK=$HOME/Projects/scratch/vram-research/bin/vkhog
OCTEN=/srv/models/embed/Octen-Embedding-4B.Q8_0.gguf
QWEN=/srv/models/agent-candidates/Qwen3.5-4B-Q4_K_M.gguf
PORT=5291
OCTEN_ARGS=(--model $OCTEN --embeddings --ctx-size 2048 --batch-size 2048 --ubatch-size 2048 --parallel 2 --kv-unified --n-gpu-layers 99 --flash-attn on)
QWEN_ARGS=(--model $QWEN --jinja -ngl 99 -fa on --ctx-size 32768 --parallel 1)

# STEPS="4 5" runs only those steps; the default is all of them.
STEPS=${STEPS:-1 2 3 4 5 6 7}
want() { [[ " $STEPS " == *" $1 "* ]]; }
log() { echo "$(date +%T) $*" | tee -a "$OUT/summary.txt"; }
free_mb() { nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits; }
vram_of() { nvidia-smi --query-compute-apps=pid,used_memory --format=csv,noheader,nounits | awk -F', ' -v p="$1" '$1==p{print $2}'; }
evict() { python3 -c 'import os,sys; fd=os.open(sys.argv[1],os.O_RDONLY); os.posix_fadvise(fd,0,0,os.POSIX_FADV_DONTNEED)' "$1"; fincore "$1" | tail -1; }

guard() {
  if pgrep -x gamescope >/dev/null; then log "ABORT: gamescope running"; exit 1; fi
  local f; f=$(free_mb)
  if [ "$f" -lt "$1" ]; then log "ABORT: free $f MB < $1 MB"; exit 1; fi
}

start() { # start <tag> <env...> -- <args...>
  local tag=$1; shift; local envs=(); while [ "$1" != "--" ]; do envs+=("$1"); shift; done; shift
  local t0; t0=$(date +%s.%N)
  env "${envs[@]}" $LS --host 127.0.0.1 --port $PORT "$@" > "$OUT/$tag.log" 2>&1 &
  SPID=$!
  local r; r=$(timeout 120 python3 servebench.py ready $PORT "$t0")
  log "$tag: $r, process VRAM $(vram_of $SPID) MiB, card free $(free_mb) MB"
}
stop() { kill "$SPID"; wait "$SPID" 2>/dev/null; sleep 1; }

guard 6000
log "card at start: free $(free_mb) MB"

# 1. Octen load, cold then warm page cache.
if want 1; then
log "evict Octen from page cache: $(evict $OCTEN)"
start octen-cold -- "${OCTEN_ARGS[@]}"; stop
for i in 1 2 3; do start octen-warm$i -- "${OCTEN_ARGS[@]}"; stop; done
fi

# 2. Octen resident, plain cudaMalloc.
if want 2; then
start octen-plain -- "${OCTEN_ARGS[@]}"
python3 servebench.py embed $PORT single 10 | tee -a "$OUT/summary.txt"
python3 servebench.py embed $PORT batch 3 | tee -a "$OUT/summary.txt"
stop
fi

# 3. Octen under unified memory, card otherwise free.
if want 3 || want 4; then
start octen-uvm GGML_CUDA_ENABLE_UNIFIED_MEMORY=1 -- "${OCTEN_ARGS[@]}"
python3 servebench.py embed $PORT single 10 | tee -a "$OUT/summary.txt"
python3 servebench.py embed $PORT batch 3 | tee -a "$OUT/summary.txt"
fi

# 4. A game stand-in arrives while Octen (UVM) is loaded: does it get its memory, and what does Octen do then?
if want 4; then
G=$(( $(free_mb) + 3000 ))
log "game stand-in asks for $G MB (free + 3000) while Octen-UVM is loaded"
$VK $G 256 0 0.5 25 > "$OUT/game-vs-octen-uvm.log" 2>&1 &
VPID=$!
sleep 4
log "Octen-UVM process VRAM now $(vram_of $SPID) MiB, card free $(free_mb) MB"
python3 servebench.py embed $PORT single 5 | tee -a "$OUT/summary.txt"
python3 servebench.py embed $PORT batch 1 | tee -a "$OUT/summary.txt"
log "after: Octen-UVM VRAM $(vram_of $SPID) MiB, card free $(free_mb) MB"
wait $VPID
grep -E 'failed|allocated [0-9]' "$OUT/game-vs-octen-uvm.log" | tee -a "$OUT/summary.txt"
awk '/fill/{print $5, $6, $7, $8, $9, $10}' "$OUT/game-vs-octen-uvm.log" | tr '\n' ';' | tee -a "$OUT/summary.txt"; echo | tee -a "$OUT/summary.txt"
sleep 2
python3 servebench.py embed $PORT single 5 | tee -a "$OUT/summary.txt"
log "game gone: Octen-UVM VRAM $(vram_of $SPID) MiB"
fi
if want 3 || want 4; then stop; fi

# 5. Qwen3.5-4B (deep search agent) resident, then UVM with the game stand-in holding all but 1500 MB.
if want 5; then
guard 5000
start qwen-plain -- "${QWEN_ARGS[@]}"
python3 servebench.py chat $PORT 3 | tee -a "$OUT/summary.txt"
stop
G=$(( $(free_mb) - 1500 ))
log "game stand-in holds $G MB, leaving 1500 MB, before Qwen-UVM starts"
$VK $G 256 0 0.5 60 > "$OUT/game-vs-qwen-uvm.log" 2>&1 &
VPID=$!
sleep 3
start qwen-uvm-over GGML_CUDA_ENABLE_UNIFIED_MEMORY=1 -- "${QWEN_ARGS[@]}"
python3 servebench.py chat $PORT 2 | tee -a "$OUT/summary.txt"
stop
kill $VPID 2>/dev/null; wait $VPID 2>/dev/null
awk '/fill/{print $5, $6, $7, $8, $9, $10}' "$OUT/game-vs-qwen-uvm.log" | tr '\n' ';' | tee -a "$OUT/summary.txt"; echo | tee -a "$OUT/summary.txt"
fi

# 6. llama-server's own sleep: VRAM while asleep, wake cost.
if want 6; then
guard 6000
start octen-sleep -- "${OCTEN_ARGS[@]}" --sleep-idle-seconds 3
python3 servebench.py embed $PORT single 1 | tee -a "$OUT/summary.txt"
sleep 8
log "asleep: process VRAM $(vram_of $SPID) MiB, card free $(free_mb) MB"
python3 servebench.py embed $PORT single 1 | tee -a "$OUT/summary.txt"
log "woken: process VRAM $(vram_of $SPID) MiB"
stop
fi

# 7. Octen on the CPU: what a query costs with no GPU at all.
if want 7; then
start octen-cpu CUDA_VISIBLE_DEVICES= -- "${OCTEN_ARGS[@]/99/0}"
python3 servebench.py embed $PORT single 10 | tee -a "$OUT/summary.txt"
python3 servebench.py embed $PORT batch 1 | tee -a "$OUT/summary.txt"
stop
fi
log "done"
