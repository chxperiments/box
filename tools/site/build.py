# Generates the site in docs/ (overview, architecture, security, benchmarks,
# docs). Edit here, run `python3 tools/site/build.py`, commit both. The
# output is plain HTML, so GitHub Pages serves it with no build step.
import os
import sys

sys.path.insert(0, os.path.dirname(__file__))
from common import page, codebox, esc, GH  # noqa: E402
import art  # noqa: E402
from urllib.parse import quote  # noqa: E402

# The prompt visitors hand to their own AI to judge whether box fits.
FIT_PROMPT = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "fit_prompt.txt")).read().strip()


# Answers are plain HTML; the same text, tags stripped, feeds the FAQPage
# structured data, so search results and the page never disagree.
FAQ = [
    ("How is this different from running code in a container?",
     "A container shares your host's kernel, so a kernel bug or a careless <code>mount</code> reaches your machine. box runs each sandbox as a microVM under KVM with its own kernel, and refuses to run one whose kernel matches the host's, which would mean it quietly became a plain container."),
    ("Do I need root?",
     "Only once, to install podman and libkrun and create the <code>krun</code> symlink. After that everything runs as you, with rootless podman. Your user needs write access to <code>/dev/kvm</code> (usually the <code>kvm</code> group), and <code>isolation: strict</code> needs a range in <code>/etc/subuid</code>. <code>box doctor</code> checks all of it and prints the fix."),
    ("Does it work on macOS or Windows?",
     "Linux with KVM is the main target. On macOS it runs through a podman machine, but <code>up</code>/<code>exec</code>, the warm pool, forks and the krun and firecracker backends are Linux-only. On Windows, use WSL2 with <code>/dev/kvm</code> available: the published benchmarks were measured that way."),
    ("How fast is it?",
     "A cold run takes about 1.3 s on podman, 0.75 s on krun and 0.3 s on firecracker. With <code>warm:</code> set, a fresh-VM run takes 20 to 45 ms, and <code>exec</code> into a running sandbox about 15 ms. <a href=\"benchmarks.html\">Method and numbers</a>."),
    ("What is kept between runs?",
     "Only <code>/data</code>, a directory on your host at <code>~/.box/data/&lt;name&gt;/</code>. Every <code>run</code> is a new VM, so installed packages, files elsewhere and processes are gone. <code>up</code> keeps one VM alive when you want state to last across commands."),
    ("Can a sandbox reach the internet or my machine?",
     "With <code>network: bridge</code> it reaches the internet; with <code>network: none</code> it has no network at all. Either way it has its own network namespace and cannot reach services on your host's loopback. It sees only the host directories its Boxfile declares, read-only unless you say <code>rw</code>."),
    ("Can my AI agent break anything?",
     "Not outside its sandbox. Inside, it can do anything, even <code>rm -rf /</code>, and the next run starts clean. Only <code>/data</code> and the host folders you declare are kept, and forks let you review its changes before they reach your data. Use <code>isolation: strict</code>: the VMM around the agent is confined and runs as a UID that is not yours. box is built for your own agents, not for hosting strangers' code, and strict sandboxes share one UID between them. <a href=\"security.html\">The threat model</a> covers what it does not protect."),
    ("Which backend should I use?",
     "Start with <code>podman</code>, the default: it supports everything. <code>krun</code> boots faster but supports <code>isolation: standard</code> only, for now. <code>firecracker</code> is fastest to start but is x86_64-only and has no network, host mounts or forks yet."),
    ("How do I let an agent use it?",
     "Add the MCP server: <code>claude mcp add box -- box mcp</code>. The agent can run, exec, read and write files, and fork, but cannot apply a fork to real data unless you start the server with <code>--allow-apply</code>. Creating and building sandboxes stays with you."),
    ("Is it a hosted service? What does it cost?",
     "Neither. box runs on your laptop or your own server, MIT-licensed and free. Nothing is sent anywhere. It also has no GPU support, so GPU workloads need something else."),
]


def faq_section():
    import json, re
    items = "".join(
        f'<details class="faq-item"><summary><span class="faq-n">{i:02d}</span>'
        f'<span class="faq-q">{q}</span><span class="faq-x" aria-hidden="true"></span></summary>'
        f'<div class="faq-a"><p>{a}</p></div></details>'
        for i, (q, a) in enumerate(FAQ, 1))
    ld = {"@context": "https://schema.org", "@type": "FAQPage", "mainEntity": [
        {"@type": "Question", "name": q,
         "acceptedAnswer": {"@type": "Answer", "text": re.sub(r"<[^>]+>", "", a).replace("&lt;", "<").replace("&gt;", ">")}}
        for q, a in FAQ]}
    return f"""
<section class="section" id="faq">
  <div class="wrap faq">
    <div class="faq-side">
      <h2>Questions, answered.</h2>
      <p class="lead">Still unsure? <a href="#ask">Ask your AI</a> with the prompt above.</p>
    </div>
    <div class="faq-list reveal">{items}</div>
  </div>
  <script type="application/ld+json">{json.dumps(ld)}</script>
</section>
"""


def ask_section():
    """The prompt as a message ready to send: copy it, or open it straight
    in Claude or ChatGPT."""
    q = quote(FIT_PROMPT)
    kb = len(FIT_PROMPT.encode()) / 1024
    return f"""
<section class="section" id="ask">
  <div class="wrap split ask">
    <div class="prose">
      <h2 style="margin-bottom:1rem">Not sure it fits? Ask your AI.</h2>
      <p>This prompt tells an assistant what box does, what it costs, where it stops and how to set it up. Paste it anywhere, describe what you are building, and it will tell you plainly whether box is the right tool. If it is not, the prompt asks it to say what is.</p>
      <ol class="ask-steps"><li>Copy the prompt</li><li>Paste it into your AI</li><li>Answer its questions</li></ol>
    </div>
    <div class="thread reveal">
      <div class="msg msg-you" data-expand>
        <div class="msg-head"><span>you</span><span>{kb:.1f} KB · plain text</span></div>
        <pre class="msg-body" id="fit-prompt">{esc(FIT_PROMPT)}</pre>
        <button class="msg-more" type="button" aria-expanded="false" aria-controls="fit-prompt">Show the whole prompt</button>
      </div>
      <div class="ask-actions">
        <button class="btn btn-primary" type="button" data-copy-from="#fit-prompt">Copy prompt</button>
        <a class="btn btn-ghost" href="https://claude.ai/new?q={q}" target="_blank" rel="noopener">Ask Claude</a>
        <a class="btn btn-ghost" href="https://chatgpt.com/?q={q}" target="_blank" rel="noopener">Ask ChatGPT</a>
      </div>
    </div>
  </div>
</section>
"""

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "docs")

