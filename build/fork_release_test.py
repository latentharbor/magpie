import base64
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("release", Path(__file__).with_name("fork_release.py"))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def test_gate_requires_exact_green_main(self):
        sha = "a" * 40
        run = {"head_sha": sha, "head_branch": "main", "event": "push", "status": "completed",
               "conclusion": "success", "head_repository": {"full_name": release.REPOSITORY}}
        self.assertTrue(release.approved(sha, sha, [run]))
        self.assertFalse(release.approved(sha, "b" * 40, [run]))
        for key, bad in (("conclusion", "cancelled"), ("head_sha", "b" * 40),
                         ("head_branch", "topic"), ("event", "pull_request"),
                         ("status", "in_progress"), ("head_repository", {"full_name": "other/repo"})):
            with self.subTest(key=key):
                self.assertFalse(release.approved(sha, sha, [dict(run, **{key: bad})]))
        self.assertTrue(release.approved(sha, sha, [dict(run, event="workflow_dispatch")]))

    def test_next_version_is_newer_than_tags(self):
        self.assertEqual(release.next_version(["v0.1.1330", "v0.1.9", "garbage", "v0.1.1331-dev"]), "0.1.1331")
        self.assertEqual(release.next_version(["v1.0.0"]), "1.0.1")

    def test_release_rerun_is_idempotent(self):
        sha = "a" * 40
        old = {"tag_name": "v0.1.1331", "target_commitish": sha, "draft": False}
        self.assertIsNone(release.release_plan(sha, [old], ["v0.1.1331"]))
        self.assertEqual(release.release_plan(sha, [dict(old, draft=True)], ["v0.1.1331"]), "0.1.1331")
        self.assertEqual(release.release_plan("b" * 40, [old], ["v0.1.1331"]), "0.1.1332")

    def test_publication_rechecks_the_exact_build_version(self):
        sha = "a" * 40
        self.assertEqual(release.checked_plan(sha, [], ["v1.0.0"], "1.0.1"), "1.0.1")
        draft = {"tag_name": "v1.0.1", "target_commitish": sha, "draft": True}
        self.assertEqual(release.checked_plan(sha, [draft], ["v1.0.1"], "1.0.1"), "1.0.1")
        for releases, tags in (([], ["v1.0.1"]), ([dict(draft, draft=False)], ["v1.0.1"])):
            with self.subTest(releases=releases), self.assertRaisesRegex(ValueError, "plan changed"):
                release.checked_plan(sha, releases, tags, "1.0.1")

    def test_manifest_uses_only_release_assets(self):
        with tempfile.TemporaryDirectory() as folder:
            folder = Path(folder)
            for name in release.ASSETS:
                (folder / name).write_bytes(name.encode())
            private = folder / "logins.json"
            private.write_text("not for upload")
            with self.assertRaisesRegex(ValueError, "exactly"):
                release.manifest(folder, "0.1.1331", [])
            private.unlink()
            release.manifest(folder, "0.1.1331", [{"tag_name": "v0.1.1330", "body": "private detail"}])
            latest = json.loads((folder / "latest.json").read_text())
            self.assertEqual(len(latest["assets"]), 13)
            self.assertEqual(latest["schema"], 1)
            self.assertEqual(latest["repository"], release.REPOSITORY)
            self.assertEqual(len(list(folder.iterdir())), 16)
            for name, asset in latest["assets"].items():
                self.assertTrue(asset["url"].startswith("https://github.com/latentharbor/magpie/releases/download/v0.1.1331/"))
                self.assertEqual(len(asset["sha256"]), 64)
                self.assertEqual(asset["size"], len(name.encode()))
                self.assertEqual(asset["platform"], release.platform(name))
            self.assertNotIn("private detail", (folder / "notes.json").read_text())

    def test_manifest_signature_with_public_rfc8032_fixture(self):
        # Public RFC 8032 test vector: never a key for a real distribution.
        seed = bytes.fromhex("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
        public = bytes.fromhex("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
        encoded = base64.b64encode(public).decode()
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"FORK_RELEASE_PRIVATE_KEY": base64.b64encode(seed).decode()}):
            folder = Path(directory)
            raw = folder / "latest.json"
            raw.write_bytes(b'{"test":"bytes to authenticate"}\n')
            release.sign_manifest(folder, encoded)
            signature = base64.b64decode((folder / "latest.json.sig").read_text(), validate=False)
            self.assertEqual(len(signature), 64)
            sigfile = folder / "test-signature.bin"
            sigfile.write_bytes(signature)
            public_der = bytes.fromhex("302a300506032b6570032100") + public
            command = ["openssl", "pkeyutl", "-verify", "-rawin", "-pubin", "-inkey", "/dev/stdin",
                       "-keyform", "DER", "-in", str(raw), "-sigfile", str(sigfile)]
            result = release.subprocess.run(command, input=public_der, stdout=release.subprocess.PIPE, stderr=release.subprocess.PIPE)
            self.assertEqual(result.returncode, 0)
            raw.write_bytes(raw.read_bytes() + b"changed")
            result = release.subprocess.run(command, input=public_der, stdout=release.subprocess.PIPE, stderr=release.subprocess.PIPE)
            self.assertNotEqual(result.returncode, 0)
            with self.assertRaisesRegex(ValueError, "does not match"):
                release.sign_manifest(folder, base64.b64encode(bytes(32)).decode())

    def test_signing_key_format_is_unambiguous(self):
        for key in ("", "invalid", base64.b64encode(bytes(31)).decode(), base64.b64encode(bytes(32)).decode() + "\n"):
            with self.subTest(length=len(key)), self.assertRaises(ValueError):
                release.key_bytes(key, 32)


if __name__ == "__main__":
    unittest.main()
import base64
