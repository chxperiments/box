"""Line-art figures for the site: white strokes on the black, drawn by CSS
(stroke-dashoffset on pathLength=1) and moved by SMIL, which site.js pauses
under prefers-reduced-motion."""

from math import cos, sin, radians


def iso_cube(cx, cy, s):
    """An isometric cube seen from above: hexagon outline plus the three
    visible inner edges, as one path."""
    k = 0.866 * s
    top, ur, lr = (cx, cy - s), (cx + k, cy - s / 2), (cx + k, cy + s / 2)
    bot, ll, ul = (cx, cy + s), (cx - k, cy + s / 2), (cx - k, cy - s / 2)
    c = (cx, cy)
    pts = lambda *p: " ".join(f"{x:.1f},{y:.1f}" for x, y in p)
    return (f"M{pts(top)} L{pts(ur, lr, bot, ll, ul)} Z "
            f"M{pts(ul)} L{pts(c, ur)} M{pts(c)} L{pts(bot)}")


def hero():
    cx, cy = 300, 300
    layers = [
        (250, "your machine", "dash"),
        (190, "VMM, confined", ""),
        (130, "KVM", ""),
        (70, "guest kernel", ""),
    ]
    paths, labels = [], []
    for i, (s, label, cls) in enumerate(layers):
        paths.append(f'<path class="s {cls}" pathLength="1" style="--i:{i}" d="{iso_cube(cx, cy, s)}"/>')
        # A leader from the cube's right edge to a label on the right.
        y = cy - s / 2 + 6
        x0 = cx + 0.866 * s
        labels.append(
            f'<g class="lbl" style="--i:{i}"><path class="s thin" d="M{x0:.0f},{y:.0f} H560"/>'
            f'<text x="568" y="{y + 4:.0f}">{label}</text></g>')
    # A command travels in from the top-left and lands in the guest.
    route = f"M40,60 C150,60 190,160 {cx},{cy}"
    return f"""<svg class="art draw" viewBox="0 0 720 600" role="img" aria-labelledby="hero-art-t">
  <title id="hero-art-t">Nested boxes: your machine, the confined VMM, KVM and the guest kernel. A command travels inward and runs in the guest.</title>
  <g class="float">
    {''.join(paths)}
    <path class="s thin" d="{route}" stroke-dasharray="2 6"/>
    <rect class="f" x="-5" y="-5" width="10" height="10" opacity="0">
      <animateMotion dur="3.6s" repeatCount="indefinite" path="{route}" keyPoints="0;1;1" keyTimes="0;0.55;1" calcMode="spline" keySplines="0.65 0 0.35 1;0 0 1 1"/>
      <animate attributeName="opacity" values="0;1;1;0;0" keyTimes="0;0.08;0.55;0.62;1" dur="3.6s" repeatCount="indefinite"/>
    </rect>
    <path class="f pulse" d="{iso_cube(cx, cy, 22).split(' M')[0]}"/>
  </g>
  {''.join(labels)}
</svg>"""


def hero_minimal():
    """The nested boxes alone, centred: the first thing on the site."""
    cx, cy = 300, 300
    paths = [
        f'<path class="s{" dash" if i == 0 else ""}" pathLength="1" style="--i:{i}" d="{iso_cube(cx, cy, s)}"/>'
        for i, s in enumerate((250, 190, 130, 70))
    ]
    route = f"M40,60 C150,60 190,160 {cx},{cy}"
    return f"""<svg class="art draw" viewBox="30 30 540 540" role="img" aria-labelledby="hero-art-t">
  <title id="hero-art-t">Nested boxes: your machine, the confined VMM, KVM and the guest kernel. A command travels inward and runs in the guest.</title>
  <g class="float">
    {''.join(paths)}
    <path class="s thin" d="{route}" stroke-dasharray="2 6"/>
    <rect class="f" x="-5" y="-5" width="10" height="10" opacity="0">
      <animateMotion dur="3.6s" repeatCount="indefinite" path="{route}" keyPoints="0;1;1" keyTimes="0;0.55;1" calcMode="spline" keySplines="0.65 0 0.35 1;0 0 1 1"/>
      <animate attributeName="opacity" values="0;1;1;0;0" keyTimes="0;0.08;0.55;0.62;1" dur="3.6s" repeatCount="indefinite"/>
    </rect>
    <path class="f pulse" d="{iso_cube(cx, cy, 22).split(' M')[0]}"/>
  </g>
</svg>"""


