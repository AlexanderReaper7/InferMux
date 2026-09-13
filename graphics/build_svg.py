#!/usr/bin/env python
# /// script
# requires-python = ">=3.12"
# dependencies = []
# ///
"""Distil the 3D truth model into static SVG.

The canvas prototype in Episteme's `graphics/logo/` is the authority on the
geometry; this is a port of a port. Every function below has a counterpart there
under the same name, and the drawing order is the same three passes: the whole
weave, then the solid over it, then the stretches genuinely nearer than the solid
painted again.

This copy was split off when the warden left Episteme, and nothing keeps the two
in step. A change to the shared solid has to be made twice or not at all; what
this file owns outright is the warden's own mark, `warden_icon` below.

What is deliberately dropped, and why each is safe:

* **Ripples.** `field()` returns 0, which is the README's "static SVG is the
  fallback and the base version". Everything downstream of the ripple - the `e`
  energy channel that widened and brightened a strand - collapses to a constant,
  so a run's paint is pure depth fog.
* **Sample density.** The canvas evaluates 300 samples per row because it is
  re-solving the field every frame anyway. Here each run is reduced by
  Ramer-Douglas-Peucker to a screen-space tolerance, so the file is a few hundred
  points rather than thirteen thousand. Polylines, not splines: README §7 wants
  splines for the *animated* deliverable, where the control points are the thing
  being interpolated. For a static mark a polyline under half a pixel of error is
  the same picture.

Known, inherited, and visible in the output: crossings are brighter than either
strand, because two translucent strokes composite additively (README §6.5). The
fix is a mask, not a tweak, and it is not a WIP icon's problem.

    uv run graphics/logo/build_svg.py            # writes the two marks
    uv run graphics/logo/build_svg.py --list     # what it would write
"""

from __future__ import annotations

import argparse
import math
from dataclasses import dataclass
from pathlib import Path

# --- parameters ---------------------------------------------------------------------


@dataclass
class Config:
    """Defaults are `cfg()`'s defaults in the canvas prototype. Anything
    overridden per-mark is stated at
    the call site, so a diff against the prototype is a diff of the call."""

    cell: float = 0.95
    grid_phase: float = 0.5
    obelisk_y: float = 0.0
    elong: float = 2.4
    girth: float = 0.42
    cam_y: float = 0.85
    cam_dist: float = 4.6
    fov: float = 34.0
    gravity: float = 0.55
    well_r: float = 2.2
    exclusion: float = 0.1
    mass_var: float = 0.3
    charge_var: float = 0.3
    weave: str = "#8DA6C0"
    albedo: str = "#9AA2AC"
    roughness: float = 0.75
    sheen: float = 0.06
    light_az: float = 0.9
    edge: float = 0.2

    # Extent of the ground plane. The canvas hard-codes these (README §6.9 files
    # that as a defect against the animation, where they cost 31k samples/frame);
    # here they are per-mark because an icon crops much tighter than a hero.
    ex: float = 14.0
    zf: float = -16.0

    # Emission, with no counterpart in the canvas.
    samples_row: int = 220
    samples_col: int = 200
    tolerance: float = 0.35  # RDP, in px of the emitted viewBox
    stops: int = 6  # gradient stops per run; the canvas uses up to 36
    stroke_scale: float = 1.0
    fog_far: float = 12.0  # added to cam_dist for the far end of the fade
    weave_gain: float = 1.0  # multiplies every strand alpha
    # Vertical framing. The canvas puts the horizon at h/2; an icon wants the
    # obelisk optically centred instead, which is a different number.
    center_y: float = 0.5
    faces: bool = True
    strands: bool = True
    # Net: the hostagent mark's one substitution. See `_net`.
    net: bool = False
    net_color: str = "#17d98e"
    # Over half the height, because that is where the room is: see ICON_CAMERA on
    # why the solid cannot grow, and `console.py`'s NET_BAND, which is this number.
    net_band: tuple[float, float] = (0.46, 0.98)  # where it may be drawn, of height
    net_radius: float = 0.045
    net_edge_w: float = 0.018
    net_upper: int = 5
    net_lower: int = 3
    net_reach: float = 0.34  # an edge joins nodes closer than this in x
    net_dip: float = 0.72  # upper rank's sag toward the lower, 0..1 of the gap
    net_clearance: float = 2.4  # node radii the dip must leave between the ranks


Vec = tuple[float, float, float]


