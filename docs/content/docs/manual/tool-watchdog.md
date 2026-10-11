---
title: Tool silence watchdog
description: Configure tool-output silence thresholds and distinguish source verification from available adapter support.
weight: 15
toc: true
---

Runs and tasks have no wall-clock duration limit. Remove
`budgets.per_run.max_duration` and `budgets.per_task.max_duration` from older
configurations: both keys are rejected, including when their value is zero.
Configured token and cost budgets remain in force. Operational deadlines for
checks, builds and readiness still apply. Historical duration audit fields remain
readable; elapsed run time no longer increments a budget counter.

## Configuration

The optional top-level configuration object is:

```json
"tool_watchdog": {
  "warn_after": "10m",
  "abort_after": "30m"
}
```

Omitting either field selects its shown default. Values must be positive Go
durations, with `warn_after` strictly less than `abort_after`. These settings
apply only when the adapter supplies the watchdog contract. Changing them does
not enable support in an adapter that lacks it.

**Claude and Codex currently advertise `ToolWatchdog: false`. Neither adapter
provides this watchdog in production.** Real correlated tool-only cancellation
is {{< status unverified >}}; R-22 remains incomplete. Existing tool-call and
result transcript events, session silence, a session stop and a whole-turn
interrupt do not substitute for the required contract.

## Behavior covered by source tests

A supported session supplies a stable generation and distinct observations for
actual tool execution, nonempty stdout/stderr output, completion and human waits.
The supervisor measures silence from receipt of those observations. At the
warning threshold it records one warning; at the abort threshold it attempts to
cancel only that tool. If the first check is already at the abort threshold, it
skips the stale warning. Output starts a new silence episode. Concurrent tools
have independent state.

Overlapping waits are paired by wait ID and freeze accumulated silence until the
last wait ends. Output during a wait resets silence but leaves it frozen. An
unrelated human Decision does not freeze every tool. Authentication or quota
suspension, run termination and session loss disarm observation. Restart does
not reconstruct watchdog state from transcripts or arm tools from replay.

Cancellation must be independently confirmed before `tool.aborted` is recorded.
Unknown outcomes are not reported as successful aborts and are not retried in the
same silence episode. The watchdog never stops the session, ends the run or
starts another request. An outcome arriving after a session replacement retains
its original run, generation and tool identity.

With ntfy configured, `tool_hung` and `tool_aborted` use the same per-task throttle
as quota and finished-run notifications. Pushes contain only task ID, kind and
link. Tool identities, output and commands stay out of the push payload. See
[phone notifications](host-setup.md#10-phone-notifications-optional).

## Source evidence and remaining verification

Issue #535's runtime source was independently reviewed at
`d685431559a1790a79b54c4654bdf1a2a834554b`. This records local source verification;
publication and actual runtime measurement remain pending.

| Criterion | Source evidence | Actual runtime status |
| --- | --- | --- |
| No run/task duration termination | `TestRunHasNoDurationDeadline`; configuration rejects retired keys; token/cost lifecycle regressions pass | Long production runs not measured here |
| Configurable thresholds, continuing output, concurrent tools and human waits | `TestToolWatchdog*` fake-clock state tests; configuration and serve composition tests | Claude/Codex support unavailable |
| Tool-only abort, ownership and generation isolation | `TestWatchdog*` fake-session integration tests, including delayed interruption publication after same-run resume and confirmed outcomes after replacement | Vendor tool-only cancellation unverified |
| Generic, throttled warning/abort/quota/finished pushes | `TestWatchdogQuotaAndFinishedPushesShareThrottleAndGenericPayload` uses a local HTTP test server | Real ntfy and phone delivery unverified |

Before either adapter can advertise support, it needs verified execution-start,
nonempty output, completion and paired human-wait observations correlated by
bounded tool IDs within an immutable session generation, including concurrent
tools. It also needs a tool-specific cancellation operation with an observed
outcome that leaves the agent session and other tools running. Implement and test
that contract per adapter, then measure it on the actual vendor/runtime setup.
No live vendor, login, container, build or phone-delivery verification was part
of this source work. R-22 and end-to-end R-23 acceptance remain open.

## Provider bridge investigation

The additional detached translator in `internal/agent/codex/command_output.go`
accepts typed output observations for an already owned running command item. It
preserves the provider item ID and the session's immutable generation, counts
nonempty UTF-8 bytes, and retains no output payload. Synthetic tests cover
ownership, malformed input, size bounds, empty output and generation binding.
This is a source foundation: raw output-notification intake still refuses,
production launch remains gated and watchdog capability remains false.

The [official Codex protocol](https://learn.chatgpt.com/docs/app-server)
distinguishes command output deltas from pending items and approvals. An item
start can precede approval, so it is not sufficient evidence of actual execution.
Separately created command/process sessions are not arbitrary model tool items;
their termination operations cannot be substituted for tool-only cancellation.
D53 also excludes the out-of-sandbox process API from the adapter control surface.

The tool catalog currently pins Codex `0.159.2`, while the retained offline
schema inventory was generated from `0.160.0` on macOS. It contains no command
output-delta notification schema. Today's documentation is not proof of either
target's wire contract. Retain and verify the exact authorized target schema
before connecting the translator to raw notifications; do not invent that
schema or hydrate ownership from output.

The [official Claude SDK control implementation](https://github.com/anthropics/claude-agent-sdk-python/blob/main/src/claude_agent_sdk/_internal/query.py)
provides `stop_task` using a provider task ID and tracks terminal task lifecycle
messages. This does not establish the complete watchdog contract for the pinned
Claude CLI `2.1.285` or for every tool type. Elapsed-time progress and heartbeats
are not stdout/stderr output.

The smallest missing integration must supply these semantics through the
trusted provider tool executor, within the existing optional agent contract:

- Bind real provider tool/execution IDs to one immutable live session generation.
  For a Claude task control path, validate the tool-use-to-task-ID relationship
  from the target's authoritative lifecycle before sending `stop_task`; never
  guess a task ID or PID from a command, transcript or file path.
- Observe actual execution after approval, nonempty stdout/stderr bytes,
  completion and paired human waits. Pending proposals, control acknowledgements,
  model text, heartbeats and replay cannot substitute for these observations.
- Implement `AbortTool` against that owned execution only. Revalidate ownership
  before dispatch, then await independent confirmation that the named execution
  stopped. A successful control response alone proves no cancellation. An
  already finished tool returns Finished; uncertainty returns Unknown.
- Keep the session and other tools live and issue no new request. Preserve the
  original generation/tool identity across in-flight completion or replacement.
  Missing control support must remain unsupported, with no session or turn
  interruption fallback.
- Verify the exact schema/artifact and exercise these conditions per provider on
  the actual target, alongside its existing mandatory launch gates. A partial
  background-task cancellation path cannot establish support for all tools.

The detached translator implements only output normalization. The executor
observations and targeted cancellation above are prerequisites, not implemented
bridge behavior or a new vendor wire protocol. R-22 remains incomplete.
