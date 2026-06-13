# gVisor runtime (opt-in)

By default sandboxd runs each sandbox under Docker's standard runtime (`runc`),
hardened with `cap-drop ALL`, read-only rootfs, `no-new-privileges`, and
per-sandbox memory/PID limits. That's a shared-kernel boundary — appropriate for
the project's threat model (authenticated users running their own code), but it
is not a defense against kernel-CVE container escape.

For stronger, per-sandbox **kernel isolation** — useful if you run less-trusted
code — sandboxd can run every sandbox under [gVisor](https://gvisor.dev)
(`runsc`). It is strictly opt-in; the default stays `runc`.

## Setup

### 1. Install runsc

Follow the official [gVisor install guide](https://gvisor.dev/docs/user_guide/install/)
(download `runsc` + `containerd-shim-runsc-v1`, verify the checksums, and place
them on `PATH`). gVisor supports x86_64 and ARM64.

### 2. Register the runtime with `--host-uds=create` (required)

sandboxd talks to the in-sandbox supervisor (`runtimed`) over a Unix socket on
the workspace bind-mount. gVisor's gofer does **not** expose a socket created
inside the sandbox to the host unless `--host-uds=create` is set — without it,
every task / exec / status call fails. Add to `/etc/docker/daemon.json`:

```json
{
  "runtimes": {
    "runsc": {
      "path": "/usr/local/bin/runsc",
      "runtimeArgs": ["--host-uds=create"]
    }
  }
}
```

Then reload Docker (a reload, not a restart, so running containers aren't
disturbed):

```
sudo systemctl reload docker
docker info | grep -i runtimes   # should list: runsc
```

### 3. Enable it in sandboxd

In `.env`:

```
SANDBOXD_RUNTIME=runsc
# optional — resolvers for sandboxes (default: 1.1.1.1,8.8.8.8)
SANDBOXD_DNS=1.1.1.1,8.8.8.8
```

Restart: `docker compose up -d`. New sandboxes now run under gVisor (`docker
inspect s-<id> -f '{{.HostConfig.Runtime}}'` → `runsc`; `uname -r` inside →
`*-gvisor`).

## How DNS is handled

On a user-defined Docker network, containers resolve names via Docker's embedded
DNS at `127.0.0.11`, which gVisor's netstack cannot reach. When
`SANDBOXD_RUNTIME=runsc`, sandboxd writes a `resolv.conf` (from `SANDBOXD_DNS`)
to its data dir and bind-mounts it into each sandbox at `/etc/resolv.conf`, so
DNS resolves. Package installs already go through the registry proxy by IP and
are unaffected.

## Trade-offs (measured on an ARM64 host with no KVM → gVisor software platform)

- **Near-parity**: dev-server serving, HTTP latency (+~1 ms/req), HMR for
  in-sandbox edits, pure CPU (~1.2×), bulk file I/O.
- **Slower**: `pnpm install` ~1.7×; syscall/metadata-heavy work ~4× (fork/exec,
  `stat`) — the gofer + syscall-interception cost. Container cold-start: +~0.1 s.
- **HMR caveat**: edits made *inside* the sandbox trigger reload; host-side edits
  to the workspace bind-mount do **not** (gVisor's gofer doesn't forward host
  filesystem events). The agent edits files inside the sandbox, so its workflow
  is unaffected.

## Status

Opt-in and experimental. The default runtime is unchanged (`runc`). Verified
end-to-end on ARM64: create → sandbox runs under `runsc` → DNS resolves →
preview serves.