def _rgb(value: str) -> tuple[int, int, int]:
    h = value.lstrip("#")
    return (int(h[0:2], 16), int(h[2:4], 16), int(h[4:6], 16))


def _norm(v: Vec) -> Vec:
    m = math.hypot(*v) or 1.0
    return (v[0] / m, v[1] / m, v[2] / m)


def _cross(p: Vec, q: Vec) -> Vec:
    return (
        p[1] * q[2] - p[2] * q[1],
        p[2] * q[0] - p[0] * q[2],
        p[0] * q[1] - p[1] * q[0],
    )


def _dot(p: Vec, q: Vec) -> float:
    return p[0] * q[0] + p[1] * q[1] + p[2] * q[2]


# --- the scene ----------------------------------------------------------------------


@dataclass
class Sample:
    x: float
    y: float
    d: float
    front: bool


class Scene:
    """One frozen frame of the truth model: camera, solid, and the fabric solver."""

    def __init__(self, k: Config, w: float, h: float) -> None:
        self.k = k
        self.w = w
        self.h = h
        self.hy = k.elong * k.girth
        self.wx = k.girth
        self.wz = k.girth
        self.oy = k.obelisk_y
        self.tip: Vec = (0.0, self.oy - self.hy, 0.0)

        self.cam: Vec = (0.0, k.cam_y, k.cam_dist)
        target: Vec = (0.0, 0.02, 0.0)
        self.fwd = _norm((target[0] - self.cam[0], target[1] - self.cam[1], target[2] - self.cam[2]))
        self.rgt = _norm(_cross(self.fwd, (0.0, 1.0, 0.0)))
        self.upv = _cross(self.rgt, self.fwd)
        self.focal = (h * 0.5) / math.tan(k.fov * math.pi / 360.0)
        self.cxs = w / 2.0
        self.cys = h * k.center_y

        self.planes = [
            (sx / self.wx, sy / self.hy, sz / self.wz)
            for sx in (1, -1)
            for sy in (1, -1)
            for sz in (1, -1)
        ]

    def project(self, p: Vec) -> tuple[float, float, float] | None:
        vx, vy, vz = p[0] - self.cam[0], p[1] - self.cam[1], p[2] - self.cam[2]
        v = (vx, vy, vz)
        d = _dot(v, self.fwd)
        if d < 0.05:
            return None
        xc = _dot(v, self.rgt)
        yc = _dot(v, self.upv)
        return (self.cxs + (xc / d) * self.focal, self.cys - (yc / d) * self.focal, d)

    def sdf(self, p: Vec) -> float:
        f = (
            abs(p[0]) / self.wx
            + abs(p[1] - self.oy) / self.hy
            + abs(p[2]) / self.wz
            - 1.0
        )
        g = math.hypot(1 / self.wx, 1 / self.hy, 1 / self.wz)
        return f / g

    def in_front(self, p: Vec) -> bool:
        """Depth test along the camera ray, exactly as the canvas does it: nothing
        is deleted to look right, a point is either nearer than the solid's near
        surface or it is not."""
        dx, dy, dz = p[0] - self.cam[0], p[1] - self.cam[1], p[2] - self.cam[2]
        length = math.hypot(dx, dy, dz)
        direction = (dx / length, dy / length, dz / length)
        o = (self.cam[0], self.cam[1] - self.oy, self.cam[2])
        t_in, t_out = -math.inf, math.inf
        for n in self.planes:
            a = _dot(n, direction)
            b = 1.0 - _dot(n, o)
            if abs(a) < 1e-9:
                if b < 0:
                    return False
                continue
            t = b / a
            if a > 0:
                t_out = min(t_out, t)
            else:
                t_in = max(t_in, t)
        if not (t_in <= t_out and t_out > 1e-4):
            return False
        return length < t_in

    # mass and charge are FIELDS over the plane, so a row and a column crossing at
    # the same spot resolve to the same 3D point. Load-bearing: per-strand
    # constants make the two families drift apart and the lattice stops reading as
    # one surface.
    @staticmethod
    def _vh(i: int, j: int) -> float:
        v = math.sin(i * 127.1 + j * 311.7) * 43758.5453
        return v - math.floor(v)

    def _noise(self, x: float, z: float, sc: float) -> float:
        bx, bz = x / sc, z / sc
        i, j = math.floor(bx), math.floor(bz)
        fx, fz = bx - i, bz - j
        sx = fx * fx * (3 - 2 * fx)
        sz = fz * fz * (3 - 2 * fz)
        a, b = self._vh(i, j), self._vh(i + 1, j)
        c, d = self._vh(i, j + 1), self._vh(i + 1, j + 1)
        return (a * (1 - sx) + b * sx) * (1 - sz) + (c * (1 - sx) + d * sx) * sz

    def fabric(self, x: float, z: float) -> Sample | None:
        k = self.k
        base = self.project((x, 0.0, z))
        if base is None:
            return None
        mass = 1 + k.mass_var * (self._noise(x, z, 3.6) * 2 - 1)
        charge = 1 + k.charge_var * (self._noise(x + 41, z - 17, 2.9) * 2 - 1)

        p = [x, 0.0, z]
        # GRAVITY: long range, attractive, sourced at the bottom tip
        dx, dy, dz = self.tip[0] - p[0], self.tip[1] - p[1], self.tip[2] - p[2]
        dist = math.hypot(dx, dy, dz) or 1e-3
        mag = (k.gravity * mass) / (1 + (dist / k.well_r) ** 2)
        p = [p[0] + dx / dist * mag, p[1] + dy / dist * mag, p[2] + dz / dist * mag]

        # the ripple term is 0 in a static mark, so nothing is added to p[1] here

        # EXCLUSION: short range, repulsive. The weave is forbidden the solid.
        margin = k.exclusion * charge
        d = self.sdf((p[0], p[1], p[2]))
        if d < margin:
            n = _norm(
                (
                    math.copysign(1.0, p[0] or 1.0) / self.wx,
                    math.copysign(1.0, (p[1] - self.oy) or 1.0) / self.hy,
                    math.copysign(1.0, p[2] or 1.0) / self.wz,
                )
            )
            push = margin - d
            soft = 1.0 if d < 0 else (1 - d / margin) ** 1.5
            p = [p[0] + n[0] * push * soft, p[1] + n[1] * push * soft, p[2] + n[2] * push * soft]

        # order guarantee: in-plane drift capped so nothing reorders laterally,
        # vertical sag capped by what perspective can invert at this depth
        lat = math.hypot(p[0] - x, p[2] - z)
        lat_lim = 0.45 * k.cell
        if lat > lat_lim:
            f = lat_lim / lat
            p[0] = x + (p[0] - x) * f
            p[2] = z + (p[2] - z) * f
        v_lim = (0.8 * k.cam_y * k.cell) / max(1.0, base[2])
        p[1] = max(-v_lim, min(v_lim, p[1]))

        s = self.project((p[0], p[1], p[2]))
        if s is None:
            return None
        return Sample(s[0], s[1], s[2], self.in_front((p[0], p[1], p[2])))

    def facets(self) -> list[tuple[list[tuple[float, float]], str]]:
        """Front-facing triangles of the octahedron, lit from real normals against
        one light. Culling is by facing, so a face never paints over its own."""
        k = self.k
        alb = _rgb(k.albedo)
        el = 0.5
        lv = _norm(
            (
                math.sin(k.light_az) * math.cos(el),
                math.sin(el),
                math.cos(k.light_az) * math.cos(el),
            )
        )
        gloss = 2 + 140 * (1 - k.roughness) ** 3
        out = []
        for sy in (1, -1):
            for sx in (1, -1):
                for sz in (1, -1):
                    a: Vec = (0.0, self.oy + sy * self.hy, 0.0)
                    b: Vec = (sx * self.wx, self.oy, 0.0)
                    c: Vec = (0.0, self.oy, sz * self.wz)
                    n = _norm(
                        _cross(
                            (b[0] - a[0], b[1] - a[1], b[2] - a[2]),
                            (c[0] - a[0], c[1] - a[1], c[2] - a[2]),
                        )
                    )
                    cen = ((a[0] + b[0] + c[0]) / 3, (a[1] + b[1] + c[1]) / 3, (a[2] + b[2] + c[2]) / 3)
                    rc = (cen[0], cen[1] - self.oy, cen[2])
                    if _dot(n, rc) < 0:
                        n = (-n[0], -n[1], -n[2])
                    to_cam = _norm((self.cam[0] - cen[0], self.cam[1] - cen[1], self.cam[2] - cen[2]))
                    if _dot(n, to_cam) <= 0:
                        continue
                    nl = max(0.0, _dot(n, lv))
                    hv = _norm((lv[0] + to_cam[0], lv[1] + to_cam[1], lv[2] + to_cam[2]))
                    amb = 0.16 + 0.14 * k.roughness
                    diff = amb + (1 - amb) * nl
                    spec = k.sheen * max(0.0, _dot(n, hv)) ** gloss
                    cl = [max(0, min(255, round(ch * diff + 255 * spec))) for ch in alb]
                    tri = [self.project(v) for v in (a, b, c)]
                    if any(t is None for t in tri):
                        continue
                    pts = [(t[0], t[1]) for t in tri]  # type: ignore[index]
                    out.append((pts, f"rgb({cl[0]},{cl[1]},{cl[2]})"))
        return out


