#!/usr/bin/env python3
"""Exercise the actual promotion shell step with a simulated image registry."""
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[1]
IMAGE = "ghcr.io/example/bot"
DIGEST = "sha256:" + "a" * 64
OTHER = "sha256:" + "b" * 64


def fake_docker():
    path = Path("registry.json")
    state = json.loads(path.read_text())
    args = sys.argv[2:]
    state["calls"].append(args)
    path.write_text(json.dumps(state))
    if args[:3] == ["buildx", "imagetools", "inspect"]:
        if state.get("error"):
            print(state["error"], file=sys.stderr)
            return 1
        if not state.get("digest"):
            print("ERROR: " + args[3] + ": not found", file=sys.stderr)
            return 1
        print("Digest:    " + state["digest"], flush=True)
        # Larger than a pipe buffer: an early-exiting reader must not truncate
        # the producer and turn an otherwise successful promotion into failure.
        sys.stdout.write("  manifest details\n" * 100000)
        return 0
    if args[:3] == ["buildx", "imagetools", "create"]:
        if state.get("create_error"):
            return 1
        state["digest"] = state.get("created_digest", DIGEST)
        path.write_text(json.dumps(state))
        return 0
    raise RuntimeError("Unexpected Docker command: " + repr(args))


class PromotionTests(unittest.TestCase):
    def run_promotion(self, **state):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        step = workflow.split("      - name: Tag the tested manifest without rebuilding\n", 1)[1]
        script = textwrap.dedent(step.split("        run: |\n", 1)[1].split("      - name:", 1)[0])
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "dist").mkdir()
            candidate = {"image": IMAGE + "@" + DIGEST, "commit": "c" * 40}
            (root / "dist/candidate.json").write_text(json.dumps(candidate))
            state["calls"] = []
            (root / "registry.json").write_text(json.dumps(state))
            docker = root / "docker"
            docker.write_text("#!/bin/sh\nexec " + shlex.join([
                sys.executable, str(Path(__file__).resolve()), "--fake-docker"
            ]) + ' "$@"\n')
            docker.chmod(0o755)
            env = dict(os.environ, IMAGE=IMAGE, VERSION="v1.0.1")
            env["PATH"] = str(root) + os.pathsep + env["PATH"]
            result = subprocess.run(["bash", "-c", script], cwd=root, env=env,
                                    capture_output=True, text=True, timeout=15)
            state = json.loads((root / "registry.json").read_text())
            release = root / "dist/release.json"
            payload = json.loads(release.read_text()) if release.exists() else None
        return result, state, payload

    def test_new_version_consumes_full_manifest(self):
        result, state, release = self.run_promotion()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(release["image"], IMAGE + "@" + DIGEST)
        self.assertEqual(release["version"], "v1.0.1")
        self.assertEqual(sum(call[2] == "create" for call in state["calls"]), 1)

    def test_retry_reuses_identical_version_without_writing(self):
        result, state, release = self.run_promotion(digest=DIGEST)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIsNotNone(release)
        self.assertTrue(all(call[2] == "inspect" for call in state["calls"]))

    def test_existing_different_version_is_never_overwritten(self):
        result, state, release = self.run_promotion(digest=OTHER)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("refusing to overwrite", result.stderr)
        self.assertIsNone(release)
        self.assertEqual(len(state["calls"]), 1)

    def test_registry_failure_is_not_treated_as_missing_image(self):
        for error in ("unauthorized", "connection timed out", "500 Internal Server Error"):
            with self.subTest(error=error):
                result, state, release = self.run_promotion(error=error)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(error, result.stderr)
                self.assertIsNone(release)
                self.assertEqual(len(state["calls"]), 1)

    def test_digest_mismatch_after_push_blocks_release(self):
        result, _, release = self.run_promotion(created_digest=OTHER)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not match", result.stderr)
        self.assertIsNone(release)

    def test_push_failure_blocks_release(self):
        result, _, release = self.run_promotion(create_error=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(release)


class CandidateMergeTests(unittest.TestCase):
    def run_merge(self, *, arm64=OTHER, **state):
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        step = workflow.split("      - name: Merge native images into the immutable candidate\n", 1)[1]
        script = textwrap.dedent(step.split("        run: |\n", 1)[1].split("  smoke:\n", 1)[0])
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "image-digests").mkdir()
            (root / "image-digests/amd64.txt").write_text(DIGEST)
            if arm64 is not None:
                (root / "image-digests/arm64.txt").write_text(arm64)
            state["calls"] = []
            (root / "registry.json").write_text(json.dumps(state))
            docker = root / "docker"
            docker.write_text("#!/bin/sh\nexec " + shlex.join([
                sys.executable, str(Path(__file__).resolve()), "--fake-docker"
            ]) + ' "$@"\n')
            docker.chmod(0o755)
            output = root / "output"
            env = dict(os.environ, REPO="example/bot", GITHUB_SHA="c" * 40,
                       GITHUB_OUTPUT=str(output))
            env["PATH"] = str(root) + os.pathsep + env["PATH"]
            result = subprocess.run(["bash", "-c", script], cwd=root, env=env,
                                    capture_output=True, text=True, timeout=15)
            state = json.loads((root / "registry.json").read_text())
            outputs = output.read_text() if output.exists() else ""
        return result, state["calls"], outputs

    def test_merge_uses_both_native_digests(self):
        result, calls, outputs = self.run_merge()
        self.assertEqual(result.returncode, 0, result.stderr)
        create = next(call for call in calls if call[2] == "create")
        self.assertEqual(create[-2:], [IMAGE + "@" + DIGEST, IMAGE + "@" + OTHER])
        self.assertIn("digest=" + DIGEST, outputs)
        self.assertIn("image=" + IMAGE, outputs)

    def test_retry_does_not_rewrite_existing_candidate(self):
        result, calls, _ = self.run_merge(digest=DIGEST)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][2], "inspect")

    def test_incomplete_or_invalid_architecture_blocks_merge(self):
        for arm64 in (None, "invalid-digest"):
            with self.subTest(arm64=arm64):
                result, calls, outputs = self.run_merge(arm64=arm64)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, [])
                self.assertEqual(outputs, "")

    def test_registry_failure_blocks_merge(self):
        result, calls, outputs = self.run_merge(error="unauthorized")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(calls), 1)
        self.assertEqual(outputs, "")


if __name__ == "__main__":
    if sys.argv[1:] and sys.argv[1] == "--fake-docker":
        sys.exit(fake_docker())
    unittest.main()
