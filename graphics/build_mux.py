#!/usr/bin/env python
# /// script
# requires-python = ">=3.12"
# dependencies = []
# ///
"""The multiplexer candidates for InferMux's icon, and the colours to try them in.

Many models on the left, one card on the right, one route through: the circuit
symbol for a multiplexer, which is what InferMux does to the GPU.

Writes the chosen mark (`MARK`) to `infermux.svg` and the web UI's favicon, and
every candidate to `candidates/index.js`, which `compare.html` loads. Each variant is an SVG
template with four colour slots (`%ACTIVE%`, `%IDLE%`, `%EDGE%`, `%FILL%`), so the
page can paint any variant in any palette without a file per pair.

Every variant is checked against the 16 px tile before it is written, at one
pixel = a sixteenth of the viewBox, 16 units of the full 256:

* no stroke thinner than one pixel,
* adjacent inputs at least one pixel apart, line to line and node to node,
* nothing past the frame, strokes and radii included,
* on an outlined body, the route at least one pixel clear of the body's edges,
* on a knockout, a pixel of body left between each cut and the edge, between two
  cuts, and between an inlaid route and the sides of its cut.

    uv run graphics/build_mux.py
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from itertools import combinations, pairwise
from pathlib import Path

SIZE = 256

# The body: left edge tall, right edge short, centred on the output.
L, R = 84.0, 180.0
LEFT_TOP, LEFT_BOT = 36.0, 220.0
RIGHT_TOP, RIGHT_BOT = 92.0, 164.0
MID = 128.0

STROKE = 16.0
X_IN, X_OUT = 30.0, 226.0
NODE_R = 18.0
EXIT_X = R - 20  # where the route turns into the output, unless the variant says


Pt = tuple[float, float]


@dataclass
class Variant:
    id: str
    title: str
    claim: str
    n: int = 3
    active: int = 0
    body: str = "outline"  # outline | accent-outline | solid | engraved | knockout
    ends: bool = True  # nodes on the inputs
    vertical: bool = False
    idle: str = "grey"  # the waiting inputs: grey | accent | tonal (the accent at IDLE_TONAL opacity)
    card: bool = True  # the output node; without it a knockout's cut runs out of the right edge
    # knockout only
    outside: bool = True  # the lines and nodes outside the body; without them the cuts run through its edges
    notch: float = 0.0  # the waiting inputs cut this far past the left edge, as dead ends
    hole: float = STROKE  # the route's cut; wider than STROKE leaves room to draw the route inside it
    # Where the diagonal ends. At R it runs parallel to the top edge, because the top
    # input sits as far under the left corner as the output does under the right one.
    exit_x: float = EXIT_X
    # Crop the frame to the body, centred, FIT_MARGIN clear of it. Only for a body with
    # nothing outside it, which otherwise sits small and off centre in a frame laid
    # out for inputs and an output.
    fit: bool = False
    # filled in by `build`
    strokes: list[float] = field(default_factory=list)
    extents: list[tuple[float, float, float, float]] = field(default_factory=list)
    holes: list[tuple[list[Pt], float]] = field(default_factory=list)


# How far the outer inputs sit in from the body's corners. Under 34 the route from
# the top input starts its diagonal less than a pixel under the top edge; over 36
# three nodes of radius 18 close to under a pixel apart. Four inputs with nodes fit
# no margin at all in this body, so `four` has none.
INPUT_MARGIN = 36.0
# How far a cut runs past the body's edge when nothing outside covers its end.
OVERRUN = 24.0
FIT_MARGIN = 10.0


def frame(v: Variant) -> tuple[float, float, float]:
    """The viewBox's corner and side. Its sixteenth is one pixel of the 16 px tile."""
    if not v.fit:
        return 0.0, 0.0, SIZE
    x0, y0, x1, y1 = L - STROKE / 2, LEFT_TOP - STROKE / 2, R + STROKE / 2, LEFT_BOT + STROKE / 2
    side = max(x1 - x0, y1 - y0) + 2 * FIT_MARGIN
    return (x0 + x1 - side) / 2, (y0 + y1 - side) / 2, side


def input_ys(n: int) -> list[float]:
    top, bot = LEFT_TOP + INPUT_MARGIN, LEFT_BOT - INPUT_MARGIN
    return [top + i * (bot - top) / (n - 1) for i in range(n)]


IDLE_TONAL = 0.4


def route(ya: float, exit_x: float, left: float = 0.0, right: float = 0.0) -> list[Pt]:
    pts = [(L - left, ya), (L, ya), (exit_x, MID), (R + right, MID)]
    return [q for i, q in enumerate(pts) if i == 0 or q != pts[i - 1]]


def d(pts: list[Pt]) -> str:
    return "M" + " L".join(f"{x:g} {y:g}" for x, y in pts)


