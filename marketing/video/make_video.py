"""box launch film: every frame drawn from scene functions of time.

    venv/bin/python make_video.py out.mp4            # full film
    venv/bin/python make_video.py --still 14.5 f.png # one frame, for checking

Black and white with one signal blue, Geist for display, IBM Plex Mono for
terminals: the site's language, in motion.
"""
import math
import os
import random
import sys

import numpy as np
import skia

W, H, FPS = 1920, 1080, 30
HERE = os.path.dirname(os.path.abspath(__file__))


def argb(r, g, b, a=1.0):
    return skia.ColorSetARGB(int(255 * max(0.0, min(1.0, a))), r, g, b)


def white(a=1.0): return argb(255, 255, 255, a)
def black(a=1.0): return argb(0, 0, 0, a)
def blue(a=1.0): return argb(0x13, 0x00, 0xF9, a)


TF = {k: skia.Typeface.MakeFromFile(os.path.join(HERE, f)) for k, f in {
    "200": "Geist-200.ttf", "300": "Geist-300.ttf", "400": "Geist-400.ttf", "600": "Geist-600.ttf",
    "mono": "PlexMono-Regular.ttf", "mono5": "PlexMono-Medium.ttf"}.items()}


def font(kind, size):
    f = skia.Font(TF[kind], size)
    f.setEdging(skia.Font.Edging.kAntiAlias)
    f.setSubpixel(True)
    return f


def text(c, s, x, y, kind, size, color, align="l", track=0.0):
    """Draw s with its baseline at y. track is letter spacing in em."""
    f = font(kind, size)
    glyphs = f.textToGlyphs(s)
    widths = f.getWidths(glyphs)
    xs, cx = [], 0.0
    for w in widths:
        xs.append(cx)
        cx += w + track * size
    total = cx - track * size if widths else 0
    if align == "c":
        x -= total / 2
    elif align == "r":
        x -= total
    if s:
        blob = skia.TextBlob.MakeFromPosTextH(s, [x + v for v in xs], y, f)
        c.drawTextBlob(blob, 0, 0, skia.Paint(AntiAlias=True, Color=color))
    return total


def rise(c, s, x, y, kind, size, color_fn, t, start, dur=0.55, align="l", track=0.0, dist=28):
    """Text that fades in while rising into place."""
    k = ease_out(prog(t, start, start + dur))
    if k > 0:
        text(c, s, x, y + (1 - k) * dist, kind, size, color_fn(k), align=align, track=track)


def text_w(s, kind, size, track=0.0):
    f = font(kind, size)
    ws = f.getWidths(f.textToGlyphs(s))
    return sum(ws) + track * size * max(0, len(ws) - 1)


# --- timing helpers ------------------------------------------------------

def clamp(v, a=0.0, b=1.0): return max(a, min(b, v))
def prog(t, a, b): return clamp((t - a) / (b - a)) if b > a else float(t >= a)
def ease_out(x): return 1 - (1 - x) ** 3
def ease_in_out(x): return 4 * x ** 3 if x < 0.5 else 1 - (-2 * x + 2) ** 3 / 2
def env(t, a, b, fade=0.4):
    """Scene opacity: fade in after a, fade out before b."""
    return ease_out(prog(t, a, a + fade)) * (1 - ease_in_out(prog(t, b - fade, b)))


def stroke(color, w=2.0, cap=skia.Paint.kRound_Cap):
    return skia.Paint(AntiAlias=True, Color=color, Style=skia.Paint.kStroke_Style, StrokeWidth=w,
                      StrokeCap=cap, StrokeJoin=skia.Paint.kRound_Join)


def fill(color):
    return skia.Paint(AntiAlias=True, Color=color, Style=skia.Paint.kFill_Style)


