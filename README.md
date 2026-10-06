# box

A box for your AI.

They can install, delete and break whatever they like inside it; nothing
outside changes. Every command runs in a disposable microVM with its own
kernel, defined in one file and started in milliseconds.

**[Docs](https://chxperiments.github.io/bluebox/docs.html)** ·
[Architecture](https://chxperiments.github.io/bluebox/architecture.html) ·
[Security](SECURITY.md) ·
[Benchmarks](bench/RESULTS.md)

## Install

```sh
curl -fsSL https://chxperiments.github.io/bluebox/install.sh | sh
box doctor    # checks podman, libkrun and KVM, and prints a fix for anything missing
```

Needs Linux with KVM, podman and libkrun.

## Quick start

```sh
box new devbox --from tiny-python     # scaffold a Boxfile
box build devbox                      # build the image, verify isolation
box run devbox -- python3 -c 'print(42)'   # one command, fresh VM

box up devbox                         # keep one VM running
box exec devbox -- pytest             # milliseconds per command
box down devbox
```

## Boxfile

```yaml
base: docker.io/library/alpine:latest
backend: podman        # podman, krun or firecracker
isolation: strict      # the VMM runs as a UID that is not yours
network: bridge        # or none, for no network at all
warm: 2                # keep VMs booted so runs start in ms
packages: [python3, git]
```

Only `/data` persists between runs. Everything else is rebuilt every time.

## Forks

```sh
box fork devbox try-a    # branch /data
box run try-a -- make
box diff try-a           # review what changed
box apply try-a          # or: box discard try-a
```

## From code and agents

- **SDKs:** [Python](sdk/python/), [TypeScript](sdk/typescript/), [Go](sdk/go/), [Rust](sdk/rust/)
- **MCP:** `claude mcp add box -- box mcp`. Agents can fork but not
  apply, unless you start it with `--allow-apply`.

```python
from sdbox import Sandbox

with Sandbox("devbox") as sb:
    print(sb.exec(["python3", "-c", "print(6 * 7)"]).stdout_text)
```

## Security

Each sandbox is a microVM under KVM, and the VMM itself is confined: no new
privileges, seccomp, a reduced capability set, its own network namespace and,
under `isolation: strict`, a UID that is not yours.
`security/escape-test.sh` tries what an agent gone wrong would. See
[SECURITY.md](SECURITY.md) for the threat model.

## Development

```sh
CGO_ENABLED=0 go build -o box ./cmd/box
go test ./...
security/escape-test.sh ./box strict    # needs KVM
```

## License

MIT
