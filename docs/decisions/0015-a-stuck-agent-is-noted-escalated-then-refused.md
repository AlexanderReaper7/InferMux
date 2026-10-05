# 0015. A stuck agent is noted, sent to a stronger model, then refused; the presets get Qwen's sampling

- Date: 2026-10-05
- Status: accepted
- Rule: for a request to this host's models, the warden counts the replies in a row, in the user's current turn, that added nothing new: every call already made in this turn, with the same output. At `stuck.annotate_after` (2) it appends a note to the last tool output and turns DRY on for that request. At `escalate_after` (3) the request goes to `escalate_to` (the 27B on reaperboi), and if the stuck model is that model, the request is refused. At `refuse_after` (4) it answers 400. The model presets set temp 1.0, top_p 0.95, top_k 20, min_p 0.

## Context

T3 thread `561a05e5` on 2026-10-05: Codex 0.159.2 on Qwopus3.6-35B-A3B, asked why the network died. Most of its probes needed root it did not have (`dmesg`, `nft`, `iptables`, `arping`). From about the tenth reply nothing new entered the context, its sentences converged on one, and the last four replies each said "The kernel is the standard NixOS 7.2.7 kernel..." and ran `nix-store --query --deriver .../linux-7.2.7.drv`, getting `unknown-deriver`, until the user interrupted it after 492 s (576,632 input tokens, 6,807 output).

The model did not degenerate inside a reply. Each reply was short and coherent. The loop was across replies: when every reply adds the same call and the same output, the transcript is an example of itself, and the model copies it. Sampling inside one reply cannot see that well, and the client cannot tell either. InferMux sees the whole turn in every request, because Codex sends it every time (`store: false`).

Sampling was llama-server's defaults (temp 0.8, top_k 40, top_p 0.95, min_p 0.05): the presets set none, the GGUF carries no `general.sampling.*`, and Codex sends none (a request captured from `codex exec` on 2026-10-05 has no temperature, top_p or penalty).

## Decision

1. **What counts** (the warden's rule, checked against every Codex thread on reaperboi). The turn starts after the last user message. A reply is the model's items between two rounds of tool output. A reply added nothing new when each of its calls, by name, arguments and output, was made earlier in the turn. A reply with no call is compared by its text. The streak is the trailing run of such replies. Codex's exec envelope puts a chunk ID and a wall time on every output, so those two lines are dropped before comparing.
2. **Waiting is progress.** Replaying all 4,481 requests in `~/.codex/sessions` found cloud threads polling a running build with `write_stdin` up to 10 times in a row, same call and same empty output, which is not stuck. A call to `write_stdin`, `sleep` or `wait_agent`, directly or from Codex's `exec` tool, or an output saying the process is still running, counts as new. After that, the replay has 24 requests at streak 1, 3 at 2 and 1 at 3, in two threads: the stuck one (2, at its last request) and a cloud thread clicking "Add variable" three times on purpose (3).
3. **The steps** (the user's choice: annotate, escalate to the 27B or fail if it is the 27B, configurable, then refuse). Defaults 2, 3, 4, set in `warden.yaml` and the Settings tab. The replay puts the stuck thread's note at its request 21, one before the user stopped it.
4. **The note goes on the last tool output.** A system or developer message would be moved to the top by llama-server's converter (nixcfg `packages/llama-cpp`, point 4), which is far from where the model reads next and invalidates the prompt cache. The client never sees the note, so the next request is read afresh and noted again if still stuck.
5. **DRY, not `logit_bias`** (the user's choice, 2026-10-05). The plan was to ban the first tokens of the repeated reply. llama-server tokenises a `logit_bias` string and biases each token separately (`server-schema.cpp`), so banning "The kernel is the standard" bans " the" and " is" everywhere. DRY penalises extending any sequence already in the context, and llama-server feeds the prompt into the sampler's history (`server-context.cpp`, `init_sampler`), so the earlier identical replies are what it penalises. It is set only for a local target, at multiplier 0.8, base 1.75, allowed length 2, over the last 65536 tokens, and never over a field the client set. The window is finite because llama-server's request schema refuses the CLI's -1 with a 400, found live on 2026-10-05 before deploying, and allocates its history buffer at that size.
6. **The escalation is one request.** The body's model becomes `escalate_to`; the next request names the stuck model again and is read afresh. The key's allow list applies to the target. The cost on reaperboi is a swap and a prefill on the 27B (pp 353-381 tok/s, so about 60 s for the thread's 23k-token turns) and tg of 6-9 tok/s.
7. **The refusal is an exception to 0004** (the user's choice). It is a 400 before the request reaches a model, never a cut stream. A Codex turn ends on it with one POST and no retry (checked against a fake server on 2026-10-05). The message names what repeated.
8. **This host's models only** (0009). Another host's request is read by that host's warden.
9. **The presets** (the user's choice, over Qwen's "general" line with presence_penalty 1.5, and over its "precise coding" line at temp 0.6). The 35B and the 27B get the Qwen3.8-27B card's thinking mode, the values Qwen ran its own agent benchmarks at. presence_penalty only covers the last 64 tokens and Qwen warns it mixes languages. Episteme's writer uses the 35B too, and gets them unless it sends its own.

## Consequences

- The comparison is exact. The thread's earlier drift ("the switch/router isn't responding to ARP" with small variations) was not caught; only the exact repeats were. A model that varies one argument each time is not caught either.
- A deliberate repeat, like clicking the same button three times, is noted at 2 and escalated at 3. The replay found one such thread in 4,481 requests.
- Codex's envelope and its waiting tools are named in the code. Another client with its own envelope may need its lines added.
- Anthropic's `/v1/messages` is not read: its tool results are user messages, and llama-server's converter drops the DRY fields. Nothing in nixcfg sends it today.
- Rejected for now, from the same conversation: a smaller model judging progress every N calls, collapsing the repeats before forwarding, a repeat count in the Performance tab, and a Codex hook, which would cover one client only.