PY, TS, GO, RS, CLI = "py", "ts", "go", "rs", "cli"


def c(s):
    return f'<span class="c">{esc(s)}</span>'


# --------------------------------------------------------------------------
# Measured numbers (bench/bench.py and the backend comparison in README).
BARS = [
    ("podman + krun", "a microVM booted per command", 1618, False),
    ("box run, podman backend", "booted per command", 1411, True),
    ("box run, krun backend", "booted per command, no podman", 1148, True),
    ("podman + runc", "plain container, shares your kernel", 450, False),
    ("box run, firecracker", "a fresh VM from a snapshot, offline", 312, True),
    ("box run, warm pool", "a fresh VM waiting in the pool", 41, True),
    ("box exec", "a command in a running VM", 15, True),
]


def bars():
    top = max(b[2] for b in BARS)
    rows = []
    for name, sub, ms, us in BARS:
        w = max(0.4, ms / top * 100)
        cls = "bar us" if us else "bar"
        rows.append(
            f'<div class="{cls}" role="listitem"><div class="label"><b>{name}</b><span>{sub}</span></div>'
            f'<div class="track" aria-hidden="true"><div class="fill" style="--w:{w:.1f}%"></div></div>'
            f'<div class="ms">{ms:,} ms</div></div>'
        )
    return '<div class="bars" role="list">' + "".join(rows) + "</div>"


# ==========================================================================
# Overview
SDK_SHORT = [
    (PY, "Python", f"""from sdbox import Sandbox

{c('# a persistent microVM, about 10 ms per command')}
with Sandbox("agent") as sb:
    sb.write_file("/data/task.py", code)
    r = sb.exec(["python3", "/data/task.py"])
    print(r.stdout_text, r.exit_code)

{c('# branch /data, try something, keep it or drop it')}
trial = Sandbox("agent").fork("trial")
trial.run("pytest -q").check()
print(trial.diff())
trial.apply()"""),
    (TS, "TypeScript", f"""import {{ Sandbox }} from "box-sdk";

{c('// a persistent microVM, about 10 ms per command')}
await new Sandbox("agent").withUp(async (sb) =&gt; {{
  await sb.writeFile("/data/task.py", code);
  const r = await sb.exec(["python3", "/data/task.py"]);
  console.log(r.stdoutText, r.exitCode);
}});

{c('// branch /data, try something, keep it or drop it')}
const trial = await new Sandbox("agent").fork("trial");
(await trial.run("pytest -q")).check();
console.log(await trial.diff());
await trial.apply();"""),
    (GO, "Go", f"""sb := box.New().Sandbox("agent")
if err := sb.Up(ctx); err != nil {{
	return err
}}
defer sb.Down(ctx)

r, err := sb.Exec(ctx, box.Cmd("python3", "/data/task.py"))
fmt.Println(string(r.Stdout), r.ExitCode)

{c('// branch /data, try something, keep it or drop it')}
trial, _ := sb.Fork(ctx, "trial")
trial.Run(ctx, box.Sh("pytest -q"))
changes, _ := trial.Diff(ctx)
trial.Apply(ctx)"""),
    (RS, "Rust", f"""use sdbox::{{Command, Sandbox}};

let sb = Sandbox::new("agent")?;
sb.up()?;
let out = sb.exec(Command::new(["python3", "/data/task.py"]))?;
println!("{{}} {{}}", out.stdout_text(), out.exit_code);
sb.down()?;

{c('// branch /data, try something, keep it or drop it')}
let trial = sb.fork("trial")?;
trial.run("pytest -q")?.check()?;
println!("{{:?}}", trial.diff()?);
trial.apply()?;"""),
]

BOXFILE_LINES = [
    (None, "# agent: everything this sandbox is", None, None),
    ("base", "base: docker.io/library/alpine:latest",
     "The image every VM of this sandbox starts from. The package manager is inferred from it.",
     "FROM docker.io/library/alpine:latest"),
    ("backend", "backend: firecracker",
     "What boots the VM: podman (default), krun (crun with libkrun, no podman), or firecracker (a snapshot restored per run).",
     "a fresh VM in about 0.3 s, restored from a snapshot"),
    ("isolation", "isolation: strict",
     "The VMM process runs as a subordinate host UID that owns nothing of yours. Recommended for agents.",
     "VMM as host UID 524288, not you"),
    ("cpus / ram_mib", "cpus: 2",
     "Real vCPUs and guest RAM, not cgroup quotas on a shared kernel.",
     "machine-config: 2 vCPU, 1024 MiB"),
    ("cpus / ram_mib", "ram_mib: 1024",
     "Real vCPUs and guest RAM, not cgroup quotas on a shared kernel.",
     "machine-config: 2 vCPU, 1024 MiB"),
    ("network", "network: none",
     "No network device at all. The agent is still reachable: on Firecracker it speaks vsock.",
     "no NIC in the guest"),
    ("readonly", "readonly: true",
     "The guest cannot change its own root. /tmp, /run and /data stay writable.",
     "read-only root disk, no RAM overlay"),
    ("timeout_seconds", "timeout_seconds: 60",
     "A wall-clock limit per command. Everything the command started is killed, and it exits 124.",
     "exit 124 after 60 s"),
    ("packages", "packages: [python3, py3-pip]",
     "Baked in at build time. Names are checked when the Boxfile is parsed, so none can smuggle in a command.",
     "RUN apk add --no-cache python3 py3-pip"),
    ("env", 'env: {PYTHONUNBUFFERED: "1"}',
     "Environment for every command. Keys are identifiers and values single-line.",
     "ENV PYTHONUNBUFFERED=1"),
]


def boxfile_explorer():
    lines = []
    for k, text, note, becomes in BOXFILE_LINES:
        if k is None:
            lines.append(f'<span class="ln">{esc(text)}</span>')
        else:
            lines.append(
                f'<span class="ln" tabindex="0" data-k="{esc(k)}" data-note="{esc(note)}" '
                f'data-becomes="{esc(becomes)}">{esc(text)}</span>'
            )
    return f"""<div class="bf">
  <div class="figure"><div class="bf-code" id="bf">{''.join(lines)}</div></div>
  <div class="figure bf-explain" aria-live="polite">
    <p class="key" id="bf-key"></p>
    <p class="what" id="bf-what"></p>
    <p class="becomes" id="bf-becomes"></p>
  </div>
</div>"""


