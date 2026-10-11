# Forge enforcement preparation (#542, #27, #230)

Status: **unverified**. This branch has source fixtures and read-only preflight
code, not live forge enforcement evidence. No scratch repository, ruleset,
branch, pull request, tag, release, deployment or credential was created.
The existing owner's GitHub credential is not the separate agent identity.
No configured verifier credential-file variable identified an available file.
Credential stores and keychains were not inspected.

## Reproduce the offline evidence

```sh
python3 -m unittest discover -s spikes/forge-enforcement -p 'test_*.py'
python3 spikes/forge-enforcement/measure.py --fixture spikes/forge-enforcement/fixture.json
```

`offline-output.json` is the unedited output of the second command. Every HTTP
status in it is invented fixture data. The release fixture deliberately allows
creation; an authorization error, missing resource or invalid payload must not
be misreported as proof of enforcement. A 403 or 409 remains a candidate denial
that needs its actual response and cause reviewed. The five live results remain
`not-run`. Fixture tests cover credential file modes and symlinks, duplicate
assignments, response redaction, identity and scratch guards, ruleset visibility,
and preflight endpoint selection without network access.

## Exact live prerequisites

1. The maintainer supplies a separate agent identity, with contents and pull
    requests write, using an owned, regular, single-link `0600` env file containing
    exactly one `GITHUB_TOKEN=` assignment. Only its filename is supplied through
    `WH_VERIFY_AGENT_ENV_FILE`. Never use the owner's ambient `gh` identity as
    the agent or inspect `gh auth`, git credential stores or keychains.
2. The coordinator creates or explicitly records a repository owned by this run
    with a `whtmp-` name, its immutable repository and owner IDs, agent ID, default
    branch and cleanup responsibility. An existing user-owned scratch repository
    is insufficient: the harness requires `owned_by_run: true` and `created_for:
    "542"`. These are coordinator attestations, not API proof of creation.
3. The coordinator records active branch/tag rulesets, requires a PR and human
    review on the default and integration branch, and verifies no agent bypass.
    Namespace and tag restrictions must be configured and inspected separately;
    the default-branch PR rule alone does not establish them. Capture ruleset
    IDs, exact conditions, bypass actors, applicable branch rules, token scopes
    and actor identity. Do not alter production settings or resources.
4. A separately reviewed live runner must exercise a valid agent topic push
    and PR open/update first, then the five negative attempts: merge its own
    PR, **native git push** to default, create a tag, release API creation and
    **native git push** outside `agent/*`. Capture sanitized native git output
    and API status/body, including successful forbidden operations. REST ref
    updates do not count as native push evidence. This harness has no mutation
    implementation; do not interpret its preflight as running these attempts.
    The future native runner must use the shared `internal/gittest` isolation
    contract: system/global Git config disabled, helpers reset, no interactive
    prompt, no SSH agent, isolated writable HOME and caches, and repository hooks
    disabled. Read the token from its private file in process; never put it in
    argv, environment values, a remote URL, git config or terminal output. It
    must never call `git credential`, `gh auth` or a keychain utility.
5. #230 additionally needs the actual WorkHarbor App installation token and
    independently recorded App/installation IDs. Compare actual `Integration`
    bypass `actor_id` to those IDs; do not guess its meaning. Capture App reads
    of `rulesets/{id}` and `rules/branches/{default}`, field presence, inherited
    organization rulesets and default-branch targeting. The current `/user`
    preflight supports a fine-grained user token only and refuses App identity.
    Missing fields, pagination or unavailable organization coverage stay unknown.

The read-only preflight can run only after coordination of steps 1–2:

```sh
python3 spikes/forge-enforcement/measure.py --preflight-manifest /path/to/manifest.json
```

The manifest schema is illustrated by `fixture.json`'s `manifest` object; replace
all fixture IDs and names with coordinator-recorded facts in a standalone file.
The program reads only `api.github.com`, disables redirects and proxies, and
never falls back to ambient credentials. Output is redacted at ingestion, with
no response headers or raw exception text. Store only sanitized evidence, never
the env file. Incomplete list pagination is disclosed rather than inferred away.
A caller should treat exit 2 as an invalid preflight and inspect file formatting
without printing credentials. A readable preflight does not establish token
permission scope, token type or approve mutation attempts.

## Cleanup and handoff

No live resources exist for this run, so there is nothing to delete. For the live
run, record creator, immutable resource IDs and all created branches, PRs, tags
and releases; coordinate cleanup of only resources owned by that run. Do not
remove a user-owned repository or unrelated resources. Revoke the run token
through the maintainer's normal credential procedure.

Leave #542's live criteria, #27's actual agent publication/enforcement criteria
and every #230 live criterion unticked. Deployment is not one of #542's five
attempts and remains a separate unmeasured R-30 requirement. The design owner
alone may change protected D62 or threat-model statuses after actual evidence;
these fixtures justify no status change.
