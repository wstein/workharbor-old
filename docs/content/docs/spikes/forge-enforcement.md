---
title: "Spike #542: forge enforcement preparation"
description: "Offline safety fixtures and missing prerequisites for measuring a separate agent identity against GitHub rulesets."
weight: 30
---

The forge controls required by R-30 remain {{< status unverified >}}. No live
mutation attempt ran on 11 October 2026: a separate agent credential supplied
through a private env file was unavailable. The existing owner credential is
not the agent identity. No scratch repository or forge resource was created.

Source preparation is committed on
[`spike/542-forge-enforcement`](https://github.com/wstein/workharbor/tree/463234beaeeb3bc36c2eda2850924f9f2f2d1f13/spikes/forge-enforcement)
at `463234beaeeb3bc36c2eda2850924f9f2f2d1f13`. The harness, synthetic fixture, unedited offline output and local
check output stay on that spike branch. Only this summary belongs on `main`.
Tracks [#542](https://github.com/wstein/workharbor/issues/542),
[#27](https://github.com/wstein/workharbor/issues/27) and
[#230](https://github.com/wstein/workharbor/issues/230).

## What the source checks establish

Seven offline tests passed after failing cases were written first. They cover
private credential file handling, duplicate assignments and symlinks, response
redaction including echoed object keys, separate user identity and scratch
repository guards, missing ruleset visibility, response classification and
read-only preflight endpoint selection using fake transport. `make check-local`
passed. These checks establish source behavior, not GitHub enforcement.

The script runs fixture analysis or a read-only GitHub preflight. It does not
implement the five live mutation attempts. It reads a token only from an owned,
regular, single-link `0600` env file named by `WH_VERIFY_AGENT_ENV_FILE`; it never
uses ambient credentials, keychains or credential stores. Preflight uses fixed
GitHub GET endpoints without redirects or proxies. Its user identity guard
cannot authenticate an App installation, and does not prove token type or scope.
Ruleset pagination and inherited organization coverage remain incomplete.

## Live results

| Attempt in #542 | Result |
| --- | --- |
| Merge the agent's own pull request | Not run |
| Native git push to the default branch | Not run |
| Create a tag | Not run |
| Create a release through the API | Not run |
| Native git push outside `agent/*` | Not run |

Every fixture HTTP status is synthetic, including a deliberately allowed
release. A 401, 404, 422 or server error is inconclusive; a denial response still
needs its actual cause reviewed. A REST ref update would not prove native git
push behavior. Deployment is outside #542's five attempts and remains a separate
unmeasured R-30 requirement. An allowed topic push and PR open/update must also
be demonstrated for #27 before failed mutation attempts can establish anything
about the intended agent's permissions.

## Prerequisites and next measurement

The maintainer must supply a separate agent token with contents and pull-request
write through the private env file. The coordinator must then create or attest a
run-owned `whtmp-` repository and record immutable repository, owner and agent
IDs, the default branch, active rulesets and cleanup responsibility. The manifest
is an ownership attestation; the API response alone cannot establish ownership
by this run. Production resources and settings are outside this measurement.

Record ruleset conditions, bypass actors and applicable rules using the actual
identity, with no agent bypass and a required PR and human review on default and
integration branches. Namespace and tag restrictions require their own controls.
A separately reviewed live runner must perform native pushes with the shared
Git isolation contract and capture sanitized git output and API responses.
Neither credentials nor raw credential-bearing exceptions belong in evidence.

For #230, the actual WorkHarbor App installation token and independently known
App and installation IDs are additionally required. Capture `rulesets/{id}` and
`rules/branches/{default}` responses and the presence of `conditions`,
`bypass_actors` and `current_user_can_bypass`. Compare `Integration.actor_id`
against those known IDs, and measure inherited organization rulesets and
`~DEFAULT_BRANCH` targeting. The current user-token preflight cannot satisfy
these App criteria or justify any change to doctor's output.

No resources require cleanup now. A live run must inventory and remove only its
own scratch resources, preserve user-owned repositories and coordinate token
revocation. The spike README contains the manifest format, reproducible commands,
credential and native git isolation requirements, and the complete handoff.

All live criteria in #542, #27 and #230 stay open. D62 and threat-model markers
remain unchanged; their design owner may update them only from actual evidence.
The full documentation check was not run because it invokes a documentation
build, which requires the maintainer's explicit approval under the umbrella
repository instructions. No documentation or artifact build ran.
