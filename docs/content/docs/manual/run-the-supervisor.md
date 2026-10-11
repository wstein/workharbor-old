---
title: Run the supervisor
description: Start whr serve as a service, make workspaces and agents, set up passkeys, and check the host with whr doctor.
weight: 4
toc: true
---

A draft of the commands that exist today; they were checked against `whr --help` of a build from `main`, not run. **Every command on this page is provisional, and none of them has been run against a release yet** ({{< status unverified >}}): the text follows the commands' own `--help` and the design, and `wh/docs` will check it against `v0.1.0` (issue #65). Prepare the host first ([Prepare the Mac mini](host-setup.md)) and install `whr` ([Install, upgrade and release](install-upgrade-release.md)).

Most commands accept `--config <file>` (default `$WHR_CONFIG`, then `~/.config/whr/config.json`) and `--json` (they are persistent flags of `whr`), but only some of them do something with `--json`; `whr version` and `whr tools` parse their own arguments and `whr ssh` has a `--config` of its own (see the list below). Stdout is data and stderr is for you, so `whr preview open ... | pbcopy` copies only the link. The commands that go through the API (`ls`, `run`, `show`, `say`, `cancel`, `pause`, `resume`, `purge`, `inbox`, `approve`, `reject`, `answer`, `usage`, `kill-all`, `open`, `ws`, `agent`, `preview`, `passkey`, and `console --status` and `--close`) print the API's envelope with `--json` instead of text. The [API reference](api-reference.md) links the published OpenAPI document and explains the `schema_version: 1` success envelope, `Event` and `ErrorEnvelope` components. The CLI currently has no `--jsonl` flag; API errors become stderr text and an exit code, even with `--json`. These differ ({{< status unverified >}}: read from the code of a build from `main`, not run):

- `whr logs --json` (with or without `-f`) prints one JSON event per line, without an envelope or `schema_version`; it omits the API `Event` component's required `task_id`. See the [provisional differences](api-reference.md#cli-json-today).
- `whr doctor --json` prints its own object (`schema_version`, `ok` and the `checks`), and `whr service status --json` and `whr github app create --json` print `schema_version`, `ok` and `data`.
- `whr version` and `whr tools` (its one subcommand is `build`) parse their own arguments and take neither `--config` nor the envelope. `whr --version` is the same as `whr version`. `whr version --json` prints `schema_version`, the version, the commit, whether the tree was dirty and the build date as plain JSON, not the envelope; `whr tools build` takes `-store`, `-shim`, `-platform`, `-tools` and `-pins` (its usage line omits `-pins`).
- `whr serve` takes `--config` but runs until stopped, so `--json` has nothing to print.
- `whr ssh` prints no envelope in any mode and ignores `--json`. Its own `--config` is a different flag: it prints a `~/.ssh/config` block (below) and does not name a configuration file, so `whr ssh --proxy` and `--refresh` cannot be pointed at a configuration file with `--config`: they use `$WHR_CONFIG`, or `~/.config/whr/config.json` when that is unset, and the `ProxyCommand` that `whr ssh --config` prints never passes `--config`.
- `whr console` without `--status` or `--close` opens an interactive shell and prints no envelope.
- `whr completion` (the shell completion scripts, with `bash`, `zsh`, `fish` and `powershell`) and `whr help` print a script or the help text, not an envelope.
- `whr setup` and `whr service uninstall` print no envelope. `whr service install` prints the path of the plist as plain text.

## Check first: `whr doctor`

`whr doctor` checks the configuration, the host and the supervisor and says what it could not verify. A check that cannot read a setting says "not verified", never "ok". `--skip <check>` leaves one out (repeatable). Run it before the first start and after any change to the host.

For a source installation under `$HOME/.local`, run `whr setup --prefix "$HOME/.local" --user <your-account>`
and `whr doctor --user <your-account>`. The doctor warns, and does not fail, when `whr` runs
from outside an administrator-owned prefix. See the [source installation](install-upgrade-release.md)
for the alpha policy and the service behavior.

## First-time setup: `whr setup` (provisional)

Two wizards, each checking a step first, showing the exact commands of its fix and running them only after you answer `y`:

- `whr setup`, as your administrator account (run `/opt/whr/bin/whr setup`, because `/opt/whr/bin` is not on its `PATH`): the base configuration (`config-first`), the `workharbor` user, power settings, firewall, SSH, FileVault (guided), automatic log-out (guided), Homebrew, the Brewfile packages, the `/opt/whr` prefix, the system config, the workspace folders and the workspace volume.
- `whr setup` again, as `workharbor` in its desktop session, not over SSH: the container system, the private `~/.config/whr`, the API token (generated, never shown), an optional API key (typed without echo).

`--dry-run` runs the read-only checks and prints every fix without running one. `--only <step>` and `--from <step>` choose steps. Secrets are written with mode `0600` and never printed.

## Run it: `whr serve` and `whr service`