overview = f"""
<section class="hero hero-center dots dots-center">
  <div class="wrap">
    <div class="hero-art hero-art-wide">{art.hero_labelled()}</div>
    <div class="hero-art hero-art-narrow">{art.hero_minimal()}</div>
    <h1>A box for your AI.</h1>
    {art.scribble(4)}
    <p class="lead">Let your agent install, delete and break things. It does it in its own virtual machine, and nothing outside it changes.</p>
    <div class="install-wrap">
      <div class="install">
        <code><span class="p">$ </span>curl -fsSL https://chxperiments.github.io/bluebox/install.sh | sh</code>
        <button class="copy" type="button" data-copy="curl -fsSL https://chxperiments.github.io/bluebox/install.sh | sh">Copy</button>
      </div>
    </div>
    <div class="cta-row">
      <a class="btn btn-primary" href="docs.html">Read the docs</a>
      <a class="btn btn-ghost" href="architecture.html">How it works</a>
    </div>
  </div>
</section>

<section class="section dots dots-tr" id="speed">
  <div class="wrap">
    <h2>Startup times, measured.</h2>
    {art.scribble(4)}
    <p class="lead">Booting a microVM takes over a second. box boots it before the command arrives, so the command waits milliseconds.</p>
    <div class="numbers reveal">
      <div><b>15<small>ms</small></b><span>a command in a running VM</span></div>
      <div><b>41<small>ms</small></b><span>a fresh VM from the warm pool</span></div>
      <div><b>0.3<small>s</small></b><span>a fresh VM restored from a snapshot</span></div>
      <div><b>0</b><span>escapes found by the escape suite, on any backend</span></div>
    </div>
    <figure class="figure reveal" style="margin-top:1.5rem">
      <div class="figure-body">{bars()}</div>
    </figure>
  </div>
</section>

<section class="section invert wipe dots dots-tr" id="engines">
  <div class="wrap">
    <h2>One interface, three engines.</h2>
    <p class="lead">Every call goes through box. The Boxfile's <code>backend:</code> picks what boots the microVM; all three give the sandbox its own kernel.</p>
    <figure class="figure">
      <div class="figure-body">{art.workflow()}</div>
    </figure>
  </div>
</section>

<section class="section" id="boxfile">
  <div class="wrap">
    <h2>The whole sandbox is one file.</h2>
    {art.scribble(5)}
    <p class="lead">box builds the image, checks that the sandbox really has its own kernel, and refuses to run it if not. Hover over a line to see what it does.</p>
    {boxfile_explorer()}
  </div>
</section>

<section class="opart" aria-label="Generative artwork: nested boxes twisting into a tunnel">
  <canvas aria-hidden="true"></canvas>
  <div class="opart-text"><div class="wrap"><p>Every run, a new machine.</p></div></div>
</section>

<section class="section" id="fork">
  <div class="wrap split">
    <div class="prose">
      <h2 style="margin-bottom:1rem">Branch, review, apply.</h2>
    {art.scribble(3)}
      <p><b>Fork</b> a sandbox and its <code>/data</code> branches in about a quarter of a second. An agent can try several approaches side by side, and the original stays untouched.</p>
      <p><b>Diff</b> lists what a trial changed. Nothing reaches your data until you <b>apply</b> it, and you <b>discard</b> the rest. Agents on MCP can fork and diff, but only you can apply.</p>
      <p><a href="docs.html#sdk-fork">Forks in the docs</a></p>
    </div>
    <div class="art-frame">{art.fork()}</div>
  </div>
</section>

<section class="section dots dots-bl" id="sdk">
  <div class="wrap split">
    <div class="prose">
      <h2 style="margin-bottom:1rem">Sandboxes as a function call.</h2>
      <p>SDKs for Python, TypeScript, Go and Rust. They talk to a local server over a Unix socket only you can open, and start it when needed. For agents, <code>box mcp</code> serves the same tools over the Model Context Protocol.</p>
      <p><a href="docs.html#sdk">SDK reference</a></p>
    </div>
    {codebox("overview-sdk", SDK_SHORT, "SDK language")}
  </div>
</section>

{ask_section()}
{faq_section()}
<section class="section" id="about">
  <div class="wrap split">
    <div class="prose">
      <h2 style="margin-bottom:1rem">An escape suite, in the repo.</h2>
      <p>It runs what an agent gone wrong would try from inside a sandbox: reaching services on your loopback, reading host files, escaping through symlinks and <code>../</code> paths, writing through read-only mounts, reading the agent's token, a fork bomb. Then it inspects the VMM from outside: capabilities, privileges, seccomp, namespaces, limits.</p>
      <p>It passes on every backend in both isolation modes. Run it yourself before you let an agent loose.</p>
      <p><a href="security.html">Read the threat model</a></p>
    </div>
    <div class="next">
      <a href="architecture.html">Architecture<span>How the three backends boot, confine and reach a VM.</span></a>
      <a href="security.html">Security<span>What the boundary is, what it is not, and the tests.</span></a>
      <a href="benchmarks.html">Benchmarks<span>Every number on this site, with method and sources.</span></a>
      <a href="docs.html">Docs<span>Boxfile, CLI, SDKs and the local API.</span></a>
    </div>
  </div>
</section>
"""

# ==========================================================================
# Architecture


LAYER_ROWS = [
    ("interface", "", [("box CLI, SDKs, MCP and local API", "Boxfile, warm pool, up / exec, forks, data import / export, isolation checks")]),
    ("launcher", "", [("podman", "builds images, runs crun"), ("crun", "an OCI spec box writes"), ("crun", "a jail container for the VMM")]),
    ("VMM", "", [("libkrun", "inside crun's process"), ("libkrun", "inside crun's process"), ("Firecracker 1.17", "block, vsock, little else")]),
    ("confinement", "boundary", [("6 capabilities", "seccomp, no_new_privs, own netns, limits"), ("6 capabilities", "seccomp, no_new_privs, own netns, limits"), ("0 capabilities", "seccomp, no_new_privs, read-only jail root")]),
    ("hypervisor", "boundary", [("KVM", "hardware virtualization: every guest runs its own kernel")]),
    ("guest", "guest", [("kernel 6.12", "libkrunfw, init.krun"), ("kernel 6.12", "libkrunfw, init.krun"), ("kernel 6.1", "box as PID 1, RAM overlay")]),
    ("agent channel", "", [("TCP on 127.0.0.1", "token-authenticated"), ("TCP via pasta", "127.0.0.1, token-authenticated"), ("vsock", "owner-only socket, token rotated per copy")]),
    ("/data", "", [("host directory", "shared by virtiofs"), ("host directory", "shared by virtiofs"), ("ext4 disk", "one VM mounts it at a time")]),
]


