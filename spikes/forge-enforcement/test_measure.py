"""Offline safety and classification checks; no GitHub enforcement claims."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("measure", Path(__file__).with_name("measure.py"))
measure = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(measure)


class SafetyTests(unittest.TestCase):
    def test_credential_file_requires_private_regular_owned_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "agent.env"
            path.write_text("GITHUB_TOKEN=test-fixture-token\n")
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                measure.read_token(path)
            path.chmod(0o600)
            self.assertEqual(measure.read_token(path), "test-fixture-token")
            link = Path(directory) / "link"
            link.symlink_to(path)
            with self.assertRaises(ValueError):
                measure.read_token(link)
            path.write_text("GITHUB_TOKEN=one\nGITHUB_TOKEN=two\n")
            with self.assertRaises(ValueError):
                measure.read_token(path)

    def test_redaction_covers_echoed_token_and_auth_headers(self):
        raw = {"message": "echo test-fixture-token", "Authorization": "Bearer other",
                "nested": {"token": "other", "test-fixture-token": "echo"}, "url": "https://test-fixture-token@example.test"}
        encoded = json.dumps(measure.sanitize(raw, "test-fixture-token"))
        self.assertNotIn("test-fixture-token", encoded)
        self.assertNotIn("other", encoded)

    def test_scratch_guard_rejects_owner_identity_and_repository_reuse(self):
        manifest = {"repository": "fixture/whtmp-forge-542", "repository_id": 542,
                    "owner_id": 1, "agent_id": 2, "created_for": "542",
                    "owned_by_run": True}
        repository = {"id": 542, "full_name": manifest["repository"],
                      "owner": {"id": 1}, "default_branch": "main"}
        measure.guard(manifest, repository, {"id": 2, "type": "User"})
        for changed in [{"id": 1, "type": "User"}, {"id": 3, "type": "User"}]:
            with self.assertRaises(ValueError):
                measure.guard(manifest, repository, changed)
        for field, value in [("repository", "fixture/workharbor"),
                              ("owned_by_run", False), ("repository_id", 543)]:
            altered = dict(manifest, **{field: value})
            with self.assertRaises(ValueError):
                measure.guard(altered, repository, {"id": 2, "type": "User"})

    def test_ruleset_missing_visibility_is_unknown_and_bypass_is_unsafe(self):
        self.assertEqual(measure.ruleset_view({})["visibility"], "incomplete")
        visible = {"id": 9, "target": "branch", "enforcement": "active",
                    "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
                    "bypass_actors": [], "rules": [{"type": "pull_request", "parameters": {
                        "required_approving_review_count": 1}}]}
        self.assertTrue(measure.ruleset_view(visible)["review_guard_present"])
        visible["bypass_actors"] = [{"actor_type": "Integration", "actor_id": 123,
                                      "bypass_mode": "always"}]
        self.assertFalse(measure.ruleset_view(visible)["review_guard_present"])
        self.assertEqual(measure.ruleset_view(visible)["integration_actor_ids"], [123])

    def test_only_authorization_denials_are_candidate_enforcement_evidence(self):
        for status in (401, 404, 422, 500):
            self.assertEqual(measure.classify(status), "inconclusive")
        for status in (403, 409):
            self.assertEqual(measure.classify(status), "denied-needs-cause-review")
        self.assertEqual(measure.classify(201), "allowed")

    def test_preflight_uses_only_guarded_read_endpoints(self):
        fixture = json.loads(Path(__file__).with_name("fixture.json").read_text())
        manifest = fixture["manifest"]
        prefix = "/repos/" + manifest["repository"]
        bodies = {"/user": fixture["identity"], prefix: fixture["repository"],
                  prefix + "/rulesets": [{"id": 9}],
                  prefix + "/rulesets/9": fixture["ruleset"],
                  prefix + "/rules/branches/main": [{"type": "pull_request"}]}
        calls = []

        def fake_get(path, token):
            calls.append(path)
            return {"path": path, "http_status": 200, "body": bodies[path]}

        with patch.object(measure, "get", side_effect=fake_get):
            report = measure.preflight(manifest, "test-fixture-token")
        self.assertEqual(set(calls), set(bodies))
        self.assertEqual(report["status"], "unverified")
        self.assertEqual(report["mutation_attempts"], "not-run")
        with patch.object(measure, "get", side_effect=AssertionError("network forbidden")):
            with self.assertRaises(ValueError):
                measure.preflight(dict(manifest, repository="fixture/workharbor"), "test-fixture-token")

    def test_fake_transport_cannot_produce_verified_live_result(self):
        fixture = json.loads(Path(__file__).with_name("fixture.json").read_text())
        with patch.object(measure, "get", side_effect=AssertionError("network forbidden")):
            result = measure.offline_report(fixture)
        self.assertEqual(result["status"], "unverified")
        self.assertEqual(result["measurement"], "offline-fixture")
        self.assertEqual(len(result["attempts"]), 5)
        self.assertTrue(all(row["live_result"] == "not-run" for row in result["attempts"]))


if __name__ == "__main__":
    unittest.main()
