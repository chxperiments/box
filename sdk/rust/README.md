# box Rust SDK

Run code in microVM sandboxes that each have their own kernel, from Rust.
Blocking API; depends only on `serde`, `serde_json` and `sha2`.

```toml
[dependencies]
sdbox = { git = "https://github.com/chxperiments/box" }
```

```rust
use sdbox::{Command, Sandbox};

let sb = Sandbox::new("agent")?;
sb.up()?;                                            // boot once, ~10 ms per exec after
let out = sb.exec(Command::new(["python3", "-c", "print(6*7)"]))?;
println!("{} {}", out.stdout_text(), out.exit_code);
sb.write_file("/data/task.py", "print(1)")?;
sb.down()?;

let out = sb.run("pytest -q")?.check()?;           // a fresh VM, then gone

let trial = sb.fork("trial")?;                     // branch /data
trial.run("python3 refactor.py")?.check()?;
for ch in trial.diff()? { println!("{} {}", ch.kind, ch.path); }
trial.apply()?;                                     // or trial.discard()?
```

| Call | What it does |
|---|---|
| `up()` / `down()` | boot the microVM and keep it running / stop it |
| `exec(cmd)` | run in the running VM; state carries over |
| `run(cmd)` | run in a fresh VM, then throw it away |
| `write_file(path, data)` / `read_file(path)` | file I/O inside the guest |
| `fork(name)` | branch the sandbox; returns the fork |
| `diff()` / `apply()` / `discard()` | on a fork: list, merge or drop its changes |
| `Client::new()?.sandboxes()` | list sandboxes |

A command is `Command::new([...])` (argv, no shell), `Command::sh("...")` or a
`&str` (shell), with `.stdin(...)` and `.timeout(secs)`. A non-zero exit is an
`Output`, not an error; `.check()` turns it into `Error::CommandFailed`. The
SDK talks to `box serve` over an owner-only Unix socket and starts it when
needed.

Tests: `cargo test`; against a real sandbox, `BOX_E2E=<sandbox> cargo test`.
