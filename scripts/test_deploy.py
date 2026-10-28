#!/usr/bin/env python3
"""Exercise the actual deployment script and native maintenance binary.

Docker/Telegram availability is simulated; SQLite, WAL snapshots, encryption,
restore and shell state transitions run for real. No real account is contacted.
"""
import json
import os
from pathlib import Path
import shlex
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import unittest

REPOSITORY = "ghcr.io/huliyoudiangou/yueer_abs_bot"
PINS = {v: REPOSITORY + "@sha256:" + c * 64 for v, c in
        [("v1.0.0", "a"), ("v1.0.1", "b"), ("v9.0.0", "c")]}


def fake_docker(args):
    root = Path(os.environ["FAKE_DOCKER_ROOT"])
    state_file = root / "fake-state.json"
    state = json.loads(state_file.read_text())

    def save():
        state_file.write_text(json.dumps(state))

    def release():
        return dict(line.split("=", 1) for line in (root / ".release.env").read_text().splitlines())

    state["calls"].append(args)
    save()
    if args[0] == "compose":
        args = args[1:]
        while args and args[0].startswith("-"):
            args = args[2:]
        cmd = args[0]
        if cmd == "version":
            print("Docker Compose fake test boundary")
        elif cmd == "stop":
            state["running"] = False
            save()
        elif cmd == "up":
            state["version"] = release()["BOT_VERSION"]
            state["running"] = True
            value = state.get("write_on_start", {}).get(state["version"])
            if value is not None:
                with sqlite3.connect(root / "data/bot_data.db") as db:
                    db.execute("UPDATE balances SET points=?", (value,))
            save()
        elif cmd == "ps":
            print("fake-container" if "-q" in args else str(state["running"]))
        elif cmd == "logs":
            print("simulated startup/health failure log")
        else:
            raise RuntimeError("unexpected compose operation: " + repr(args))
        return 0
    if args[0] == "pull":
        if state.get("pull_fail"):
            print("simulated registry unavailable", file=sys.stderr)
            return 1
        return 0
    if args[:2] == ["image", "inspect"]:
        if "--format" not in args:
            print("{}")
            return 0
        version = args[-1].rsplit(":", 1)[1]
        print(state.get("tags", {}).get(version, PINS[version]))
        return 0
    if args[0] == "inspect":
        healthy = state.get("version") not in state.get("fail_health", [])
        print(("running healthy" if healthy else "running unhealthy") if state["running"] else "exited unhealthy")
        return 0
    if args[0] == "run":
        args = args[1:]
        while args and args[0].startswith("-"):
            args = args[1:] if args[0] in ("--rm", "--read-only") else args[2:]
        image, cmd = args[0], args[1:]
        schema = "9" if image == PINS["v9.0.0"] else "1"
        if cmd == ["version", "--schema"]:
            print(schema)
            return 0
        if cmd == ["version"]:
            print(json.dumps({"version": "test", "commit": "test", "schemaCompatibility": schema}))
            return 0
        index = cmd.index("maintenance")
        cmd = cmd[index:]
        if os.environ.get("BOT_TEST_DOCKER_IMAGE"):
            return subprocess.run([
                os.environ["REAL_DOCKER"], "run", "--rm", "--network", "none", "--read-only", "--tmpfs", "/tmp", "--user", f"{os.getuid()}:{os.getgid()}",
                "--mount", f"type=bind,src={root / 'data'},dst=/app/data",
                "--mount", f"type=bind,src={root / '.env'},dst=/app/.env,readonly",
                "--mount", f"type=bind,src={root / 'deploy-state/backups'},dst=/backups",
                "--entrypoint", "/app/ccoff-abs-bot", os.environ["BOT_TEST_DOCKER_IMAGE"], *cmd]).returncode
        cmd = [str(root / "deploy-state/backups" / a.removeprefix("/backups/")) if a.startswith("/backups/") else a for a in cmd]
        env = dict(os.environ, DATABASE_URL="data/bot_data.db")
        return subprocess.run([os.environ["BOT_TEST_BINARY"], *cmd], cwd=root, env=env).returncode
    raise RuntimeError("unexpected Docker operation: " + repr(args))


