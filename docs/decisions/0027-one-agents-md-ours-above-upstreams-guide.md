# 0027. One AGENTS.md: InferMux's rules above llama-swap's guide

- Date: 2026-10-15
- Status: accepted
- Rule: `AGENTS.md` is InferMux's rules and map, then llama-swap's own `AGENTS.md`, verbatim, under the heading "llama-swap's own guide". `CLAUDE.md` is upstream's one line, `@AGENTS.md`. Both merge as usual; only `README.md` keeps ours.

## Context

Until 2026-10-15 InferMux's rules were in `CLAUDE.md`, merged with `merge=ours`, and `AGENTS.md` was upstream's guide as llama-swap ships it. An agent that reads `AGENTS.md` and not `CLAUDE.md`, such as Codex, read upstream's guide and none of InferMux's rules. Upstream ships both files, its `CLAUDE.md` being `@AGENTS.md`, an import Claude Code reads.

## Decision

The agent offered two ways, through the coordinating session, in round 14 of 2026-10-15: (a) our file becomes `AGENTS.md`, kept with `merge=ours`, upstream's guide moves to a file of its own refreshed at each merge, and `CLAUDE.md` becomes a symlink to `AGENTS.md`; (b) one `AGENTS.md`, upstream's guide appended after ours. The agent recommended (a), and said of (b) that every change upstream makes to the guide conflicts at merge. The user chose (b), relayed by the coordinating session as "10: append. One AGENTS.md with InferMux's rules on top and upstream's guide appended below. The user chose this over moving the guide aside, with the merge conflicts at upstream changes to the guide as the known cost."

The agent's claim about the cost was wrong, measured once the file was built. A 3-way merge of each of upstream's 16 changes to its guide, from its creation on 2026-02-15 to v262, into this file with the guide's old version at the end: 15 merge with no conflict and leave the guide equal to upstream's new one. One conflicts, 27782ab of 2026-07-30, which changed the guide's first line: a change there, or a line added above it, touches the end of our part. The guide is upstream's only while nothing of ours is written into it, so ours stays above its heading.

The agent's calls: `CLAUDE.md` is upstream's `@AGENTS.md`, byte for byte, rather than a symlink, so it merges with no conflict unless both sides change it; and `.gitattributes` keeps `merge=ours` for `README.md` alone.

## Consequences

- Codex and Claude read the same file.
- A merge of a new llama-swap release that changes the guide's first line conflicts in `AGENTS.md`: keep both, ours above.
- The rules and upstream's guide differ, for example on commit messages, which upstream wraps at 80 columns. The map's line on the guide keeps its old scope: follow it when touching anything outside `internal/warden/` and `infermux.go`.

## Verification

The replay above, with `git merge-file` on each upstream commit that modified `AGENTS.md` up to v262. Not seen: a `git merge` of a new release with this file, and a session that read `CLAUDE.md`'s import.