def samples(pts: list[Pt]) -> list[Pt]:
    return [(a[0] + (b[0] - a[0]) * i / 40, a[1] + (b[1] - a[1]) * i / 40) for a, b in pairwise(pts) for i in range(41)]


BODY_D = f"M{L:g} {LEFT_TOP:g} L{R:g} {RIGHT_TOP:g} L{R:g} {RIGHT_BOT:g} L{L:g} {LEFT_BOT:g} Z"


def build(v: Variant) -> str:
    out: list[str] = []
    ys = input_ys(v.n)
    ya = ys[v.active]
    idle = [y for i, y in enumerate(ys) if i != v.active]

    def line(pts: list[Pt], colour: str, w: float = STROKE, opacity: float = 1.0) -> None:
        v.strokes.append(w)
        op = f' stroke-opacity="{opacity:g}"' if opacity < 1 else ""
        out.append(f'<path d="{d(pts)}" fill="none" stroke="{colour}"{op} stroke-width="{w:g}" stroke-linecap="round" stroke-linejoin="round"/>')

    def node(x: float, y: float, r: float, colour: str, opacity: float = 1.0) -> None:
        v.extents.append((x - r, y - r, x + r, y + r))
        op = f' fill-opacity="{opacity:g}"' if opacity < 1 else ""
        out.append(f'<circle cx="{x:g}" cy="{y:g}" r="{r:g}" fill="{colour}"{op}/>')

    idle_paint = {"grey": ("%IDLE%", 1.0), "accent": ("%ACTIVE%", 1.0), "tonal": ("%ACTIVE%", IDLE_TONAL)}[v.idle]

    # the body
    v.strokes.append(STROKE)
    v.extents.append((L - STROKE / 2, LEFT_TOP - STROKE / 2, R + STROKE / 2, LEFT_BOT + STROKE / 2))
    if v.body != "knockout":
        fill, edge = {
            "outline": ("%FILL%", "%EDGE%"),
            "accent-outline": ("%FILL%", "%ACTIVE%"),
            "solid": ("%EDGE%", "%EDGE%"),
            "engraved": ("%ACTIVE%", "%ACTIVE%"),
        }[v.body]
        out.append(f'<path d="{BODY_D}" fill="{fill}" stroke="{edge}" stroke-width="{STROKE:g}" stroke-linejoin="round"/>')
    else:
        # A cut whose end nothing covers has to overrun the edge, or it stops short
        # inside the body's stroke and reads as a dent rather than an opening.
        v.holes.append((route(ya, v.exit_x, 0.0 if v.outside else OVERRUN, 0.0 if v.outside and v.card else OVERRUN), v.hole))
        for y in idle if v.notch else []:
            v.holes.append(([(L - OVERRUN, y), (L + v.notch, y)], STROKE))
        cuts = "".join(
            f'<path d="{d(pts)}" fill="none" stroke="#000" stroke-width="{w:g}" stroke-linecap="round" stroke-linejoin="round"/>' for pts, w in v.holes
        )
        out.append(f'<mask id="cut"><rect width="256" height="256" fill="#fff"/>{cuts}</mask>')
        out.append(f'<path d="{BODY_D}" fill="%ACTIVE%" stroke="%ACTIVE%" stroke-width="{STROKE:g}" stroke-linejoin="round" mask="url(#cut)"/>')

    # the waiting inputs, then the one through
    x0 = X_IN if v.ends else X_IN - 10
    if v.outside:
        for y in idle:
            line([(x0, y), (L, y)], idle_paint[0], opacity=idle_paint[1])
    card = v.outside and v.card
    end = [(X_OUT, MID)] if card else []
    inlay = v.body == "knockout" and v.hole > STROKE
    if v.body == "engraved":  # dark on the accent body, accent outside it
        line([(x0, ya), (L, ya)], "%ACTIVE%")
        line(route(ya, v.exit_x)[1:], "%FILL%")
        if card:
            line([(R, MID), (X_OUT, MID)], "%ACTIVE%")
    elif v.body != "knockout" or inlay:
        start = [(x0, ya)] if v.outside else []
        line(start + route(ya, v.exit_x)[1:] + end, "%ACTIVE%")
    elif v.outside:
        line([(x0, ya), (L, ya)], "%ACTIVE%")
        if card:
            line([(R, MID), (X_OUT, MID)], "%ACTIVE%")
    if v.outside:
        if v.ends:
            for i, y in enumerate(ys):
                node(X_IN, y, NODE_R, *(("%ACTIVE%", 1.0) if i == v.active else idle_paint))
        else:
            v.extents.append((x0 - STROKE / 2, ys[0] - STROKE / 2, x0, ys[-1] + STROKE / 2))
        if card:
            node(X_OUT, MID, NODE_R, "%ACTIVE%")

    body = "\n".join(out)
    if v.vertical:  # models along the top, the card at the bottom
        body = f'<g transform="rotate(90 128 128)">\n{body}\n</g>'
    fx, fy, side = frame(v)
    return f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="{fx:g} {fy:g} {side:g} {side:g}" width="256" height="256">\n<title>InferMux</title>\n{body}\n</svg>\n'


