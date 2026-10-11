---
title: Roadmap
description: "Existing platforms, open decisions and spikes, and the delivery plan."
weight: 6
toc: true
---

## 11. Existing platforms

| Option | Use | Gap |
| --- | --- | --- |
| Coder | Workspace portal, IDE access | No task supervision; Apple backend unverified; external provisioners Premium |
| DevPod | Portable environments | Client-only; no central service |
| Portainer | Infra UI, templates, REST API | No agent progress/review semantics; add-ons need Business Edition + Kubernetes |
| Socktainer | Docker API over Apple Container | Partial compatibility; exec and restart recovery limits |
| Eclipse Che | Kubernetes dev workspaces | Heavy |
| code-server | Browser editor | No orchestration; Open VSX |
| **Agent-task supervisors** (OpenHands, Vibe Kanban, Sculptor, Coder Agents) | Assessed by desk research (issue #5, D22): none fits release 1 | See the scorecard below |

**Agent-task supervisors and vendor remotes** (desk research from docs and repositories, 1 October 2026, issue #5; nothing was installed, so every capability below is as documented, and the marked ones are {{< status unverified >}}). Fit is against release 1: phone or browser remote, detached runs, mid-run messages, approvals routed to a human, Apple Container, default-deny egress, supervisor-only push, more than one forge.

| Candidate | Runtime | Agents | Approvals to a human | Push | Forges | Fit | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- |
| OpenHands 1.24 (MIT) | Docker or a plain process; no Apple Container | Own agent; Claude Code and Codex over ACP | Confirmation mode for its own agent; for ACP agents {{< status unverified >}} | Agent holds forge tokens | GitHub, GitLab, Bitbucket | 5 | Borrow: ACP as an adapter protocol, the confirmation states |
| Vibe Kanban 0.1.44 (Apache-2.0) | Host worktrees, no isolation | Claude Code, Codex, Gemini and others | Plan approval cards | The UI pushes, opens and can merge PRs | GitHub, Azure Repos | 3 | Reject: the company shut down and its phone pairing went with its cloud service |
| Sculptor 0.48 (MIT, research preview) | Desktop app; Docker backend experimental | Claude Code, Pi | None: tool permissions are auto-approved | Pushes and opens a PR on a click | GitHub | 3 | Borrow: its Claude control-protocol integration and editable message queue |
| Coder Agents 2.36 (AGPL; Coder Tasks was removed) | Terraform templates; no Apple Container provisioner | Its own loop on API keys; no Claude Code or Codex subscription | Plan mode only | Agent pushes as the user | Through templates ({{< status unverified >}}) | 2 | Reject |
| Claude Code Remote Control and on the web | Local CLI relayed by the vendor, or vendor VMs | Claude only | Permission modes; approval from the phone {{< status unverified >}} | The cloud agent pushes itself | GitHub only | 3 | Complement: a reference phone UX and a fallback for Claude |
| Codex cloud | Vendor containers | Codex only | {{< status unverified >}} | User-initiated PR | GitHub only | 2 | Complement |

The original rating table missed agent-task supervisors; it is re-rated here (D22).

| Strategy | Original fit | Revised note |
| --- | --- | --- |
| Existing runner + thin supervisor + native Apple Container | 9 | Preferred; Claude Code first, Codex CLI second (D12, §5.2) |
| Same supervisor via Portainer/Socktainer | 7 | → ~4–5; optional shim only |
| Coder workspace layer + task supervisor | 7 | → ~3: Coder Tasks was removed and Coder Agents runs its own loop on API keys (D22) |
| Portainer + templates alone | 4 | Insufficient |
| Full new Codespaces/DevPod replacement | 3 | Excessive scope |
| Adopt/extend an agent-task supervisor | — | 2–3: none supports Apple Container, default-deny egress, host-routed approvals under subscription logins and supervisor-only push together (D22) |

## 12. Open decisions and spikes (reordered)

Ordered by what is cheap and blocks the most work. The measured results are on the [spike pages](../spikes/_index.md).

1. **Runner scorecard** (value 10, effort 3). Target agents are Claude Code, Codex CLI and Google Antigravity; Aider, OpenHands, Goose and others are scored for reference. Claude Code ships first and Codex CLI second. Antigravity ships a CLI (`agy`) with a headless print mode, so the gate is met: spike #1 drove it headless with typed events and resume. It has no mid-run injection and no approval channel in print mode, so it is a second-tier adapter in degraded mode (§5.2). Its account requirements and vendor terms for headless use are {{< status unverified >}}. Spike #1 (issue #1) measured Claude Code, and Codex CLI and Antigravity in part; the results are in §5.2, and still open are a real Codex run (usage limit until 3 October), a login that expires mid-session, what the usage-limit `status` reads once exhausted, and approvals for Codex and Antigravity. One page comparing them on: headless mode, permission/approval bypass, session-ID resume after process or VM kill, mid-run message injection (stdin vs resumed turn; a release 1 requirement, §5.2), structured event output (also required), how "blocked, needs human" is reported. Also score subscription sign-in for Claude Code and Codex CLI (all {{< status unverified >}}): headless or device-code login, where the token is stored, whether it survives a container restart and a Mac reboot, refresh behaviour inside a container, what happens when two environments share one login, how an expired login or exhausted usage window is signalled, current vendor terms for this kind of use, and whether a run keeps going with no client attached. Pause via SIGSTOP or stop-after-turn is not a resumed session; most CLIs resume only between turns.
2. **Adopt-or-extend** (value 9, effort 3): decided by desk research, build (D22, §11). Hands-on checks of the unverified claims are optional and only worth doing if a candidate adds Apple Container support or host-routed approvals.
3. **Apple Container native spike**, merged with benchmarking. Run as spike #2 (issue #2, branch `spike/apple-container`, `RESULTS.md`); measured on Apple Container 1.5.0, macOS 26.6.2. Compatibility checklist:
    - [x] Create/start/stop/delete representative workspaces (start about 1.1 s; use `--init`, §5.1)
    - [x] Enforce explicit CPU/memory (vCPU count and a cgroup limit inside the VM)
    - [x] Preserve project data across stop/start and rebuild (volumes and bind mounts; §4.4)
    - [ ] SSH, VS Code, selected JetBrains IDE (code-server optional). Not tested; `exec` works
    - [x] Recover after runtime/manager restart (§5.3): every container comes back `stopped` with its data
    - [ ] Recover after a Mac reboot (LaunchAgent vs LaunchDaemon; auto-login/FileVault implications). Not triggered; no plist for the services exists on disk, so `container system start` has to run after login
    - [ ] Private registry pulls and credential handling. Public pulls from Docker Hub worked; private registries not tested
    - [x] Agent session and its auth directory survive environment stop/start and rebuild: after `container stop` and `start`, the same agent session resumed from the home volume and remembered an earlier instruction, with the login supplied per exec. Surviving a Mac reboot was not tested
    - [ ] VPN reachability, forwarding or jump host. Not tested
    - [x] **Default-deny egress and network isolation controls** (`--internal` network plus a proxy sidecar; §7.2)
    - [x] **Escape tests: guest cannot reach host or sockets; forbidden mounts rejected** (mounted unix sockets unusable; mount rejection is the adapter's job; §7.4)
    - [ ] Memory behaviour at 1 then 4 instances, including pressure and swap; 8 only once 4 is measured (#39). Not started; an idle agent in a container used about 277 MiB
    - [x] Stock images with a shared read-only tool store (§5.6)
    - [x] Agent run inside a container with a real login (spike #2, `05c-agent-run.sh`): a stock image on an `--internal` network, tools from the store, the model reached only through the proxy sidecar, the spike #1 harness on the host driving it with live events, token deltas, a mid-run message and a resume after a container restart. The agent container used about 290 MiB
    - [x] Approvals from inside the container (§4.2, D26): the stdio control protocol over `container exec -i`, measured with its crash and deadline cases (spike #7)
    - [x] A reliable cancel from the host (§5.1, D25): measured in spike #10 using the `whr-shim` launcher from the tool store (§5.6, D19). Signalling the host exec client fails; signalling the process group via `container exec <id> /tools/whr-shim kill` terminates cooperative processes in ~10 ms and stubborn trees after a 500 ms grace in ~514 ms, with 0 orphans left
    - [ ] Repositories mounted from the host (§4.5): bind-mount speed with `node_modules`-style trees and much larger repositories
4. **Autonomy and approval policy** (§6) and threat model (§7): decided (D36); the policy table is implemented with a fixed floor (issues #4, #51), and the [threat model](../threat-model.md) is written (issue #11). Trust tiers for untrusted input remain (issue #53).
5. **Persistence semantics** (§4.4): decided (D16, D39). Open: a checkout on a volume for repositories that track more than about 50 000 files. It needs the branch exported from inside the guest for hostgit, for example as a bundle written by a disposable helper container that mounts the volume read-only; measured cost of seeding such a volume: 68 s for 153 000 files (spike #40).
6. **Primary forge** for release 1: decided, GitHub through a GitHub App (D15). A login provider is not needed before OAuth; release 1 signs in with a static token (§9.5).
7. **CI credentials and event handling** for Gitea/Drone (medium term).
8. The `whr` grammar of the slice is decided (D37); the stack too (D3, D8, D14).

Reboot considerations also include power-loss/UPS behaviour and macOS auto-update reboot policy.

## 13. Delivery

### Release 1: one vertical slice

Built CLI first (D12): the slice is the core loop through `whr`; the web UI and the phone client come after it. The [Dogfood milestone](https://github.com/wstein/workharbor/milestone/5) comes first: the subset that lets WorkHarbor run its own issues (D34). Werner's subsequent [D53](native-codex.md) prioritizes full native Codex support across WorkHarbor and crewbook as P1, before Antigravity; its contract and measurements do not wait for #28. The release boundary and measured capability gates stay in force.

- [ ] Apple Container backend, one host, native adapter (built; whether it is a supported runtime is decided later)
- [ ] Docker-compatible backend through the host's existing socket (the supported runtime, the proof of concept), with `whr setup` finding the socket and installing no runtime ([requirements](../requirements.md) R-13, R-40)
- [ ] Built-in agent adapters for Claude Code (first, built) and Codex CLI (a target: not built, #35; §5.2, §5.5), with observed progress and validated recovery
- [ ] Task/workspace/run/decision model with durable state, event log, reconciler
- [ ] Workspaces with named agents (D42) in two steps: the dogfood slice runs one named agent per workspace, with its agent clone and the bundle export (#90, #91); several agents in one environment at once follow in R1 Complete (#94)
- [ ] `whr` CLI (scripting contract, completion, `doctor`)
- [ ] Web UI with inbox, live transcript, send-message, start task, pause/resume/cancel, transcript purge (§9.3, §5.4)
- [ ] SSH access (certificates, `whr ssh --config`)
- [ ] Policy table, per-run credentials, egress proxy, resource budgets, audit log, `whr kill-all`
- [ ] Installable PWA as the phone client: web app manifest and a service worker for the app shell, so the remote-control UI (§9.3) installs to the Home Screen. Stays inside the server-rendered stack (D8), needs HTTPS on the VPN hostname, and signs in with a per-device revocable passkey (D45), not a token
- [ ] Static-token login for the host, which also signs in to the web UI until the first passkey is enrolled (D45; see the matrix)
- [ ] GitHub through a GitHub App installation (D15)
- [ ] Runtime and forge adapters as interfaces with one implementation each

**Explicitly out of release 1:** code-server, JetBrains validation, OAuth, editor launch and takeover in the UI, CI adapter, multi-host, scheduler beyond an admission counter.

### Capability matrix: as built (issue #248)

What the checklist above promises, and how far each item has come, as read from the code of `main` (the composition is `internal/serve/real.go` and `serve.go`) and from the named spikes. Four stages, each including the one before:

- **Specified**: the design or a decision describes it.
- **Implemented**: code and tests exist, mostly against fakes.
- **Integrated**: `whr serve` composes it (`serve.Build`, `serve.Run`), so a running supervisor uses it.
- **Measured**: a named spike or live test showed it working on Apple Container 1.5.0. The spikes ran on a development Mac, so no row is measured on the reference Mac mini until the slice demo (#28) runs.

A target in the checklist above is a target, not a claim, until its row says *integrated*. The status shortcode says how far the evidence goes: `verified` for what a named spike or test measured, `unverified` for what is built but not measured, `decided` for what is only specified, `open` for what has no code or an open question.

| Capability | Stage | Evidence and open ends |
| --- | --- | --- |
| Apple Container runtime adapter, whr-shim cancel, volumes and bind mounts | Measured | Spikes #2, #10, #40 and `builder-store`: {{< status verified >}}. Reached from the real stack in the [integration run](../spikes/serve-integration.md): {{< status verified >}} up to the agent's first request |
| Egress proxy sidecar (`whr-proxy`) on an internal network | Measured | The integration run: the guest's direct request failed and the same one through the sidecar reached the allowlisted host ({{< status verified >}}). The host's global IPv6 prefixes are refused in tests only ({{< status unverified >}}, threat model T5) |
| Claude Code agent adapter | Integrated | `claude.New` in `Build`. The integration run reached Claude's login prompt ({{< status verified >}}); a signed-in turn and resume wait for a logged-in agent home on the dev host (#82's spike, agent-signin.md, is closed), and the quota stop waits for #36 ({{< status unverified >}}) |
| Codex CLI agent adapter | Specified (full native support); Implemented (bounded offline foundation) | [#35](https://github.com/wstein/workharbor/issues/35) delivered the [app-server foundation](https://github.com/wstein/workharbor/blob/d2b1515a4584d4af2b18a45eac245137d44e3798/internal/agent/codex/doc.go) and offline tests. Public start/resume remain unsupported; `Build` composes Claude only. Native production integration and target model/runtime measurements remain {{< status unverified >}} under [D53](native-codex.md); see the separate pin/doctor evidence below |
| Antigravity agent adapter | Specified | Medium term (§5.2, [spike](../spikes/agy.md)); no adapter: {{< status open >}} |
| Task, workspace, run and Decision model, event log, reconciler | Integrated | `Run` reconciles at start and every 30 s; the integration run reconciled, stopped on SIGTERM and showed the inbox. Recovery of a real interrupted agent run is tested on fakes only ({{< status unverified >}}) |
| Workspaces with named agents, agent clone, bundle export | Integrated | #90 and #91 (closed). Several agents in one environment at once is #94 (open): {{< status decided >}} |
| Publish path: prepare, the check, per-SHA approval, signed commit, push (D51) | Integrated | `PublishFor` in `Run`, and off with a logged line when a pusher, committer or repository copy is missing. Not run against GitHub: {{< status unverified >}} until #28 |
| GitHub App forge adapter behind the Guard, the policy table and workflow presets (D15, D47) | Integrated | `newGitHub`, `NewForgeAccess` and `NewIssueAccess`, which hand `serve` the narrow access, not the raw adapter (#247). Only a stand-in for the GitHub API was used; the real App waits for #73 and #27: {{< status unverified >}} |
| Per-run credentials, resource budgets, audit log, `whr kill-all` | Integrated | #37 (closed): `service.AgentCredentials`, the budgets in the configuration, `store` audit entries, the `kill-all` command and API route. API-key mode with a key proxy is medium term (#83, D48): {{< status decided >}} |
| JSON API on a host-only unix socket, API token | Integrated | `api.ListenSocket` and `api.Listen` in `Run` (D29). Loopback unreachable from guests: {{< status verified >}} (#69); the forwarder and `pf` rules are not measured ({{< status unverified >}}) |
| Web UI: inbox, live transcript, send-message, start, pause, resume, cancel | Integrated | `web.Options` in `Run`, htmx and SSE. Not driven from a real phone: {{< status unverified >}} |
| Web sign-in: the host token, then passkeys (D45) | Integrated | See the note below. Passkeys need `public_url`; without it the log says they are off. Untested on a real phone: {{< status unverified >}} |
| Installable PWA shell | Integrated | `manifest.webmanifest`, `sw.js` and the offline page in `internal/web/static`, served by `web`. Installing to the Home Screen on iOS: {{< status unverified >}} |
| Console and SSH access with short-lived certificates (D43, #32) | Integrated | `console.ssh_ca_key_file` loads the authority in `Build`; the console image is built by `ensureConsole`. The image build has a live test that skips without `container`; an SSH session on the host is {{< status unverified >}} |
| Previews (D33, #72) | Integrated | `serve.Run` wires them when the configuration turns them on, but the Apple adapter does not implement `runtime.Previewer`, so opening one says the runtime cannot reach the port. Waits on #69's forwarding result: {{< status open >}} |
| Devcontainer features and a repository's Dockerfile (D38) | Integrated | `Environment` in `Build`. The libraries are tested against a fake registry; the build itself is the spike's: {{< status unverified >}} end to end (§5.1) |
| Notifications through ntfy (§9.4) | Integrated | `ntfyNotifier`. Web Push is medium term: {{< status decided >}} |
| `whr doctor`, `whr setup`, completion, launchd install | Implemented | `internal/doctor`, `internal/setup`, `internal/launchd`; commands in `internal/cli`. Run on the reference host only with `--dry-run` by the lanes: {{< status unverified >}} |
| Release pipeline and the install from a draft (D24) | Implemented | Not measured against a real draft release (T19, #180): {{< status unverified >}} |
| Tool store: pinned agent CLIs, `whr-shim`, `whr-proxy` mounted read-only | Integrated | `checkToolStore` and `toolProfile` in `Build`, `SpecOptions`. Spike `musl-cli` measured the musl builds ({{< status verified >}}) |
| CI adapter | Specified | An interface only (`internal/ci`, §10); Drone is medium term: {{< status open >}} |
| Docker and Podman backends, remote hosts, Firecracker, Proxmox, vSphere, a Windows host | Specified | Medium and long term; no code: {{< status open >}} (#140) |

**Web sign-in as composed.** The checklist items "Single static-token login" and "per-device revocable tokens" for the PWA are the plan that D45 replaced. `Run` builds token authentication (`web.NewTokenAuth`) always, and a passkey service only when `public_url` is set (`serve.go`, the passkey block). The host token signs in to the web UI until the first passkey is enrolled; then `OnFirstEnrolled` ends the sessions the token started, and from then on each phone or tablet signs in with its own passkey, which the administrator revokes per device (`OnRevoked` ends that passkey's sessions). Enrolment and revocation happen only from the host. There are no per-device tokens. The checklist items above say so.

**Codex as built.** [#35](https://github.com/wstein/workharbor/issues/35) delivered a bounded native app-server foundation with [offline schema fixtures](https://github.com/wstein/workharbor/blob/d2b1515a4584d4af2b18a45eac245137d44e3798/internal/agent/codex/testdata/README.md) from CLI 0.160.0; [public `Start` and `Resume`](https://github.com/wstein/workharbor/blob/d2b1515a4584d4af2b18a45eac245137d44e3798/internal/agent/codex/adapter.go) still return `ErrUnsupported`. Separately, [#164](https://github.com/wstein/workharbor/issues/164) delivered the [admitted 0.159.2 Linux arm64-musl pin](https://github.com/wstein/workharbor/blob/731bd296061e6e10bcf5ee1376409b15d3cd7329/internal/toolstore/testdata/README.md), bounded extraction and archive/binary hash checks; GNU coverage remains unfinished. [Doctor checks tool-store integrity](https://github.com/wstein/workharbor/blob/5dc1797b469339aa52a802d854e3a92d4e8683f6/internal/doctor/host.go), which establishes neither native protocol nor model access. The pin and schema versions are separate evidence, not a supported production tuple. [`Build`](https://github.com/wstein/workharbor/blob/5dc1797b469339aa52a802d854e3a92d4e8683f6/internal/serve/real.go) composes only `claude.New`. Full native production support remains specified by [D53](native-codex.md); integration, exact requested/resolved model and effort, native configuration isolation and target-runtime measurements remain {{< status unverified >}} until the existing gates are met.

### Releases (D24)

- **When.** The pipeline is built during release 1 and stays dormant; the first release, `v0.1.0`, is cut when the slice demo (issue #28) passes, so the Mac mini runs `whr serve` from a released binary under launchd (issue #38). Until then, a signed prerelease tag `v0.1.0-alpha.N` on a green commit of `main` gives a dogfood build: it is published as a pre-release (never to the tap), and the administrator installs it from its release archive: checked against `checksums.txt`, then, when `gh` is installed (an upgrade), checked with `gh attestation verify` against the build-provenance attestation signed by this repository's release workflow (a first install has no `gh` and skips that), unpacked, and `sudo ./install.sh <tag>` installs `whr`, `whr-shim` and `whr-proxy` from that archive, which must match the tag and its line in `checksums.txt`, into a prefix (default `/opt/whr`) that should be admin-owned (warned, not enforced, in the alpha: D58); `make install-release VERSION=<tag>` from a clone checks the checksums and, with `gh`, the attestation. Then one `0.x` release per milestone. `v1.0.0` waits until the JSON API (OpenAPI, D3), the adapter `contract_version` and the database migrations are stable and an upgrade with a backup has been tested.
- **Version.** The tag is the only source: `git describe` is stamped into the binary with `-ldflags`, built with `-trimpath`; `whr version` prints the version, commit and whether the tree was dirty, and `whr version --json` gives the same as data on stdout with a `schema_version`. Without a tag the version is `v0.0.0-<commits>-g<sha>`, never empty, and a build that was not stamped (plain `go build`) reads it from the Go build info. No version file.
- **Prepare.** For a release to publish (not a dogfood prerelease), an ordinary commit, which an agent may make: `chore(release): prepare vX.Y.Z` regenerates CHANGELOG.md with git-cliff for that version. CI must pass on it.
- **Tag.** Only the human, signed and annotated: `git tag -s vX.Y.Z`. A tag ruleset lets only the repository admin create `v*` tags and forbids updating or deleting them.
- **Build.** A workflow triggered by the tag checks that the tag is annotated and signed by a known key (an SSH key listed in `.github/release-signers`), that the tagged commit is on `main` and that CI passed on it. It then runs GoReleaser (pinned): the host binary `whr` for `darwin/arm64` only (no Intel or Linux host build is released) and the guest binaries `whr-shim` and `whr-proxy` for `linux/arm64`, packed into one archive with `install.sh`, checksums, an SBOM, a build-provenance attestation and release notes from git-cliff, into a **draft** release. Only that job gets `contents: write` and the attestation permissions.
- **Publish.** The human checks the draft and publishes it. A second workflow, triggered by the publication, updates the Homebrew tap (`brew install wstein/tap/whr`), so the tap never points at a draft; a prerelease does not update it. The workflow checks the assets' checksums and attestations, renders a formula (the macOS `whr` plus the guest binaries as a resource in `libexec/whr`, and the shell completions) and pushes it to `wstein/homebrew-tap` with a deploy key of that repository only, kept in a `homebrew-tap` environment.
- **macOS distribution.** The tap is the supported install path. Signing and notarizing a downloaded binary need an Apple Developer ID and are deferred until someone other than the developer installs it from a download.

### Medium term

**Auth-mode phasing** (D41): subscription logins first, with the guardrails of D40 (quota stops, the shared usage window, the agent's permission mode; #36, #48, #82); API keys as a full peer in the medium term (#83); an enterprise offering in the long term.

- [ ] API-key mode as a full peer (D41, #83): the host-side key proxy that issues a key per run (§7.3) instead of the configuration's key file, spend budgets (#37) and per-key usage (#48)
- [ ] OAuth providers, Gitea/Forgejo/GitLab/GitHub adapters
- [ ] Podman backend (decided later), remote Linux hosts (host worker becomes remote-capable)
- [ ] Alpine as a first-class console base (#93): a small image and a fast package manager for the human's console (D43); no agent runs there, so musl only affects the human's own tools
- [ ] Firecracker as a runtime target (#140): a microVM with its own guest kernel per environment, the isolation model of Apple Container, on a Linux host with KVM (remote, or a Linux VM on the Mac if nested virtualization allows it, {{< status unverified >}}). It boots a kernel and a root filesystem rather than an OCI image, so stock and devcontainer images need a conversion or Kata Containers or firecracker-containerd underneath; the spike decides, and the runtime conformance suite is the gate
- [ ] Antigravity adapter in degraded mode (`agy` print mode, §5.2, §12) and additional runners
- [ ] Out-of-process adapter plugin loader with conformance checks (§5.5, §7.8)
- [ ] Drone CI with revision-aware feedback
- [ ] Resource-aware scheduling, recovery improvements
- [ ] code-server, UI editor launch and takeover, `whr top`
- [ ] Web onboarding wizard over the same service layer (§9.5); release 1 uses `whr login` and `whr doctor`
- [ ] Web Push alongside ntfy (§9.4) for the installed PWA. iOS Web Push needs the app installed to the Home Screen ({{< status unverified >}})

### Long term

- [ ] An enterprise offering: several developers on one supervisor under the vendors' commercial terms (Team or Enterprise plans, API keys or a cloud provider), with per-user identity and audit. D40's one-human rule is for consumer subscriptions; this needs its own decision (D41)
- [ ] Proxmox VE and VMware vSphere/ESXi as runtime targets (#140): environments as full VMs on an existing virtualization cluster, each with its own kernel (a Proxmox LXC container shares the host kernel and does not meet §7's bar), driven through their APIs with credentials scoped to one pool or folder, an isolated network per environment with the egress sidecar, and the runtime conformance suite as the gate. A full VM boots far slower than Apple Container's 1.1 s (spike #2), which may call for pooled environments
- [ ] Alpine as a first-class base for agent environments (#93), once each agent CLI has a musl build that passes `agenttest` and the common toolchains work on musl (Python wheels, Node native modules); the tool store then pins musl builds next to the glibc ones (D19)
- [ ] A Windows host with Hyper-V VMs (#140): the supervisor on Windows, each environment a Hyper-V VM with its own kernel. A host port, not only a runtime: the Windows counterparts of Homebrew (D28), the `pf` rules (D29), launchd and `0600` secret files are needed, and whether Firecracker (#140) can run inside WSL2 is {{< status unverified >}}
- [ ] Other microVM/VM platforms, Kubernetes where useful
- [ ] Multiple hosts and placement policies
- [ ] Wider forge/CI coverage

**Not planned:**

- Native iOS and Android apps. The installable PWA (release 1) is the mobile client; the JSON API stays the contract for any client.
- VirtualBox as a runtime. One VM per environment would meet §7, but it adds little next to Apple Container on the Mac and Firecracker, Proxmox or VMware on Linux; it is driven only through `VBoxManage`, boots full VMs slowly, and its shared folders are slow where spike #40 already found bind mounts costly. Whether it runs on Apple-silicon Macs is {{< status unverified >}}.
- WSL2 as a runtime. Its distributions share one utility VM and one Linux kernel, so environments would be separated by namespaces only, which fails §7's bar of a kernel per environment. Windows is a host platform instead (#140).

Phases are proposals, not a schedule.