`whr serve` runs the supervisor: the reconciler, the JSON API and the web UI. The API listens **only** on a private unix socket, `api.sock` in `state_dir` (the directory must be a real directory owned by `workharbor` with mode `0700`); the web UI defaults to the loopback address `listen`, which a forwarder such as `tailscale serve` carries to your phone. Alternatively, configure one literal unicast VPN interface IP and port ([host setup, step 7](host-setup.md#7-reach-it-from-your-phone)); wildcard and DNS addresses are refused. A direct non-loopback listener requires an effective HTTPS public URL and an enrolled passkey before startup, never permits API-token web sign-in, and always uses secure session cookies. Restart on loopback for host enrolment or recovery if those prerequisites are missing. An accepted IP proves neither WireGuard ownership nor guest isolation; reference-host binding and reachability are {{< status unverified >}} (issue #539). Only one `whr serve` can run per state directory.

If a repository's workflow in the configuration (its preset or its integration branch) differs from the one recorded, `whr serve` stops: that is a policy change and must be confirmed. This also happens once after upgrading across migration 0016, see [Install, upgrade and release](install-upgrade-release.md). Start it once with `--accept-workflow-change`, or, with a passkey enrolled, start it normally and confirm the change on the web page *Changes* (it applies at the next start).

`whr service install` writes a macOS LaunchAgent that starts the container system and then `whr serve`, kept alive, in the `workharbor` user's login session. `whr service status` says whether it is loaded; `whr service uninstall` removes it (the logs stay). The LaunchAgent needs a logged-in session: see the automatic log-out step in the host page.

## Workspaces and agents

A workspace is a folder with an agent clone of one repository and its environment; an agent is a named role in it with its own worktree and branch `agent/<role>`.

```text
whr ws add <name> --path <folder> --repo <owner/name> --role <role> [--from <path-or-url>] [--branch <the publication target>] [--instructions <text>]
whr ws ls
whr ws rebuild <workspace>
whr ws rm <name>
whr agent add <workspace> <role> [--instructions <text>]
whr agent ls [workspace]
whr agent rm <workspace>/<role>
```

`ws add` seeds the clone and starts the environment, which takes a while. The workspace's branch is the repository's publication target (its `integration_branch`, `develop` for `integration` when none is set, the forge's default branch for `published`); `--branch` is accepted only when it names that same branch, and the command says which two differ when it does not. The folder must be empty and below a workspace root from the configuration. `ws rebuild` recreates the environment from the image the repository resolves to now (a new devcontainer commit, an allowed feature source or a bumped base image reaches a long-lived workspace only that way). It is refused while a run of the workspace is live; the folder, worktrees, branches and volumes stay. `ws rm` works only when the workspace has no agents, removes its environment and home volume, and leaves the folder. `agent rm` is refused while the agent has an unfinished task; its worktree and branch stay in the clone.

## Passkeys (provisional)

Once a passkey is enrolled, the web UI signs in with it, and a review ("Ready to push?"), an egress host, a workflow change and revoking the forge tokens each need a fresh passkey assertion that names exactly what you approve. Passkeys need `public_url` in the configuration (whr's https name).

```text
whr passkey add [name]    # prints a one-time link; open it on the phone (works once, 5 minutes)
whr passkey ls
whr passkey rm <id>       # an ID or a unique prefix; ends that passkey's sessions
```

The first passkey ends every session the API token started, and from then on the token no longer signs in to the web UI (it stays for the CLI). Passkeys are enrolled and revoked only here, on the host.

## Previews (provisional)

When an agent runs a dev server in its environment, `whr preview open <task> <port>` shows it through the supervisor on a port of its own. Set `preview.first_port` and `preview.last_port` in the configuration (at most 50 ports) and map them with the forwarder. Only ports the repository's environment declares (`forwardPorts`, or `customizations.workharbor.previewPorts` in its `devcontainer.json`) can be opened, and only while the environment runs.

```text
whr preview open <task> <port>   # prints a link that works once, for 5 minutes
whr preview ls
whr preview link <preview>       # another link
whr preview close <preview>
```

A preview is agent-written code in your browser: it has an origin of its own, but a browser shares cookies between ports of one host name, so a preview can sign you out of the web UI. Previews are {{< status unverified >}} on a real runtime: the Apple Container adapter cannot reach an environment's ports yet.

## The console

`whr console [workspace]` opens a shell in an environment without an agent, with every workspace mounted read-only (`--write` mounts the named one read-write). `--status` says whether it is open and `--close` closes it. See the design (D43) for what it can reach.

`whr ssh [-- command...]` opens an SSH session in the console with a certificate that lasts minutes; your key stays on your machine and the console listens on no port. Turning it on is described in [Prepare the Mac mini](host-setup.md) (the `ssh-ca` step). `whr ssh --config` prints a `~/.ssh/config` block for editors; `--forward` allows port forwarding.


### A certificate from the browser

The authenticated **Console SSH** page (`/console/ssh`) accepts one public key
of at most 4096 bytes. Open the console on the host and enrol a passkey first.
Keep the private key on your device. Port forwarding is off unless you select
it explicitly. Issuing a certificate requires a fresh passkey assertion for
that console, key and forwarding choice; a replaced console requires a new
request. The page returns the certificate, pinned host public key, principal
and expiry for copying into your SSH client. It keeps no browser storage copy.

This completes the browser certificate source path of issue #112, not remote
SSH access: VPN/forwarder reachability (#69), remote client configuration and
T16 verification remain {{< status open >}}. The handler and shared service
are checked with local Go tests; a remote connection from a phone or tablet is
{{< status unverified >}}. Revisit when those prerequisites are available and
run the remote client verification before claiming remote SSH support.
