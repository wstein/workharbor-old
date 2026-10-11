---
title: Pull request landing
description: "Design note: replace make land with the GitHub pull request flow, a gate workflow and commit statuses."
weight: 10
toc: true
---

## Landing through pull requests (#407)

{{< status open >}} Implemented in steps (gate workflow, ruleset payloads and apply script, statuses; retirement in #414); the measured and unverified markers below still apply. Human decision (8 October 2026): landing is fragile and hurts DX, so it moved to the standard GitHub pull request flow. The old `make land`, `scripts/land.sh`, the `land` pointer branch and the local review and confirm notes are retired history; the notes refs stay in the repository unread.

Each item of the issue checklist is marked here as verified (how) or unverified. Verified on 8 October 2026 with read-only `gh api` GET calls, the repository files, or GitHub documentation as recalled (named as such).

### Flow

1. **One PR per branch, rebase and merge.** The repository allows rebase merge only (squash and merge commits are off), so the atomic conventional commits survive. The human pressing the merge button replaces the typed short SHA and the terminal of `land.sh`. {{< status verified >}} Merge settings read with `gh api repos/wstein/workharbor`; auto-merge allowed, `delete_branch_on_merge` on.
2. **Ruleset on `main`.** Add a pull request rule, required status checks and no bypass. {{< status verified >}} for the current state, see the table below.
3. **No required approvals.** A solo maintainer cannot approve his own PR. The desk sets the actual reviewer context on the PR head SHA (`POST /repos/{r}/statuses/{sha}`); the evidence goes into a PR comment. This replaces `refs/notes/review` and `TO_LAND.md`.
4. **A `gate` workflow.** On `pull_request`, read-only, pinned actions, job-level `permissions: {contents: read, statuses: read}` (`statuses` is a documented permission scope, {{< status unverified >}} until a workflow runs). It lists the changed files, derives the class, reads `GET /repos/{r}/commits/{head}/statuses`, and fails unless the required tier is present and its `creator.login` is the human or the desk identity.
5. **Stacking.** Small independent PRs merged in order, each rebased on the new `main`. A chain of PR bases is optional; the fast lane (#405) maps onto one PR per lane.
6. **Roles.** The dispatcher opens a draft PR per branch until CLEAR (during the #439 trial the desk opens the PR instead of the dispatcher); the desk sets the statuses and posts the evidence comment; CI-watch reads the checks of the PR; only the desk marks it ready (`gh pr ready`), once all checks are green on the exact head and the required `review/*` status is success; the human merges (or enables auto-merge once the checks are green). Trial (#439, revisit 2026-11-08): after the required CLEAR on the exact head SHA the desk makes the first push of that topic branch (never force; later commits may also be pushed by the PR-owning author, see manual), opens the draft PR with `Closes #N`, posts `review/<tier>` and the evidence comment, has a read-only background helper watch the checks and report once (check name, job URL, first failing test line; monitoring never blocks the desk) and marks the PR ready when they are green and `review/*` is success; carve-out paths still need `review/opus` or `review/gpt-6.1-sol-high`. Never: merge; push `main` or tags; force-push; change rulesets or repository settings; push without the required CLEAR. Finished work branches are deleted after the merge, `spike/*` never automatically; a PR stays within 10 commits and about 500 lines. Full text, failure report format and cleanup commands: [manual](../manual/sessions-and-agents.md#desk-push-trial-439). Commands and the closing-line policy are in the [manual](../manual/sessions-and-agents.md#pull-request-flow-412).

### Class logic and its single copy

The approved review models and efforts are defined in [`AGENTS.md`](https://github.com/wstein/workharbor/blob/main/AGENTS.md), Models. Claude defaults remain unchanged; the gate also accepts the explicit Sol contexts documented in the [manual](../manual/sessions-and-agents.md#pull-request-flow-412). Sol high qualifies for carve-out changes, while Sol medium qualifies only for ordinary changes. The reviewer evidence and context must name the actual model and effort. The gate uses its base copy of the scripts, so a PR head change alone cannot enable these contexts.

The retired `scripts/land.sh` derived the class from `git diff --name-only <main> <tip>`, never from the note. Each path is lowercased and matched in order:

- **carve-out** (needs an Opus or approved Sol high CLEAR): `agents.md`, `claude.md`, `.claude/*`, `.agents/*` (any depth), `.github/*`, `docs/content/*design*`, `docs/content/*threat*`, and every path not allowed below.
- **ordinary** (any CLEAR): `internal/exitcode/*`, `internal/version/*`, `internal/docscheck/*`, `docs/*.md`, `readme.md`, `changelog.md`, `contributing.md`, `license`.
- One carve-out path makes the whole branch a carve-out. The retired landing flow allowed a carve-out tip with a Sonnet CLEAR only when every commit of `main..tip` was covered by its own Opus CLEAR or an equal patch-id (#365); the gate does not inherit those reviews.

The path matcher is one file, `scripts/path-class.sh` (POSIX sh, reads newline-separated paths on stdin, prints `ordinary` or `carve-out`; a name containing a newline splits into fragments that need not fall to carve-out, so the gate uses `scripts/gate-class.sh`, which reads NUL-separated paths and fails closed to carve-out for such a path). The gate will run it from the base branch checkout (never from the PR head, so a PR cannot change those scripts), and `land.sh` called it until retirement. Patch-id inheritance is not part of the gate (see Stacking cost).

### Verified and unverified

| # | Item | State | Basis |
| --- | --- | --- | --- |
| 1 | Rebase merge only, auto-merge, branch deletion | verified | `gh api repos/wstein/workharbor` |
| 2 | Ruleset `main` (id 24335774), active, no bypass actors | verified | `gh api repos/wstein/workharbor/rulesets/24335774`: rules are `deletion`, `required_linear_history`, `non_fast_forward`; `bypass_actors` is empty; `current_user_can_bypass` is `never` |
| 2 | Missing in the ruleset (before step 5) | verified | no `pull_request` rule, no `required_status_checks` rule. Direct pushes to `main` are therefore still possible (fast-forwards), which the retired `make land` relied on. Adding the PR rule ends that; "admins cannot bypass" already holds (empty bypass list) |
| 3 | Statuses by the desk's token | unverified | the Statuses API exists and the token has `push: true` (`gh api repos/wstein/workharbor --jq .permissions`); nothing was posted, because the task forbids writes |
| 4 | Workflows and check names | verified | `.github/workflows` and `gh api repos/wstein/workharbor/commits/main/check-runs`: ci has `check (1.26.x)`, `check (1.27.x)`, `agent-cli`, `commits` (pull_request only); docs-quality has `links`, `spelling`, `site`; security has `actionlint`, `zizmor`, `govulncheck`, `dependency-review` (pull_request only); scan has `secrets`, `docs-vulnerabilities`; `analyze` is CodeQL. The required names are these job names (matrix names include the Go version); `gate` does not exist yet. Pick the set before the ruleset change: a check that runs only on pull_request must not be required for pushes |
| 4 | Required check from a `pull_request` workflow | unverified | ordinary GitHub behavior (checks of any workflow can be required once they ran in the repository), not tested here. A required check that does not run (path filter, skipped job) blocks merging: `gate` must run on every PR |
| 4 | Status `creator` is recorded | verified | the Statuses API returns a `creator` object per status (seen in the API schema; the history of `main` has no statuses to show an example) |
| 4 | Workflow can read statuses | unverified | `statuses: read` is a documented `permissions` scope; not run here |
| 5 | Merge queue on a personal repository | unverified | recalled from the documentation: merge queue is for organization-owned repositories; the owner here is type User. Plan without it |
| 7 | Migration | decided | below |
| 9 | crewbook files | unverified | not readable from this worktree (`/Users/werner/workspaces` holds only `workharbor`) |

### Security consequences

- **Who can set a status.** Anyone with write access, with any context name and any token of theirs, including every agent whose token can push. A status is not a signature. The gate may only accept a status whose `creator.login` is the human or one named desk identity; that check is as strong as the secrecy of that token, and the desk token is a write token on this repository.
- **What the gate can trust.** The changed paths (computed by the gate from the PR, not from a note), the class rule (run from the base branch) and the creator of a status. It cannot trust the PR text, a comment, a status description, or the head of the PR for the gate's own code. It cannot tell whether a reviewer really reviewed.
- **Forks.** The workflow uses `pull_request` (never `pull_request_target`, a hard rule). A fork PR gets a read-only token and no secrets, so its gate cannot read statuses reliably ({{< status unverified >}}); fork PRs are outside the flow, only branches of this repository are landed.
- **The human merging his own PR.** Nothing separates author from merger; the merge click is the consent, and the checks and statuses are the only guard. A compromised human token can set any status and merge: the same trust as the retired `land.sh`, whose prompt was a UX safeguard against a same-user agent, not a boundary.
- **Rebase merge changes the SHAs.** Statuses sit on the PR head SHA and do not carry to the new commits on `main`; they gate the merge only. After the merge, `main` carries no review record except the PR comment, so the evidence comment is the audit trail, and it must name the head SHA and the tier.
- **Weaker than today in one respect.** The retired `land.sh` ran `check-local`, `commitlint` and the secrets range on the exact tip locally; PR checks run on a merge ref or head in CI instead. The required list in the ruleset must include `commits` and `secrets`.

### Turnaround budget

Goal (human): fast local development, a loop under 5 minutes; Opus review before the PR, or earlier on demand. The gate stays path-class based (same logic as the retired `land.sh`), so the budget changes only when checks and reviews run.

1. **Local loop.** Author time in minutes. Local checks are the targeted tests of the touched packages, `typos` and lint; no `-race ./...`, and `./scripts` tests only when `scripts/` changed. The heavy full run is the `ci` workflow on the PR, not a local gate.
2. **Pre-PR Opus review, under 5 minutes.** It runs on the local branch diff before the PR is opened. The result is a verdict line plus evidence. The PR is then opened ready with the actual reviewer context set, and CI runs in parallel with nothing else blocking. An earlier Opus review of a commit range is allowed at any time (desk command or agent brief).
3. **What keeps Opus under 5 minutes.** Small PRs: at most 10 commits and about 500 changed lines; larger work splits into several PRs. The reviewer gets the diff, the issue acceptance and the author's checklist result, reads only what the diff touches, runs at most `go vet` plus targeted tests once (private `GOCACHE`, deleted afterwards), does no mutation and no full test, and does not re-review an unchanged SHA.
4. **Measured data and assumption.** In the sessions so far, first Opus reviews took about 2.5 to over 10 minutes, and 5 of 12 first rounds were NOT CLEAR. Assumption {{< status unverified >}}: a pre-review author checklist (#405 B) lowers the NOT CLEAR rate, and review rounds, not CI, are the main time cost. The checklist is only worth keeping if the rate drops.

### Stacking cost

CLEAR is bound to a head SHA. A rebase changes every SHA after the first changed base, so each later PR needs new statuses. The patch-id inheritance of the retired `land.sh` (#365) is lost: for an unchanged patch the desk re-posts the CLEAR on the new head together with a `git range-diff` showing the patches equal. Chained PR bases keep the diff small, but retargeting after the base merges still rebases. Expect one status round per PR per rebase; the fast lane (#405) needs one PR per lane.

Measured on 2026-10-08 in `wstein/pr-stack-lab`, a lab repository with a copy of the real gate scripts from `main` 848057e (`gh stack` v0.2.0; ruleset requires only `gate` from the GitHub Actions app, rebase-only):

1. The per-PR class works in a stack: a README PR on `main` is ordinary, a scripts PR on the parent branch is carve-out. Without a status both fail; with `review/sonnet` only, ordinary passes and carve-out fails; with `review/opus` the carve-out PR passes after a rerun.
2. Posting a status does not start the gate; `gh run rerun` is needed, and a rerun of a still-running run is refused.
3. The `ready_for_review` type fires the gate; a still-queued gate blocks `gh stack merge` atomically.
4. With `gate` success on both heads, `gh stack merge` merged both PRs. Required checks are enforced per PR inside the atomic stack merge, so an unreviewed lower PR blocks the whole stack. A gate must use `base.sha`, never `base_ref` (in a stacked PR run `base_ref` was `main` while the PR base was the parent branch).
5. After merging only the bottom PR, GitHub retargets the next PR to `main` and moves its head; the new head has no statuses and is blocked, so each remaining PR needs its review status re-posted on the new head plus a gate rerun after every merge below it.

A status from a second account could not be measured alone; the allow-list check is unit-tested only {{< status unverified >}}.

Rule: the human merges with `gh stack merge`; agents never merge. After every merge below a PR the head moves and its statuses are gone: the desk runs `scripts/stack-status.sh` (dry run first, then `--apply`), which re-posts `review/<tier>` on each current head, accepts a moved head only with a `git range-diff` proof that all patches are unchanged (`=`), and reruns the gate. A changed patch needs a new review.

### What must change at retirement

Done in #414. In this repository: `scripts/land.sh` and its tests (`scripts/land_*_test.go`, `scripts/linear_land_test.go`, `scripts/land_confirmation_test.go`); the Makefile targets `land`, `land-list`, `land-next`, `land-all`, `land-preview` (and `.PHONY`); the `land` pointer branch and its deprecated name `landing` (`.agents/design.md`, `.agents/helper.md`, `.agents/review.md`); `refs/notes/review` and `refs/notes/confirm` and `docs/confirmation-record.md` with `internal/confirm` (decided: the refs stay as history, `internal/confirm` is removed); `TO_LAND.md` (kept by the dispatcher outside git here; not present in the tree); `scripts/index-state.sh` and `.claude/settings.json` allowances that mention landing; `AGENTS.md` (commands line, "Merge into `main` only through `make land`", board statuses); `.agents/code.md`, `.agents/docs.md`, `.agents/dispatch.md`, `.agents/review.md`; `docs/content/docs/manual/sessions-and-agents.md` (landing, landing order, `land-preview`); and `CONTRIBUTING.md`.

In crewbook (merged mode): `docs/git-history.md#landing-pointer` and `#rebase-re-review`, and the `tools/review_lines.py` gate, plus the same landing text in its agent docs. {{< status unverified >}} Those files could not be read; the desk files that issue (checklist 9).

### Migration plan

1. **Ruleset check.** Decide the required check set from the table; the rules are not switched on yet (no evaluate mode is used). The PR rule and required checks are switched on only after the gate is proven (step 2). `scripts/ruleset-apply.sh` saves a backup of the live ruleset before `apply`, and `restore` is the escape hatch (manual, Ruleset change for the PR flow). Exit: check set decided and payloads committed.
2. **Shared path script and `gate`.** Add `scripts/path-class.sh`, make `land.sh` call it (tests unchanged), add the `gate` workflow. The gate (`.github/workflows/gate.yml`, #411) checks out the base commit, lists the changed paths with `git diff --name-only --no-renames base...head`, runs the base copy of `scripts/gate-class.sh` (which calls `path-class.sh` and fails closed to carve-out for a path containing a newline), reads the head's statuses and decides in `scripts/gate-check.sh` (latest status per context; creator must be `wstein`; the optional desk login `GATE_DESK` stays empty, because all sessions share the human's token. A separate desk identity, a machine user or a GitHub App, is a possible later hardening, not now). Known limit: a `pull_request` run uses the workflow file of the PR's own merge ref, so a same-repo PR can edit `gate.yml` (or `GATE_HUMAN`, or add `exit 0`) and the edited gate reports a pass; the base checkout protects the scripts, not the workflow that calls them. Such a PR touches `.github/*` (carve-out), so it needs `review/opus` or `review/gpt-6.1-sol-high` and the human merging sees it. Rulesets cannot pin a required workflow to a ref on a user-owned repo {{< status unverified >}} (recalled as org-only). Anyone using the `wstein` token can post their own `review/*` status; only allow-list creators count. Posting a status does not re-trigger the `gate` job (measured, #450: PR #449 gate passed at 11:51:44 on `review/opus` success, the status went `pending` at 11:52:37, the ruleset requires only `gate`, so the stale pass stayed). `.github/workflows/gate-rerun.yml` (trigger `status`, default-branch file, no checkout, `actions: write`) therefore re-runs the head's gate run whenever a `review/*` status is posted; a manual `gh run rerun` stays possible {{< status unverified >}} (the status trigger has not run on a real PR yet). {{< status unverified >}} Not run on a real PR yet. The `pull_request` trigger lists `edited`, so a base change or edit re-runs the gate. A test PR proves the gate before the PR rule and required checks are switched on. Exit: `make check-local` green, and on the test PR the gate fails without a status, fails with a status from another account, passes with the right one for each class.
3. **Statuses and comments.** The desk posts `review/*` statuses and the evidence comment next to the notes (both, in parallel). Exit: three PRs show the same verdict as the note. Also measure the pre-PR Opus review time and the NOT CLEAR rate on those PRs against the budget above.
4. **First PR-flow merge** (done) of a real branch, with `make land` untouched. Exit: it merged by rebase, branch deleted, `main` linear.
5. **Enforce** (done; ruleset read back with `gh api`: `pull_request` and `required_status_checks` present; direct-push refusal not tested). Turn on the PR rule and required checks; `make land` stops working for direct pushes, so keep it only until step 4 succeeded. Exit: a direct push to `main` is refused.
6. **Retire** (done in #414) `land.sh`, the targets, the `land` pointer, `TO_LAND.md` and the notes, and update the files above. Exit: `docscheck` and `make check-local` green, no remaining reference found by `grep -rn "make land\|land.sh\|refs/notes/review"`.
