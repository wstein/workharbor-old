#!/usr/bin/env python3
"""Offline fixture analysis and credential-safe, read-only GitHub preflight.

This harness never performs mutation attempts. Native pushes and API mutation
responses must be captured by a separately reviewed live runner on owned scratch
resources. Fixture success proves only source behavior, never forge enforcement.
"""
import argparse
import json
import os
from pathlib import Path
import re
import stat
import urllib.error
import urllib.request
import urllib.parse

ATTEMPTS = (
    "merge-own-pr", "native-push-default", "create-tag", "release-api",
    "native-push-outside-agent-namespace",
)


def read_token(path):
    """Open atomically without following symlinks; never return file content errors."""
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(descriptor, "r", encoding="utf-8") as source:
            metadata = os.fstat(source.fileno())
            if (not stat.S_ISREG(metadata.st_mode)
                    or stat.S_IMODE(metadata.st_mode) != 0o600
                    or metadata.st_uid != os.getuid() or metadata.st_nlink != 1):
                raise ValueError("credential file must be owned, regular, single-link and 0600")
            content = source.read(16385)
    except (OSError, UnicodeError):
        raise ValueError("credential file cannot be read safely") from None
    if len(content) > 16384:
        raise ValueError("credential file exceeds size limit")
    rows = [line for line in content.splitlines() if line and not line.startswith("#")]
    if len(rows) != 1 or not rows[0].startswith("GITHUB_TOKEN="):
        raise ValueError("credential file must contain exactly one GITHUB_TOKEN assignment")
    token = rows[0].removeprefix("GITHUB_TOKEN=")
    if not token or any(character.isspace() for character in token):
        raise ValueError("credential assignment is empty or malformed")
    return token


def sanitize(value, token):
    """Redact at ingestion, including arbitrary server echoes and credential fields."""
    if isinstance(value, dict):
        return {sanitize(key, token): ("[REDACTED]" if re.search(
            r"authorization|token|secret|password|cookie", key, re.I)
            else sanitize(item, token)) for key, item in value.items()}
    if isinstance(value, list):
        return [sanitize(item, token) for item in value]
    if isinstance(value, str):
        return value.replace(token, "[REDACTED]") if token else value
    return value


def guard(manifest, repository, identity):
    name = manifest.get("repository", "")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/whtmp-[A-Za-z0-9_.-]+", name):
        raise ValueError("only an explicitly owned whtmp repository is permitted")
    if manifest.get("created_for") != "542" or manifest.get("owned_by_run") is not True:
        raise ValueError("scratch ownership attestation is missing")
    if (repository.get("full_name") != name
            or repository.get("id") != manifest.get("repository_id")
            or repository.get("owner", {}).get("id") != manifest.get("owner_id")):
        raise ValueError("repository identity differs from coordinated manifest")
    if (identity.get("type") != "User" or not identity.get("id")
            or identity.get("id") != manifest.get("agent_id")
            or identity.get("id") == manifest.get("owner_id")):
        raise ValueError("a separate, manifest-pinned fine-grained user identity is required")
    if not repository.get("default_branch"):
        raise ValueError("default branch is not visible")


def ruleset_view(data):
    conditions = data.get("conditions")
    bypass = data.get("bypass_actors")
    visible = isinstance(conditions, dict) and isinstance(bypass, list)
    refs = conditions.get("ref_name", {}) if isinstance(conditions, dict) else {}
    review = any(rule.get("type") == "pull_request"
                  and rule.get("parameters", {}).get("required_approving_review_count", 0) >= 1
                  for rule in data.get("rules", []))
    return {
        "visibility": "complete" if visible else "incomplete",
        "current_user_can_bypass_present": "current_user_can_bypass" in data,
        "current_user_can_bypass": data.get("current_user_can_bypass"),
        "integration_actor_ids": [actor.get("actor_id") for actor in (bypass or [])
                                  if actor.get("actor_type") == "Integration"],
        "review_guard_present": bool(visible and bypass == [] and review
                                      and data.get("enforcement") == "active"
                                      and data.get("target") == "branch"
                                      and refs.get("include") == ["~DEFAULT_BRANCH"]
                                      and refs.get("exclude") == []),
    }