# --- polyline reduction --------------------------------------------------------------


def rdp(points: list[Sample], tol: float) -> list[Sample]:
    """Ramer-Douglas-Peucker on screen coordinates. Iterative, because a strand
    that is nearly straight recurses to its own length and Python's stack is not
    the place to discover that."""
    if len(points) < 3:
        return points
    keep = [False] * len(points)
    keep[0] = keep[-1] = True
    stack = [(0, len(points) - 1)]
    while stack:
        lo, hi = stack.pop()
        if hi <= lo + 1:
            continue
        ax, ay = points[lo].x, points[lo].y
        bx, by = points[hi].x, points[hi].y
        dx, dy = bx - ax, by - ay
        span = math.hypot(dx, dy)
        worst, at = -1.0, -1
        for i in range(lo + 1, hi):
            px, py = points[i].x, points[i].y
            if span < 1e-9:
                dist = math.hypot(px - ax, py - ay)
            else:
                dist = abs(dy * px - dx * py + bx * ay - by * ax) / span
            if dist > worst:
                worst, at = dist, i
        if worst > tol and at > 0:
            keep[at] = True
            stack.append((lo, at))
            stack.append((at, hi))
    return [p for p, k in zip(points, keep, strict=True) if k]


# --- emission -------------------------------------------------------------------------


