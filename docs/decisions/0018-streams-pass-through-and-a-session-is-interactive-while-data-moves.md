# 0018. Streams pass through unchanged, routed by model; a session is interactive while data moves

- Date: 2026-10-07
- Status: accepted; built and tested 2026-10-07 against stub backends. No real streaming backend has connected
- Rule: A WebSocket upgrade that names a model in `?model=` goes to that model's server at the path and query the client sent, as `/upstream/<model><path>` would, and every byte after the upgrade passes unchanged, binary frames included. A session counts toward the warden only while data moves: it is in flight while a text, binary or continuation frame crossed either way in the last 10 s, and its last data frame counts as an interactive request's end for `interactive_recent_seconds`. An open connection, a ping and a pong count for nothing. Before the warden unloads a model with sessions open, or cancels a batch session, it closes each with 1013 (Try Again Later) and the reason; a config reload closes them with 1012 (Service Restart). The stats record a session as one row.
- Keeps 0004 (the user's own request is never killed: a session moving data defers the unload as a request in flight does), 0006, 4 (the key in `openai-insecure-api-key.<key>`), 0009 (only this host's models are gated and counted), 0014 (timed at the front door) and 0016 (failover only before the answer starts, which for a WebSocket is the upgrade).

## Context

The user, 2026-10-07: "Lets make streaming a first class citizen in infermux. Both audio and everything else." The speech-to-text bench that day (`~/Projects/scratch/streaming-measure/RESULTS.md`) ran WhisperLiveKit, whisper.cpp's server and faster-whisper side by side. WhisperLiveKit streams over WebSockets on `/asr` and on a Deepgram-style `/v1/listen`; OpenAI's realtime clients open `/v1/realtime?model=`.

What was there:

- SSE for `/v1/chat/completions`, `/v1/responses` and `/v1/messages` streamed end to end.
- A WebSocket reached a model only as `/upstream/<model>/...`. `/v1/realtime` is not a llama-swap route, so `/v1/realtime?model=x` answered 404, although the warden and the remote router already read the model from `?model=`.
- The warden counted an open WebSocket as an interactive request in flight for as long as it stayed open (0006, 6). A session left open in a browser tab would keep the models on the card through a game, with no end. When a model was unloaded anyway, its server died under the session, and the client saw the TCP connection drop without a close frame: code 1006, the same as a crash.
- The stats recorded POSTs only, so a session left no row. A text-to-speech reply on `/v1/audio/speech`, audio in chunks of unknown length, was recorded without the time of its first chunk, and had never been checked to arrive chunk by chunk.
- No real realtime client had ever connected (CLAUDE-TODO).

## Decision

The user, 2026-10-07:

1. **Each backend's own streaming protocol, passed through unchanged, routed by model.** InferMux does not translate between protocols. Rejected: one InferMux protocol that each backend is translated into, such as OpenAI's realtime events in front of WhisperLiveKit, which is a codec per backend to keep in step with releases nobody here controls.
2. **A long-lived session counts as interactive only while data moves.** An open but idle session must not stop the warden from yielding the card: `interactive_recent_seconds` (0004) runs from the session's last data, not from the connection being open.
3. **Binary frames pass through as they came.** Whatever the backend speaks reaches the client byte for byte and is never re-encoded.
4. **The stats understand sessions** (0014): one row per session, not per request, with the first transcript delta for speech to text, the first audio chunk for text to speech, bytes each way, the duration, and the time active and idle.

How it is built, the agent's choices:

