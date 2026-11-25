"""Offline assertions on rendered credential sources (no cluster or real Secrets)."""
import base64
from pathlib import Path
import subprocess
import unittest

import yaml

CHART = Path(__file__).resolve().parents[1] / "pulse"
TOKEN = "PULSE_INGEST_TOKEN"
BOUND = "PULSE_INGEST_TOKENS"
FILE = "PULSE_INGEST_TOKENS_FILE"
PASSWORD = "PULSE_AUTH_PASSWORD"
HASH = "PULSE_AUTH_PASSWORD_HASH"
AUTH_SECRET = "test-pulse-auth"
INGEST_SECRET = "test-pulse-viewer"  # also the generated name in 0.1.0


def render(values):
    return subprocess.run(
        ["helm", "template", "test", str(CHART), "-f", "-"],
        input=yaml.safe_dump(values), text=True, capture_output=True,
    )


class Credentials(unittest.TestCase):
    def manifests(self, values):
        result = render(values)
        self.assertEqual(result.returncode, 0, result.stderr)
        return [d for d in yaml.safe_load_all(result.stdout) if d]

    def pod_templates(self, values):
        templates = {d["metadata"]["labels"]["app.kubernetes.io/component"]: d["spec"]["template"]
                     for d in self.manifests(values) if d["kind"] == "Deployment"}
        self.assertEqual(set(templates), {"viewer", "prober"})
        return templates

    def check(self, values, secrets, auth, ingest, prober, extra_keys=()):
        docs = self.manifests(values)
        managed = {d["metadata"]["name"]: set(d["data"]) for d in docs if d["kind"] == "Secret"}
        self.assertEqual(managed, secrets)
        pods = {d["metadata"]["labels"]["app.kubernetes.io/component"]: d["spec"]["template"]["spec"]
                for d in docs if d["kind"] == "Deployment"}
        containers = {role: pod["containers"][0] for role, pod in pods.items()}
        for c in containers.values():
            self.assertNotIn("envFrom", c)
        viewer = {e["name"]: e for e in containers["viewer"]["env"]}
        self.assertEqual({k for k in viewer if k.startswith("PULSE_INGEST_")}, {ingest[0]})
        for key in [auth[1], *extra_keys]:
            self.assertEqual(viewer[key]["valueFrom"]["secretKeyRef"]["name"], auth[0])
            self.assertEqual(viewer[key]["valueFrom"]["secretKeyRef"]["key"], key)
        if ingest[0] == FILE:
            self.assertEqual(viewer[FILE]["value"], "/etc/pulse/ingest-tokens")
            volumes = {v["name"]: v for v in pods["viewer"]["volumes"]}
            self.assertEqual(volumes["ingest-tokens"]["secret"], {
                "secretName": ingest[1], "items": [{"key": "bindings", "path": "ingest-tokens"}],
            })
            self.assertIn({"name": "ingest-tokens", "mountPath": "/etc/pulse", "readOnly": True},
                          containers["viewer"]["volumeMounts"])
        else:
            self.assertEqual(viewer[ingest[0]]["valueFrom"]["secretKeyRef"], {"name": ingest[1], "key": ingest[0]})
        self.assertEqual(containers["prober"]["env"], [{"name": prober[0], "valueFrom": {
            "secretKeyRef": {"name": prober[1], "key": TOKEN},
        }}])
        return {d["metadata"]["name"]: d["data"] for d in docs if d["kind"] == "Secret"}

    def test_default_generated_shared(self):
        data = self.check({}, {AUTH_SECRET: {PASSWORD}, INGEST_SECRET: {TOKEN}},
                          (AUTH_SECRET, PASSWORD), (TOKEN, INGEST_SECRET), (TOKEN, INGEST_SECRET))
        self.assertEqual(len(base64.b64decode(data[INGEST_SECRET][TOKEN])), 32)

    def test_external_auth_with_explicit_shared(self):
        data = self.check({"viewer": {"auth": {"existingSecret": "ui"}, "ingest": {"token": "fixture"}}},
                          {INGEST_SECRET: {TOKEN}}, ("ui", PASSWORD), (TOKEN, INGEST_SECRET),
                          (TOKEN, INGEST_SECRET), [HASH])
        self.assertEqual(base64.b64decode(data[INGEST_SECRET][TOKEN]), b"fixture")

    def test_external_shared(self):
        self.check({"viewer": {"auth": {"passwordHash": "fixture"}, "ingest": {"existingSecret": "shared"}}},
                   {AUTH_SECRET: {HASH}}, (AUTH_SECRET, HASH), (TOKEN, "shared"), (TOKEN, "shared"))

    def test_one_external_combined_secret(self):
        self.check({"viewer": {"auth": {"existingSecret": "combined"}, "ingest": {"existingSecret": "combined"}}},
                   {}, ("combined", PASSWORD), (TOKEN, "combined"), (TOKEN, "combined"), [HASH])

    def test_bound_env(self):
        data = self.check({"viewer": {"ingest": {"tokens": "prober=fixture", "existingSecret": "sender"}},
                           "prober": {"tokenEnv": "SENDER_TOKEN"}},
                          {AUTH_SECRET: {PASSWORD}, INGEST_SECRET: {BOUND}}, (AUTH_SECRET, PASSWORD),
                          (BOUND, INGEST_SECRET), ("SENDER_TOKEN", "sender"))
        self.assertEqual(base64.b64decode(data[INGEST_SECRET][BOUND]), b"prober=fixture")

    def test_file_is_only_ingest_source(self):
        self.check({"viewer": {"auth": {"existingSecret": "ui"}, "ingest": {
            "token": "ignored", "tokens": "ignored=too", "existingTokensSecret": "receiver",
            "tokensSecretKey": "bindings", "existingSecret": "sender"}}},
            {}, ("ui", HASH), (FILE, "receiver"), (TOKEN, "sender"), [PASSWORD])

    def test_no_release_revision_dependency(self):
        # helm template always sets Revision to 1; comparing two renders alone
        # would miss a rollout on every live upgrade.
        for path in (CHART / "templates").rglob("*"):
            if path.is_file():
                self.assertNotIn(".Release.Revision", path.read_text(), str(path))

    def test_prober_only_and_unrelated_changes(self):
        before = self.pod_templates({})
        after = self.pod_templates({"prober": {"pushInterval": "15s"}})
        self.assertEqual(before["viewer"], after["viewer"])
        self.assertNotEqual(before["prober"], after["prober"])
        self.assertEqual(before, self.pod_templates({"ingress": {"enabled": True}}))

    def test_revisions_roll_only_env_consumers(self):
        for ingest, replicas, ingest_consumers in [
            ({}, 1, {"viewer", "prober"}),
            ({"existingSecret": "shared"}, 1, {"viewer", "prober"}),
            ({"tokens": "prober=fixture", "existingSecret": "sender"}, 1, {"viewer", "prober"}),
            ({"existingTokensSecret": "receiver", "existingSecret": "sender"}, 1, {"prober"}),
            ({"tokens": "remote=fixture"}, 0, {"viewer"}),
            ({"existingTokensSecret": "receiver"}, 0, set()),
        ]:
            viewer = {"ingest": ingest, "auth": {"existingSecret": "ui"} if ingest else {}}
            values = {"viewer": viewer, "prober": {"replicaCount": replicas}}
            before = self.pod_templates(values)
            for section, consumers in [("auth", {"viewer"}), ("ingest", ingest_consumers)]:
                with self.subTest(ingest=ingest, section=section):
                    changed = {**viewer, section: {**viewer[section], "revision": 2}}
                    after = self.pod_templates({**values, "viewer": changed})
                    self.assertEqual({role for role in before if before[role] != after[role]}, consumers)

    def test_inline_credentials_change_only_secrets(self):
        for auth_key in ["password", "passwordHash"]:
            for ingest_key in ["token", "tokens"]:
                with self.subTest(auth_key=auth_key, ingest_key=ingest_key):
                    def readable(value):
                        ingest = ({"tokens": "prober=" + value, "existingSecret": "sender"}
                                  if ingest_key == "tokens" else {"token": value})
                        docs = self.manifests({"viewer": {"auth": {auth_key: value}, "ingest": ingest}})
                        return [d for d in docs if d["kind"] != "Secret"]

                    self.assertEqual(readable("fixture-one"), readable("fixture-two"))

    def test_expected_probers(self):
        for replicas, viewer, expected in [
            (1, {}, "bundled"),
            (0, {}, ""),
            (0, {"expectedProbers": ["remote-west", "remote-east"]}, "remote-west,remote-east"),
        ]:
            with self.subTest(replicas=replicas, viewer=viewer):
                pods = self.pod_templates({"viewer": viewer, "prober": {"replicaCount": replicas, "name": "bundled"}})
                env = pods["viewer"]["spec"]["containers"][0]["env"]
                self.assertEqual([e["value"] for e in env if e["name"] == "PULSE_EXPECTED_PROBERS"], [expected])

    def test_incomplete_or_conflicting_config(self):
        for values, message in [
            ({"ingest": {"existingTokensSecret": "receiver"}}, "require viewer.ingest.existingSecret"),
            ({"ingest": {"tokens": "prober=fixture"}}, "require viewer.ingest.existingSecret"),
            ({"auth": {"existingSecret": INGEST_SECRET}}, "collides"),
            ({"ingest": {"existingSecret": AUTH_SECRET}}, "collides"),
        ]:
            with self.subTest(values=values):
                result = render({"viewer": values})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)


if __name__ == "__main__":
    unittest.main()
