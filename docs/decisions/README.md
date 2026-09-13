# Decision log

Why llama-warden is built the way it is. One file per decision: the date, what was chosen, what was rejected, and what it was measured against.

These are records, not instructions. The *rule* a decision produced lives in [CLAUDE.md](../../CLAUDE.md), which is read every session; the reasoning lives here, which is read when someone asks "why is this like this" or is about to undo it. Grep this directory before changing something that looks arbitrary.

Nothing here is edited to reflect later changes. A decision that was replaced gets a `Status:` line pointing at the one that replaced it, and the old record stays, because the reason it was wrong is the useful part.

| # | decision | date |
|---|---|---|
| [0001](0001-the-warden-decides-and-says-so.md) | The warden decides, and says so: policy leaves Episteme, and a verdict is pushed | 2026-09-13 |

The records this project came from stay in Episteme's own log: 0023 (the agent holds no policy), 0024 (brake on contention, a pause has an author), 0041 (the agent applies a configuration it is handed), 0042 (one console behind a tray icon). 0001 above supersedes the first and takes half of the second.