def hero_labelled():
    """The centred hero box with its layers named, alternating left and
    right so the composition stays centred on the box."""
    cx, cy = 300, 300
    layers = [(250, "your machine", "l"), (190, "VMM, confined", "r"), (130, "KVM", "l"), (70, "guest kernel", "r")]
    paths, labels = [], []
    for i, (s_, name, side) in enumerate(layers):
        paths.append(f'<path class="s{" dash" if i == 0 else ""}" pathLength="1" style="--i:{i}" d="{iso_cube(cx, cy, s_)}"/>')
        y = cy - s_ / 2 + 4 + i * 22
        if side == "r":
            x0, x1 = cx + 0.866 * s_, 640
            labels.append(f'<g class="lbl" style="--i:{i}"><path class="s thin" d="M{x0:.0f},{y:.0f} H{x1}"/>'
                          f'<circle class="f" cx="{x0:.0f}" cy="{y:.0f}" r="3"/><text class="hl" x="{x1 + 12}" y="{y + 7:.0f}">{name}</text></g>')
        else:
            x0, x1 = cx - 0.866 * s_, -40
            labels.append(f'<g class="lbl" style="--i:{i}"><path class="s thin" d="M{x0:.0f},{y:.0f} H{x1}"/>'
                          f'<circle class="f" cx="{x0:.0f}" cy="{y:.0f}" r="3"/><text class="hl" x="{x1 - 12}" y="{y + 7:.0f}" text-anchor="end">{name}</text></g>')
    route = f"M40,60 C150,60 190,160 {cx},{cy}"
    return f"""<svg class="art draw" viewBox="-260 30 1120 540" role="img" aria-labelledby="hero-art-l">
  <title id="hero-art-l">Nested boxes: your machine, the confined VMM, KVM and the guest kernel. A command travels inward and runs in the guest.</title>
  <g class="float">
    {''.join(paths)}
    <path class="s thin" d="{route}" stroke-dasharray="2 6"/>
    <rect class="f" x="-5" y="-5" width="10" height="10" opacity="0">
      <animateMotion dur="3.6s" repeatCount="indefinite" path="{route}" keyPoints="0;1;1" keyTimes="0;0.55;1" calcMode="spline" keySplines="0.65 0 0.35 1;0 0 1 1"/>
      <animate attributeName="opacity" values="0;1;1;0;0" keyTimes="0;0.08;0.55;0.62;1" dur="3.6s" repeatCount="indefinite"/>
    </rect>
    <path class="f pulse" d="{iso_cube(cx, cy, 22).split(' M')[0]}"/>
  </g>
  {''.join(labels)}
</svg>"""