_cell = 0


def layer_row(label, kind, cells):
    span = ' style="grid-column: span 3"' if len(cells) == 1 else ""
    global _cell
    out = f'<div class="layers-label">{label}</div>'
    for t, sub in cells:
        _cell += 1
        style = f' style="--n:{_cell}' + ('; grid-column: span 3"' if len(cells) == 1 else '"')
        out += f'<div class="layer {kind}"{style}><b>{t}</b><span>{sub}</span></div>'
    return out


def cell(title, sub, extra=""):
    return f'<div class="cell {extra}"><b>{title}</b><span>{sub}</span></div>'


stack = f"""
<div class="layers build">
  <div class="layers-corner"></div>
  {''.join(f'<div class="layers-col"><b>{n}</b><span>{d}</span></div>' for n, d in [("podman", "the default"), ("krun", "libkrun, no podman"), ("firecracker", "a snapshot per run")])}
  {''.join(layer_row(*r) for r in LAYER_ROWS)}
</div>
<div class="legend"><span><i class="b"></i>security boundary</span><span><i class="g"></i>inside the guest</span><span><i></i>host side</span></div>
"""

architecture = f"""
<section class="page-head dots dots-tr">
  <div class="wrap">
    <h1>Architecture</h1>
    <p class="lead">One interface, three ways to boot a microVM. Everything above the launcher is the same on all three; what is below it depends on <code>backend:</code> in the Boxfile.</p>
  </div>
</section>

<section class="section" style="border-top:0;padding-top:0">
  <div class="wrap">
    <figure class="figure" style="margin-bottom:2rem">
      <div class="figure-body">{art.architecture()}</div>
    </figure>
    <figure class="figure">
      <div class="figure-body">{stack}</div>
    </figure>
  </div>
</section>

<section class="section invert wipe">
  <div class="wrap split">
    <div class="prose">
      <h2 style="margin-bottom:1rem">podman and krun: libkrun</h2>
      <p>libkrun is a library that runs a process as a microVM. crun's libkrun handler uses it to boot an OCI image with its own kernel; box verifies that at build by comparing the guest's kernel with the one a plain container sees, and again before every command.</p>
      <p>libkrun's own documentation says the guest and its VMM share one security context: the VMM does the guest's file I/O and opens its network connections. So box treats the VMM's confinement as the boundary behind a libkrun escape, and keeps only the six capabilities virtiofs and low ports need.</p>
      <p>The <code>krun</code> backend writes the OCI spec itself and drives crun directly, removing podman's share of a cold boot (about 0.85 s of 1.4 s), with the same confinement.</p>
    </div>
    <div class="prose">
      <h2 style="margin-bottom:1rem">firecracker: a snapshot per run</h2>
      <p>Firecracker is the VMM AWS built to run other people's code in Lambda and Fargate. It emulates a block device, vsock and very little else, and it does not open the guest's network connections on the host.</p>
      <p>box boots each image once, waits for its agent, and snapshots the VM. Every run restores that snapshot instead of booting a kernel. Before anything runs, the copy is made distinct: the agent token baked into the snapshot is rotated, the guest mixes in fresh randomness and takes the host clock, and only then is <code>/data</code> mounted.</p>
      <p>The VMM runs in a crun container with no capabilities at all and a read-only root holding only its binary, kernel, image, VM directory, disk and <code>/dev/kvm</code>: what Firecracker's jailer provides, without the root it needs.</p>
    </div>
  </div>
</section>

<section class="section">
  <div class="wrap split">
    <div>
      <h2 style="margin-bottom:1rem">A run, from the pool</h2>
      <ol class="steps">
        <li><div><b>Claim</b><span>A waiting VM is taken by renaming its state file; two runs can never get the same one.</span></div></li>
        <li><div><b>Check</b><span>The guest reports its kernel; a VM that answers with the host's is refused.</span></div></li>
        <li><div><b>Execute</b><span>The command runs through the agent, with stdout, stderr, exit code and timeout passed through.</span></div></li>
        <li><div><b>Discard</b><span>The VM is destroyed. A detached tender boots its replacement in the background.</span></div></li>
      </ol>
    </div>
    <div>
      <h2 style="margin-bottom:1rem">A run, from a snapshot</h2>
      <ol class="steps">
        <li><div><b>Start the VMM</b><span>Firecracker starts in its jail container, with the image's memory mapped copy-on-write.</span></div></li>
        <li><div><b>Restore</b><span>The paused VM resumes where its agent was waiting, in tens of milliseconds.</span></div></li>
        <li><div><b>Make it distinct</b><span>Rotate the token, reseed randomness, set the clock, mount <code>/data</code>.</span></div></li>
        <li><div><b>Execute and flush</b><span>Run the command, write <code>/data</code> out, then tear the VM down.</span></div></li>
      </ol>
    </div>
  </div>
</section>

<section class="section">
  <div class="wrap">
    <h2>Data and forks</h2>
    <p class="lead">Everything except <code>/data</code> resets after each run, so a fork only has to branch one directory. That is why it is fast.</p>
    <div class="art-frame" style="margin-bottom:2rem">{art.fork()}</div>
    <div class="table-wrap build">
      <table>
        <thead><tr><th>Operation</th><th>What happens</th><th>Cost</th></tr></thead>
        <tbody>
          <tr><td class="mono">fork</td><td>A new sandbox whose <code>/data</code> is an overlay on the parent's, mounted inside podman's user namespace without root.</td><td class="num">~0.25 s</td></tr>
          <tr><td class="mono">diff</td><td>Read straight from the fork's upper layer, whiteouts and opaque directories included. No VM boots.</td><td class="num">ms</td></tr>
          <tr><td class="mono">apply</td><td>Merges into the parent through an os.Root, so a symlink the guest planted cannot redirect a write.</td><td class="num">ms</td></tr>
          <tr><td class="mono">data export</td><td>Copies <code>/data</code> to a host directory. Symlinks stay links, devices are skipped, setuid bits dropped.</td><td class="num">size-bound</td></tr>
        </tbody>
      </table>
    </div>
  </div>
</section>
"""

# ==========================================================================
# Security

COMMON = ["its own kernel, under KVM", "no_new_privs", "seccomp", "its own network namespace", "pids and memory limits"]
CONF = [
    ("strict mode: VMM as a UID that is not yours", ["yes", "not yet", "yes"]),
    ("host directories shared in", ["/data, declared mounts", "/data, declared mounts", "none: /data is a disk"]),
    ("agent channel", ["TCP on 127.0.0.1 + token", "TCP on 127.0.0.1 + token", "vsock, owner-only socket"]),
]


