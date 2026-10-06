# Roadmap

Where box is going. The full plan, with a checklist and a definition of
done for each milestone, is [#16]. Milestones ship in order, because each one
depends on the one before. There are no dates.

## Shipped

- **v0.2:** Boxfile validation, declarative `mounts:`, `restore`, named
  snapshots, isolation re-checked on every run.
- **v0.3:** VMM confinement and `isolation: strict`, the escape suite,
  `up`/`exec`, the warm pool, forks, three backends (podman, krun,
  firecracker), `box data`, SDKs for Python, TypeScript, Go and Rust, and
  the MCP server.

## Next

- **v0.4, sandboxes isolated from each other:** a UID per sandbox, image builds
  inside a VM, strict mode on krun, a `/data` size limit ([#10]), safer
  restore ([#15]), cross-sandbox escape tests.
- **v0.5, network policy:** egress allowlists through a forced proxy ([#7]),
  no route to the host, the LAN or other sandboxes, a connection log ([#8]),
  networking for Firecracker.
- **v0.6, secrets and supply chain:** credential brokering, pinned base
  images, signed releases with an SBOM and provenance, vulnerability scanning.
- **v0.7, robustness:** fuzzing, fault and soak tests, cleanup after crashes,
  upgrade tests.
- **v0.8, interfaces and platforms:**
  - **gRPC API** next to the local HTTP API: typed, with streamed `exec`
    output and generated SDK clients, and the transport for server mode.
  - **Lima backend:** each sandbox as a [Lima](https://lima-vm.io/) VM. On
    macOS it uses Apple's Virtualization framework directly, so every feature
    works there without a podman machine; on Linux it adds QEMU as a fourth
    engine.
  - Logs and metrics, a systemd unit, a versioned Boxfile and API with a
    compatibility promise, published SDKs, deb/rpm/Homebrew packages.
- **v0.9, multi-tenant server mode:** tenants, quotas, Firecracker only, an
  audit log.
- **v1.0:** an external security review and a disclosure process.

Also planned: a fuller guest kernel so Kubernetes in the `k8s` example gets
overlay networking and Services ([#9]).

[#7]: https://github.com/chxperiments/bluebox/issues/7
[#8]: https://github.com/chxperiments/bluebox/issues/8
[#9]: https://github.com/chxperiments/bluebox/issues/9
[#10]: https://github.com/chxperiments/bluebox/issues/10
[#15]: https://github.com/chxperiments/bluebox/issues/15
[#16]: https://github.com/chxperiments/bluebox/issues/16
