# 0012. A model names its GGUF on Hugging Face, and the UI downloads it from main

- Date: 2026-10-03
- Status: accepted
- Rule: A model file may carry `metadata.hf: {model: org/repo/file.gguf, mmproj: org/repo/file.gguf}`. On save, infermux-ui writes the derived paths under its `-hf-dir` into `--model` and `--mmproj`, and fetches each file from the repository's `main`: again only when main's ETag differs from the one the file was taken at. A gated or private repo takes the token in `-hf-token-file`.
- Builds 0006, 8.

## Context

0006, 8 decided that a model file may name its GGUF on Hugging Face, with the path derived under `/srv/models/hf/` and the download on save, with progress in the UI, by the host that runs the model. Under 0008 each host's UI edits its own files, so "the host that runs it" is the UI that saved it. Until now every GGUF was put in `/srv/models` by hand.

## Decision

The user, 2026-10-03:

1. **Per flag, following main** (rejected: pinned to the commit fetched, which a re-save would not move; the main GGUF only, leaving each vision model's mmproj to be placed by hand). A save re-checks main, so a re-save is how a model is updated. The cost the user accepted: the same model file can mean different weights on two hosts, or on one host before and after a save.
2. **A token from sops** (rejected: public repos only). The token is read from a file per request and sent to huggingface.co only; Go drops it on the redirect to the CDN.

How a file follows main: a HEAD of `resolve/main/<file>`, without following the redirect, gives `X-Linked-Etag`, the LFS object's SHA-256, and `X-Linked-Size`. The file is kept with its ETag beside it as `file.gguf.etag`. A download goes to `file.gguf.part`, with the ETag it is for in `file.gguf.part.etag`, and is resumed with a Range request only at that ETag. A body shorter than the size is an error and never becomes the model file. The rename puts the new file in place while a running llama-server keeps the old one open.

A model edited as text cannot name a source: the paths are written into the table's `--model` and `--mmproj`. An empty source removes `metadata.hf` and keeps the paths.

## Consequences

- The daemon may reload before the download finishes. Starting the model fails until it has; the UI shows the progress.
- Progress lives in infermux-ui's memory: a restart forgets it, and the next save resumes the `.part`.
- Nothing deletes a GGUF a model stopped naming.