def conf_table():
    cells = ['<div class="conf-corner"></div>'] + [f'<div class="conf-h">{b}</div>' for b in ("podman", "krun", "firecracker")]
    cells.append('<div class="conf-l">VMM capabilities</div>')
    for n in (6, 6, 0):
        cells.append(f'<div class="conf-big"><b>{n}</b><span>{"of podman\'s 11" if n else "none at all"}</span></div>')
    for label, vals in CONF:
        cells.append(f'<div class="conf-l">{label}</div>')
        for v in vals:
            cls = "conf-v no" if v == "not yet" else "conf-v"
            cells.append(f'<div class="{cls}">{v}</div>')
    cells = "".join(f'{c[:4]} style="--n:{k}"{c[4:]}' if c.startswith("<div") else c for k, c in enumerate(cells))
    common = "".join(f"<span>{c}</span>" for c in COMMON)
    return f"""<div class="conf build">
  <p class="conf-common"><b>Every backend:</b> {common}</p>
  <div class="conf-grid">{cells}</div>
</div>"""


CHECKS = [
    "Guest runs its own kernel, not the host's",
    "No service on host loopback is reachable (127.0.0.1, localhost, gateway, host.containers.internal)",
    "Host paths outside declared mounts are invisible",
    "Symlinks in shared directories resolve inside the guest",
    "No ../ traversal out of a shared directory",
    "Read-only mounts refuse writes, and cannot be remounted writable",
    "The read-only root refuses writes",
    "The agent's token is not visible inside the guest",
    "The agent binary cannot be overwritten",
    "The SDK server's socket is not exposed to the guest",
    "No /dev/kvm in the guest",
    "VMM has no_new_privs and a seccomp filter",
    "VMM holds only the capabilities it needs (none on Firecracker)",
    "VMM runs as a UID that is not yours, under strict",
    "VMM is in its own network namespace",
    "VMM has a pids limit and a memory limit",
    "The host stays responsive during a guest fork bomb",
]

security = f"""
<section class="page-head dots dots-tr">
  <div class="wrap">
    <h1>Security</h1>
    <p class="lead">box assumes the agent inside will do the worst it can, by mistake or because a prompt injection told it to. This page lists what stands between the agent and your machine, and what does not.</p>
  </div>
</section>

<section class="section" style="border-top:0;padding-top:0">
  <div class="wrap split">
    <div class="prose">
      <h2 style="margin-bottom:1rem">Two layers</h2>
      <p><b>The guest kernel.</b> Every sandbox is a microVM under KVM. A kernel exploit inside takes over a guest that is thrown away. box proves the guest has its own kernel at build and checks it again before every command.</p>
      <p><b>The VMM's confinement.</b> A guest that breaks its VMM gets whatever the VMM has. So the VMM gets as little as possible: dropped capabilities, no new privileges, seccomp, its own namespaces, resource limits, only the files the Boxfile declares, and under <code>isolation: strict</code> a host UID that owns nothing of yours.</p>
    </div>
    <div>
      {art.security()}
      <div class="note prose">
      <p><b>Use <code>isolation: strict</code> for agents.</b> Under standard isolation the VMM runs as your user, so escaping both the guest kernel and the VMM lands in your account. Strict maps it to your first subordinate UID instead.</p>
      </div>
    </div>
  </div>
</section>

<section class="section invert wipe">
  <div class="wrap">
    <h2>Confinement, per backend</h2>
    <p class="lead">Read off the running VMM by the escape suite, not from configuration.</p>
    {conf_table()}
  </div>
</section>

<section class="section">
  <div class="wrap">
    <h2>The escape suite</h2>
    {art.scribble(5)}
    <p class="lead">Every check below passes on podman (strict), krun (standard) and Firecracker (strict and standard). Run it yourself:</p>
    <pre class="code" style="margin-bottom:1.75rem">security/escape-test.sh "$(command -v box)" strict firecracker</pre>
    <div class="checks build">{''.join(f'<div style="--n:{i}">{x}</div>' for i, x in enumerate(CHECKS))}</div>
  </div>
</section>

<section class="section">
  <div class="wrap split">
    <div class="prose">
      <h2 style="margin-bottom:1rem">What it is not</h2>
      <ul>
        <li><b>Not egress control, yet.</b> A sandbox with a network can send what it can read. Per-sandbox allowlists are planned.</li>
        <li><b>Not a secrets boundary.</b> Anything in <code>/data</code>, a mount or <code>env:</code> is the workload's. Credential brokering is planned.</li>
        <li><b>Not a boundary between strict sandboxes.</b> They share one subordinate UID.</li>
        <li><b>Not a check on a lying guest.</b> The kernel comparison catches a runtime that fell back to a container, not a guest that fakes <code>uname</code>.</li>
        <li><b>Not multi-tenant.</b> box is one user's tool on their own machine.</li>
      </ul>
    </div>
    <div class="prose">
      <h2 style="margin-bottom:1rem">Guest-written files on the host</h2>
      <p>A guest can put anything in <code>/data</code>: symlinks to <code>/etc/shadow</code>, archive entries with <code>../</code>, device nodes, setuid bits. box's own tools never follow them. Restore, apply and export go through <code>os.Root</code>; exports skip devices and drop setuid bits; strict sandboxes' files are handled inside podman's namespace, where the subordinate UID is reachable and yours is not.</p>
      <p>Reporting: open a private security advisory on <a href="{GH}/security">GitHub</a>. Please do not open a public issue for an escape.</p>
    </div>
  </div>
</section>
"""

# ==========================================================================
# Benchmarks

BENCH = [
    ("host process", "none", "0.8", "19"),
    ("podman + runc", "container", "450", "514"),
    ("podman + krun", "microVM", "1,618", "1,821"),
    ("box run, cold (podman)", "microVM", "1,411", "1,514", True),
    ("box run, warm pool", "microVM, fresh per run", "41", "149", True),
    ("box exec", "microVM, persistent", "15", "62", True),
    ("pydantic-monty, new session", "interpreter subprocess", "0.5", "12"),
]
BACKENDS = [
    ("podman", "1,282", "1,612", "6 caps"),
    ("krun", "756", "1,032", "6 caps"),
    ("firecracker", "312", "312", "0 caps"),
]