class DeploymentRehearsal(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.build = tempfile.TemporaryDirectory(prefix="bot-maintenance-")
        cls.binary = str(Path(cls.build.name) / "bot")
        cls.repo = Path(__file__).resolve().parents[1]
        go = shutil.which("go")
        if not go and (cls.repo / ".tools/go/bin/go").is_file():
            go = str(cls.repo / ".tools/go/bin/go")
        cls.docker = None
        if go:
            subprocess.run([go, "build", "-o", cls.binary, "."], cwd=cls.repo, check=True,
                           env=dict(os.environ, CGO_ENABLED="1", GOFLAGS="-mod=readonly"))
        else:
            cls.docker = shutil.which("docker")
            if not cls.docker:
                raise RuntimeError("Deployment rehearsal requires Go or Docker")
            subprocess.run([cls.docker, "build", "-t", "ccoff-abs-bot:maintenance-rehearsal", "."], cwd=cls.repo, check=True)

    @classmethod
    def tearDownClass(cls):
        cls.build.cleanup()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="bot-deploy-rehearsal-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "deployment"
        self.root.mkdir()
        shutil.copy(self.repo / "deploy.sh", self.root)
        shutil.copy(self.repo / "docker-compose.yml", self.root)
        (self.root / "data").mkdir()
        (self.root / ".env").write_text(
            "APP_ENV=production\nABS_MODE=real\nTELEGRAM_BOT_TOKEN=123:test\n"
            "ADMIN_TELEGRAM_IDS=1\nABS_API_URL=https://abs.example.invalid\n"
            "ABS_API_KEY=fake\nABS_TEMPLATE_USER_ID=fake\n"
            "DATABASE_URL=data/bot_data.db\nBACKUP_ENCRYPT_KEY=" + "k" * 64 +
            "\nSECURITY_PEPPER=" + "p" * 64 + "\n")
        with sqlite3.connect(self.root / "data/bot_data.db") as db:
            db.executescript("CREATE TABLE deployment_metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL); INSERT INTO deployment_metadata VALUES('schema_compatibility','1'); CREATE TABLE balances(id INTEGER PRIMARY KEY,points INTEGER); INSERT INTO balances VALUES(1,123);")
        self.state_path = self.root / "fake-state.json"
        self.state_path.write_text(json.dumps({"running": False, "calls": []}))
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        shim = bin_dir / "docker"
        shim.write_text("#!/bin/sh\nexec " + shlex.quote(sys.executable) + " " + shlex.quote(str(Path(__file__).resolve())) + ' --fake-docker "$@"\n')
        shim.chmod(0o755)
        self.env = dict(os.environ, PATH=str(bin_dir) + os.pathsep + os.environ["PATH"],
                        FAKE_DOCKER_ROOT=str(self.root), BOT_TEST_BINARY=self.binary,
                        DEPLOY_WAIT_SECONDS="0")
        if self.docker:
            self.env.update(REAL_DOCKER=self.docker, BOT_TEST_DOCKER_IMAGE="ccoff-abs-bot:maintenance-rehearsal")
        for name in ("APP_ENV", "ABS_MODE", "DATABASE_URL", "BACKUP_ENCRYPT_KEY", "SECURITY_PEPPER", "IMAGE_REPOSITORY"):
            self.env.pop(name, None)

    def state(self, **changes):
        state = json.loads(self.state_path.read_text())
        state.update(changes)
        self.state_path.write_text(json.dumps(state))
        return state

    def run_deploy(self, *args, success=True):
        result = subprocess.run(["bash", "deploy.sh", *args], cwd=self.root,
                                env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        self.assertFalse((self.root / "deploy-state/.lock").exists(), "lock not released")
        return result

    def points(self):
        with sqlite3.connect(self.root / "data/bot_data.db") as db:
            return db.execute("SELECT points FROM balances").fetchone()[0]

    def test_failed_update_compatible_rollback_and_explicit_restore(self):
        self.run_deploy("update", "v1.0.0")
        self.state(fail_health=["v1.0.1"], write_on_start={"v1.0.1": 456})
        result = self.run_deploy("update", "v1.0.1", success=False)
        self.assertIn("NOT restored", result.stderr)
        self.assertFalse(self.state()["running"])
        self.assertEqual(self.points(), 456, "failed update silently restored a snapshot")
        backup = (self.root / "deploy-state/last-backup.txt").read_text().strip()
        self.run_deploy("rollback", "v1.0.0")
        self.assertEqual(self.points(), 456, "image rollback lost post-upgrade writes")
        self.run_deploy("restore", backup, success=False)
        self.assertTrue(self.state()["running"], "missing confirmation stopped the Bot")
        self.run_deploy("restore", backup, "--confirm-data-loss")
        self.assertEqual(self.points(), 123)
        self.assertTrue(self.state()["running"])
        self.assertTrue(list((self.root / "deploy-state/backups").glob("*before-restore*")))

    def test_pull_or_compatibility_failure_keeps_old_instance(self):
        self.run_deploy("update", "v1.0.0")
        self.state(pull_fail=True)
        self.run_deploy("update", "v1.0.1", success=False)
        self.assertTrue(self.state()["running"])
        self.state(pull_fail=False)
        self.run_deploy("update", "v9.0.0", success=False)
        self.assertTrue(self.state()["running"])
        self.run_deploy("rollback", "v1.0.1", success=False)
        self.assertTrue(self.state()["running"])

    def test_cached_rollback_does_not_need_registry(self):
        self.run_deploy("update", "v1.0.0")
        self.run_deploy("update", "v1.0.1")
        self.state(pull_fail=True)
        self.run_deploy("rollback", "v1.0.0")
        self.assertEqual(self.state()["version"], "v1.0.0")

    def test_online_backup_and_external_copy(self):
        self.run_deploy("update", "v1.0.0")
        external = Path(self.temp.name) / "external"
        external.mkdir()
        self.run_deploy("set-backup-destination", str(external))
        self.run_deploy("backup")
        self.assertTrue(self.state()["running"])
        filename = (self.root / "deploy-state/last-backup.txt").read_text().strip()
        self.assertEqual((external / filename).read_bytes(), (self.root / "deploy-state/backups" / filename).read_bytes())

    def test_repointed_tag_and_overlapping_operation_are_rejected(self):
        self.run_deploy("update", "v1.0.0")
        self.state(tags={"v1.0.0": PINS["v1.0.1"]})
        self.run_deploy("update", "v1.0.0", success=False)
        self.assertTrue(self.state()["running"])
        lock = self.root / "deploy-state/.lock"
        lock.mkdir()
        result = subprocess.run(["bash", "deploy.sh", "backup"], cwd=self.root, env=self.env, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(lock.exists(), "removed another process's lock")
        lock.rmdir()


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--fake-docker":
        sys.exit(fake_docker(sys.argv[2:]))
    unittest.main()