def partial(c, pts, frac, paint, closed=False):
    """Draw the first frac of a polyline's length: line art drawing itself."""
    if closed:
        pts = pts + [pts[0]]
    segs = [math.dist(pts[i], pts[i + 1]) for i in range(len(pts) - 1)]
    left = sum(segs) * clamp(frac)
    if left <= 0:
        return
    p = skia.Path()
    p.moveTo(*pts[0])
    for i, L in enumerate(segs):
        if left >= L:
            p.lineTo(*pts[i + 1])
            left -= L
        else:
            k = left / L
            p.lineTo(pts[i][0] + (pts[i + 1][0] - pts[i][0]) * k, pts[i][1] + (pts[i + 1][1] - pts[i][1]) * k)
            break
    c.drawPath(p, paint)


def bezier(p0, p1, p2, p3, n=40):
    out = []
    for i in range(n + 1):
        t = i / n
        a, b, cc, d = (1 - t) ** 3, 3 * (1 - t) ** 2 * t, 3 * (1 - t) * t ** 2, t ** 3
        out.append((a * p0[0] + b * p1[0] + cc * p2[0] + d * p3[0], a * p0[1] + b * p1[1] + cc * p2[1] + d * p3[1]))
    return out


# --- shapes --------------------------------------------------------------

def iso_points(cx, cy, s):
    k = 0.866 * s
    top, ur, lr = (cx, cy - s), (cx + k, cy - s / 2), (cx + k, cy + s / 2)
    bot, ll, ul = (cx, cy + s), (cx - k, cy + s / 2), (cx - k, cy - s / 2)
    return [top, ur, lr, bot, ll, ul], [[ul, (cx, cy), ur], [(cx, cy), bot]]


def iso_cube(c, cx, cy, s, frac, color, w=2.0, solid=None):
    hexa, inner = iso_points(cx, cy, s)
    if solid is not None and frac >= 1:
        p = skia.Path()
        p.addPoly(hexa, True)
        c.drawPath(p, fill(solid))
    paint = stroke(color, w)
    partial(c, hexa, frac, paint, closed=True)
    for seg in inner:
        partial(c, seg, prog(frac, 0.5, 1.0), paint)


def spinning_cube(c, cx, cy, s, theta, face=None, edge=None, ew=3.0):
    """A solid cube turning on its vertical axis: blue faces, white edges."""
    face = face if face is not None else blue()
    edge = edge if edge is not None else white()
    e = math.radians(28)

    def proj(p):
        x, y, z = p
        x1, z1 = x * math.cos(theta) + z * math.sin(theta), -x * math.sin(theta) + z * math.cos(theta)
        return cx + s * x1, cy - s * (y * math.cos(e) + z1 * math.sin(e))

    sides = [(1, 0), (0, 1), (-1, 0), (0, -1)]
    def quad(nx, nz):
        if nx:
            return [(nx, -1, -1), (nx, -1, 1), (nx, 1, 1), (nx, 1, -1)]
        return [(-1, -1, nz), (1, -1, nz), (1, 1, nz), (-1, 1, nz)]
    faces = [quad(*sd) for sd in sides] + [[(-1, 1, -1), (1, 1, -1), (1, 1, 1), (-1, 1, 1)]]
    for f in faces:
        p = skia.Path()
        p.addPoly([proj(v) for v in f], True)
        c.drawPath(p, fill(face))
    for j, f in enumerate(faces):
        if j < 4:
            nx, nz = sides[j]
            if -nx * math.sin(theta) + nz * math.cos(theta) >= -1e-6:
                continue
        p = skia.Path()
        p.addPoly([proj(v) for v in f], True)
        c.drawPath(p, stroke(edge, ew))


def card(c, x, y, w, h, color=None):
    c.drawRect(skia.Rect(x, y, x + w, y + h), fill(color if color is not None else white()))


# --- background: the site's fading dot field -----------------------------