def _fmt(v: float) -> str:
    return f"{v:.2f}".rstrip("0").rstrip(".")


class Svg:
    def __init__(self, k: Config, w: float, h: float) -> None:
        self.k = k
        self.w = w
        self.h = h
        self.defs: list[str] = []
        self.body: list[str] = []
        self._gid = 0

    def _fog(self, d: float) -> float:
        k = self.k
        a, b = 1.2, k.cam_dist + k.fog_far
        return max(0.0, min(1.0, (b - d) / (b - a)))

    def run(self, samples: list[Sample]) -> None:
        """One unbroken path per visible run, with the depth fade carried by a
        gradient along it rather than by chopping it into pieces."""
        if len(samples) < 2:
            return
        k = self.k
        weave = _rgb(k.weave)
        fogs = [self._fog(p.d) for p in samples]
        m_base = sum(fogs) / len(fogs)
        if m_base <= 0.02:
            return

        a, z = samples[0], samples[-1]
        horiz = abs(z.x - a.x) >= abs(z.y - a.y)
        span = (z.x - a.x) if horiz else (z.y - a.y)

        def alpha(fog: float) -> float:
            return min(0.95, fog * 0.6 * k.weave_gain)

        if abs(span) < 2:
            paint = f"rgb({weave[0]},{weave[1]},{weave[2]})"
            opacity = f' stroke-opacity="{_fmt(alpha(m_base))}"'
        else:
            self._gid += 1
            gid = f"w{self._gid}"
            stride = max(1, len(samples) // k.stops)
            stops: list[tuple[float, float]] = []
            last = -1.0
            for i in range(0, len(samples), stride):
                off = (samples[i].x - a.x) / span if horiz else (samples[i].y - a.y) / span
                off = max(0.0, min(1.0, off))
                if off <= last:
                    continue
                last = off
                stops.append((off, alpha(fogs[i])))
            if last < 1.0:
                stops.append((1.0, alpha(fogs[-1])))
            # stop-color on every stop, never inherited: SVG's default stop-color
            # is BLACK, so a gradient carrying only stop-opacity paints black on a
            # black page and the whole weave silently disappears.
            rgb = f"rgb({weave[0]},{weave[1]},{weave[2]})"
            body = "".join(
                f'<stop offset="{_fmt(o)}" stop-color="{rgb}" stop-opacity="{_fmt(al)}"/>'
                for o, al in stops
            )
            x1, y1 = (a.x, a.y) if horiz else (a.x, a.y)
            x2, y2 = (z.x, a.y) if horiz else (a.x, z.y)
            self.defs.append(
                f'<linearGradient id="{gid}" gradientUnits="userSpaceOnUse" '
                f'x1="{_fmt(x1)}" y1="{_fmt(y1)}" x2="{_fmt(x2)}" y2="{_fmt(y2)}" '
                f'>{body}</linearGradient>'
            )
            paint = f"url(#{gid})"
            opacity = ""

        pts = rdp(samples, k.tolerance)
        d = "M" + " L".join(f"{_fmt(p.x)} {_fmt(p.y)}" for p in pts)
        width = (0.5 + m_base * 1.3) * k.stroke_scale
        self.body.append(
            f'<path d="{d}" fill="none" stroke="{paint}"{opacity} '
            f'stroke-width="{_fmt(width)}" stroke-linecap="round"/>'
        )

    def polygon(self, pts: list[tuple[float, float]], fill: str, edge: str | None) -> None:
        d = "M" + " L".join(f"{_fmt(x)} {_fmt(y)}" for x, y in pts) + " Z"
        stroke = f' stroke="{edge}" stroke-width="0.8" stroke-linejoin="round"' if edge else ""
        self.body.append(f'<path d="{d}" fill="{fill}"{stroke}/>')

    def document(self, title: str, desc: str) -> str:
        defs = ("<defs>" + "".join(self.defs) + "</defs>\n") if self.defs else ""
        return (
            f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {_fmt(self.w)} {_fmt(self.h)}" '
            f'width="{_fmt(self.w)}" height="{_fmt(self.h)}" role="img" aria-label="{title}">\n'
            f"<title>{title}</title>\n<desc>{desc}</desc>\n"
            f"{defs}" + "\n".join(self.body) + "\n</svg>\n"
        )


def _strand_runs(scene: Scene) -> list[list[Sample | None]]:
    k = scene.k
    zn = k.cam_dist * 0.62
    lines: list[list[Sample | None]] = []
    i_min = math.ceil(k.zf / k.cell - k.grid_phase)
    i_max = math.floor(zn / k.cell - k.grid_phase)
    for i in range(i_min, i_max + 1):
        z = (i + k.grid_phase) * k.cell
        n = k.samples_row
        lines.append([scene.fabric(-k.ex + (2 * k.ex * q) / n, z) for q in range(n + 1)])
    n_cols = math.floor(k.ex / k.cell)
    for j in range(-n_cols - 1, n_cols + 1):
        x = (j + k.grid_phase) * k.cell
        a = scene.project((x, 0.0, zn))
        b = scene.project((x, 0.0, k.zf))
        if a and b and ((a[0] < -20 and b[0] < -20) or (a[0] > scene.w + 20 and b[0] > scene.w + 20)):
            continue
        n = k.samples_col
        lines.append([scene.fabric(x, k.zf + ((zn - k.zf) * q) / n) for q in range(n + 1)])
    return lines


def _emit(svg: Svg, samples: list[Sample | None], front_only: bool) -> None:
    run: list[Sample] = []
    for s in samples:
        if s is not None and (not front_only or s.front):
            run.append(s)
        else:
            svg.run(run)
            run = []
    svg.run(run)


def _net(svg: Svg, scene: Scene) -> None:
    """The warden mark's field, drawn as a network.

    Not a distillation: the solid above it is a port of the 3D model, this is
    screen space and a gesture. It carries the claim the mark is built on, though
    - the upper rank SAGS toward the obelisk's lower tip, so the solid is embedded
    in what it stands in and bends it. The field is a network because that is what
    this program runs.

    Every node is placed against `net_band` with its own radius already
    subtracted, and the sag is clamped against `net_clearance` rather than chosen,
    so neither the frame nor the rank below can be collided with by construction.
    Tuning coordinates until they happen to fit is how the first pass produced
    four arrangements that all clipped.
    """
    k = scene.k
    if not k.net:
        return
    color = _rgb(k.net_color)
    stroke = f"rgb({color[0]},{color[1]},{color[2]})"
    radius = k.net_radius * svg.h
    low_x, high_x = radius, svg.w - radius
    low_y, high_y = k.net_band[0] * svg.h + radius, k.net_band[1] * svg.h - radius
    span = high_y - low_y
    if span <= 0:
        return

    dip = max(0.0, min(k.net_dip, 1.0 - k.net_clearance * radius / span))
    upper: list[tuple[float, float]] = []
    for i in range(k.net_upper):
        t = i / max(1, k.net_upper - 1)
        sag = 1.0 - (2.0 * t - 1.0) ** 2  # level at the edges, deepest under the tip
        upper.append((low_x + t * (high_x - low_x), low_y + sag * dip * span))
    lower = [
        (low_x + (i + 0.5) / k.net_lower * (high_x - low_x), high_y)
        for i in range(k.net_lower)
    ]

    width = k.net_edge_w * svg.h

    def edge(a: tuple[float, float], b: tuple[float, float], alpha: float) -> None:
        svg.body.append(
            f'<path d="M{_fmt(a[0])} {_fmt(a[1])} L{_fmt(b[0])} {_fmt(b[1])}" fill="none" '
            f'stroke="{stroke}" stroke-opacity="{_fmt(alpha)}" '
            f'stroke-width="{_fmt(width)}" stroke-linecap="round"/>'
        )

    for a in lower:
        for b in upper:
            if abs(a[0] - b[0]) < k.net_reach * svg.w:
                edge(a, b, 0.73)
    for a, b in zip(upper, upper[1:]):
        edge(a, b, 0.59)
    for p in upper + lower:
        svg.body.append(
            f'<circle cx="{_fmt(p[0])}" cy="{_fmt(p[1])}" r="{_fmt(radius)}" fill="{stroke}"/>'
        )


def render(k: Config, w: float, h: float, title: str, desc: str) -> str:
    scene = Scene(k, w, h)
    svg = Svg(k, w, h)
    lines = _strand_runs(scene) if k.strands else []

    for samples in lines:  # the whole weave, complete
        _emit(svg, samples, False)
    _net(svg, scene)  # behind the solid, which is what makes it stand IN the net
    if k.faces:
        alb = _rgb(k.albedo)
        edge = None
        if k.edge > 0:
            dk = [round(ch * 0.3) for ch in alb]
            edge = f"rgba({dk[0]},{dk[1]},{dk[2]},{k.edge})"
        for pts, fill in scene.facets():  # the solid covers what it stands in front of
            svg.polygon(pts, fill, edge)
    for samples in lines:  # near stretches restored on top
        _emit(svg, samples, True)
    return svg.document(title, desc)


# --- the marks ------------------------------------------------------------------------

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

# One camera, written once, and kept that way now there is only one mark left in
# this file: two call sites carrying the same four numbers is two call sites that
# eventually carry different ones.
#
# `fov` is 30 rather than the prototype's 34 because these are ICONS: a 16 px tile
# is read at a glance and wants less margin than a scene does. It does not go
# further, and that is measured rather than taste. The solid's lower tip has to
# land IN the field's sag - that contact is the whole claim - and the field is
# already at the bottom of the frame, so the solid's height is bounded by the sag
# above and the frame below. fov 26 (a 16% zoom) drives the tip through the net
# and out the floor. The most that fits is about 4%, which is not worth a number
# nobody can justify later. Room is bought by widening the FIELD instead, which is
# what `net_band` does.
ICON_CAMERA = {"fov": 30.0, "cam_y": 0.9, "cam_dist": 4.6, "center_y": 0.46}


def warden_icon() -> tuple[Path, str]:
    """The local model, WIP. The solid stands in a neural network whose upper
    rank sags toward its lower tip.

    Inherited from Episteme, whose mark is the same solid in a field of plain
    lines, and split from it when the warden became its own project. Divergence
    is allowed now and nothing keeps the two in step: this is the only copy of
    the geometry that the warden's mark is built from."""
    # `ICON_CAMERA` puts the lower tip at 95% of the height, well inside the net's
    # band, so the solid's lower half sits INSIDE the network and hides part of it.
    # Standing clear of the net would say the two are adjacent; the occlusion is
    # the "embedded in the field" claim, made by overlap rather than asserted.
    k = Config(**ICON_CAMERA, strands=False, net=True)
    return HERE / "warden-icon.svg", render(
        k,
        256,
        256,
        "llama-warden",
        "An elongated octahedron standing in a neural network that bends toward its lower tip: the local model the warden keeps.",
    )


MARKS = {"warden": warden_icon}


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print the outputs and write nothing")
    ap.add_argument("--only", choices=sorted(MARKS), help="build one mark")
    args = ap.parse_args()

    for name, build in MARKS.items():
        if args.only and name != args.only:
            continue
        path, content = build()
        if args.list:
            print(f"{name}: {path} ({len(content)} bytes)")
            continue
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")
        print(f"wrote {path} ({len(content)} bytes)")


if __name__ == "__main__":
    main()