def architecture():
    cols = [("podman", 300), ("krun", 600), ("firecracker", 900)]
    rows = [
        ("launcher", 160, ["podman, crun", "crun, our spec", "crun jail"]),
        ("VMM", 236, ["libkrun", "libkrun", "Firecracker"]),
        ("confinement", 312, ["6 caps, seccomp", "6 caps, seccomp", "0 caps, seccomp"]),
        ("guest", 464, ["kernel 6.12", "kernel 6.12", "kernel 6.1"]),
    ]
    out = []
    out.append('<rect class="s" pathLength="1" style="--i:0" x="170" y="40" width="860" height="54"/>')
    out.append('<text class="big lbl" x="194" y="74" style="--i:0">box: CLI, SDKs, local API</text>')
    for r, (name, y, cells) in enumerate(rows, start=1):
        out.append(f'<text class="dim lbl" x="20" y="{y + 28}" style="--i:{r}">{name}</text>')
        for (col, x), label in zip(cols, cells):
            out.append(f'<rect class="s" pathLength="1" style="--i:{r}" x="{x - 130}" y="{y}" width="260" height="46"/>')
            out.append(f'<text class="lbl" x="{x - 114}" y="{y + 28}" style="--i:{r}">{label}</text>')
    out.append('<rect class="s" pathLength="1" style="--i:4" x="170" y="388" width="860" height="46"/>')
    out.append('<text class="lbl" x="194" y="416" style="--i:4">KVM: own kernel per sandbox</text>')
    out.append('<text class="dim lbl" x="20" y="416" style="--i:4">hypervisor</text>')
    for i, (col, x) in enumerate(cols):
        out.append(f'<text class="big lbl" x="{x - 130}" y="140" style="--i:1">{col}</text>')
        # The flow runs in the gap beside each column, never through text.
        gx = x + 142
        line = f"M{gx},94 V510"
        out.append(f'<path class="s dash" style="--i:{i}" d="{line}"/>')
        out.append(
            f'<circle class="sig" r="4" opacity="0"><animateMotion dur="2.4s" begin="{i * 0.4}s" repeatCount="indefinite" path="{line}"/>'
            f'<animate attributeName="opacity" values="0;1;1;0" keyTimes="0;0.1;0.85;1" dur="2.4s" begin="{i * 0.4}s" repeatCount="indefinite"/></circle>')
    return f"""<svg class="art draw" viewBox="0 0 1060 530" role="img" aria-labelledby="arch-art-t">
  <title id="arch-art-t">A command flows from the box interface down each backend, through its launcher, VMM and host confinement, across KVM, into the guest.</title>
  {''.join(out)}
</svg>"""


def fork():
    """Fork, try, apply: an agent branches its sandbox into three trials,
    two are thrown away and one is merged back. Loops on CSS timing."""
    ys = {"a": 50, "b": 120, "c": 190}
    out = ['<path class="s trunk" pathLength="1" d="M40,120 H200"/>',
           '<circle class="f" cx="40" cy="120" r="6"/>',
           '<text x="40" y="96" text-anchor="middle">agent</text>',
           '<text x="200" y="104" text-anchor="middle" class="dim">fork</text>']
    for k, y in ys.items():
        out.append(f'<g class="trial t-{k}">'
                   f'<path class="s branch" pathLength="1" d="M200,120 C270,120 290,{y} 360,{y}"/>'
                   f'<rect class="s box" x="360" y="{y - 14}" width="28" height="28"/>'
                   f'<rect class="f run" x="367" y="{y - 7}" width="14" height="14"/>'
                   f'<text x="400" y="{y + 5}">trial-{k}</text>'
                   + ('' if k == "b" else f'<text class="dim tag" x="470" y="{y + 5}">discarded</text>')
                   + '</g>')
    out.append('<path class="s merge" pathLength="1" d="M470,120 C540,120 560,120 640,120"/>'
               '<text class="tag apply-t" x="555" y="104" text-anchor="middle">apply</text>'
               '<circle class="f merge-dot" cx="650" cy="120" r="6"/>'
               '<text class="tag apply-t" x="650" y="150" text-anchor="middle">/data</text>')
    return f"""<svg class="art fork-loop sans" viewBox="0 0 700 240" role="img" aria-labelledby="fork-art-t">
  <title id="fork-art-t">A sandbox forks into three trials. Two are discarded; one is applied back.</title>
  {''.join(out)}
</svg>"""


