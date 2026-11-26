"""Exercise pilot adapters against temporary native layouts, without touching host services."""
import importlib.util
from pathlib import Path
import tempfile
import unittest


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


if __name__ == "__main__":
    unittest.main()