def _seg_dist(p: Pt, a: Pt, b: Pt) -> float:
    ax, ay = a
    bx, by = b
    px, py = p
    dx, dy = bx - ax, by - ay
    t = max(0.0, min(1.0, ((px - ax) * dx + (py - ay) * dy) / (dx * dx + dy * dy)))
    return math.hypot(px - (ax + t * dx), py - (ay + t * dy))


TOP_EDGE = ((L, LEFT_TOP), (R, RIGHT_TOP))
BOT_EDGE = ((L, LEFT_BOT), (R, RIGHT_BOT))


def to_edge(pts: list[Pt]) -> float:
    """Closest approach to the slanted edges' centre lines, over the stretch strictly inside the body."""
    inside = [p for p in samples(pts) if L + STROKE < p[0] < R - STROKE]
    return min(min(_seg_dist(p, *TOP_EDGE), _seg_dist(p, *BOT_EDGE)) for p in inside)


def check(v: Variant) -> list[str]:
    """What breaks the 16 px tile. Empty means it passes."""
    bad = []
    fx, fy, side = frame(v)
    px = side / 16
    thin = [w for w in v.strokes if w < px]
    if thin:
        bad.append(f"stroke {min(thin):g} < {px:g}")
    ys = input_ys(v.n)
    gap = min(b - a for a, b in pairwise(ys))
    if gap - STROKE < px:
        bad.append(f"input lines {gap - STROKE:.1f} apart")
    if v.ends and v.outside and gap - 2 * NODE_R < px:
        bad.append(f"input nodes {gap - 2 * NODE_R:.1f} apart")
    for x0, y0, x1, y1 in v.extents:
        if x0 < fx or y0 < fy or x1 > fx + side or y1 > fy + side:
            bad.append(f"leaves the frame: {x0:.0f},{y0:.0f} {x1:.0f},{y1:.0f}")
    if v.body in ("outline", "accent-outline", "engraved"):
        clear = to_edge(route(ys[v.active], v.exit_x)) - STROKE
        if clear < px:
            bad.append(f"route {clear:.1f} from the body's edge")
    if v.body == "knockout":
        # what is left of the body between a cut and its outer edge
        for pts, w in v.holes:
            if any(L + STROKE < x < R - STROKE for x, _ in samples(pts)):
                left = to_edge(pts) + STROKE / 2 - w / 2
                if left < px:
                    bad.append(f"{left:.1f} of body between a cut and the edge")
        # what is left between two cuts
        for (a, wa), (b, wb) in combinations(v.holes, 2):
            sb = samples(b)
            between = min(min(math.dist(p, q) for q in sb) for p in samples(a)) - wa / 2 - wb / 2
            if between < px:
                bad.append(f"cuts {between:.1f} apart")
        if v.exit_x == R:
            top = (RIGHT_TOP - LEFT_TOP) / (R - L)
            cut = (MID - ys[v.active]) / (R - L)
            if abs(top - cut) > 1e-9:
                bad.append(f"cut's slope {cut:.3f} is not the top edge's {top:.3f}")
        if v.hole > STROKE and (v.hole - STROKE) / 2 < px:
            bad.append(f"route {(v.hole - STROKE) / 2:.1f} from its cut's sides")
    return bad


