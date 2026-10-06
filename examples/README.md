# Examples

Ready-to-use Boxfiles. Each directory is one sandbox.

The examples are built into box, so you can start a sandbox from one
directly, without this repo:

```sh
box new agent --from tiny-python
box build agent
box run agent -- python3 --version
```

Any other files that come with an example (such as `lab-aws`'s `main.tf`) are
placed in the new sandbox's `/data`.

### Tiny

Small images for agent code and scripts. They build and pull faster, use less
disk, and leave less inside the VM for a workload to exploit.

| Example | Image | What it has |
|---|---|---|
| [`tiny-busybox`](tiny-busybox/Boxfile) | ~5 MB | busybox `sh` and coreutils; for scripts and static binaries |
| [`tiny-alpine`](tiny-alpine/Boxfile) | ~25 MB | bash, curl, git |
| [`tiny-python`](tiny-python/Boxfile) | ~63 MB | Python 3 + pip on Alpine (musl: see the file's note on wheels) |
| [`tiny-node`](tiny-node/Boxfile) | ~90 MB | Node.js + npm |

For comparison, `python-dev` on Debian is several hundred MB. Set `warm: 1` or
more in any of them to get runs that start in ~50ms.

### Development

| Example | What it shows |
|---|---|
| [`python-dev`](python-dev/Boxfile) | apt packages, a pip build step, env vars |
| [`node-dev`](node-dev/Boxfile) | a global npm install as a build step |
| [`go-build`](go-build/Boxfile) | `pkgmgr` override, read-only root |
| [`ai-agent`](ai-agent/Boxfile) | blueprint user + sudo, read-only root, per-run timeout |
| [`offline`](offline/Boxfile) | `network: none` — no egress at all |
| [`os-lab`](os-lab/Boxfile) | a real kernel: `mount`, `sysctl`, `modprobe` work |

### Labs

| Example | What it shows |
|---|---|
| [`k8s`](k8s/Boxfile) | a real single-node k3s cluster you start with `start-cluster`, plus kubectl and Helm |
| [`lab-aws`](lab-aws/) | AWS CLI + Terraform against [Floci](https://floci.io/), a local AWS emulator; see [`main.tf`](lab-aws/main.tf) |

**lab-aws**, verified end to end from inside the microVM: S3 (create/put/get), a
full `terraform apply` (VPC, subnet, internet gateway, route table, security
group, EC2 instance, S3 bucket — 8 resources), and a Python **Lambda** that
executes and returns its payload.

Start Floci on the host first with [`floci-up.sh`](lab-aws/floci-up.sh), which
handles the rootless-podman details Lambda needs (a container socket, and
putting Floci's spawned Lambda containers on a shared network so their callback
to the Runtime API works). Then:

```sh
./examples/lab-aws/floci-up.sh          # start Floci on the host
box build lab-aws
box shell lab-aws
# inside: the sandbox reaches Floci at host.containers.internal:4566 (preset)
cp /path/to/main.tf /data && cd /data
terraform init && terraform apply
aws s3 ls
```

The sandbox ships AWS CLI v2 and Terraform, with `AWS_ENDPOINT_URL` preset, so
tools talk to Floci with no flags.

Three things about the lab sandboxes worth knowing, because they are honest
limits rather than bugs:

- **State is ephemeral.** Each `box run` is a fresh VM. Start a cluster or
  a service inside `box shell`, and keep anything you want to keep in
  `/data`. A k3s cluster's own state resets with the VM.
- **box runs a command, not a full boot.** systemd is not PID 1, so live
  `systemctl start/enable` on services is limited. Start what you need by hand
  inside `box shell`, as `start-cluster` does for k3s.
- **Kubernetes runs, with a networking caveat.** `k8s` boots a
  real k3s cluster (verified: node Ready, pods scheduled and serving). The
  minimal guest kernel has no VXLAN or nf_conntrack, so overlay CNI and
  Services do not work; `start-cluster` uses host-gw with kube-proxy off, and
  pods should run with `hostNetwork: true`. The API, scheduling, RBAC, kubectl,
  running containers and Helm all work. Full overlay networking would need a fuller guest kernel
  (a possible future via libkrun's external-kernel support).

For `lab-aws`, run Floci separately (it serves AWS APIs on port 4566), then
point the sandbox's endpoint vars at it. If Floci is on the host, use the
host's bridge address rather than `localhost`, since inside the sandbox
`localhost` is the sandbox.

## Notes

- **`ai-agent`** is the one to look at for running a coding agent. Point it at
  `box run agent -- <command>`: the agent works in a fresh microVM each
  time, `/data` carries the project between commands, and `timeout_seconds`
  caps any single command.
- **`go-build`** shows `pkgmgr: apt`. The package manager is normally inferred
  from the base image, but `golang:...` is not a name the tool recognises, so
  it is set explicitly.
- **`os-lab`** is the case a container cannot cover: it has its own kernel, so
  kernel-level commands act on the sandbox and reset with it.
- **`offline`** has no route out, so every tool must be listed under
  `packages` — there is no installing anything at run time.