def make_dots():
    s = skia.Surface(W, H)
    c = s.getCanvas()
    c.clear(black())
    R = math.hypot(W, H) * 0.42
    for y in range(11, H, 22):
        for x in range(11, W, 22):
            d = math.hypot(x - W / 2, y - H / 2)
            a = 0.16 * max(0.0, 1 - d / R) ** 1.6
            if a > 0.004:
                c.drawCircle(x, y, 1.15, fill(white(a)))
    return s.makeImageSnapshot()


DOTS = None


# --- scenes --------------------------------------------------------------

CY_BOX = 430  # where the blue box appears; the dying screen collapses to it


def hook_layer(t):
    """The terminal on its own layer, so the glitch can tear and fold it."""
    surf = skia.Surface(W, H)
    c = surf.getCanvas()
    c.clear(skia.ColorTRANSPARENT)
    text(c, "02:14  ·  your agent, unattended", 160, 250, "mono", 30, white(0.5 * ease_out(prog(t, 0.1, 0.6))))
    cmd = "rm -rf --no-preserve-root /"
    n = int(len(cmd) * prog(t, 0.7, 2.0))
    x = 160 + text(c, "$ ", 160, 360, "mono5", 54, white(0.55))
    w = text(c, cmd[:n], x, 360, "mono5", 54, white())
    if t < 2.4 and (int(t * 2.4) % 2 == 0 or t < 2.0):
        c.drawRect(skia.Rect(x + w + 6, 318, x + w + 36, 370), fill(white()))
    if t > 2.35:  # the output pours out after Enter
        rng = random.Random(7)
        dirs = ["usr/bin", "usr/lib", "etc", "home/dev/project", "var/lib", "opt", "srv/data", "root", "usr/share"]
        files = ["python3", "ssh", "config", "id_rsa", "main.go", "notes.md", "db.sqlite", ".env", "kubeconfig", "secrets.yaml"]
        lines = [f"removed '/{rng.choice(dirs)}/{rng.choice(files)}'" for _ in range(80)]
        shown = int((t - 2.35) * 70)
        for i, ln in enumerate(lines[max(0, shown - 14):shown]):
            text(c, ln, 160, 450 + i * 40, "mono", 28, white(0.35 + 0.4 * (i / 14)))
    return surf.makeImageSnapshot()


def scene_hook(c, t):
    """0-4.2: an agent, unattended, wipes the machine; the screen tears,
    then switches off like an old set, down to a dot where the box appears."""
    if t >= 4.25:
        return
    img = hook_layer(min(t, 3.2))
    so = skia.SamplingOptions()
    if t < 3.2:
        c.drawImage(img, 0, 0)
        return
    if t < 3.55:
        # a corrupted signal: bands of the picture jump sideways over a blue fringe
        g = ease_out(prog(t, 3.2, 3.35))
        rng = random.Random(int(t * 15))  # a new tear every other frame
        tint = skia.Paint(ColorFilter=skia.ColorFilters.Blend(blue(0.85), skia.BlendMode.kSrcIn))
        c.drawImage(img, 9 * g + rng.uniform(-3, 3), rng.uniform(-2, 2), so, tint)
        y0 = 0
        while y0 < H:
            h = rng.randint(24, 140)
            dx = rng.uniform(-48, 48) * g if rng.random() < 0.45 else 0
            c.drawImageRect(img, skia.Rect(0, y0, W, y0 + h), skia.Rect(dx, y0, W + dx, y0 + h), so, None)
            y0 += h
        return
    # switch-off: fold to a bright line, the line to a dot, the dot out
    k = ease_in_out(prog(t, 3.55, 3.8))
    if k < 1:
        c.save()
        c.translate(0, CY_BOX)
        c.scale(1, max(0.004, 1 - k))
        c.translate(0, -CY_BOX)
        c.drawImage(img, 0, 0)
        c.restore()
    line = ease_out(prog(t, 3.62, 3.8))
    if line > 0:
        w = W * 0.82 * (1 - ease_in_out(prog(t, 3.8, 4.02)))
        a = 1 - ease_in_out(prog(t, 4.02, 4.22))
        if w > 6:
            c.drawRect(skia.Rect(W / 2 - w / 2, CY_BOX - 1.5, W / 2 + w / 2, CY_BOX + 1.5), fill(white(line * a)))
        c.drawCircle(W / 2, CY_BOX, 4 + 3 * prog(t, 3.8, 4.02), fill(white(line * a)))