K = "knockout"
VARIANTS = [
    Variant("classic", "Classic", "Three models, an outlined body, one straight route to the card."),
    Variant("four", "Four inputs", "More models than one card holds. Lines only: four nodes do not fit a pixel apart in this body.", n=4, ends=False),
    Variant("solid", "Solid body", "A filled grey body, the route on top. A heavier silhouette.", body="solid"),
    Variant("lines", "No nodes", "Lines only on the input side.", ends=False),
    Variant("vertical", "Vertical", "Turned 90 degrees: models along the top, the card at the bottom.", vertical=True),
    Variant("knockout", "Knockout", "The body in the accent colour, the route cut out of it.", body=K),
    Variant("k-lines", "Knockout, no nodes", "Lines only on the input side.", body=K, ends=False),
    Variant("k-four", "Knockout, four inputs", "Four lines in, one cut through.", body=K, n=4, ends=False),
    Variant("k-vertical", "Knockout, vertical", "Models along the top, the card at the bottom.", body=K, vertical=True),
    Variant("k-notches", "Knockout, dead ends", "The waiting models cut a short way into the body and stop. The chosen one cuts all the way through.", body=K, notch=20),
    Variant("k-bare", "Knockout, body only", "Nothing outside the body. The route is a cut from edge to edge: the fewest parts, the biggest at 16 px.", body=K, outside=False),
    Variant("k-bare-notches", "Knockout, body only, dead ends", "Body only, with the waiting models as short cuts that stop. The cut runs parallel to the top edge. The chosen mark.", body=K, outside=False, notch=20, exit_x=R, fit=True),
    Variant("k-inlay", "Knockout, inlaid route", "A wide cut with the route drawn inside it, so the route keeps the accent colour and still reads as a cut.", body=K, hole=48),
    Variant("k-inlay-bare", "Knockout, inlaid, body only", "The inlaid route with nothing outside the body.", body=K, hole=48, outside=False),
    Variant("k-mono", "Knockout, one colour", "Everything in the accent. The waiting models are told apart by stopping at the body; the chosen one cuts through.", body=K, idle="accent"),
    Variant("k-mono-notches", "Knockout, one colour, dead ends", "One colour, and the waiting models also cut a short way in and stop.", body=K, idle="accent", notch=20),
    Variant("k-mono-lines", "Knockout, one colour, no nodes", "One colour, lines only on the input side.", body=K, idle="accent", ends=False),
    Variant("k-tonal", "Knockout, tonal", "The waiting models in the accent at 40%, so the icon is one hue.", body=K, idle="tonal"),
    Variant("k-tonal-notches", "Knockout, tonal, dead ends", "Tonal, with the dead-end cuts.", body=K, idle="tonal", notch=20),
    Variant("k-open", "Knockout, open output", "No card node: the cut runs out of the body's right edge.", body=K, card=False),
    Variant("k-mono-open", "Knockout, one colour, open output", "One colour, and the cut runs out of the right edge.", body=K, idle="accent", card=False),
    Variant("engraved", "Engraved", "An accent body with the route drawn dark on it instead of cut. Differs from the knockout only where the background is not dark: a tile, or white.", body="engraved"),
    Variant("accent-outline", "Accent outline", "The classic, with the body's outline in the accent instead of grey.", body="accent-outline"),
    Variant("accent-outline-tonal", "Accent outline, tonal", "Accent outline, the waiting models at 40%.", body="accent-outline", idle="tonal"),
]

# The accent is the question; the greys are shared. Order is the order on the page.
GREYS = {"idle": "#6B7178", "edge": "#8A929B", "fill": "#1E2226"}
PALETTES = [
    {"id": "sky", "title": "sky (the UI's accent)", "active": "#38BDF8"},
    {"id": "blue", "title": "blue", "active": "#5B8CFF"},
    {"id": "violet", "title": "violet", "active": "#A78BFA"},
    {"id": "fuchsia", "title": "fuchsia", "active": "#E879F9"},
    {"id": "white", "title": "white", "active": "#E8ECF0"},
    {"id": "green", "title": "green (Episteme's)", "active": "#17D98E"},
    {"id": "amber", "title": "amber (the UI's pause)", "active": "#FBBF24"},
]

HERE = Path(__file__).resolve().parent

# The mark, chosen 2026-10-08 from compare.html (README.md says why), and where it goes.
MARK = ("k-bare-notches", "white")
MARK_FILES = [HERE / "infermux.svg", HERE.parent / "webui" / "public" / "favicon.svg"]


def paint(svg: str, palette: dict[str, str]) -> str:
    for slot in ("active", "idle", "edge", "fill"):
        svg = svg.replace(f"%{slot.upper()}%", palette[slot])
    return svg


def main() -> None:
    entries, failed = [], False
    for v in VARIANTS:
        svg = build(v)
        bad = check(v)
        status = "ok" if not bad else "FAILS 16 px: " + "; ".join(bad)
        failed |= bool(bad)
        print(f"{v.id:12} {status}")
        entries.append({"id": v.id, "title": v.title, "claim": v.claim, "svg": svg, "check": bad})
    palettes = [{**GREYS, **p} for p in PALETTES]
    out = HERE / "candidates" / "index.js"
    out.parent.mkdir(exist_ok=True)
    out.write_text(
        "// Generated by graphics/build_mux.py. Edit that, not this.\n"
        f"window.ICONS = {json.dumps({'variants': entries, 'palettes': palettes}, indent=1)};\n",
        encoding="utf-8",
    )
    print(f"wrote {out}")

    variant = next(e for e in entries if e["id"] == MARK[0])
    palette = next(p for p in palettes if p["id"] == MARK[1])
    for path in MARK_FILES:
        path.parent.mkdir(exist_ok=True)
        path.write_text(paint(variant["svg"], palette), encoding="utf-8")
        print(f"wrote {path}")
    if failed:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
