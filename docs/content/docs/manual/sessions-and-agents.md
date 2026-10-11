---
title: Sessions and agents
description: Which sessions to keep open while you build WorkHarbor with agents, which model each runs on, and why.
weight: 10
toc: true
---

How to set up the sessions and subagents that build WorkHarbor itself. This is about the development workflow in this repository, not about running agents with `whr`. The rules are in [`AGENTS.md`](https://github.com/wstein/workharbor/blob/main/AGENTS.md) (Project board, Models, Context and cost, GitHub rate limit) and in the lane prompts in [`.agents/`](https://github.com/wstein/workharbor/tree/main/.agents); this page does not copy them, except the procedure detail that left `AGENTS.md` to keep it under the 24,000-byte cap of Antigravity's always-on rules {{< status unverified >}} (the last section; #233). A rule or prohibition always stays stated in `AGENTS.md` itself. `wh/dispatch` has not run yet, so what it does here is {{< status unverified >}}.

## Sessions to keep open

| Session | Model | When |
| --- | --- | --- |
| `wh/desk` | Sonnet | Always on. Your point of contact: status, discussion, filing and routing ([`desk.md`](https://github.com/wstein/workharbor/blob/main/.agents/desk.md)). |
| `wh/dispatch` | Sonnet | Always on. Pulls cards, starts the lane agents and the reviews, merges, moves cards and hands over ([`dispatch.md`](https://github.com/wstein/workharbor/blob/main/.agents/dispatch.md)) {{< status unverified >}}. |
| `wh/design` | Opus | Optional. You can open an Opus `wh/design` session yourself while a decision is waiting; it decides in the issues, writes a resume note and ends ([`design.md`](https://github.com/wstein/workharbor/blob/main/.agents/design.md)). Normally `wh/dispatch` starts a design subagent instead (below). |

The lane agents, the reviewers and the helpers are subagents that `wh/dispatch` starts, not sessions of their own. Open the `wh/desk` and `wh/dispatch` prompts in `.agents/` in two sessions and check the model in each. Ask `wh/desk` for status, not `wh/dispatch`.

## Development branch names

New issue-work branches for developing WorkHarbor use
`<category>/<issue>-<slug>`. The category is an existing Conventional Commit
type (`feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `build`, `ci`, `chore`,
`style` or `revert`), or `spike` for measurement work. The issue is the actual
positive issue number, without `#` or leading zeros. The short slug uses
lowercase letters and digits in words separated by single hyphens; it starts
and ends with a letter or digit. For example, `feat/331-landing-queue` and
`spike/89-bundle-export`.

This convention applies to development issue branches, including new spike
branches. Product branches created by `whr` (such as `agent/<role>`), `main`,
remote refs, bot branches and test fixtures keep their existing contracts.
Rename only a branch you own; coordinate any rename with its author. A rename
keeps the commit SHA, so a review status bound to that SHA remains valid.

## Shared and personal Claude Code settings

The tracked `.claude/settings.json` shares project permissions without granting access to a maintainer’s external checkouts. Put personal paths and standing approvals (never board writes: [`AGENTS.md`](https://github.com/wstein/workharbor/blob/main/AGENTS.md), GitHub rate limit) in `.claude/settings.local.json`, which Git ignores, including when you create it by hand. Keep credentials out of both files.

The [Claude Code permission grammar](https://code.claude.com/docs/en/permissions#read-and-edit) defines `~/` file rules from the current user’s home and `//` rules from the filesystem root. The shared sensitive-directory denies also match those directories outside the current home. Bash rules match command text; their wildcards do not resolve a home directory or provide process isolation. Native enforcement across macOS and Linux remains {{< status unverified >}}; static configuration checks do not complete the session-usage verification in #274.

## Subagents and their models

The repository no longer ships pinned Claude Code subagent types or slash commands (`wh-*`). A session or subagent that a lane starts has its model set explicitly by whoever starts it, never inherited. Tool grants are not enforced any more either: the read-only helper and the read-only reviewers were limited by `tools:` frontmatter in the deleted prompts and are now prose only, so the starter (the dispatcher's Agent call) must restrict the tools.

A decision that loosens a Hard rule or a security control, changes release scope or order, costs money, publishes or sets product direction is not made by the subagent: it goes to you first, through `wh/desk` ([`AGENTS.md`](https://github.com/wstein/workharbor/blob/main/AGENTS.md), Models).

## The review gate

A review subagent is `wh/review`: its comment `CLEAR <full sha> role=review model=<m>` with no open findings is the review note, and no separate `wh/review` session is needed. The desk also posts it as the `review/*` status on the PR head SHA (Pull request flow below). The desk pushes the topic branch and opens the PR only after a CLEAR note with no open findings. The author never starts the review of its own change in its own context, and a dispatcher never reviews. A security-relevant change needs the Opus reviewer. You push only reviewed work.

## Pull request flow (#412)

Design: [pr-flow-landing](../design/pr-flow-landing.md) (Flow, Turnaround budget, Stacking cost, Migration plan). Every branch merges through a pull request; the old local landing (`make land`, the `land` pointer branch and the local review and confirm notes) is retired (#414) and its notes refs stay only as history.

1. **Draft PR.** For each branch the dispatcher runs `gh pr create --draft --base main --head <branch> --title "<issue title>" --body-file <file>`. The body holds `Closes #N`, the acceptance summary and a placeholder line `Verdict evidence: pending`. (During the #439 trial the desk does this after CLEAR; see [Desk push trial](#desk-push-trial-439).) The PR stays a draft until CLEAR; then the desk, and no other role, runs `gh pr ready <n>` once all checks are green on the exact head SHA and the required `review/*` status is success.
2. **Pre-PR review.** The required independent review runs on the local branch diff within the turnaround budget (design note, Turnaround budget); the PR is then ready with the actual reviewer context set. Choose the reviewer model with `sh scripts/review-class.sh <branch>`: it prints the path class and the required `review/*` context(s) from the same logic as the `gate` (a carve-out needs `review/opus` or `review/gpt-6.1-sol-high`).
3. **Status.** The desk posts the verdict on the PR head SHA:

```sh
gh api repos/wstein/workharbor/statuses/<sha> \
  -f state=success -f context=review/opus -f description="CLEAR role=review model=opus"
```

`<sha>` is the full head SHA of the PR (a status on any other SHA gates nothing). `state` is `success` for CLEAR; `failure` for NOT CLEAR. `context` identifies the actual reviewer: `review/sonnet`, `review/opus`, `review/gpt-6.1-sol-medium` or `review/gpt-6.1-sol-high`. Ordinary changes accept all four; carve-out changes accept only `review/opus` or `review/gpt-6.1-sol-high`. Other model and effort contexts fail closed. Never label a Sol review as Opus. The gate reads its scripts from the PR base, so Sol contexts count only after that base contains this mapping; updating the PR head alone does not enable them. `description` is a short human-readable line, at most 140 characters (from the GitHub docs, unverified here); the gate never trusts it. The gate checks the status creator, so the call runs under the human or desk identity (a token the design note names), never a token other than the allow-listed identity. Any newer status with the same context replaces the older one (from the GitHub docs, unverified here). Posting a status does not re-trigger the `gate` job, so after stamping the desk re-runs it (`gh run rerun <run-id>` or the Actions UI) {{< status unverified >}} (GitHub docs and recall, not measured).
4. **Evidence comment.** The desk posts one PR comment naming the full head SHA, the actual reviewer context (including the effort for Sol), the verdict and the evidence link or summary. After merge this comment is the only audit trail, since the rebase merge changes the SHAs.
5. **Re-posting after a rebase.** A rebase changes the head SHA, so the statuses are posted again on the new head. For an unchanged patch series the comment includes the output of `git range-diff <old-base>..<old-head> <new-base>..<new-head>` showing every commit as equal (`=`); any other marker means a new review, not a re-post. Desk how-to for a stack: `scripts/stack-status.sh --tier opus --reviewed <pr>=<oldbase>..<oldhead> <pr>...` (bottom PR first; dry run, add `--apply` to post and rerun the gate; it refuses when a range-diff shows a changed patch or a gate run is still running). The stack helper currently accepts only Sonnet and Opus tiers; re-post a Sol review directly with its actual model and effort context and the same range-diff proof, then rerun the gate. The human merges with `gh stack merge`; agents never merge.
6. **Merge.** The human merges with rebase and merge (the only merge method allowed). CI-watch reads the PR checks (`gh pr checks <n>`), and a red check on `main` after the merge is a defect issue as before.
7. **Stacks.** Related or dependent issues are stacked automatically with `gh stack`; the desk decides at dispatch time and records the order in the dispatch note. Who may run which `gh stack` command: [gh stack](gh-stack.md).
8. **Hand over.** The desk hands over the PR URL, its head SHA and any new `unverified` markers. Nobody but Werner merges.

Statuses are not signatures: anyone with write access can set one with any context; only the creator check in the gate separates them (design note, Security consequences). Known limits of the gate {{< status unverified >}}: a PR can edit `.github/workflows/gate.yml` itself (a carve-out path, so it needs `review/opus` or `review/gpt-6.1-sol-high` and the human sees it in the diff), and any holder of the human's token can post a `review/*` status.

## Desk push trial (#439)

Trial, revisit on 2026-11-08 after a few PRs and record the outcome (what went wrong, what the watcher missed). This is the one exception to "Push only when the human asks" in `AGENTS.md`.

- **Rule.** After the required CLEAR on the exact head SHA (and after any rebase with its range-diff proof) the desk may push that topic branch (never force), open the PR as a draft with `Closes #N` in the body, post `review/<tier>` plus the evidence comment, hand the check watching to a background helper, and mark the PR ready when the checks are green. Carve-out paths still need `review/opus` or `review/gpt-6.1-sol-high`. Only the desk runs `gh pr ready`; authors, reviewers, helpers, watchers and `wh/dispatch` never do. The human merges, by rebase. The PR-owning author may push further commits to its own topic branch to edit its PR (same rules: plain or `-u`, never force); reviewers, helpers, watchers and `wh/dispatch` never push.
- **Never.** Merge; push `main` or tags; force-push; change rulesets or repository settings; push a branch without the required CLEAR.
- **Settings (#448).** `.claude/settings.json` has no blanket `git push` deny. It allows `git push origin <p>/*` and `git push -u origin <p>/*` for `p` in `docs fix feat chore ci`, and denies force (`--force*`, `-f`, `-uf`, `-fu`), `+` and `:` refspecs, `main`, `master`, `HEAD`, `refs/`, tags, `--delete`, `-d`, `--mirror`, `--all`, `--prune`, `--no-verify` and `git -C/-c/--git-dir/--work-tree … push`; deny beats allow. It also allows the read-only `gh run watch` and `gh pr checks`. `gh pr ready` stays asking (it changes PR state). Measured in headless `claude -p` against a throwaway local bare remote (Claude Code 2.1.292, Haiku as the caller): the plain and `-u` topic pushes ran; `--force`, `-f`, `-uf`, `-fu`, `--force-with-lease`, `+branch`, `HEAD:main`, `branch:main`, `branch main`, `--delete`, `-d`, `--tags`, `--follow-tags`, `--mirror`, `--no-verify`, `git -C . push` and `git push -C .` did not run. A `--no-verify` push ran before its deny was added. The role split is a written rule, not a guarantee: the patterns do not cover other combined short flags (for example `-vf`), abbreviated long options (`--tag`, `--del`), extra refspec words after the topic branch (`git push origin docs/x develop`), pushes inside a script, an alias, `git remote set-url`, another remote name, or a different tool (`gh api`), and the allow list is not per role (every session in the project shares it). Branch rulesets on `main` remain the backstop. Project allows apply only in a trusted workspace; a measurement from an untrusted one is void. {{< status unverified >}} for interactive prompts (only the non-interactive result was measured).
- **Size budget.** A PR stays within 10 commits and about 500 lines.
- **Monitoring never blocks.** The desk and dispatch run no foreground sleep or poll loop. CI and check watching goes to a read-only background Haiku helper (model set explicitly, brief per `.agents/helper.md`) that reports once. A failure report holds the check name, the job URL and the first failing test line (`--- FAIL:` plus the next line). The helper never judges, reruns or comments; the desk decides. Red CI that the PR did not cause becomes a defect issue (never a "known failure", for example #438), and the desk reruns the failed jobs with `gh run rerun <id> --failed`.
- **Branch cleanup.** Automatic deletion after the merge covers finished work branches only (`fix/`, `feat/`, `docs/`, `chore/`, `ci/`). `spike/*` branches are never deleted automatically. GitHub deletes the remote head branch; local cleanup uses `git branch -d`, and `-D` only with `git cherry` or patch-id proof that the commits are in `main`.

## Setup steps

1. Run `make hooks` in every clone and worktree.
2. The coordinator prepares the lane worktrees (lanes do not run `git worktree add`, #328): it reuses an idle one first (`git switch -c <branch> main`) and creates one only when none is free, `git worktree add ../workharbor-<role> --detach main`, for `platform`, `runtime`, `docs` and `verify`, and `../workharbor-platform-2` for a second platform issue at the same time.
3. Open the `wh/desk` and `wh/dispatch` prompts and check their models.
4. Allow subagent starts without a prompt in the dispatch session only. The dispatcher's card moves are covered by [`AGENTS.md`](https://github.com/wstein/workharbor/blob/main/AGENTS.md) (GitHub rate limit); do not add a settings allow for them. Leave other board writes (`ready`, `session`, `priority`, `add`) asking each time.

### Client capacity

For development sessions using [crewbook](../glossary.md#names), start with capacity for eight subagents. `wh/dispatch` is an always-on session in its own terminal (see the table above), not a subagent of `wh/desk`, so neither session counts. From the dispatch session the subagents are three authors, two independent reviewers, one design batch and two bounded helpers: all eight slots, none spare. Each further author needs one more slot or one fewer helper or reviewer; there is no upper cap on code workers, and the desk accepts whatever count the human demands. A count from the desk session needs no more. This is a capacity recommendation, not a measured optimum or a request to fill every slot {{< status unverified >}}. The limits in [`AGENTS.md`](https://github.com/wstein/workharbor/blob/main/AGENTS.md) still apply: no upper cap on code workers (default two, three with approval), one editor per worktree, disjoint editing scopes and explicitly pinned role models. When actual capacity is lower, sequence work while preserving coordination and independent review.

**Codex.** For a fresh crewbook session from the repository:

```sh
codex -m gpt-6.1-sol -c model_reasoning_effort="low" -c agents.max_concurrent_threads_per_session=8 '$crewbook'
```

`$crewbook` invokes the crewbook skill by name in Codex (the skill's public name is `crewbook`, per the [glossary](../glossary.md#names)) {{< status unverified >}}; the single quotes keep the shell from expanding `$crewbook` as a variable and pass the text literally. The command sets no lane: the session becomes `wh/dispatch` or `wh/desk` when you paste [`.agents/dispatch.md`](https://github.com/wstein/workharbor/blob/main/.agents/dispatch.md) or [`.agents/desk.md`](https://github.com/wstein/workharbor/blob/main/.agents/desk.md) into it, as those prompts say. To persist just the capacity setting, add it to the existing `[agents]` table in user configuration; create that table only if it is absent:

```toml
[agents]
max_concurrent_threads_per_session = 8
```

The [official OpenAI configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference) defines this as the limit on concurrently open spawned-agent threads, excluding the primary thread {{< status unverified >}}. Codex chooses the default when unset {{< status unverified >}}. `agents.max_threads` is the legacy alias {{< status unverified >}}. A completed agent's turn does not by itself establish that its thread has closed; use the host's reported capacity.

**Claude Code.** For version 2.1.217 or later {{< status unverified >}}, start a session with:

```sh
CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS=8 CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH=3 claude
```

The [official nesting documentation](https://code.claude.com/docs/en/sub-agents#let-subagents-spawn-their-own-subagents) defines spawn depth as subagent layers below the main conversation {{< status unverified >}}. From the dispatch session, dispatch → author → bounded helper is two layers, so a depth of 2 suffices; 3 stays valid and leaves one spare layer, and the desk session is not an ancestor of either. Versions 2.1.217–2.1.218 default to one layer; 2.1.219 and later default to three {{< status unverified >}}. The explicit depth setting makes the example work with either default. It does not replace pinned tool restrictions, permissions or repository controls.

The [official concurrency documentation](https://code.claude.com/docs/en/sub-agents#concurrent-subagent-limit) accepts a positive integer {{< status unverified >}} and gives a default of 20 {{< status unverified >}}. It counts running subagents and blocks new Agent-tool starts at the limit {{< status unverified >}}. `/subtask` forks occupy slots but bypass the cap; resuming finished subagents can exceed it {{< status unverified >}}. Ultracode sessions are exempt, and workflows and agent teams have separate limits {{< status unverified >}}. Keep the existing role model pins; this setting changes capacity only.

**Antigravity (`agy`).** The [official subagent documentation](https://www.antigravity.google/docs/subagents/) describes parallel subagents and the `/agents` panel for inspecting their states {{< status unverified >}}. Inspect available capacity in the actual host and sequence work as needed. That source establishes no equivalent numerical concurrency setting {{< status unverified >}}; its nesting depth of ten is not a concurrency limit {{< status unverified >}}. Native role bindings and production support for this workflow remain {{< status unverified >}}.

## The project board

The sessions share one GitHub token, and a board query is the expensive call, so they read and write the board only through `scripts/board-snapshot.sh` (needs `bash`, `jq` and a logged-in `gh`; it holds no token). Never run `gh project item-list` or `gh project item-edit` yourself. The rules are in [`AGENTS.md`](https://github.com/wstein/workharbor/blob/main/AGENTS.md) (GitHub rate limit, Who sets which status); the script's header comment lists every mode.

```text
scripts/board-snapshot.sh                     # the snapshot JSON, cached for 5 minutes
scripts/board-snapshot.sh queue wh/platform   # the lane's Todo cards, P1 first, then by issue number
scripts/board-snapshot.sh card 132            # one card
scripts/board-snapshot.sh --refresh           # force a query
scripts/board-snapshot.sh budget              # refreshes, their cost and the lowest GraphQL budget left, last 24 hours; no gh call (#186)
```

Who may write cards, and what still asks, is set once in [`AGENTS.md`](https://github.com/wstein/workharbor/blob/main/AGENTS.md) (GitHub rate limit): authors, reviewers, design and `wh/desk` report outcomes to the dispatcher. `move` sets `Todo`, `In progress`, `Blocked` and `In review`; it refuses `Done` before any call. A card stays `In review` until the human closes or merges the PR; `Done` follows when the issue closes. A card moved by hand in the browser is not seen until the snapshot is 5 minutes old or a read passes `--refresh`.

### Board move and sync

`scripts/board-snapshot.sh move <n> <status>` reads the Status option id from the project field, writes the card, reads that one card back with a fresh query and exits 1 on a mismatch; a card already at the status gets no write. `scripts/board-snapshot.sh sync [--dry-run]` reconciles every open card and prints `#n old -> new (reason)` for each change; it calls `move` only where the status differs, so a second run changes nothing. Sync acts on positive evidence only and never lowers a card without it:

| Signal | Effect |
| --- | --- |
| Registry block (`.git/crewbook/registry.md` of the shared checkout) whose name starts with the issue number (`## 51`, `## 53-docs`, `## #57`) and phase `blocked` (waiting on a decision, the human or another issue) | Move to `Blocked` |
| Registry phase `start requested`, or a worktree on a branch `<type>/<n>-...` (author started), card `Todo` or without status | Move to `In progress` |
| Card `In progress` with neither a worktree nor such a phase (idle) | Move to `Todo` |
| Anything else, including phase `done` and no signal | Card unchanged |

`In review` is never set or lowered by sync: the dispatcher moves a card to `In review` when the PR opens. `Done` is never set; closed issues and `Done` cards are left alone. The registry is read up to its `Resume:` line; sync stops without changes when it is missing or unreadable. A reused worktree still on a stale `<n>-` branch counts as a signal, so detach an idle worktree. Call points: the dispatcher runs `sync` at its start and after every hand-back; run `sync --dry-run` first when unsure. Sync is a card write and follows the same permission rule as the other non-`move` writes, and the card-owner rule above.

## Rules of thumb

- Code workers (issue subagents that edit) run at the same time, each in its own worktree (two may both be `wh/platform`, in `../workharbor-platform` and `../workharbor-platform-2`, when their issues touch no file in common), and only one editing subagent per worktree. There is no upper cap: the default is two (three with the human's approval), and the desk accepts whatever count the human demands (H164, H175).
- Keep the Opus session short and end it after each decision.
- Do not hand running agents to a new session.

## Why

A long session pays for its whole history on every turn. Measured on 3 October 2026 (Werner's usage report, recorded in [#167](https://github.com/wstein/workharbor/issues/167)): Opus cost $25.38 of $35.69, and about 80 % of its tokens were the `wh/design` session re-reading its own history (54M of 67.4M cache reads over 209 requests) {{< status verified >}}. So the long-lived sessions run on Sonnet, Opus is used where it pays (decisions and reviews) and ends quickly, and each issue runs in a fresh subagent whose context is discarded when it returns. Keeping the default count of code workers low also spares the one GitHub token every session shares ([rate limit](https://github.com/wstein/workharbor/blob/main/AGENTS.md)).

## Procedure detail moved out of AGENTS.md

`AGENTS.md` stays below 20,000 bytes (a test in `internal/docscheck` fails above 24,000: #233). What follows is procedure and reference that a session needs only when it does the step; every rule and prohibition is still stated in `AGENTS.md`.

### Make targets

```bash
make build         # go build -o bin/whr ./cmd/whr
make test          # go test ./...  (make test-short skips the slowest, for the inner loop)
make race          # go test -race on the packages with goroutines of their own
make fmt           # gofumpt + goimports (make fmt-check fails on unformatted sources)
make lint          # golangci-lint (pinned; runs via go run)
make check-local   # fmt-check, lint, editorconfig, enabled hooks: before every commit
make check         # full fmt/vet/lint/editorconfig/test/race suite: CI or explicit human request
make check-ci      # what CI runs beyond make check: docs build, typos, lychee, gitleaks, actionlint
make commitlint    # check this branch's commits against the commit rules
make hooks         # enable hooks and the commit template (once per clone and worktree)
make generate      # compile the web UI's templ templates (the generated files are committed)
make docs          # build the Hugo site into _site (make docs-serve for live reload; the first run downloads the pinned prebuilt Hugo extended v0.165.0 into .cache/hugo and verifies its SHA-256, so no install and no Go build of Hugo is needed)
make temp-ls       # list temporary containers, volumes, networks and images (LANE=<lane> to narrow)
make temp-clean    # remove one lane's: LANE=<lane> is required
```

For each work item, run focused behaviour and regression checks for the changed code and affected dependencies, including race checks where concurrency changes. For documentation, check the affected build, links and wording; for generated files, verify source and output together. Record commands, outcomes, scope and exact candidate SHA in the handoff. Independent review covers that exact SHA after a rewrite. Reuse successful unaffected evidence; repeat checks only for relevant changes, failures or unresolved concerns.

Local checks are mechanical (`make check-local`, `make commitlint`); the secret scan and the generated-template check run in the hooks and in CI. Focused test selection and evidence remain the author and reviewer's responsibility, not a receipt enforced by make. Full local `make check` and `make check-ci` suites require an explicit human request before push; hosted CI retains its full suites. Hooks always run and are never bypassed.

The `Makefile` also has `install`, `install-release`, `changelog` and `editorconfig`. `make check-ci` needs `typos` and `lychee` (`brew install typos-cli lychee`).

### Lanes

The models and tasks of the lanes are in the [sessions table](#sessions-to-keep-open) near the top of this page and in [`AGENTS.md`](https://github.com/wstein/workharbor/blob/main/AGENTS.md) (Models). Two lanes are not in the table: `wh/review` (Opus) reviews independently before every push and is at least as strong as the author, and `wh/spikes` runs spikes on new tools on Antigravity (Gemini); `wh/review` reviews its results. A helper (`.agents/helper.md`) does one quick task for a lane: find and report, web research and issue drafts go to a read-only helper; mechanical edits, small tests and checks go to an editing helper. Board writes stay with the designated dispatcher through `scripts/board-snapshot.sh`; the lane reports outcomes to it. A helper is not a lane, and neither type edits a security-relevant path.

### Commits

```text
feat(domain): add run interrupted state

Why the change was made.

Refs: #12
Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>
```

Use the actual exposed model ID (`unknown` if unavailable), never a guessed model. The examples apply only to sessions exposing that model. Attribution identities and the legacy `Assisted-by` compatibility rule are in [AGENTS.md](../../../../AGENTS.md).

Fold a fix into its commit with `git commit --fixup` and an autosquash rebase (`GIT_SEQUENCE_EDITOR=: git rebase -i --autosquash main`), never a "fix the previous commit" commit. Until the beta, a fix to an already-pushed commit may be folded in the same way.

`Refs` and `Closes` also accept `owner/repo#12` and comma lists. `Closes #N` goes in the PR body and every commit keeps `Refs: #N` only (human decision, 2026-10-08). The trailer spellings: commitlint also accepts Close, Closes, Closed, Fix, Fixes, Fixed, Resolve, Resolves, Resolved, Refs and Related in any letter case with the colon optional (`CLOSES #12`), and refuses them in the subject. That GitHub closes the issue for these spellings is taken from its docs, not measured here. The changelog lists `feat`, `fix`, `perf`, `revert` and breaking changes. The repository allows only rebase merges, so every commit lands on `main` as written. Only Werner force-pushes, by lifting the `main` ruleset for it.

### Spike pages

A spike page under `docs/content/docs/spikes/` links the spike branch once it is pushed and says "a local branch until it is pushed" before that. Spike files that reached `main` anyway are removed from its tree, and the page links them at the commit that added them, which stays in `main`'s history (#194).

### Layout

The design is in `docs/content/docs/design/` (start at `_index.md`; §3 `decisions.md`, §4 `domain.md`, §5 and §8 `architecture.md`, §6 and §7 `security.md`, §9 and §10 `interfaces.md`, §11 to §13 `roadmap.md`), the threat model in `threat-model.md`; also `glossary.md`, `spikes/` and `manual/`. `cmd/` holds `whr` (the single binary), `whr-proxy` (the egress proxy in the sidecar), `whr-shim` (the in-guest launcher, D25) and `commitlint`. Among the packages of `internal/`: `domain`, `policy`, `store`, `hostgit`, `service`, `api`, `cli`, `serve`, `web`, `config`, `toolstore`, `egress`, `devcontainer`; the adapter contracts in `runtime/`, `agent/`, `forge/` and `ci/` have fakes and conformance suites in `runtimetest/` and `agenttest/`.

### Issues through REST

`gh api repos/wstein/workharbor/issues/<n>` reads an issue, `.../issues/<n>/comments -f body=...` comments, `-X PATCH .../issues/<n> -f body=...` ticks criteria, and `-X POST .../issues` opens one, then `scripts/board-snapshot.sh add` puts it on the board. REST does not prompt for labels, so a new issue carries one type label (`bug`, `enhancement`, `documentation`, `ops`, `decision`, `spike`, `security`, ...) and an `area:` label where one fits, set on the create with `-f "labels[]=<name>"` once per label. Before stopping for a rate limit, `gh api rate_limit` costs no points; a secondary limit with points left is waited out once for a minute and retried once (#214). `--url` on a project command trips a secondary limit even with points left, which is why the board goes through the script.

### Landing and worktrees

Work merges through a pull request (Pull request flow above); nobody commits on `main` directly. Keep your worktree your own and `git status --short` empty before you hand a branch over (commit or ask first; never stash someone else's work). Rebase onto `main` with `git rebase main` when `main` moved, fold fixes into the commit they belong to (your own unpushed commits only; for a content fix, stage it and run `git commit --fixup <sha>`, then `GIT_SEQUENCE_EDITOR=: git rebase -i --autosquash main`), and never use `--no-verify`. Never remove `index.lock`, or anything else, in the shared checkout, and never switch it yourself: stop and tell the human. After the merge, `git switch --detach main`, then `git branch -d <branch>`; use `-D` only after `git merge-base --is-ancestor <branch> main` confirms the branch is in.
**Shared checkout.** In the shared checkout run no git command other than the ones the flow names; `git status` rewrites the index. An `index.lock` there that persists is the human's to clear, never the lane's.


History: until the PR flow, the pointer branch `land` (deprecated name `landing`) was the develop branch and the local notes refs `review` and `confirm` recorded the review stamps and the human's confirmations. They are no longer written or read; the refs stay in the repository as history and nothing deletes them automatically.

Each lane's worktree is created by the coordinator when none is free, with `git worktree add <worktree> --detach main` and `make hooks` (the Setup steps above). In `wh/platform`'s second worktree, `workharbor.lane=wh/platform-2` keeps its temporary resources out of `make temp-clean LANE=wh/platform`.

### Ruleset change for the PR flow (human only)

The reviewed payloads are `.github/rulesets/main.active.json` (the target) and `main.previous.json` (the three old rules, for rollback); `scripts/ruleset-apply.sh` shows, plans, applies and restores them on ruleset 24335774 of `wstein/workharbor` (#413, design: [PR flow](../design/pr-flow-landing.md), migration steps 1 and 5). An agent never changes the ruleset: `plan`, `apply` and `restore` are dry runs by default, and the change is made only with `--apply` on a terminal after typing the confirmation line. No environment variable skips either. Before `apply` writes anything it saves the live ruleset as a new timestamped file (never overwritten) under `${XDG_STATE_HOME:-$HOME/.local/state}/workharbor/` (or `--backup-dir DIR`) and prints its path.

Required checks: `gate`, `commits`, `secrets`, `check (1.26.x)`, `check (1.27.x)`, `agent-cli`, `spelling`, `site`, `actionlint`, `zizmor`, `govulncheck`, `dependency-review`. Each has no path filter and runs on every pull request (workflow files read on 8 October 2026). Left out on purpose: `analyze` and `scorecard` (no `pull_request` trigger, so required they would block every merge), `links` (external network, flaky) and `docs-vulnerabilities`. `required_approving_review_count` is 0 because a solo maintainer cannot approve his own PR (the desk statuses and the `gate` carry the review); only `rebase` is allowed; thread resolution, stale-review dismissal and code-owner review are off because no approvals exist and bot threads would block the merge. `bypass_actors` stays empty (human decision); the escape hatch in an emergency is the restore command below. The `gate` entry carries `integration_id` 15368 (GitHub Actions), measured read-only on 8 October 2026 with `gh api /apps/github-actions --jq '{id,slug,name}'` (`show` and `plan` print this note), so a hand-posted `gate` status does not satisfy the rule; the other checks have no `integration_id`, because their source may differ. Limit: a PR can edit `gate.yml`, and a ruleset cannot pin a required workflow on a user-owned repository {{< status unverified >}}; the human merges every PR and reads changes to `.github/` himself, so `gate.yml` edits are always carve-out and need `review/opus` or `review/gpt-6.1-sol-high`. `strict_required_status_checks_policy` is `false`: strict would force a rebase and a full CI run before every merge of a stack {{< status unverified >}}.

Order:

1. Merge the `gate` workflow first (migration step 2). Open a test PR from a branch of this repository and prove the gate before any rule is on: it fails without a status, fails with a status from another account, and passes with the right one, for each class. See the other checks run on it too ({{< status unverified >}}: a required check from a `pull_request` workflow).
2. `scripts/ruleset-apply.sh show` and read the diff; `scripts/ruleset-apply.sh plan` prints the dry run.
3. Do the first PR-flow merge of a real branch (migration step 4): rebase merge, branch deleted, `main` linear. Required checks cannot be enforced earlier, because the old flow pushes directly.
4. Switch on: `scripts/ruleset-apply.sh apply --apply` (migration step 5). It prints the backup path first. Direct pushes to `main` stop working here.
5. Verify from a scratch clone: `git push origin <any local commit>:main` is refused by the ruleset. Do not run it from a checkout whose `main` has unpushed work.
6. Rollback or emergency escape hatch, any time: `scripts/ruleset-apply.sh restore <backup-file> --apply` (the file printed by step 4, in `${XDG_STATE_HOME:-$HOME/.local/state}/workharbor/`). `scripts/ruleset-apply.sh apply --stage previous --apply` restores the three old rules instead.
