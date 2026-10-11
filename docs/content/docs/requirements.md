---
title: Requirements
description: What WorkHarbor must do for the developer who uses it today, with what is built, what comes next and what was dropped.
weight: 6
toc: true
---

This page is the requirements catalog, version 3. It replaces the customer catalog v2, which described a target system (zero trust, short tasks, per-action approvals, Apple Container only) that is not how WorkHarbor is used. It starts from the working proof of concept and adds features one step at a time. Where this page and the [design](design/_index.md) differ, this page states the direction and the design is updated next; the [glossary](glossary.md) words apply.

Each requirement has a state: **built** (the proof of concept does it; reported by the maintainer, not yet measured here {{< status unverified >}}), **next** (planned in this order), **later**, or **dropped**.

## Usage model

One developer works interactively. A request such as "fix issues 1, 2 and 3 and open a pull request" goes to the [Crewbook](https://github.com/wstein/crewbook) desk, a role of the external skill set, which hands the work to subagents. A run may take hours. The developer can close the laptop and later watch, instruct or stop the run from anywhere.

- Every run starts from a human request. There is no queue, scheduler or automation mode, also not with an API key; other tools cover automation. D57 stays as decided: it already allows the agent and its subagents to control their own steps within one human request {{< status decided >}}.
- A subscription login is the normal case. An API key is for providers without subscriptions, because API pricing is too high for a personal coding agent.
- The only step that needs the human is the merge of a pull request. The human approves it and carries the responsibility.

## Requirements

| ID | Requirement | State |
| --- | --- | --- |
| **Work** | | |
| R-01 | A human request starts every run; subagents and skills work within it for hours | built |
| R-02 | The agent commits, pushes its own branch, opens the pull request and updates issues. This is the proof of concept; D62 (provisional) amends D18 to match, and the design and glossary keep showing the old rule until the code changes | built |
| R-03 | Agents keep the project board and issues current so the human can read the overview; the human does not move cards | built |
| **Environment** | | |
| R-05 | Build directories and caches live outside the workspace mount, under `/var/tmp`, so they vanish when the environment is rebuilt. Build outputs that are artifacts stay on the mount. Reason: creating many small files on the mount took 290 ms against 26 ms on the home volume, one run on the reference host {{< status unverified >}} | built |
| R-10 | A run executes in an isolated environment: a stock language image with the BundleDock toolbox (common command-line tools, separate from the tool store of the agent CLIs) mounted read-only | built |
| R-11 | Vendor logins and agent configuration stay in the environment, kept by a persistent home volume; `whr` never reads or stores them (D40) | built |
| R-12 | The host home, `~/.ssh` and runtime sockets are never mounted into an environment | built |
| R-13 | WorkHarbor supports Docker-compatible runtimes through the socket the host already has. Other runtimes (Apple Container, whose adapter is built, Podman, Firecracker) are decided later. The adapter contract names its capabilities and assumes no Docker semantics | next |
| **Remote control** | | |
| R-20 | The developer sees progress, sends instructions and stops a run from a phone or laptop away from home | built |
| R-21 | The web app is reachable from a laptop or phone away from home over Tailscale (`tailscale serve` to the loopback listener, D29), as the setup step `tailscale-serve` does | built |
| R-24 | The web app is also reachable over WireGuard. Where a forwarder is not wanted, the listen address is configurable and the web app stays authenticated | next |
| R-22 | A tool call without output for 10 minutes raises a warning; after 30 minutes it is aborted. Both limits are configurable; a run itself has no time limit | next |
| R-23 | A push notification through ntfy reports watchdog warnings, quota stops and finished runs; the notifier exists, the watchdog events are new | next |
| **Host** | | |
| R-40 | `whr setup` never installs a container runtime. It finds the Docker-compatible socket that is already available, from `DOCKER_HOST`, the Docker context or a known socket path {{< status unverified >}}, checks it and uses it. When none is found, it reports the step as not verified and links a documentation page. That page recommends one runtime (OrbStack on macOS {{< status unverified >}}), lists the alternatives and says which ones WorkHarbor supports. The socket is never mounted into an environment | next |
| R-41 | `whr doctor` checks and `whr setup` sets the host settings an unattended Mac needs: no system or disk sleep, restart after a power cut, wake on network access and Power Nap off. The step exists as `power` (host step 4); real changes need the administrator, agents only report or use `--dry-run` | built |
| **Safety** | | |
| R-30 | An agent never merges, tags, releases or deploys. The forge enforces it through token scope and branch protection, not through prompts | built |
| R-35 | `whr doctor` warns, and does not refuse, when agent push is enabled and the forge ruleset is not verified. Whether a ruleset stops a write-scoped token from merging its own pull request is not measured {{< status unverified >}} | next |
| R-31 | Secrets never appear in logs, arguments or commits; stored text passes the redactor | built |
| R-32 | The audit log is a tamper-evident hash chain | built |
| R-33 | A credential proxy keeps forge tokens outside the environment | later |
| R-34 | Egress: first record outbound hosts, then offer an allowlist | later |

## Dropped from catalog v2

| Dropped | Why |
| --- | --- |
| Zero trust as a principle; default-deny egress as a precondition | Not required for the working model; egress becomes an optional later step (R-34) |
| Per-action tool approvals, fail-closed timeouts, the 2,000-character cap and truncation warning | The agent runs without single approvals; only the merge needs the human |
| Approval of the pushed commit SHA | The pull request is the review point |
| Short tasks, hard pause and process-group cancellation as requirements | Runs last hours; the tool watchdog (R-22) replaces them |
| Apple Container as the only runtime | Docker is the supported runtime; Apple Container stays an adapter that is decided later (R-13) |
| Passkey step-up on every sensitive action | One sign-in per device is enough; the merge stays with the forge |
| Cold and warm start latency and the Mac mini sizing as acceptance criteria | Not requirements of use; kept in the design as measurements |
| The full devcontainer file copied into the catalog | The Anchorage `.devcontainer/devcontainer.json` is the contract; this page only states the invariants of R-10 to R-12 |

## Open

- Whether ntfy delivery to iOS through a self-hosted server works {{< status unverified >}}; ntfy.sh is the first step.
- The Mac mini runs its system from an external SSD over USB 3.2 (20 Gbit/s) {{< status unverified >}}; the workspace is a mount, everything else is image, toolbox and volumes. The host has 10 CPU cores and 16 GB of memory, of which 10 to 12 GB are a realistic limit for the container VM {{< status unverified >}} (reported by the maintainer); the number of parallel runs is bounded by that. A cold `go vet ./...` of this repository, a proxy for four agents compiling at once, took 11 s and about 0.8 GB alone and 39 to 41 s and about 3.2 GB with four at the same time (two runs, in the Docker environment of the reference host, 11 October 2026) {{< status verified >}}; agent processes and test runs add to that, and the memory of one and four environments themselves (#39) is not measured yet. Sequential disk speed is not measured (the 1 GiB write test was served partly from cache).
- `pmset -g` on the host showed system sleep after 1 minute (held off only by the `dasd` daemon), disk sleep after 10 minutes, no restart after a power failure and Power Nap on. The five values were then set by hand and read back correctly on 11 October 2026 {{< status verified >}} (`pmset -g`, the maintainer's terminal); the `power` step of `whr setup` itself ran only against fake output so far {{< status unverified >}}.
- `whr` supervises runs only through the Apple Container adapter today. Making it work with the Docker environment of the proof of concept is the first change after the design update (R-13).
- Whether D57 gets in the way of the proof of concept, for example when instructing a run from a phone, is checked against the running system, not assumed.