def scene_turn(c, t):
    """4.2-7.8: relax, it was in a box."""
    a = env(t, 4.2, 7.8, 0.35)
    if a <= 0:
        return
    c.saveLayerAlpha(None, int(255 * a))
    grow = ease_out(prog(t, 4.2, 4.9))
    spinning_cube(c, W / 2, CY_BOX, 120 * grow, t * 0.9)
    text(c, "Relax.", W / 2, 720, "300", 72, white(ease_out(prog(t, 4.7, 5.2))), align="c", track=-0.02)
    line = "It was in a box."
    text(c, line, W / 2, 820, "200", 96, white(ease_out(prog(t, 5.3, 5.9))), align="c", track=-0.035)
    # marker stroke under "box"
    full = text_w(line, "200", 96, -0.035)
    start = W / 2 - full / 2 + text_w("It was in a ", "200", 96, -0.035)
    bw = text_w("box", "200", 96, -0.035)
    k = ease_out(prog(t, 6.0, 6.6))
    if k > 0:
        pts = bezier((start - 6, 850), (start + bw * 0.3, 842), (start + bw * 0.7, 858), (start + bw + 8, 846))
        partial(c, pts, k, stroke(blue(), 9))
    c.restore()


def scene_brand(c, t):
    """7.8-12.2: the logo draws itself; box; a box for your AI."""
    a = env(t, 7.8, 12.2, 0.35)
    if a <= 0:
        return
    c.saveLayerAlpha(None, int(255 * a))
    cx, cy = W / 2, 400
    for i, s in enumerate([230, 172, 116, 62]):
        f = ease_in_out(prog(t, 8.0 + i * 0.3, 9.0 + i * 0.3))
        iso_cube(c, cx, cy, s, f, white(0.55 + 0.15 * i), 2.2)
    k = ease_out(prog(t, 9.6, 10.0))
    if k > 0:
        hexa, inner = iso_points(cx, cy, 20 * k)
        p = skia.Path()
        p.addPoly(hexa, True)
        c.drawPath(p, fill(white()))
    rise(c, "box", W / 2, 800, "600", 150, lambda k: white(k), t, 10.0, 0.6, align="c", track=-0.04)
    rise(c, "A box for your AI.", W / 2, 890, "300", 56, lambda k: white(0.8 * k), t, 10.4, 0.6, align="c", track=-0.02)
    c.restore()


