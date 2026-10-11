---
title: Security notes
description: What you accept by running coding agents under WorkHarbor, and how to limit it.
weight: 6
toc: true
---

The short version of the [threat model](../threat-model.md) for whoever runs WorkHarbor. Agents are treated as untrusted: they read text strangers wrote, and they follow it.

## What WorkHarbor is built to enforce

These are the design's guarantees. `whr serve` and the commands exist but have not run against a release, so none of this is {{< status verified >}} on a real setup unless it says so. Each item says how far the code goes (checked against `main` on 3 October 2026); the [design's status table](../design/_index.md) and the threat model track the rest.

- **Agents never push, merge, tag, release or deploy.** They commit in their own checkout. WorkHarbor pushes a branch only after you approve its exact commit in a "Ready to push?" decision, and, in the `integration` and `published` presets, a GitHub ruleset makes a human review the pull request (D15, D18, D47); the `prototype` preset opens no pull request and fast-forwards a non-default integration branch after your approval. *Policy table, the push flow with its per-commit approval and the GitHub App client are implemented. That a real GitHub ruleset stops the App from merging is checked by you (`whr doctor` only reminds you) and not measured ({{< status unverified >}}, issue #105).*
- **Approvals fail closed.** A request nobody answers in time, an approval for different code, or a lost connection to the agent ends in a denial (design §4.2). *Implemented in the domain and in the Claude Code adapter's approval channel (an unanswered request is denied after a timeout), covered by tests with a stub agent; not run against a real agent yet ({{< status unverified >}}).*
- **Each workspace runs in its own lightweight VM** on its own internal network; this environment is the isolation boundary, not the agent. The agents of one workspace share the VM, its network and egress allowlist, the agent home (the login and every agent's sessions) and the agent clone's `.git`, so they can read and change each other's work. Agents that must not touch each other go in separate workspaces. The VM reaches the model provider only through a proxy that allows listed host names (an entry matches exactly that name; `*.` entries come only from the configuration and the built-in list) on ports 443 and 80 and refuses private, local, link-local and carrier-grade-NAT addresses, also when a name resolves to one, and it cannot reach other LAN devices or other workspaces (design §7.2). It **can** reach your Mac's own services that listen on the network, even from its isolated network, so WorkHarbor defaults to loopback and you turn off or harden the rest. A configured VPN interface IP does not prove that guests cannot reach it; that isolation is {{< status unverified >}} ([host setup, step 8](host-setup.md#8-firewall-and-ssh)). *The runtime adapter and the proxy are implemented and pass their conformance suite on Apple Container (issues #26, #78). The proxy also refuses the Mac's own global IPv6 prefixes (issue #124, closed; tested in the code, not measured on the real runtime, {{< status unverified >}}). The prefix comes from the interface mask, so a /128 mask (DHCPv6, utun) refuses only the Mac's own address, not its LAN; this is not measured on macOS ({{< status unverified >}}, left open by the closing comment of #124). No code sets the `pf` rules that keep guests off your Mac's services, and they are not measured ({{< status unverified >}}, issue #69).*
- **Your home directory, SSH keys and secrets folders are never mounted** into an agent environment, and git on the host never runs commands an agent planted in its checkout (design §4.4, §4.5). *Mount checks (the home directory, `~/.ssh` and their parents are refused) and hardened host git (no hooks, no fsmonitor) are implemented and tested.*
- **`whr` never handles your subscription login.** You sign in inside each environment through the vendor's own flow, only you start runs, and a run that hits its quota pauses and asks you (design D40, [vendor terms](vendor-terms.md)). *Decided; the configuration refuses a subscription token. How sign-in works inside an environment is not measured yet ({{< status unverified >}}, issue #82).*

## What you accept

- **The agent's login sits inside its environment.** With a subscription login (Claude or ChatGPT sign-in) you sign in inside the environment, and `whr` never sees the credential ([vendor terms](vendor-terms.md)). Any process in that environment can read it while a run is active. Delete environments you no longer use, and revoke the login at the vendor if one is ever exposed.
- **Data can leave through the model provider.** The proxy allows the provider's API host, so an agent could send repository contents there. That is also how agents work: send only repositories you are willing to share with the provider.
- **The proxy matches host names.** It does not stop domain fronting through an allowed host.

## Your part

- **Revoke a token that was exposed**, at once: a token pasted into a chat, a terminal or a log is exposed. Agents and scripts pass tokens only through a `0600` env file, never on a command line.
- **Review before you approve.** "Ready to push?" shows the commits and the diff: approve only what you have read.
- **Treat issue text from strangers as untrusted.** WorkHarbor holds a run on an issue from an author who is not an owner, member or collaborator, and asks you first.
- **Keep WorkHarbor off the open network**: Tailscale or your router's VPN, never a port forward to WorkHarbor itself ([host setup, step 7](host-setup.md#7-reach-it-from-your-phone)).
- **Encrypt an external SSD** on its own, because FileVault does not cover it, and **keep `workharbor`'s session logged in** (no automatic log-out), because the supervisor and the container system run in it ([host setup, step 3](host-setup.md#3-filevault-and-restarts)).

## Who can reach it

- **The JSON API is on a host-only unix socket**, `api.sock` in `workharbor`'s state directory (directory `0700`, socket `0600`), reached only by the CLI as the `workharbor` user. **The forwarder or configured VPN listener carries the web UI alone**, and the TCP web listener serves no `/v1` route, so a leaked API token cannot answer a review, allow an egress host or enrol a passkey from the phone network. The web UI defaults to loopback; `listen` may instead name one literal unicast VPN interface IP and port. Wildcard, DNS, multicast, broadcast, link-local and scoped-zone addresses are refused (D29, [design §7.5](../design/security.md#7-security)). *Implemented and tested: the server refuses to start if the socket is reachable by others. The forwarder and the `pf` rules are not measured yet ({{< status unverified >}}, issue #69).*
- **The web UI listens on loopback by default.** A configured non-loopback IP does not prove WireGuard ownership or guest isolation; actual VPN binding and reachability remain {{< status unverified >}} (issue #539). No guest reaches a loopback listener ({{< status verified >}} in [the host-reachability spike](../spikes/host-reachability.md), issue #69).

## Passkeys

A non-loopback listener starts only with an effective HTTPS `public_url` (or `board.public_url`) and an enrolled passkey that the store can read. It never accepts the API token for web sign-in, including after last-key revocation, and its session cookies are always `Secure` with the `__Host-` prefix. Configure trusted HTTPS separately; the listen setting does not provide TLS. Missing prerequisites require a restart on loopback for host enrolment or recovery.

With a passkey enrolled, the web UI signs in with it; the API token no longer signs in to the web UI (it stays for the CLI) (D45, [design §7.5](../design/security.md#7-security), [Run the supervisor](run-the-supervisor.md#passkeys-provisional)).

- **Enrol and revoke only on the host**, with `whr passkey add` and `whr passkey rm`. The web UI has no route that adds or removes one.
- **A fresh passkey assertion is needed** for a "Ready to push?" review, an egress host, a feature source for an environment, a workflow change (the `/changes` page) and revoking the forge tokens. It names exactly what you approve, including the commit or the host. Other answers need a signed-in session only.
- **There is no lockout after failed sign-ins**: a passkey assertion cannot be guessed, and a lockout would only let whoever reaches the forwarder keep you out. Beginning sign-ins is limited to 600 a minute in all, only to save CPU, and a flood can delay a new sign-in while it lasts; signed-in sessions, step-ups and the host CLI are unaffected.

*Implemented and tested without a real phone ({{< status unverified >}}).*

## Previews

A preview shows agent-written code in your browser ([design D33](../design/decisions.md)). Each has a loopback listener of its own, so you map each preview port with your forwarder to give it its own HTTPS port (`whr` does not set the forwarder up; {{< status unverified >}}), and the browser gives it its own origin. The proxy replaces the app's CSP with its own, opens it only with a single-use link (valid 5 minutes) that becomes a per-preview cookie, forwards only to the one environment and port, and closes it when its environment stops, when you close it, after 12 hours, when the supervisor restarts and when the last web session that opened it ends (one opened from the host CLI does not end with a session).

- **A preview can still leak by navigation.** The CSP stops its script from fetching or loading images from another site, but a link, a top-level navigation or a prefetch hint can carry data out past the egress allowlist. This is an accepted risk ([threat model](../threat-model.md)): only you open a preview, so look at where it navigates, and open one only for work you can read.
- **Browsers do not isolate cookies by port**, so the preview and the UI share a host name. The proxy never sends `whr_` cookies to the app and drops any it tries to set under those names. Whether the app can still sign you out of the UI is not measured ({{< status unverified >}}).

## If your phone is lost

1. On the host, as `workharbor`: `whr passkey ls`, then `whr passkey rm <id>`. This revokes the passkey and ends the web sessions it started.
2. If you hold another passkey, sign in with it and open `/devices` to check that no other session is listed, and sign out any that is.
3. Enrol a new passkey on the host with `whr passkey add`; recovery is always you at the host. **If you revoked the last passkey, only a loopback listener accepts the API token again.** A configured non-loopback listener stays passkey-only and unavailable for sign-in; restart with a loopback `listen` address and recover through the host CLI before restoring the VPN listener. If the token may be exposed, replace it.
4. If the phone also held an agent's subscription login, revoke it at the vendor ([vendor terms](vendor-terms.md)); `whr kill-all` cannot do that.
