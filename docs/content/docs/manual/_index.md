---
title: Manual
description: Setting up and running WorkHarbor on a Mac mini.
weight: 3
---

How to set up and run WorkHarbor. WorkHarbor is building release 1. The service and the commands exist, but none has run against a release yet, so the pages on running it are drafts and every command is provisional. Command names follow the [design](../design/_index.md).

{{< cards >}}
  {{< card link="first-run" title="Start here: First run" subtitle="The path from a prepared host to the first task (draft, provisional)." icon="arrow-right" >}}
  {{< card link="host-setup" title="Prepare the Mac mini" subtitle="macOS, user, power, Homebrew, existing runtime, VPN, firewall, backups." icon="desktop-computer" >}}
  {{< card link="container-runtime" title="Container runtime" subtitle="Existing sockets, runtime choices and current support limits." icon="server" >}}
  {{< card link="security" title="Security notes" subtitle="What you accept by running agents, and how to limit it." icon="shield-check" >}}
  {{< card link="vendor-terms" title="Agent vendor terms" subtitle="What a subscription login means under WorkHarbor, with the sources." icon="scale" >}}
  {{< card link="run-the-supervisor" title="Run the supervisor" subtitle="whr serve, workspaces, passkeys, previews, doctor (draft, provisional)." icon="server" >}}
  {{< card link="daily-use" title="Daily use" subtitle="Start a task, follow it, answer, pause, resume (draft, provisional)." icon="play" >}}
  {{< card link="install-upgrade-release" title="Install, upgrade and release" subtitle="The release archive, the tap, upgrade notes, cutting a release (draft, operators)." icon="download" >}}
  {{< card link="sessions-and-agents" title="Sessions and agents" subtitle="Which sessions to keep open, their models, the review gate (draft)." icon="users" >}}
  {{< card link="gh-stack" title="Stack pull requests" subtitle="Related issues as a gh stack: who runs what, adopting branches (provisional)." icon="collection" >}}
  {{< card link="troubleshooting" title="Troubleshooting" subtitle="What to check when whr fails to start." icon="server" >}}
  {{< card link="board-tooling" title="Board tooling" subtitle="Scoped repository queues and reviewed setup of existing GitHub projects." icon="collection" >}}
  {{< card link="operations-digest" title="Operations digest" subtitle="An index of decisions, procedures, how-tos and known gaps, linked to their sources." icon="collection" >}}
  {{< card link="json-schemas" title="JSON Schemas" subtitle="Provisional editor schemas for config.json and the devcontainer block." icon="document-text" >}}
{{< /cards >}}

The drafts are checked against the first release, `v0.1.0` (issue #65).