def scene_terminal(c, t):
    """12.2-19.4: wipe it, run again, it's fresh, with its own kernel."""
    a = env(t, 12.2, 19.4, 0.35)
    if a <= 0:
        return
    c.saveLayerAlpha(None, int(255 * a))
    x0, y0, w, h = 120, 230, 1080, 620
    card(c, x0, y0, w, h)
    c.drawLine(x0, y0 + 64, x0 + w, y0 + 64, stroke(black(0.15), 1.5))
    text(c, "~/agent", x0 + 32, y0 + 42, "mono", 24, black(0.5))
    spinning_cube(c, x0 + w - 48, y0 + 32, 13, t * 1.2, face=blue(), edge=white(), ew=1.5)
    rows = [
        (12.6, 13.5, "$ ", "box run agent -- rm -rf --no-preserve-root /", None),
        (13.7, None, "", "exit 0  ·  41 ms  ·  that VM is gone", "dim"),
        (14.3, 14.9, "$ ", "box run agent -- ls /", None),
        (15.1, None, "", "bin  data  dev  etc  home  lib  root  tmp  usr  var", "out"),
        (15.7, 16.2, "$ ", "box run agent -- uname -r", None),
        (16.4, None, "", "6.12.91", "kernel"),
    ]
    y = y0 + 140
    for start, end, prompt, body, kind in rows:
        if t < start:
            break
        if end is None:
            k = ease_out(prog(t, start, start + 0.25))
            col = black(0.5 * k) if kind == "dim" else black(0.85 * k)
            wtxt = text(c, body, x0 + 32, y, "mono", 30, col)
            if kind == "kernel":
                text(c, "  ← its own kernel, not yours", x0 + 32 + wtxt, y, "mono", 30, blue(k))
        else:
            n = int(len(body) * prog(t, start, end))
            px = x0 + 32 + text(c, prompt, x0 + 32, y, "mono5", 30, black(0.45))
            text(c, body[:n], px, y, "mono5", 30, black())
        y += 74 if kind else 62
    # caption
    k1 = ease_out(prog(t, 12.8, 13.4))
    text(c, "Every command gets", 1290, 470, "300", 58, white(k1), track=-0.03)
    text(c, "a fresh microVM.", 1290, 540, "300", 58, white(k1), track=-0.03)
    k2 = ease_out(prog(t, 15.4, 16.0))
    text(c, "Its own kernel.", 1290, 640, "400", 30, white(0.7 * k2))
    text(c, "Nothing outside changes.", 1290, 682, "400", 30, white(0.7 * k2))
    c.restore()


def scene_speed(c, t):
    """19.4-24.0: fast enough to forget it's there."""
    a = env(t, 19.4, 24.0, 0.35)
    if a <= 0:
        return
    c.saveLayerAlpha(None, int(255 * a))
    text(c, "Fast enough to forget it's there.", W / 2, 280, "300", 64, white(ease_out(prog(t, 19.6, 20.1))), align="c", track=-0.03)
    stats = [(15, "ms", "a command in a running VM", 0), (41, "ms", "a fresh VM from the warm pool", 0),
             (0.3, "s", "a fresh VM restored from a snapshot", 1)]
    for i, (v, unit, label, dec) in enumerate(stats):
        cx = W / 2 + (i - 1) * 560
        st = 20.2 + i * 0.35
        k = ease_out(prog(t, st, st + 0.9))
        if prog(t, st, st + 0.01) <= 0:
            continue
        num = f"{v * k:.{dec}f}"
        nw = text_w(num, "200", 200, -0.04)
        uw = text_w(unit, "300", 70)
        left = cx - (nw + 16 + uw) / 2
        text(c, num, left, 640, "200", 200, white(), track=-0.04)
        text(c, unit, left + nw + 16, 640, "300", 70, white(0.75))
        c.drawLine(cx - 200, 690, cx - 200 + 400 * k, 690, stroke(blue(), 4, skia.Paint.kButt_Cap))
        text(c, label, cx, 750, "400", 28, white(0.65 * k), align="c")
    c.restore()