def bench_table():
    rows = []
    for r in BENCH:
        us = len(r) > 4
        rows.append(f'<tr{" class=\"us\"" if us else ""}><td>{r[0]}</td><td>{r[1]}</td><td class="num">{r[2]} ms</td><td class="num">{r[3]} ms</td></tr>')
    return f"""<div class="table-wrap build"><table>
<thead><tr><th>Path</th><th>Isolation</th><th class="num">No-op</th><th class="num">Python workload</th></tr></thead>
<tbody>{''.join(rows)}</tbody></table></div>"""


def backend_table():
    rows = "".join(f'<tr><td class="mono">{a}</td><td class="num">{b} ms</td><td class="num">{c_} ms</td><td>{d}</td></tr>' for a, b, c_, d in BACKENDS)
    return f"""<div class="table-wrap build"><table>
<thead><tr><th>Backend</th><th class="num">Fresh-VM run</th><th class="num">up</th><th>VMM capabilities</th></tr></thead>
<tbody>{rows}</tbody></table></div>"""


benchmarks = f"""
<section class="page-head dots dots-tr">
  <div class="wrap">
    <h1>Benchmarks</h1>
    <p class="lead">Where every number on this site comes from, and how to reproduce it. All are medians of repeated runs, timed from the host, on one machine.</p>
  </div>
</section>

<section class="section" style="border-top:0;padding-top:0">
  <div class="wrap">
    <figure class="figure">
      <div class="figure-body">{bars()}</div>
    </figure>
  </div>
</section>

<section class="section invert wipe">
  <div class="wrap">
    <h2>Against the baselines</h2>
    <p class="lead">Median of 10 runs after one warm-up. The Python workload is <code>sum(i * i for i in range(100_000))</code>.</p>
    {bench_table()}
    <p class="small muted" style="margin-top:1rem">Monty is Pydantic's sandboxed Python interpreter, listed for scale: it runs a subset of Python in microseconds, with no VM and no Linux programs. A warm-pool run is measured while the pool refills in the background; with the refill finished it measures 20 to 30 ms.</p>
  </div>
</section>

<section class="section">
  <div class="wrap">
    <h2>Backends, side by side</h2>
    <p class="lead">Same alpine image, 1 vCPU, 512 MiB, <code>network: none</code>, median of 7. podman and krun were measured with networking for <code>up</code>, which they need for their agent.</p>
    {backend_table()}
  </div>
</section>

<section class="section">
  <div class="wrap split">
    <div class="prose">
      <h2 style="margin-bottom:1rem">Method</h2>
      <ul>
        <li>x86_64 Linux under WSL2 with nested KVM, podman 5.8, crun 1.28, libkrun 1.19, Firecracker 1.17.</li>
        <li>Times are from starting the command on the host to having its exit status: what an agent loop waits for.</li>
        <li>One machine. Absolute numbers will differ on yours; the ratios between rows are what carries over.</li>
      </ul>
    </div>
    <div>
      <h2 style="margin-bottom:1rem">Reproduce</h2>
      <pre class="code">python3 bench/bench.py -n 20
security/escape-test.sh ./box strict firecracker</pre>
    </div>
  </div>
</section>
"""

# ==========================================================================
# Docs

def api_code(name, py, ts, go, rs):
    return codebox(f"api-{name}", [(PY, "Python", py), (TS, "TypeScript", ts), (GO, "Go", go), (RS, "Rust", rs)], "SDK language")


BOXFILE_FIELDS = [
    ("base", "image reference", "alpine:latest", "The image every VM starts from."),
    ("backend", "podman, krun, firecracker", "podman", "What boots the VM. See Architecture."),
    ("isolation", "standard, strict", "standard", "strict runs the VMM as a subordinate UID."),
    ("cpus", "1 to 16", "2", "vCPUs."),
    ("ram_mib", "128 and up", "2048", "Guest RAM in MiB."),
    ("network", "bridge, none", "bridge", "Internet access, or none at all."),
    ("readonly", "bool", "false", "Read-only root; /tmp and /data stay writable."),
    ("timeout_seconds", "int", "0", "Per-command limit; exits 124. 0 is unlimited."),
    ("warm", "0 to 8", "0", "VMs kept booted for run (podman, krun)."),
    ("warmup", "shell lines", "", "Run once in each warm VM before it serves a run."),
    ("packages", "names", "", "Installed with the image's package manager."),
    ("run", "shell lines", "", "Extra build steps."),
    ("env", "map", "", "Environment for every command."),
    ("mounts", "host, guest, mode", "", "Host directories shared in (podman, krun); ro by default."),
    ("blueprint", "users, write_files, runcmd", "", "Provisioning applied at build time."),
    ("pkgmgr", "apk, apt, dnf", "inferred", "Override the package manager."),
    ("passt", "bool", "false", "A real NIC with a default route (podman, krun)."),
    ("seccomp", "path", "", "A seccomp profile for the VMM."),
]

CLI = [
    ("new <name> [--from example]", "Create a sandbox, optionally from a shipped example"),
    ("build <name>", "Build the image and prove the sandbox has its own kernel"),
    ("run <name> -- <cmd>", "Run one command in a fresh VM"),
    ("up / down <name>", "Keep a VM running, or stop it"),
    ("exec <name> -- <cmd>", "Run in the running VM; state carries over"),
    ("shell <name>", "Interactive session"),
    ("fork <name> <fork>", "Branch /data"),
    ("diff / apply / discard <fork>", "Review, merge or drop a fork's changes"),
    ("data import / export <name> <dir>", "Move files into or out of /data"),
    ("snapshot / restore <name> [label]", "Archive /data and roll back"),
    ("reset <name>", "Empty /data"),
    ("serve", "The local API the SDKs use"),
    ("doctor", "Check the host setup, with a fix for each failure"),
    ("ls, env, logs, verify", "Inspect sandboxes"),
    ("rename, destroy, nuke", "Remove or rename"),
]

API = [
    ("GET", "/v1/version", ""),
    ("GET", "/v1/sandboxes", "name, up, warm, warm_target, network, base"),
    ("POST", "/v1/sandboxes/{name}/up", ""),
    ("POST", "/v1/sandboxes/{name}/down", ""),
    ("POST", "/v1/sandboxes/{name}/exec", "argv, stdin (base64), timeout_seconds"),
    ("POST", "/v1/sandboxes/{name}/run", "same, in a fresh VM"),
    ("POST", "/v1/sandboxes/{name}/fork", "as"),
    ("GET", "/v1/sandboxes/{name}/diff", "returns changes: [kind, path]"),
    ("POST", "/v1/sandboxes/{name}/apply", "returns changes: n"),
    ("POST", "/v1/sandboxes/{name}/discard", "returns changes: n"),
]


def rows(items, fmt):
    return "".join(fmt(*i) for i in items)