def security():
    layers = [
        (40, "your machine"),
        (90, "VMM: few or no capabilities, seccomp, own namespaces"),
        (140, "KVM"),
        (190, "guest: root here, only here"),
    ]
    out = []
    for i, (inset, label) in enumerate(layers):
        w, h = 760 - 2 * inset, 420 - 2 * inset
        out.append(f'<rect class="s{" dash" if i == 0 else ""}" pathLength="1" style="--i:{i}" x="{inset}" y="{inset}" width="{w}" height="{h}"/>')
        out.append(f'<text class="lbl{" dim" if i == 0 else ""}" style="--i:{i}" x="{inset + 14}" y="{inset + 24}">{label}</text>')
    checks = ["loopback", "host files", "symlinks", "mounts", "token", "fork bomb"]
    ticks = "".join(
        f'<g class="tick" style="--i:{i}"><path class="s" d="M{250 + i * 52},248 l6,7 l12,-14"/></g>' for i in range(len(checks)))
    return f"""<svg class="art draw" viewBox="0 0 760 420" role="img" aria-labelledby="sec-art-t" style="--scan:180px">
  <title id="sec-art-t">Four nested layers between a workload and your machine, with the escape suite's checks passing.</title>
  {''.join(out)}
  <path class="s scan" d="M200,232 H560"/>
  {ticks}
</svg>"""


def spinning_box(cx, cy, s, frames=24, dur=4.8):
    """A solid box turning on its vertical axis, as SMIL keyframes. The
    silhouette is the union of its faces filled in the signal blue, the one
    blue object on the page; the edges of the faces turned toward the viewer
    are drawn in white. A quarter
    turn repeats exactly, so the loop has no seam."""
    e = radians(28)
    corners = [(x, y, z) for x in (-1, 1) for y in (-1, 1) for z in (-1, 1)]
    def proj(p, t):
        x, y, z = p
        x1, z1 = x * cos(t) + z * sin(t), -x * sin(t) + z * cos(t)
        return cx + s * x1, cy - s * (y * cos(e) + z1 * sin(e))
    # Side faces by outward normal, each as four corners in order; then the top.
    sides = [(1, 0), (0, 1), (-1, 0), (0, -1)]
    def quad(nx, nz):
        if nx:
            return [(nx, -1, -1), (nx, -1, 1), (nx, 1, 1), (nx, 1, -1)]
        return [(-1, -1, nz), (1, -1, nz), (1, 1, nz), (-1, 1, nz)]
    faces = [quad(nx, nz) for nx, nz in sides] + [[(-1, 1, -1), (1, 1, -1), (1, 1, 1), (-1, 1, 1)]]
    fill, edges, vis = [[] for _ in faces], [[] for _ in faces], [[] for _ in faces]
    for k in range(frames + 1):
        t = radians(90 * k / frames)
        for j, f in enumerate(faces):
            d = "M" + " L".join(f"{x:.1f},{y:.1f}" for x, y in (proj(p, t) for p in f)) + " Z"
            fill[j].append(d)
            if j < 4:
                nx, nz = sides[j]
                nz1 = -nx * sin(t) + nz * cos(t)
                vis[j].append("1" if nz1 < -1e-6 else "0")
            else:
                vis[j].append("1")
    def anim(attr, vals):
        return f'<animate attributeName="{attr}" dur="{dur}s" repeatCount="indefinite" values="{";".join(vals)}"/>'
    g = []
    for j in range(5):
        g.append(f'<path class="sig" d="{fill[j][0]}">{anim("d", fill[j])}</path>')
    for j in range(5):
        g.append(f'<path class="cube-edge" d="{fill[j][0]}" stroke-opacity="{vis[j][0]}">'
                 f'{anim("d", fill[j])}{anim("stroke-opacity", vis[j])}</path>')
    return f'<g class="lbl spin-box" style="--i:2">{"".join(g)}</g>'