5. **Any WebSocket upgrade with `?model=` is routed.** Such a request, outside `/upstream/`, `/comfyui/`, `/api/` and `/warden/`, goes to the model as `/upstream/<model><path>?<query>` would, so the backend gets the path and query the client sent: `/v1/realtime?model=x` (OpenAI's realtime), `/v1/listen?model=x` (Deepgram's), `/asr?model=x` (WhisperLiveKit's own). The rewrite sits just in front of llama-swap, inside the warden, the stats and the remote router, which all read the model from `?model=` already. Another host's model is therefore forwarded as `/v1/realtime?model=<its name there>` and rewritten on that host. Rejected: a list of paths, which makes each new backend's path a code change and adds nothing, because `/upstream/<model>/` already reaches every path of a model's server. Rejected: the paths in llama-swap's own route table, an upstream edit paid at every merge (0004). The model has to be in the URL because the backend is chosen before the upgrade. A client that names it later, inside the session, such as OpenAI's `?intent=transcription`, has to add `?model=`.
   Chunked HTTP needs no new route. `/v1/audio/speech` is llama-swap's, and its reverse proxy flushes a reply without Content-Length as each chunk comes, which a test now asserts chunk by chunk. A transcription with `stream: true` answers SSE on llama-swap's `/v1/audio/transcriptions`.
   The key is checked as for any request (0006, 4): Bearer, `x-api-key`, Basic's password, or the subprotocol `openai-insecure-api-key.<key>`. The headers reach the backend as llama-swap passes them for any request, the offered subprotocols included, so a backend can still answer with one of them.
6. **Data is a text, binary or continuation frame, and it moves while one crossed in the last 10 s.** InferMux reads each frame's header in both directions to find the frame boundaries and the opcode, and reads no payload except to find the first output (8). A ping or a pong is not data: a client's keep-alive every 20 s would otherwise keep an idle session interactive forever. While data moves, a session is a request in flight. A priority process (0017), which waits only for requests in flight, therefore does not unload under a user who is speaking, nor while a backend takes a few seconds to answer the last audio: in the bench, faster-whisper's final text came 3.1 to 3.6 s after the audio ended and whisper.cpp's 4.1 to 4.5 s. After 10 s without data a session is idle and counts only through its last data frame, which stamps the interactive clock as a request's end does, so an ordinary yield waits `interactive_recent_seconds` after the last frame. Until the upgrade completes the session is an ordinary request in flight: a client waiting for its model to load is waiting. The 10 s is a constant, not a setting.
   Rejected: a session counted as long as it is open, which is what the user's rule ends. Rejected: a notion per protocol of "the backend is still working", such as an OpenAI response not yet done, which needs a parser per protocol (1). Rejected: control frames as activity. The cost: a keep-alive sent as data, such as Deepgram's `{"type": "KeepAlive"}`, counts as data, because InferMux cannot tell it from audio without reading the protocol.
7. **A session ends with a close code the client can act on.** Before the warden unloads the models with sessions open, which by then are all idle (6), and when it cancels a batch session on a yield, it writes a close frame to the client and closes both connections: 1013 Try Again Later, with the reason, such as `infermux: the GPU was yielded: ComfyUI has 1 job queued`. `/warden/unload` does the same. A config reload (0005, 4) closes them with 1012 Service Restart before it stops the models. A client should reconnect when it has something to send, not at once: opening a session loads its model, so an eager reconnect during a game puts the model back on the card. The frame is written only at a frame boundary toward the client; mid-frame, the connection is closed without one. Rejected: letting the backend's death close the session, which the client sees as 1006, the same as a crash. Rejected: 1001 Going Away, which says the server is going down. Rejected: 1012 for a yield, which tells a client to reconnect at once. Rejected: codes of our own in 4000 to 4999, which a generic client library reports as an unknown failure, and which no client here reads.
8. **A session is one row in the stats** (0014), added when it ends:
   - `ttft_ms` is from the request's arrival to the first output, a model load included, as for a request.
   - `session.upgrade_ms` is from arrival to the 101.
   - `session.first_output_ms` is from the client's first data frame after the upgrade to the backend's first output. Output is a binary frame from the backend, which is audio, or a text frame whose JSON carries text: an event whose `type` ends in `.delta` (OpenAI's realtime transcript, text and audio deltas), an `input_audio_transcription.completed` event, a Deepgram `Results` with a transcript, or a WhisperLiveKit message with text in `lines` or `buffer_transcription`. This is the first transcript delta for speech to text and the first audio chunk for text to speech. Text frames are read for it only until the first output, and only up to 64 KB each.
   - `request_bytes` and `response_bytes` are every byte after the upgrade, client to backend and back, frame headers included.
   - `duration_ms` is from arrival to the end. `session.active_ms` and `session.idle_ms` split the time after the upgrade by 6's rule: the 10 s after each data frame are active, and the rest is idle.
   - `session.close_code` and `session.closed_by` (`client`, `backend` or `infermux`) come from the first close frame either way.
   A session's status is 101; an upgrade the backend or llama-swap refused is a request row with its status. A session's TTFT stays out of the model's TTFT median, which it would distort with the time the user took to start speaking; the summary counts sessions apart and gives `first_output_ms` its own median and 95th percentile.
   A reply of type `audio/*` gets its first chunk as its first output, so the `ttft_ms` of `/v1/audio/speech` is the time to the first audio byte.
9. **Failover only before the upgrade** (0016). A place that answers the upgrade with 502, 503 or 504 is skipped as for any request. Once a backend has answered 101, the session is that place's, and a session that breaks later is the client's to open again.

## Out

- **Translation between protocols** (1).
- **WebRTC.** Its media goes over UDP between peers that negotiate through a signalling server, so it does not pass through an HTTP proxy, and no backend here speaks it.
- **The Responses API over WebSocket.** `streaming-measure` put what it could save at the InferMux hop, 4.6 ms of a 48k-token turn, against 61.6 ms of llama-server's own handling that stays, because InferMux would still send the whole history to llama-server over HTTP.

## Consequences

- llama-swap counts an open session as a request in flight on its model, idle or not. It takes one of the model's `concurrencyLimit` slots (10 unless set) for as long as it is open, a request for another model in the same group queues until it closes, and the TTL never unloads the model under it. The warden's rule does not reach llama-swap's scheduler. Not decided.
- The warden reads an idle session's last data at each tick, so an owed unload is paid up to `poll_seconds` after the window ends, as after a request.
- A session that starts moving data between the warden's check and the unload is closed with it. The window is the time between the check and `UnloadAll`; a request has the same race.
- An unload through llama-swap's own API or UI, and a backend that crashes, still drop a session without a close frame.
- The close frame is sent and both connections are closed at once, without waiting for the client's close in reply.
- A backend that checks the `model` in the query sees the name the client sent, not llama-swap's `useModelName`; for another host's model, its name on that host.
- WhisperLiveKit's final text came up to 20.8 s after the audio ended (p95, on the CPU). A backend that silent for over 10 s while it works can be unloaded by a priority process under the user's own session.
