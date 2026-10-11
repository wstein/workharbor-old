---
title: Container runtime
description: Use an existing Docker-compatible socket; setup never installs a runtime.
weight: 4
toc: true
---

WorkHarbor uses the container runtime you already have. `whr setup` never installs or pins a runtime or installs its Linux kernel, including with `--yes`, an answer file or an explicitly selected step. A dry run prints no runtime installation commands. Choose and manage the runtime yourself, then check its existing socket as the account that runs WorkHarbor:

```bash
whr setup --only runtime --dry-run
```

The setup `runtime` step and `whr doctor` share a read-only check. Discovery checks these sources in order:

1. `runtime.endpoint` in the selected WorkHarbor configuration.
2. `DOCKER_HOST`.
3. The Docker context: `DOCKER_CONTEXT` when named, otherwise the current context, read with `docker context inspect` when the Docker CLI is available.
4. Known local paths: `~/.docker/run/docker.sock`, `~/.orbstack/run/docker.sock`, then `/var/run/docker.sock`.

An explicit endpoint or named context is authoritative. If it is invalid or unavailable, WorkHarbor reports it instead of silently selecting another daemon. Only local `unix:///absolute/socket` endpoints are accepted; TCP, HTTP and SSH endpoints are not supported. A socket must answer `GET /_ping` with `OK` and `GET /version` with an Engine API version of at least 1.45. Requests have a two-second deadline and bounded responses. Discovery never starts the runtime, creates a container or changes configuration.

A successful check prints the socket and marks the **ping and API check** `ok`. This verifies connectivity only. With no answering compatible socket, the step is `not_verified` and links this page. These behaviors are {{< status verified >}} against fake Unix sockets in `internal/doctor` (`TestRuntimeSocket`, `TestMissingRuntimeSocket`, `TestSocketSources`, `TestDefaultContext`, `TestSocketProtocol` and `TestSocketCancellation`). Discovery against a real runtime on macOS is {{< status unverified >}}.

Configure the existing socket explicitly to use the Docker adapter. Merge this block into your existing `config.json`; discovery does not write it or change an existing backend:

```json
{
  "runtime": {
    "backend": "docker",
    "endpoint": "unix:///absolute/path/to/docker.sock",
    "toolbox": "/absolute/path/to/bundledock/toolbox"
  }
}
```

The toolbox is an existing, separate BundleDock utility bundle on the host. Setup neither installs it nor proves that it works in a guest. Omitted `runtime.backend` continues to select the legacy Apple Container adapter. When `backend` is `docker`, setup omits the Apple Container system, kernel, standard-user and Homebrew pin diagnostics. The socket stays on the supervisor host and is never mounted into an agent environment.

**Provisioning is blocked in the current Docker adapter (#533).** Every normal valid environment spec requires a positive `DiskMB`. The adapter does not implement that quota and rejects the spec before creating any daemon resource. There is no implicit unbounded tier. An answering socket therefore does not make Docker environments usable; both quota support and live acceptance remain open. The socket-discovery change does not bypass this admission check.

For a new macOS host, the recommendation is **OrbStack** {{< status unverified >}} (R-40). This is a recommendation to evaluate, not a measured WorkHarbor compatibility claim. Alternatives and their support status are:

| Runtime | WorkHarbor status |
| --- | --- |
| OrbStack | Docker-compatible socket candidate; real compatibility {{< status unverified >}}, provisioning blocked by #533 |
| Docker Desktop | Alternative Docker-compatible socket candidate; real compatibility {{< status unverified >}}, provisioning blocked by #533 |
| Colima | Alternative Docker-compatible socket candidate; real compatibility {{< status unverified >}}, provisioning blocked by #533 |
| Apple Container | Legacy adapter with previously [measured adapter evidence](../spikes/apple-container.md); supported-runtime direction is deferred under R-13; setup leaves its installation to you |
| Podman | Future support decision under R-13; compatibility with this Docker adapter {{< status unverified >}} |

The discovery and adapter source tests do not establish production support for any of these products. See the [requirements catalog](../requirements.md) (R-12, R-13 and R-40) and [host setup](host-setup.md) for the surrounding setup steps.