def workflow():
    """box as a workflow: three ways in, one router, three engines, each
    booting its own microVM. Signals travel every connector."""
    out = []
    W = 1120

    def node(x, y, w, h, title, sub="", i=0):
        cls = "s"
        g = [f'<rect class="{cls}" pathLength="1" style="--i:{i}" x="{x}" y="{y}" width="{w}" height="{h}"/>']
        ty = y + (h / 2 + 6 if not sub else h / 2 - 3)
        g.append(f'<text class="big lbl" style="--i:{i}" x="{x + 18}" y="{ty:.0f}">{title}</text>')
        if sub:
            g.append(f'<text class="dim lbl" style="--i:{i}" x="{x + 18}" y="{ty + 20:.0f}">{sub}</text>')
        return "".join(g)

    def flow(d, i, dur, begin):
        return (f'<path class="s thin" pathLength="1" style="--i:{i}" d="{d}"/>'
                f'<circle class="sig" r="4" opacity="0"><animateMotion dur="{dur}s" begin="{begin}s" repeatCount="indefinite" path="{d}"/>'
                f'<animate attributeName="opacity" values="0;1;1;0" keyTimes="0;0.1;0.85;1" dur="{dur}s" begin="{begin}s" repeatCount="indefinite"/></circle>')

    # Inputs
    inputs = [("CLI", "box run, exec", 70), ("SDKs", "Python, TS, Go, Rust", 230), ("MCP", "for AI agents", 390)]
    for k, (t, sub, y) in enumerate(inputs):
        out.append(node(20, y, 190, 64, t, sub, i=0))
        out.append(flow(f"M210,{y + 32} C300,{y + 32} 310,262 402,262", 1, 2.2, k * 0.5))
    # Router
    out.append(spinning_box(465, 262, 40))
    # Engines
    engines = [("podman", "libkrun via podman", "1.28 s", 70), ("krun", "libkrun, direct", "0.76 s", 230), ("firecracker", "snapshot restore", "0.31 s", 390)]
    for k, (t, sub, ms, y) in enumerate(engines):
        out.append(flow(f"M528,262 C620,262 640,{y + 32} 720,{y + 32}", 3, 2.0, 1.1 + k * 0.45))
        out.append(node(720, y, 200, 64, t, sub, i=4))
        out.append(f'<g class="lbl" style="--i:5"><rect class="f" x="{928}" y="{y + 20}" width="62" height="24"/>'
                   f'<text class="on-strong" x="{959}" y="{y + 37}" text-anchor="middle">{ms}</text></g>')
        # Each engine boots its own microVM: a small box with a kernel inside.
        cx, cy = 1062, y + 32
        out.append(flow(f"M990,{cy} H1032", 6, 1.2, 2.2 + k * 0.45))
        out.append(f'<path class="s" pathLength="1" style="--i:6" d="{iso_cube(cx, cy, 26)}"/>'
                   f'<path class="f pulse" d="{iso_cube(cx, cy, 8).split(" M")[0]}"/>')
    out.append('<text class="dim lbl" style="--i:6" x="1062" y="490" text-anchor="middle">own kernel</text>')
    return f"""<svg class="art draw workflow sans" viewBox="0 0 {W} 500" role="img" aria-labelledby="wf-t">
  <title id="wf-t">Calls from the CLI, the SDKs and MCP go to box, which reads the Boxfile's backend and boots the sandbox with podman, krun or Firecracker, each a microVM with its own kernel.</title>
  {''.join(out)}
</svg>"""


# ---------------------------------------------------------------------------
# Street mark: a marker underline.

def scribble(seed=3):
    """A quick marker underline: two loose passes."""
    import random
    rnd = random.Random(seed)
    a = f"M4,{12 + rnd.uniform(-2, 2):.1f} C60,{4 + rnd.uniform(-2, 2):.1f} 140,{16 + rnd.uniform(-2, 2):.1f} 236,{8 + rnd.uniform(-2, 2):.1f}"
    b = f"M18,{22 + rnd.uniform(-2, 2):.1f} C90,{14 + rnd.uniform(-2, 2):.1f} 160,{24 + rnd.uniform(-2, 2):.1f} 226,{18 + rnd.uniform(-2, 2):.1f}"
    return (f'<svg class="scribble on-view" viewBox="0 0 240 30" aria-hidden="true">'
            f'<path class="s mark" pathLength="1" style="--i:0" d="{a}"/><path class="s mark" pathLength="1" style="--i:2" d="{b}"/></svg>')