def scene_fork(c, t):
    """24.0-29.4: let it try three things. Keep one."""
    a = env(t, 24.0, 29.4, 0.35)
    if a <= 0:
        return
    c.saveLayerAlpha(None, int(255 * a))
    text(c, "Let it try three things. Keep one.", W / 2, 230, "300", 64, white(ease_out(prog(t, 24.2, 24.7))), align="c", track=-0.03)
    S, ox, oy = 2.2, 190, 300   # the site's fork figure, scaled up

    def P(x, y): return (ox + x * S, oy + y * S)
    trunk = [P(40, 120), P(200, 120)]
    partial(c, trunk, ease_in_out(prog(t, 24.6, 25.1)), stroke(white(), 3))
    c.drawCircle(*P(40, 120), 11, fill(white()))
    text(c, "agent", *P(40, 92), "400", 30, white(), align="c")
    text(c, "fork", *P(200, 100), "400", 28, white(0.55 * ease_out(prog(t, 25.0, 25.4))), align="c")
    discard = ease_in_out(prog(t, 26.4, 26.9))
    for i, (name, y) in enumerate([("trial-a", 50), ("trial-b", 120), ("trial-c", 190)]):
        chosen = name == "trial-b"
        fade = 1 - 0.75 * discard if not chosen else 1
        pts = bezier(P(200, 120), P(270, 120), P(290, y), P(360, y))
        partial(c, pts, ease_in_out(prog(t, 25.1 + i * 0.12, 25.8 + i * 0.12)), stroke(white(fade), 3))
        k = ease_out(prog(t, 25.7 + i * 0.12, 26.0 + i * 0.12))
        if k > 0:
            bx, by = P(360, y)
            c.drawRect(skia.Rect(bx, by - 32, bx + 64, by + 32), stroke(white(fade * k), 3, skia.Paint.kButt_Cap))
            inner = blue(k) if chosen and t > 26.9 else white(fade * k)
            c.drawRect(skia.Rect(bx + 16, by - 16, bx + 48, by + 16), fill(inner))
            text(c, name, bx + 88, by + 11, "mono", 30, white(fade * k))
            if not chosen:
                text(c, "discarded", bx + 250, by + 11, "mono", 26, white(0.5 * discard))
    m = ease_in_out(prog(t, 27.0, 27.8))
    merge = [P(470, 120), P(650, 120)]
    partial(c, merge, m, stroke(blue(), 5))
    if m >= 1:
        c.drawCircle(*P(650, 120), 13, fill(blue()))
        text(c, "/data", *P(650, 158), "mono", 30, white(), align="c")
    k = ease_out(prog(t, 27.4, 27.9))
    text(c, "you apply", *P(560, 100), "400", 30, blue(k) if k else blue(0), align="c")
    k = ease_out(prog(t, 28.0, 28.5))
    text(c, "Your agent can fork and diff. Only you can apply.", W / 2, 920, "400", 34, white(0.75 * k), align="c")
    c.restore()


def scene_plug(c, t):
    """29.4-32.6: works with the agent you already use."""
    a = env(t, 29.4, 32.6, 0.3)
    if a <= 0:
        return
    c.saveLayerAlpha(None, int(255 * a))
    text(c, "Plugs into the agent you already use.", W / 2, 360, "300", 64, white(ease_out(prog(t, 29.6, 30.1))), align="c", track=-0.03)
    cmd = "claude mcp add box -- box mcp"
    w = text_w("$ " + cmd, "mono5", 40) + 96
    x = W / 2 - w / 2
    card(c, x, 450, w, 110)
    n = int(len(cmd) * prog(t, 30.0, 30.9))
    px = x + 48 + text(c, "$ ", x + 48, 520, "mono5", 40, black(0.45))
    text(c, cmd[:n], px, 520, "mono5", 40, black())
    k = ease_out(prog(t, 31.0, 31.5))
    text(c, "MCP   ·   Python   ·   TypeScript   ·   Go   ·   Rust", W / 2, 680, "mono", 32, white(0.7 * k), align="c")
    c.restore()


def scene_trust(c, t):
    """32.6-35.4: zero escapes, and you can check."""
    a = env(t, 32.6, 35.8, 0.55)
    if a <= 0:
        return
    c.saveLayerAlpha(None, int(255 * a))
    k = ease_out(prog(t, 32.7, 33.2))
    w0 = text_w("0", "200", 400)
    we = max(text_w("escapes", "300", 90, -0.03), text_w("found by the escape suite", "400", 32))
    left = W / 2 - (w0 + 40 + we) / 2
    text(c, "0", left, 620, "200", 400, white(k))
    text(c, "escapes", left + w0 + 40, 540, "300", 90, white(k), track=-0.03)
    text(c, "found by the escape suite", left + w0 + 44, 600, "400", 32, white(0.6 * k))
    checks = ["host loopback", "host files", "symlinks", "read-only mounts", "agent token", "fork bomb"]
    widths = [44 + text_w(ch, "mono", 24) for ch in checks]
    gap = 46
    x = W / 2 - (sum(widths) + gap * (len(checks) - 1)) / 2
    y = 800
    for i, ch in enumerate(checks):
        kk = ease_out(prog(t, 33.3 + i * 0.12, 33.6 + i * 0.12))
        if kk > 0:
            partial(c, [(x, y), (x + 10, y + 12), (x + 30, y - 12)], kk, stroke(blue(), 4))
            text(c, ch, x + 44, y + 9, "mono", 24, white(0.75 * kk))
        x += widths[i] + gap
    k = ease_out(prog(t, 34.2, 34.6))
    text(c, "It ships in the repo. Run it yourself.", W / 2, 930, "400", 32, white(0.7 * k), align="c")
    c.restore()


