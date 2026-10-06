# box Python SDK

Run code in microVM sandboxes that each have their own kernel, from Python.
No dependencies beyond the standard library.

```sh
pip install ./sdk/python        # from a box checkout
box new agent --from tiny-python && box build agent
```

```python
from sdbox import Sandbox

# A persistent microVM: up on enter, down on exit. ~10ms per exec.
with Sandbox("agent") as sb:
    sb.write_file("/data/task.py", "print(sum(range(10)))")
    r = sb.exec(["python3", "/data/task.py"])
    print(r.stdout_text, r.exit_code, r.duration_ms)

# A fresh microVM per call, destroyed afterwards. With `warm: 2` in the
# Boxfile it comes from a pool of booted VMs and starts in ~20-45ms.
r = Sandbox("agent").run("python3 -c 'print(6 * 7)'")
r.check()   # raises CommandFailed on a non-zero exit
```

| Call | What it does |
|---|---|
| `Sandbox(name).up()` / `.down()` | boot the microVM and keep it running / stop it |
| `.exec(cmd, stdin=None, timeout=None)` | run in the running VM; state carries over |
| `.run(cmd, timeout=None)` | run in a fresh VM, then throw it away |
| `.write_file(path, data)` / `.read_file(path)` | file I/O inside the guest |
| `.fork(name)` | branch the sandbox; returns the fork, whose `/data` overlays this one |
| `.diff()` / `.apply()` / `.discard()` | on a fork: list changes (`Change(kind, path)`), merge into the parent, or drop |
| `Client().sandboxes()` | list sandboxes, whether each is up, its warm pool |

`cmd` is a shell string (`"ls -la | head"`) or an argv list
(`["ls", "-la"]`, no shell). A non-zero exit returns a `Result`; it is not
an exception. A timeout exits `124` with `timed_out=True`.

**How it connects:** the SDK talks to `box serve` over a Unix socket that
only your user can open, and starts the server the first time it is needed.
An auto-started server exits after 15 minutes unused, and steps aside once
idle when box is upgraded.

**Files:** `write_file` and `read_file` run inside the guest, not on the host,
so a path or symlink the sandbox controls can never redirect a write onto a
host file.
