#!/usr/bin/env python3
"""Latency benchmark: box against its baselines and neighbours.

Every tool runs the same workloads, timed on the host from "start the
command" to "have its exit status" -- what a caller such as an agent loop
actually waits for.

    python3 bench/bench.py [-n 20] [--box PATH] [--monty-python PATH]

It builds two throwaway sandboxes in a private BOX_HOME (your own
sandboxes are not touched) and removes them when done. Needs podman with
krun, and a static box (CGO_ENABLED=0). Monty is measured when the
Python given by --monty-python can import pydantic_monty.
"""

import argparse
import json
import os
import shutil
import statistics
import subprocess
import sys
import tempfile
import time

PY = "print(sum(i * i for i in range(100_000)))"
WORKLOADS = {"noop": ["true"], "python": ["python3", "-c", PY]}
IMAGE_BASE = "docker.io/library/debian:bookworm-slim"


def timed(argv, env=None):
    t = time.perf_counter()
    r = subprocess.run(argv, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    dt = time.perf_counter() - t
    if r.returncode != 0:
        raise RuntimeError(f"{argv} exited {r.returncode}: {r.stderr.decode()[-300:]}")
    return dt


def series(fn, n, gap=0.0):
    fn()  # warm caches: the first call measures disk, not the tool
    out = []
    for _ in range(n):
        if gap:
            time.sleep(gap)
        out.append(fn())
    return out


def summary(xs):
    xs = sorted(xs)
    p95 = xs[min(len(xs) - 1, round(0.95 * (len(xs) - 1)))]
    return {"min": xs[0], "median": statistics.median(xs), "p95": p95}


def box_sandbox(bb, env, name, warm):
    subprocess.run([bb, "new", name], env=env, check=True, stdout=subprocess.DEVNULL)
    with open(os.path.join(env["BOX_HOME"], "sandboxes", name, "Boxfile"), "w") as f:
        f.write(f"base: {IMAGE_BASE}\ncpus: 1\nram_mib: 512\nwarm: {warm}\n"
                "packages:\n  - python3\n")
    subprocess.run([bb, "build", name], env=env, check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def wait_warm(bb, env, name, want, timeout=60):
    end = time.time() + timeout
    while time.time() < end:
        ls = subprocess.run([bb, "ls"], env=env, capture_output=True, text=True).stdout
        if f"warm {want}/{want}" in ls:
            return
        time.sleep(0.2)
    raise RuntimeError("pool never filled")


def monty_series(python, n):
    """Monty 1.0 runs code in a pool of worker processes. A checkout is a fresh
    REPL session (the counterpart of a pooled `box run`); feeding the same
    session again keeps its state (the counterpart of `box exec`)."""
    code = f"""
import json, time, pydantic_monty
src = {PY!r}.replace("print(", "(")
ts = {{}}
with pydantic_monty.Monty(min_processes=2) as m:
    time.sleep(0.5)
    for w, s in (("noop", "None"), ("python", src)):
        fresh, same = [], []
        with m.checkout() as sess:
            sess.feed_run(s)
        for _ in range({n}):
            t = time.perf_counter()
            with m.checkout() as sess:
                sess.feed_run(s)
            fresh.append(time.perf_counter() - t)
        with m.checkout() as sess:
            sess.feed_run(s)
            for _ in range({n}):
                t = time.perf_counter(); sess.feed_run(s); same.append(time.perf_counter() - t)
        ts[w] = {{"fresh": fresh, "same": same}}
print(json.dumps(ts))
"""
    try:
        r = subprocess.run([python, "-c", code], capture_output=True, text=True, timeout=300)
    except (OSError, subprocess.TimeoutExpired):
        return None  # no such interpreter, or a hung one: skip Monty
    if r.returncode != 0:
        return None
    return json.loads(r.stdout)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-n", type=int, default=20)
    ap.add_argument("--box", default=shutil.which("box") or "box")
    ap.add_argument("--monty-python", default=sys.executable)
    ap.add_argument("--json", help="also write raw results here")
    args = ap.parse_args()

    home = tempfile.mkdtemp(prefix="box-bench-")
    env = dict(os.environ, BOX_HOME=home)
    bb, n = args.box, args.n
    results = {}

    def record(tool, isolation, workload, xs):
        results.setdefault(tool, {"isolation": isolation})[workload] = summary(xs)
        s = results[tool][workload]
        print(f"  {tool:<28} {workload:<7} median {s['median']*1000:9.2f} ms", flush=True)

    try:
        print("building sandboxes...", flush=True)
        box_sandbox(bb, env, "bench-cold", 0)
        box_sandbox(bb, env, "bench-warm", 2)
        image = "localhost/box/bench-cold:latest"

        for w, argv in WORKLOADS.items():
            record("host process", "none", w, series(lambda: timed(argv), n))
            record("podman + runc", "container", w,
                   series(lambda: timed(["podman", "run", "--rm", image] + argv), n))
            record("podman + krun", "microVM", w,
                   series(lambda: timed(["podman", "run", "--rm", "--runtime", "krun", image] + argv), n))
            record("box run (cold)", "microVM", w,
                   series(lambda: timed([bb, "run", "bench-cold", "--"] + argv, env), n))
            # Spaced so each run finds the pool refilled: this measures the
            # latency a pooled run gets, not how fast the pool refills.
            wait_warm(bb, env, "bench-warm", 2)
            record("box run (warm: 2)", "microVM, fresh per run", w,
                   series(lambda: timed([bb, "run", "bench-warm", "--"] + argv, env), n, gap=2.5))

        subprocess.run([bb, "up", "bench-cold"], env=env, check=True, stdout=subprocess.DEVNULL)
        for w, argv in WORKLOADS.items():
            record("box exec (up)", "microVM, persistent", w,
                   series(lambda: timed([bb, "exec", "bench-cold", "--"] + argv, env), n))
        subprocess.run([bb, "down", "bench-cold"], env=env, stdout=subprocess.DEVNULL)

        m = monty_series(args.monty_python, n)
        if m:
            for w, modes in m.items():
                record("monty, new session", "interpreter subprocess", w, modes["fresh"])
                record("monty, same session", "interpreter subprocess", w, modes["same"])
        else:
            print("  pydantic-monty: not importable, skipped")
    finally:
        subprocess.run([bb, "nuke", "-y"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        shutil.rmtree(home, ignore_errors=True)

    print(f"\n{'tool':<28} {'isolation':<24} {'noop median':>12} {'python median':>14} {'python p95':>11}")
    for tool, r in results.items():
        cells = [f"{r[w]['median']*1000:.2f} ms" if w in r else "-" for w in ("noop", "python")]
        p95 = f"{r['python']['p95']*1000:.2f} ms" if "python" in r else "-"
        print(f"{tool:<28} {r['isolation']:<24} {cells[0]:>12} {cells[1]:>14} {p95:>11}")
    if args.json:
        with open(args.json, "w") as f:
            json.dump(results, f, indent=2)


if __name__ == "__main__":
    main()