docs = f"""
<section class="page-head invert wipe">
  <div class="wrap">
    <h1>Documentation</h1>
    <p class="lead">Install, define a sandbox, and drive it from the CLI or from code.</p>
  </div>
</section>

<div class="wrap docs">
  <aside class="toc" aria-label="On this page">
    <p>Start</p>
    <a href="#install">Install</a>
    <a href="#quickstart">Quick start</a>
    <p>Reference</p>
    <a href="#boxfile">Boxfile</a>
    <a href="#cli">CLI</a>
    <a href="#sdk">SDKs</a>
    <a href="#sdk-exec">exec and run</a>
    <a href="#sdk-files">Files</a>
    <a href="#sdk-fork">Forks</a>
    <a href="#sdk-errors">Results and errors</a>
    <a href="#mcp">MCP for agents</a>
    <a href="#api">Local API</a>
  </aside>

  <article class="doc">
    <h2 id="install">Install</h2>
    <p>box needs Linux with KVM (or macOS with a podman machine), podman, and crun built with libkrun. The firecracker backend downloads a pinned Firecracker and guest kernel on first use and verifies their SHA-256.</p>
    <pre class="code">curl -fsSL https://chxperiments.github.io/bluebox/install.sh | sh
box doctor          {c('# checks podman, KVM, krun, subordinate UIDs; prints a fix for each failure')}</pre>

    <h2 id="quickstart">Quick start</h2>
    <pre class="code">box new agent --from tiny-python    {c('# a ~60 MB Python sandbox')}
box build agent                     {c('# builds, then proves it has its own kernel')}
box run agent -- python3 -c 'print(6*7)'

box up agent                        {c('# keep one VM running')}
box exec agent -- pip list
box down agent</pre>
    <div class="note"><p>For agents, add <code>isolation: strict</code> to the Boxfile. The <code>tiny-python</code>, <code>tiny-node</code> and <code>ai-agent</code> examples already have it.</p></div>

    <h2 id="boxfile">Boxfile</h2>
    <p>One YAML file per sandbox at <code>~/.box/sandboxes/&lt;name&gt;/Boxfile</code>. Unknown keys are rejected, so a typo fails loudly.</p>
    <div class="table-wrap build"><table>
      <thead><tr><th>Field</th><th>Values</th><th>Default</th><th>Meaning</th></tr></thead>
      <tbody>{rows(BOXFILE_FIELDS, lambda f, v, d, m: f'<tr><td class="mono">{f}</td><td>{v}</td><td class="mono">{d}</td><td>{m}</td></tr>')}</tbody>
    </table></div>

    <h2 id="cli">CLI</h2>
    <div class="table-wrap build"><table>
      <thead><tr><th>Command</th><th>What it does</th></tr></thead>
      <tbody>{rows(CLI, lambda a, b: f'<tr><td class="mono">box {esc(a)}</td><td>{b}</td></tr>')}</tbody>
    </table></div>

    <h2 id="sdk">SDKs</h2>
    <p>Python, TypeScript, Go and Rust. Each talks to <code>box serve</code> over a Unix socket only your user can open, and starts it when nothing is listening. Pick a language once; every sample on this page follows.</p>
    {api_code("install",
      'pip install ./sdk/python          ' + c('# from a box checkout') + '\n\nfrom sdbox import Client, Sandbox',
      'npm install ./sdk/typescript      ' + c('// from a box checkout') + '\n\nimport { Client, Sandbox } from "box-sdk";',
      'go get github.com/chxperiments/bluebox/sdk/go\n\nimport "github.com/chxperiments/bluebox/sdk/go"',
      'cargo add --git https://github.com/chxperiments/bluebox sdbox\n\nuse sdbox::{Client, Command, Sandbox};')}

    <h3 id="sdk-up">up and down</h3>
    <p>Boot a VM once and keep it running for <code>exec</code>. The context-manager forms bring it down afterwards unless it was already up.</p>
    {api_code("up",
      'sb = Sandbox("agent")\nsb.up()\n...\nsb.down()\n\n' + c('# or') + '\nwith Sandbox("agent") as sb:\n    ...',
      'const sb = new Sandbox("agent");\nawait sb.up();\n...\nawait sb.down();\n\n' + c('// or') + '\nawait sb.withUp(async (sb) =&gt; {\n  ...\n});',
      'sb := box.New().Sandbox("agent")\nif err := sb.Up(ctx); err != nil {\n\treturn err\n}\ndefer sb.Down(ctx)',
      'let sb = Sandbox::new("agent")?;\nsb.up()?;\n...\nsb.down()?;')}

    <h3 id="sdk-exec">exec and run</h3>
    <p><code>exec</code> runs in the running VM, so state carries over. <code>run</code> takes a fresh VM and throws it away. A command is a shell string or an argv list; a timeout exits 124.</p>
    {api_code("exec",
      'r = sb.exec(["python3", "-c", "print(6*7)"])\nr = sb.exec("ls -la | head", timeout=10)\nr = sb.exec("wc -c", stdin=b"bytes")\n\nr = Sandbox("agent").run("pytest -q")',
      'let r = await sb.exec(["python3", "-c", "print(6*7)"]);\nr = await sb.exec("ls -la | head", { timeout: 10 });\nr = await sb.exec("wc -c", { stdin: Buffer.from("bytes") });\n\nr = await new Sandbox("agent").run("pytest -q");',
      'r, err := sb.Exec(ctx, box.Cmd("python3", "-c", "print(6*7)"))\nr, err = sb.Exec(ctx, box.Command{\n\tArgv: []string{"sh", "-c", "ls -la | head"}, Timeout: 10 * time.Second})\n\nr, err = sb.Run(ctx, box.Sh("pytest -q"))',
      'let r = sb.exec(Command::new(["python3", "-c", "print(6*7)"]))?;\nlet r = sb.exec(Command::sh("ls -la | head").timeout(10))?;\nlet r = sb.exec(Command::new(["wc", "-c"]).stdin(b"bytes".to_vec()))?;\n\nlet r = Sandbox::new("agent")?.run("pytest -q")?;')}

    <h3 id="sdk-files">Files</h3>
    <p>Reads and writes happen inside the guest, so a path or symlink the sandbox controls can never redirect them onto a host file. For bulk moves, use <code>box data import</code> and <code>export</code>.</p>
    {api_code("files",
      'sb.write_file("/data/task.py", "print(1)")\ndata = sb.read_file("/data/result.json")',
      'await sb.writeFile("/data/task.py", "print(1)");\nconst data = await sb.readFile("/data/result.json");',
      'err := sb.WriteFile(ctx, "/data/task.py", []byte("print(1)"))\ndata, err := sb.ReadFile(ctx, "/data/result.json")',
      'sb.write_file("/data/task.py", "print(1)")?;\nlet data = sb.read_file("/data/result.json")?;')}

    <h3 id="sdk-fork">Forks: fork, diff, apply, discard</h3>
    <p>A fork has its parent's image and a <code>/data</code> overlaid on the parent's. Running it never touches the parent. <code>diff</code> lists changes as <code>A</code> added, <code>M</code> modified, <code>D</code> deleted; <code>apply</code> merges them into the parent; <code>discard</code> drops them. Apply is refused while either side is up.</p>
    {api_code("fork",
      'trial = Sandbox("agent").fork("trial")\ntrial.run("python3 refactor.py").check()\n\nfor ch in trial.diff():\n    print(ch.kind, ch.path)\n\nif tests_pass:\n    trial.apply()       ' + c('# returns how many changes were merged') + '\nelse:\n    trial.discard()',
      'const trial = await new Sandbox("agent").fork("trial");\n(await trial.run("python3 refactor.py")).check();\n\nfor (const ch of await trial.diff()) console.log(ch.kind, ch.path);\n\nif (testsPass) await trial.apply();\nelse await trial.discard();',
      'trial, err := box.New().Sandbox("agent").Fork(ctx, "trial")\ntrial.Run(ctx, box.Sh("python3 refactor.py"))\n\nchanges, _ := trial.Diff(ctx)\nfor _, ch := range changes {\n\tfmt.Println(ch.Kind, ch.Path)\n}\nif testsPass {\n\ttrial.Apply(ctx)\n} else {\n\ttrial.Discard(ctx)\n}',
      'let trial = Sandbox::new("agent")?.fork("trial")?;\ntrial.run("python3 refactor.py")?.check()?;\n\nfor ch in trial.diff()? {\n    println!("{} {}", ch.kind, ch.path);\n}\nif tests_pass { trial.apply()?; } else { trial.discard()?; }')}

    <h3 id="sdk-errors">Results and errors</h3>
    <p>A non-zero exit is a result, not an exception. Errors carry a machine-readable code: <code>no_sandbox</code>, <code>not_up</code>, <code>bad_name</code>, <code>bad_request</code>, <code>no_server</code>.</p>
    {api_code("errors",
      'r = sb.exec("make test")\nr.exit_code, r.stdout, r.stderr, r.duration_ms, r.timed_out\nr.ok; r.stdout_text\nr.check()               ' + c('# raises CommandFailed on a non-zero exit') + '\n\nfrom sdbox import NotFound, NotUp, BoxError',
      'const r = await sb.exec("make test");\nr.exitCode; r.stdout; r.stderr; r.durationMs; r.timedOut;\nr.ok; r.stdoutText;\nr.check();              ' + c('// throws CommandFailed on a non-zero exit') + '\n\nimport { NotFound, NotUp, BoxError } from "box-sdk";',
      'r, err := sb.Exec(ctx, box.Sh("make test"))\nr.ExitCode; r.Stdout; r.Stderr; r.Duration; r.TimedOut\nr.OK()\n\nbox.IsNotFound(err); box.IsNotUp(err)',
      'let r = sb.exec("make test")?;\nr.exit_code; r.stdout; r.stderr; r.duration_ms; r.timed_out;\nr.ok(); r.stdout_text();\nlet r = r.check()?;      ' + c('// Error::CommandFailed on a non-zero exit') + '\n\nmatch err { Error::NotFound(_) | Error::NotUp(_) => {}, _ => {} }')}

    <h2 id="mcp">MCP for agents</h2>
    <p><code>box mcp</code> serves box over the Model Context Protocol on stdin and stdout, so an agent in Claude Code, Claude Desktop, Cursor or any MCP client can run code in your sandboxes. Every tool goes through the same handler as the local API, with the same checks. Sandboxes are still created and built by you, on the CLI.</p>
    <pre class="code">claude mcp add box -- box mcp</pre>
    <pre class="code">{{ "mcpServers": {{ "box": {{ "command": "box", "args": ["mcp"] }} }} }}</pre>
    <div class="table-wrap build"><table>
      <thead><tr><th>Tool</th><th>What it does</th></tr></thead>
      <tbody>
        <tr><td class="mono">list_sandboxes</td><td>The sandboxes the agent may use</td></tr>
        <tr><td class="mono">run</td><td>A command in a fresh VM</td></tr>
        <tr><td class="mono">up / exec / down</td><td>Keep a VM and run commands in it, state carried over</td></tr>
        <tr><td class="mono">read_file / write_file</td><td>File I/O inside the guest</td></tr>
        <tr><td class="mono">fork / diff / discard</td><td>Branch <code>/data</code>, review the changes, drop them</td></tr>
      </tbody>
    </table></div>
    <div class="note"><p><b><code>apply</code> is not offered to agents</b> unless you start the server with <code>--allow-apply</code>. A fork exists so that you review an agent's work before it reaches real data; an agent that could apply its own fork would skip that.</p></div>

    <h2 id="api">Local API</h2>
    <p>HTTP with JSON bodies on <code>~/.box/box.sock</code>, owner-only. <code>stdout</code>, <code>stderr</code> and <code>stdin</code> are base64. The SDKs are thin clients of this.</p>
    <div class="table-wrap build"><table>
      <thead><tr><th>Method</th><th>Path</th><th>Body or result</th></tr></thead>
      <tbody>{rows(API, lambda m, p, b: f'<tr><td class="mono">{m}</td><td class="mono">{esc(p)}</td><td>{b}</td></tr>')}</tbody>
    </table></div>
  </article>
</div>
"""

PAGES_OUT = {
    "index.html": ("Overview", "box runs every command in a disposable microVM with its own kernel, defined in one file, started in milliseconds.", overview),
    "architecture.html": ("Architecture", "How box boots, confines and reaches a microVM on podman, krun and Firecracker.", architecture),
    "security.html": ("Security", "box's threat model, VMM confinement per backend, and the escape suite.", security),
    "benchmarks.html": ("Benchmarks", "box latency against containers, microVMs and interpreters, with method and sources.", benchmarks),
    "docs.html": ("Docs", "Install box, write a Boxfile, and drive sandboxes from the CLI or the Python, TypeScript and Go SDKs.", docs),
}

for fn, (title, desc, body) in PAGES_OUT.items():
    with open(os.path.join(OUT, fn), "w") as f:
        f.write(page(fn, title, desc, body))
print("wrote", ", ".join(PAGES_OUT))
