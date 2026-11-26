"""Exercise pilot adapters against temporary native layouts, without touching host services."""
import importlib.util
import os
from pathlib import Path
import stat
import tempfile
import threading
import unittest
from unittest.mock import patch


def load(tool):
    file = Path(__file__).resolve().parents[1] / "integrations" / tool / "adapter.py"
    spec = importlib.util.spec_from_file_location(tool, file)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class PilotTest(unittest.TestCase):
    def test_doctor_preserves_native_file_and_reports_activation(self):
        do = load("pto-system-doctor").invoke
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "doctor"
            self.assertEqual(do(root, {"action": "probe"})["data"]["presence"], "absent")
            (root / "app").mkdir(parents=True)
            (root / "app/pto-system-doctor").touch()
            (root / "config").mkdir()
            path = root / "config/resource-guard.conf"
            old = '# note\nHEALTHY_POLL_SECONDS="10"  # comment\nALERT_COOLDOWN_SECONDS=900\nLOCAL_SETTING=yes\n'
            path.write_text(old)
            path.chmod(0o600)
            req = {"action": "apply-config", "values": {"HEALTHY_POLL_SECONDS": 20},
                   "expected": {"HEALTHY_POLL_SECONDS": 10}}
            self.assertEqual(do(root, req)["status"], "pending-activation")
            self.assertEqual(path.read_text(), old.replace('"10"', '"20"'))
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            changed = do(root, {**req, "values": {"HEALTHY_POLL_SECONDS": 30}})
            self.assertEqual(changed["status"], "conflict")
            self.assertEqual(do(root, {"action": "read-config"})["data"]["HEALTHY_POLL_SECONDS"], 20)

    def test_native_queue_queries(self):
        do = load("npu-taskqueue").invoke
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "queue"
            self.assertEqual(do(root, {"action": "probe"})["data"]["presence"], "absent")
            (root / "app").mkdir(parents=True)
            self.assertEqual(do(root, {"action": "probe"})["data"]["presence"], "unknown")
            cmd = root / "app/task-submit"
            cmd.write_text('#!/usr/bin/env python3\nimport json,sys\n'
                           'if sys.argv[1:] == ["--monitor","--json"]: print(json.dumps({"status":"stale","summary":None}))\n'
                           'elif sys.argv[1:] == ["--usage-ranking","--days","3","--sort","utilization","--limit","20","--json"]: print("[]")\n'
                           'else: sys.exit(1)\n')
            cmd.chmod(0o700)
            (root / "app/.pto-task-release").write_text("a" * 40)
            self.assertEqual(do(root, {"action": "probe"})["data"]["installedVersion"], "a" * 40)
            self.assertEqual(do(root, {"action": "custom.queue-status"})["data"]["status"], "stale")
            req = {"action": "custom.usage-report", "params": {"days": 3}}
            self.assertEqual(do(root, req)["status"], "succeeded")
            self.assertEqual(do(root, {**req, "params": {"days": 31}})["status"], "failed")

    def test_doctor_serializes_replacements_and_reports_uncertain_sync(self):
        doctor = load("pto-system-doctor")
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "app").mkdir()
            (root / "app/pto-system-doctor").touch()
            (root / "config").mkdir()
            path = root / "config/resource-guard.conf"
            path.write_text("HEALTHY_POLL_SECONDS=10\nALERT_COOLDOWN_SECONDS=900\n")
            req = {"action": "apply-config", "values": {"HEALTHY_POLL_SECONDS": 20},
                   "expected": {"HEALTHY_POLL_SECONDS": 10}}
            replacing, waiting = threading.Event(), threading.Event()
            original_replace, original_lock = os.replace, doctor.fcntl.flock
            replies = {}

            def replace(src, dest):
                replacing.set()
                if not waiting.wait(3):
                    raise RuntimeError("second operation never attempted its lock")
                return original_replace(src, dest)

            def lock(fd, mode):
                if threading.current_thread().name == "second":
                    waiting.set()
                return original_lock(fd, mode)

            def invoke(label, request):
                replies[label] = doctor.invoke(root, request)["status"]

            with patch.object(doctor.os, "replace", replace), patch.object(doctor.fcntl, "flock", lock):
                first = threading.Thread(target=invoke, args=("first", req))
                first.start()
                self.assertTrue(replacing.wait(3))
                second = threading.Thread(name="second", target=invoke,
                                          args=("second", {**req, "values": req["expected"]}))
                second.start()
                first.join(4)
                second.join(4)
            self.assertEqual(replies, {"first": "pending-activation", "second": "conflict"})
            sync = os.fsync

            def sync_file_only(fd):
                if stat.S_ISDIR(os.fstat(fd).st_mode):
                    raise OSError("directory sync unavailable")
                return sync(fd)

            with patch.object(doctor.os, "fsync", sync_file_only):
                result = doctor.invoke(root, {**req, "expected": req["values"],
                                               "values": {"HEALTHY_POLL_SECONDS": 30}})
            self.assertEqual(result["status"], "unknown")
            self.assertIn("HEALTHY_POLL_SECONDS=30", path.read_text())


if __name__ == "__main__":
    unittest.main()
