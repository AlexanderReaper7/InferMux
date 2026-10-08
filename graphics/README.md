# InferMux's mark

A multiplexer's body, white, with one cut through it and two that stop.

The body is the circuit symbol for a multiplexer: a trapezoid, tall on the input side, short on the output side. InferMux is that for the GPU. Many models, one card, one at a time. The cut that runs from the left edge to the right is the model on the card now. The two short cuts from the left are the models waiting, which reach the card and stop.

## Decided 2026-10-08

- **The obelisk in a network is Episteme's, not InferMux's.** It came from Episteme with the warden and went back the same day, with its generator and what was written about it, to Episteme's `graphics/obelisk-net/`. It is in this repository's history up to `7803a13`.
- **The multiplexer, as a knockout, body only, with dead ends**, chosen by the user from the candidates in `compare.html`. Nothing is drawn outside the body: no input lines, no nodes, no output.
- **White, `#E8ECF0`.** The web UI's colours already mean things: emerald is the verdict "running", amber is yielded, pause and batch, red is an error, and sky is the accent for buttons, focus and "ours". White is none of them.
- **The cut runs parallel to the body's top edge**, so the strip of body above it is the same width all the way along. That holds because the cut enters 36 units under the left corner and leaves 36 under the right one; `check` in `build_mux.py` fails the build if the two slopes differ.

## Sized for 16 px

At 16 px one pixel is a sixteenth of the viewBox. `build_mux.py` refuses to write a mark that breaks any of these, and each was seen to fail on a bad input:

- No stroke or cut under a pixel.
- At least a pixel of body between a cut and the edge. The strip above the through cut is 31.1 units, 2.3 px.
- At least a pixel between two cuts. The dead ends run 20 units past the left edge for that reason: 36 brings the middle one within 7.7 units of the through cut.
- Nothing outside the frame.

The mark's viewBox is `22 18 220 220`, not `0 0 256 256`: cropped to the body, centred on it, 10 units clear (`fit`, decided 2026-10-08). In the full frame, which was laid out for the variants with inputs and an output, the body sat 4 units right of centre and was 16% smaller.

## Files

| File | What it is |
| --- | --- |
| `build_mux.py` | The generator. `uv run graphics/build_mux.py` checks every variant, writes the mark, and writes `candidates/index.js`. `MARK` names the chosen variant and colour. |
| `infermux.svg` | The mark. Generated. |
| `../webui/public/favicon.svg` | The same file, as the web UI's favicon. Generated, because `webui/` is all the web UI's nix build sees. |
| `compare.html`, `candidates/index.js` | The candidates, viewed at real pixel sizes, in any colour. Open the HTML file directly. |