def scene_end(c, t):
    """35.4-39.0: the end card builds piece by piece as the last scene fades.
    The box lands and "box" rises on the music's downbeat at 35.8, with the
    voice."""
    if t < 35.4:
        return
    grow = ease_out(prog(t, 35.4, 35.85))
    spinning_cube(c, W / 2, 250, 52 * grow, t * 0.9)
    rise(c, "box", W / 2, 520, "600", 170, lambda k: white(k), t, 35.8, 0.6, align="c", track=-0.04, dist=36)
    rise(c, "A box for your AI.", W / 2, 610, "300", 58, lambda k: white(0.85 * k), t, 36.3, 0.6, align="c", track=-0.02)
    cmd = "curl -fsSL https://chxperiments.github.io/box/install.sh | sh"
    w = text_w("$ " + cmd, "mono", 30) + 80
    x = W / 2 - w / 2
    k = ease_out(prog(t, 36.8, 37.3))
    if k > 0:
        dy = (1 - k) * 20
        card(c, x, 690 + dy, w, 84, white(k))
        px = x + 40 + text(c, "$ ", x + 40, 744 + dy, "mono", 30, black(0.45 * k))
        text(c, cmd, px, 744 + dy, "mono", 30, black(k))
    rise(c, "open source  ·  MIT  ·  github.com/chxperiments/box", W / 2, 880, "mono", 26,
         lambda k: white(0.55 * k), t, 37.25, 0.6, align="c", dist=14)


SCENES = [scene_hook, scene_turn, scene_brand, scene_terminal, scene_speed, scene_fork, scene_plug, scene_trust, scene_end]
DUR = 39.0


def frame(t):
    global DOTS
    if DOTS is None:
        DOTS = make_dots()
    s = skia.Surface(W, H)
    c = s.getCanvas()
    c.clear(black())
    if t > 4.2:  # the dot field arrives with the box
        c.saveLayerAlpha(None, int(255 * ease_out(prog(t, 4.2, 5.0))))
        c.drawImage(DOTS, 0, 0)
        c.restore()
    for sc in SCENES:
        sc(c, t)
    return s.makeImageSnapshot().toarray(colorType=skia.kRGBA_8888_ColorType)[:, :, :3]


def main():
    if sys.argv[1] == "--still":
        t, out = float(sys.argv[2]), sys.argv[3]
        skia.Image.fromarray(np.ascontiguousarray(np.dstack([frame(t), np.full((H, W), 255, np.uint8)])),
                             colorType=skia.kRGBA_8888_ColorType).save(out, skia.kPNG)
        return
    import imageio_ffmpeg
    out = sys.argv[1]
    w = imageio_ffmpeg.write_frames(out, (W, H), fps=FPS, codec="libx264", pix_fmt_out="yuv420p",
                                    macro_block_size=8, output_params=["-crf", "16", "-preset", "slow", "-movflags", "+faststart"])
    w.send(None)
    n = int(DUR * FPS)
    for i in range(n):
        w.send(np.ascontiguousarray(frame(i / FPS)))
        if i % 150 == 0:
            print(f"frame {i}/{n}", flush=True)
    w.close()
    print("wrote", out)


if __name__ == "__main__":
    main()
