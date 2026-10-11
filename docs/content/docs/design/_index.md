---
title: Design
description: Architecture, security model and delivery plan for WorkHarbor.
weight: 1
toc: true
---

**CLI:** `whr` · **Status:** Revised design, updated 2 October 2026 · Building release 1, dogfood first (D34): what is below is implemented and tested, mostly against fakes, and the first end-to-end run of a real issue on the reference Mac mini (#28, after #73) is next. Spikes are measured with committed scripts and published on the [spike pages](../spikes/_index.md). Results are marked in the sections they affect.

| Status | What | Where |
| --- | --- | --- |
| Decided | D1 to D49, D51 to D55 and D57 to D61 (D50 is reserved, #112; D56 is reserved, #309) | §3; new decisions are tracked in the issue that needs them (the [M0 Decisions milestone](https://github.com/wstein/workharbor/milestone/1) is closed) |
| Implemented | The domain (state machines, Decisions, the aggregate and its guards), the policy table, the SQLite store with redaction at ingest, the service layer and reconciler, hostgit (checkout checks, the repository cache, prepare and push, the editor copy), the runtime and agent contracts with their conformance suites, the Apple Container adapter with the egress proxy sidecar (ports 443 and 80, public addresses only, exec environment through a `0600` env file), the tool store (https only, size cap), the config file (secrets outside every root, an API key only, D40) and `make install` (development only, from independently reviewed clean current local `main`, including unpublished commits), push after approval with follow-up rounds as fast-forwards, the Claude Code adapter (`dontAsk` with an allowlist, or `manual` with host approvals over the stdio control channel), ntfy notifications, `whr version`, the `whr-shim` launcher, and the design-drift test; `whr serve` with the JSON API on a host-only socket, the CLI (D37), the web UI with passkey sign-in and step-up, previews and the usage dashboard, workspaces with named agents and the bundle export, the console with SSH certificates, workflow presets (D47), the setup wizard (D46), the launchd job, the board mirror, devcontainer environments with egress requests, release installs from drafts and the Homebrew tap workflow, and fuzz tests of the untrusted-input parsers | Packages under `internal/` and `cmd/`; what remains for the first dogfood run is the [Dogfood milestone](https://github.com/wstein/workharbor/milestone/5), the rest of release 1 the [R1 Slice](https://github.com/wstein/workharbor/milestone/2) and [R1 Complete](https://github.com/wstein/workharbor/milestone/3) milestones |
| Spiked | Agent contract (Claude Code, Codex CLI, Antigravity); Apple Container; host cancel with `whr-shim`; approvals over stdio; planted Claude Code config; host reachability; bind mount against volume, and a volume over a bind mount | Issues #1, #2, #10, #7, #68, #69, #40 and #80; the [spike pages](../spikes/_index.md); results in §4.2, §4.4, §5.1 to §5.3, §5.6, §7 |
| Open spikes | Signing in to the agent inside an environment (D40); what guests reach on the host with `pf` rules | Issues #82, #69 |
| Planned | The release 1 slice and the rest of release 1 | §13; [R1 Slice](https://github.com/wstein/workharbor/milestone/2) and [R1 Complete](https://github.com/wstein/workharbor/milestone/3) milestones |

Revision of the original discussion summary (`agent-work-supervisor-summary.md`) after a four-role team review (architecture, security, product/CLI, feasibility/ops). Review ratings are value/effort out of 10. Claims about Apple Container that spike #2 measured are stated as measured in §4.4, §5.1, §5.3, §5.6, §7 and §12. Claims about Socktainer, Coder and Portainer, and anything marked {{< status unverified >}}, still come from the original sources and are unverified until the remaining spikes in §12 are done.

The design is split by topic. Section numbers (§) are the same on every page, so a reference such as §4.5 finds its page here:

| Page | Sections |
| --- | --- |
| This page | §1 Goal, §2 Requirements, §14 Review log, §15 References |
| [Decisions](decisions.md) | §3 Key decisions (D1 onwards) |
| [Domain model](domain.md) | §4 State machines, Decisions, lifecycle, persistence, topics and cleanup before push |
| [Architecture](architecture.md) | §5 Adapters, reconciler, events, plugins, tool store, usage; §8 Resources |
| [External skill sets](skill-sets.md) | §5.8 Selection, loading, pinning, provenance and migration (D52) |
| [Native Codex supervision](native-codex.md) | §5.9 Native protocol, loading, policy isolation and measurements (D53) |
| [Canonical native role bindings](native-bindings.md) | §5.10 Versioned roles, host projection and effective evidence (D54) |
| [Guarded forge MCP](forge-mcp.md) | §5.11 Run-scoped policy checks, forge operations and provider tiers (D55) |
| [Per-client API credentials](api-clients.md) | §5.12 Named API clients and audit actors (#364) |
| [Security](security.md) | §6 Policy and autonomy, §7 Security |
| [Host egress allowlist](egress-allowlist.md) | Design note under §7: allowed destinations, rule sketches, doctor check (#361) |
| [Pull request landing](pr-flow-landing.md) | Design note: landing through pull requests, gate workflow and statuses (#407) |
| [Homebrew bootstrap](homebrew-bootstrap.md) | Design note: installing Homebrew from setup on a fresh Mac, options and recommendation (#491) |
| [Interfaces](interfaces.md) | §9 CLI, scripting contract, web UI, notifications, onboarding, mobile clients; §10 Forge, CI and identity |
| [Roadmap](roadmap.md) | §11 Existing platforms, §12 Open decisions and spikes, §13 Delivery |

{{< design-anchors >}}

## 1. Goal

A self-hosted service in which AI coding agents carry out project work independently while one developer acts as human-in-the-loop (HitL): intervening, answering questions, reviewing and approving.

- Agents work repository issues, modify code, run tests and commit in their own checkouts. After a cleanup step and the developer's approval the supervisor pushes the branch and opens or updates the PR (§4.5).
- Usage is a live coding assistant, not an automation pipeline: the developer chats with an agent about the work, then detaches while it works, for minutes or hours, and returns when it needs them. A personal tool with one developer and a handful of concurrent sessions.
- The developer attaches via chat, SSH or an editor temporarily. Disconnecting never interrupts the agent.
- Web UI and `whr` CLI are two front ends over one service layer: the CLI calls the JSON API, the web UI is server-rendered HTML (see D8).
- First host: an Apple-silicon Mac mini, recommended M6 with 32 GB memory and 512 GB storage (D32). The working proof of concept runs its environments on a Docker-compatible runtime {{< status unverified >}}, which is the supported runtime; the adapter that is built targets Apple Container, and the Docker-compatible adapter is {{< status open >}}. Other runtimes are decided later ([requirements](../requirements.md), R-13).

The central concept is an **agent task supervisor with managed workspaces**, not an editor-centred dev environment.

**Positioning.** WorkHarbor is a self-hosted, agent-agnostic remote for coding agents. From a phone or a browser the developer starts a task, chats with the agent, watches progress, answers its questions and pauses or cancels it, with Claude Code, Codex CLI and later other agents behind one interface (§5.2). It runs on the developer's own machine with their existing agent logins, so there is no hosted service in the middle. Hosted remote offerings from the agent vendors cover one vendor each; where they fall short for a given agent (for example a missing mobile or remote client) is {{< status unverified >}} and is checked in the §12 spike. The boundary stays: agents never merge, tag, release or deploy (§6).

## 2. Requirements

| Area | Requirement |
| --- | --- |
| Deployment | On-premises, one developer |
| Hardware | Apple-silicon Mac mini: M6 with 32 GB memory and 512 GB storage recommended; on a budget 16 GB, with 512 GB or with 256 GB plus an external SSD; 24 GB / 512 GB in between (D32, §8) |
| Runtime | Supported: a Docker-compatible runtime through the socket the host already has (the proof of concept {{< status unverified >}}; `whr setup` installs no runtime). Only the Apple Container adapter (per-container VM isolation) is built; the Docker-compatible adapter is a target {{< status open >}}. Decided later: Apple Container and Podman as supported runtimes, Firecracker microVMs on Linux (#140), Proxmox VE and VMware vSphere, and a Windows host with Hyper-V VMs (#140); not VirtualBox or WSL2 as runtimes (§13) |
| Capacity | **4 concurrent instances realistic, 8 a stretch goal** (see §8) |
| Overhead | Light operational and resource cost |
| Access | WireGuard or Tailscale (`tailscale serve` to the loopback web UI, D29), temporary SSH, VS Code / JetBrains via SSH; code-server optional and later |
| Forges | Gitea, Forgejo, Codeberg, GitLab, GitHub (release 1: GitHub, D15) |
| CI | Drone medium/long term, behind an adapter |
| Auth | OAuth for the five forges (release 1: a static access token and passkeys, §9.5, D45; the forge through a GitHub App, D15) |

## 14. Review log

Reviewers disagreed on three points; the resolutions adopted here:

- **Forge handoff:** manual compare-URL handoff (product) vs one forge, no half-state (architect). Adopted: one forge, no half-state; since D15 that forge is GitHub through a GitHub App, not a PAT.
- **Web UI:** defer entirely (ops) vs minimal inbox (product, architect). Adopted then: a minimal read-mostly UI with inbox, since decisions are answered there. Since superseded by the remote-control scope of §9.3 (D8, D12).
- **UI stack (D8):** `templ` + htmx (option 1 of 7 weighed) over Svelte, Preact, React and others. The cost is that the UI does not consume the JSON API directly; the shared service layer keeps the two front ends consistent.

Skipped for now: separate identity service, Kubernetes, multi-host placement, Portainer/Coder UI integration.

## 15. References

Carried over from the original summary; compatibility claims require testing against pinned versions.

- Apple Container: [README](https://github.com/apple/container), [technical overview](https://github.com/apple/container/blob/main/docs/technical-overview.md), [container machine](https://github.com/apple/container/blob/main/docs/container-machine.md), [VS Code Remote-SSH example](https://github.com/apple/container/blob/main/examples/container-machine-vscode/README.md)
- Socktainer: [repository](https://github.com/socktainer/socktainer)
- Editors: [code-server FAQ](https://github.com/coder/code-server/blob/main/docs/FAQ.md), [JetBrains remote development](https://www.jetbrains.com/help/idea/remote-development-overview.html)
- Platforms: [Coder](https://coder.com/docs/about), [external provisioners](https://coder.com/docs/admin/provisioners), [DevPod](https://devpod.sh/docs/what-is-devpod), [Eclipse Che](https://eclipse.dev/che/docs/)
- Portainer: [Add-ons](https://docs.portainer.io/admin/add-ons), [custom templates](https://docs.portainer.io/user/docker/templates/custom), [REST API](https://docs.portainer.io/api/access)
- OAuth: [GitHub](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps), [Gitea](https://docs.gitea.com/development/oauth2-provider/), [Forgejo](https://forgejo.org/docs/latest/admin/advanced/oauth2-provider/), [Codeberg](https://docs.codeberg.org/integrations/keycloak/), [GitLab](https://docs.gitlab.com/api/oauth2/)
