# Security

## What box is for

Giving an AI agent a machine of its own to work in, on your machine, so
nothing it does reaches the rest of it. The threat is the agent: it may make
a mistake, or a prompt injection may turn it against you, and the boundary
has to hold either way.

## What the boundary is

Two layers, checked separately.

**The guest kernel.** Every sandbox is a microVM (libkrun on KVM) with its own
kernel. A guest kernel exploit takes over the guest, which is thrown away.
`box build` proves a sandbox got its own kernel by comparing it with the
kernel a plain container sees, and every `run`, `exec` and `up` re-checks that
the runtime is still the one that passed.

**The VMM's confinement.** libkrun's own documentation states that the guest
and its VMM "pertain to the same security context": the VMM is a host process
that performs the guest's file I/O and network connections. So a guest that
breaks libkrun gets whatever the VMM has, and box gives the VMM as little
as it can:

- its own network namespace: nothing on the host's loopback is reachable
- podman's default seccomp filter and `no_new_privs`
- 6 capabilities, the ones virtiofs and low ports need; the other 5 podman
  grants by default are dropped
- a pids limit and a memory limit (`ram_mib` + 256 MiB)
- `/data` capped by `data_size` (see below), so an agent cannot fill your disk
- only the directories the Boxfile declares, `/data` and any `mounts:`
- with `isolation: strict`, a subordinate host UID of its own, which owns
  nothing of yours and nothing of any other sandbox's

Under standard isolation the VMM runs as your user. Escaping both the guest
kernel and libkrun then lands in your account. Use `isolation: strict` for
agents.

Under strict, each sandbox gets its own slot of 65536 IDs from your
subordinate range (`/etc/subuid`, `/etc/subgid`), recorded with the sandbox
and freed when it is destroyed; a fork shares its parent's, because they share
files. Its `/data` belongs to that UID, with your group and mode 0750: you can
read it, no other sandbox's UID can. When the range has no free slot, box
refuses to start the sandbox and prints the `usermod` line that adds one,
rather than let two sandboxes share a UID. `box doctor` shows how many strict
sandboxes your range holds. A destroyed sandbox's kept `/data` is handed back
to you, so a later sandbox given the same slot does not own it.

## /data's size

`data_size` in the Boxfile caps `/data` (new Boxfiles say 10G). On
firecracker `/data` is a disk of exactly that size, so the guest's own kernel
refuses a write that does not fit. On podman and krun `/data` is a host
directory shared over virtiofs, and a rootless process cannot put a quota on
it, so box measures it: it refuses to start a sandbox that is over, and
checks every 2 seconds while one runs -- a cold run, an `exec`, a warm run, or
a process left running in an `up` sandbox -- stopping the VM when it goes
over. A fast writer can overshoot by what it writes in those 2 seconds; the
limit is what the sandbox must get back under, not a wall. A Boxfile without
`data_size` has no limit on podman and krun (8G on firecracker), and
`box inspect` warns about it. The escape suite fills `/data` and checks both.

## Checking it yourself

`box inspect <name> --security-report` (or `=json`) states what a sandbox's
VMM can and cannot touch: its host UID, capabilities, `no_new_privs`,
seccomp, namespaces and limits, the network, `/data` and every host mount,
and what box last proved about the kernel boundary. It is read back out of
the configuration box hands the backend -- podman's arguments, or the OCI
spec for krun and the Firecracker jail -- so it cannot describe a confinement
box does not apply. When the sandbox is up, the running VMM is read from
`/proc` as well, and any difference is a failure (exit status 1). The escape
suite checks the report against the VMM itself.

## What it is not

- **Not a sandbox for image builds.** `box build` runs `podman build`, so a
  Boxfile's `run:` and `blueprint` steps execute in an ordinary rootless
  container, on the host kernel. Build only Boxfiles you would run yourself.
  Builds inside a microVM are planned.
- **Not a check on a lying guest.** The kernel comparison catches a runtime
  that silently fell back to a container. A guest that fakes `uname` passes.
- **Not a filter on what the guest runs.** `seccomp:` in a Boxfile filters the
  VMM, not the guest. Inside, the workload is root on its own kernel; that is
  the design.
- **Not egress control, yet.** `network: bridge` is full internet; `none` is
  none. A sandbox that may reach the internet may exfiltrate what it can read,
  which is everything in `/data` and any mount. Per-sandbox allowlists are
  planned.
- **Not a secrets boundary.** Anything you put in `/data`, a mount or `env:` is
  the workload's to read and send. Credential brokering is planned.
- **Not multi-tenant.** box is one user's tool. Running other people's
  code as a service needs a jailer-class boundary (Firecracker's, for example)
  that box does not have.

## Backends

The confinement above is the same on the `podman` and `krun` backends: the
krun backend writes it into the OCI spec directly instead of asking podman
for it. The krun backend refuses `isolation: strict` for now rather than run
the VMM as you.

The `firecracker` backend is the strongest. Firecracker is built for hostile
multi-tenant code, has a minimal device model (block, vsock) and no
host-side proxy for the guest's sockets (it has no network yet at all). Its
VMM runs with **no** capabilities, besides the confinement above, and has no
host directory shared into the guest: `/data` is a disk. Every run restores
a snapshot, and each restored copy rotates the agent token baked into the
snapshot, reseeds its randomness and resets its clock before anything runs.
`box data export` treats what it copies out as hostile.

The escape suite runs against every backend and both isolation modes.

## The agent channel (`up`, `exec`, the SDK)

A running sandbox has box itself as the guest's main process, reached
over a port published on `127.0.0.1` only. It answers only to a random
per-VM token (`~/.box/run/<name>.json`, mode 0600), given to podman
through its environment so it never appears in `ps`. Unauthenticated
connections are capped at 16 and given 3 seconds, so a local flood cannot lock
you out. The guest cannot reach the host's loopback, so it cannot reach its own
agent or another sandbox's. The SDK server listens on a Unix socket created
owner-only (`umask` before `listen`), and is never placed in a shared
directory.

## Host-side handling of guest-written files

A guest can write anything into `/data`: symlinks to `/etc/passwd`, `..`
entries in an archive, device nodes, setuid bits. box's own tools never
follow a guest's symlink on the host and host-side `/data` operations on a strict sandbox go
through `podman unshare`, where the sandbox's subordinate UID is reachable.
Files written into a strict sandbox's `/data` from outside (`box data import`,
`restore`) are given to that sandbox afterwards, so its guest owns all of it. Your own tools are your responsibility: do not `cp -L` or run scripts out
of `/data` without looking.

`restore` reads archives itself, in Go, rather than with the host's `tar`,
and only gzip-compressed tar. Every write goes through an `os.Root` on a
staging directory. The whole archive is refused if any entry is absolute,
climbs out with `..`, hard-links outside the archive, passes through or
replaces a symlink an earlier entry planted, or is a device or fifo. Symlink
targets are kept as written, since the guest resolves them. Setuid and setgid
bits are dropped. Only a complete extraction is swapped in for `/data`.

## Tests

`security/escape-test.sh` runs what a hostile workload would try, from inside
a sandbox, and checks the VMM's confinement from outside:

```sh
security/escape-test.sh "$(command -v box)" strict
```

It needs KVM, so it does not run in the hosted CI. `.github/workflows/
security.yml` runs it on a self-hosted runner labelled `kvm`, and before every
release it is run by hand on a Linux host.

## Reporting

Open a private security advisory on the GitHub repository, or email the
maintainer. Please do not open a public issue for an escape.