def classify(status):
    if 200 <= status < 300:
        return "allowed"
    if status in (403, 409):
        return "denied-needs-cause-review"
    return "inconclusive"


def offline_report(fixture):
    guard(fixture["manifest"], fixture["repository"], fixture["identity"])
    return {
        "measurement": "offline-fixture", "status": "unverified",
        "ruleset_view": ruleset_view(fixture["ruleset"]),
        "attempts": [{"name": name, "live_result": "not-run",
                      "fixture_http_status": fixture["attempt_statuses"][name],
                      "fixture_classification": classify(fixture["attempt_statuses"][name])}
                      for name in ATTEMPTS],
        "limitations": ["No native git push or API mutation was executed.",
                        "No deployment control was measured.",
                        "App actor_id meaning and organization inheritance remain unmeasured."],
    }


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def get(path, token):
    """Fixed GitHub origin, GET only, no redirects, proxy credentials or error URLs."""
    if not path.startswith("/") or "?" in path or "#" in path:
        raise ValueError("invalid API path")
    request = urllib.request.Request("https://api.github.com" + path, headers={
        "Authorization": "Bearer " + token, "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "whtmp-forge-542",
    })
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        response = opener.open(request, timeout=30)
    except urllib.error.HTTPError as error:
        response = error
    except (OSError, urllib.error.URLError):
        return {"path": path, "http_status": None, "body": "transport failure"}
    with response:
        raw = response.read(1048577)
        if len(raw) > 1048576:
            return {"path": path, "http_status": response.code, "body": "response too large"}
        body = raw.decode("utf-8", errors="replace")
        try:
            body = json.loads(body)
        except ValueError:
            pass
        return sanitize({"path": path, "http_status": response.code, "body": body}, token)


def preflight(manifest, token):
    # Validate the target before making even a read request.
    guard(manifest, {"full_name": manifest.get("repository"),
                      "id": manifest.get("repository_id"),
                      "owner": {"id": manifest.get("owner_id")}, "default_branch": "pending"},
          {"id": manifest.get("agent_id"), "type": "User"})
    identity = get("/user", token)
    repository = get("/repos/" + manifest["repository"], token)
    if identity["http_status"] != 200 or repository["http_status"] != 200:
        return {"measurement": "live-read-only-preflight", "status": "unverified",
                "responses": [identity, repository], "blocked": "identity or repository unreadable"}
    guard(manifest, repository["body"], identity["body"])
    prefix = "/repos/" + manifest["repository"]
    listing = get(prefix + "/rulesets", token)
    responses = [identity, repository, listing]
    if listing["http_status"] == 200 and isinstance(listing["body"], list):
        for row in listing["body"]:
            ruleset_id = row.get("id")
            if isinstance(ruleset_id, int) and ruleset_id > 0:
                responses.append(get(prefix + "/rulesets/" + str(ruleset_id), token))
    branch = urllib.parse.quote(repository["body"]["default_branch"], safe="")
    responses.append(get(prefix + "/rules/branches/" + branch, token))
    return {"measurement": "live-read-only-preflight", "status": "unverified",
            "responses": responses, "mutation_attempts": "not-run",
            "limitations": ["Fine-grained user preflight; does not measure App visibility.",
                            "Ruleset listing is not paginated; completeness is unverified.",
                            "No token scope or forge enforcement inference is permitted."]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", type=Path)
    parser.add_argument("--preflight-manifest", type=Path)
    args = parser.parse_args()
    if bool(args.fixture) == bool(args.preflight_manifest):
        parser.error("choose exactly one fixture or read-only preflight manifest")
    try:
        if args.fixture:
            report = offline_report(json.loads(args.fixture.read_text()))
        else:
            filename = os.environ.get("WH_VERIFY_AGENT_ENV_FILE")
            if not filename:
                raise ValueError("WH_VERIFY_AGENT_ENV_FILE is required; no ambient credential fallback")
            token = read_token(filename)
            report = preflight(json.loads(args.preflight_manifest.read_text()), token)
        print(json.dumps(report, indent=2, sort_keys=True))
    except (ValueError, OSError, KeyError, TypeError):
        # Never print exception text: parsing and network errors may contain secrets.
        parser.exit(2, "preflight failed safely; inspect manifest/file format without printing credentials\n")


if __name__ == "__main__":
    main()
