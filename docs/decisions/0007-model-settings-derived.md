# 0007. Each model's settings derived from its command and GGUF; Codex gets them as its catalog

- Date: 2026-10-03
- Status: accepted
- Rule: `/v1/models` carries each local model's context window, input modalities and reasoning efforts as `meta.infermux`, derived from the model's llama-server command and the GGUF it loads. Nothing is declared by hand. A request with `client_version` in the query, which only Codex sends, gets the same list in Codex's catalog format, if `-codex-prompt` names the prompt.md of the Codex installed beside it.
- Keeps 0006 (the other host's models come with their facts, read from its own `/v1/models`).

## Context

T3's Local Codex instance reaches InferMux through a Codex provider. Codex asks the provider for its model catalog at start, `GET /v1/models?client_version=0.159.2`, and expects `{"models":[ModelInfo]}` (`codex-rs/protocol/src/openai_models.rs`). llama-swap's OpenAI list failed that decode, so Codex fell back to built-in guesses: no reasoning levels, a 272k context window. The user had worked around that by hand: `-c model_context_window=65536` and a `customModels` list in T3's launch settings. Neither follows a model file that changes.

The user asked that clients get the models at runtime "along with other model settings like available effort levels or context size".

## Decision

**All derived** (the user's choice over declaring the facts in each model file's `metadata:`, or a mix):

- The context window is the context llama.cpp reports once the model has run (`meta.n_ctx`). Before that it is `--ctx-size`, or the GGUF's `<arch>.context_length` if there is none. It is divided by `--parallel` unless `--kv-unified` is set, because llama.cpp splits the cache between slots. `--fit` can lower the allocated context below `--ctx-size`, which is why the loaded value wins.
- Image input means `--mmproj`.
- The reasoning efforts are read from the chat template (`--chat-template-file`, an inline `--chat-template`, or the GGUF's `tokenizer.chat_template`). llama-server v0.5.0 turns effort "none" into `enable_thinking: false` and passes any other effort to the template, and the Qwen-family templates raise an exception on one they do not list. So the levels offered are only those the template compares `reasoning_effort` against. A template that reads only `enable_thinking` offers none and medium. `--reasoning off` or `--reasoning-budget 0` offers nothing; `--reasoning-effort` sets the default.

The template is read with regular expressions, not rendered. A template that takes efforts in some form the expressions miss offers on and off only, which is safe: the worst case is a missing level, never an exception.

**The same `/v1/models`** (the user's choice over a separate catalog path). Codex alone puts `client_version` in the query; every other client gets the OpenAI list it gets today.

**The prompt comes from the installed Codex.** Codex rejects a catalog entry without `model_messages.instructions_template` or `base_instructions`, and the text it expects is its own `prompt.md`, which changes between Codex versions. InferMux does not copy it into the repository; the NixOS configuration passes the pinned Codex's file. Without `-codex-prompt`, Codex gets the plain list and its fallback, as before.

## Rejected

- Facts in each model file's `metadata:`. Exact, but a second copy of what the command and the GGUF already say, and it drifts when either changes.
- A separate catalog URL in Codex's `model_catalog_json`. Codex reads that file at start only, and the URL would be one more thing to configure per client.
