# box TypeScript SDK

Run code in microVM sandboxes that each have their own kernel, from Node.
Node 18+, no dependencies.

```sh
npm install ./sdk/typescript          # from a box checkout
box new agent --from tiny-python && box build agent
```

```ts
import { Sandbox } from "box-sdk";

// A persistent microVM: ~10ms per exec.
const sb = new Sandbox("agent");
await sb.withUp(async (sb) => {
  await sb.writeFile("/data/task.py", "print(sum(range(10)))");
  const r = await sb.exec(["python3", "/data/task.py"]);
  console.log(r.stdoutText, r.exitCode, r.durationMs);
});

// A fresh microVM per call, destroyed afterwards. With `warm: 2` in the
// Boxfile it comes from a pool of booted VMs and starts in ~20-45ms.
const r = await new Sandbox("agent").run("python3 -c 'print(6 * 7)'");
r.check(); // throws CommandFailed on a non-zero exit
```

| Call | What it does |
|---|---|
| `sb.up()` / `sb.down()` | boot the microVM and keep it running / stop it |
| `sb.withUp(fn)` | up, run fn, down (unless it was already up) |
| `sb.exec(cmd, { stdin, timeout })` | run in the running VM; state carries over |
| `sb.run(cmd, { timeout })` | run in a fresh VM, then throw it away |
| `sb.writeFile(path, data)` / `sb.readFile(path)` | file I/O inside the guest |
| `sb.fork(name)` | branch the sandbox; resolves to the fork, whose `/data` overlays this one |
| `sb.diff()` / `sb.apply()` / `sb.discard()` | on a fork: list changes (`{kind, path}`), merge into the parent, or drop |
| `new Client().sandboxes()` | list sandboxes, whether each is up, its warm pool |

`cmd` is a shell string (`"ls -la | head"`) or an argv array
(`["ls", "-la"]`, no shell). A non-zero exit resolves to a `Result`; it is
not an exception. A timeout exits `124` with `timedOut: true`. Errors are
`BoxError` with a `code`, or its subclasses `NotFound` and `NotUp`.

**How it connects:** the SDK talks to `box serve` over a Unix socket that
only your user can open, and starts the server the first time it is needed.
An auto-started server exits after 15 minutes unused, and steps aside once
idle when box is upgraded.

**Files:** `writeFile` and `readFile` run inside the guest, not on the host,
so a path or symlink the sandbox controls can never redirect a write onto a
host file.
