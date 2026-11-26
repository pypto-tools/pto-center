#!/usr/bin/env python3
"""Read-only pilot adapter for the native pto-task CLI."""
import json
from pathlib import Path
import re
import subprocess
import sys


def result(status, message, data=None):
    return {"status": status, "summary": message, "data": data}


def invoke(root, req):
    entry = root / "app" / "task-submit"
    action = req["action"]
    if action == "probe":
        presence = "present" if entry.is_file() else ("unknown" if root.exists() else "absent")
        info = {"presence": presence}
        marker = root / "app" / ".pto-task-release"
        if presence == "present" and marker.is_file():
            commit = marker.read_text().strip()
            if re.fullmatch(r"[0-9a-f]{40}", commit):
                info["installedVersion"] = commit
        return result("succeeded", presence, info)
    if not entry.is_file():
        return result("skipped", "未安装")
    if action == "custom.queue-status":
        args = ["--monitor", "--json"]
    elif action == "custom.usage-report":
        days = req.get("params", {}).get("days")
        if type(days) is not int or not 1 <= days <= 30:
            return result("failed", "统计天数应在 1 到 30 之间")
        args = ["--usage-ranking", "--days", str(days), "--sort", "utilization", "--limit", "20", "--json"]
    else:
        return result("failed", "未知动作")
    completed = subprocess.run([str(entry), *args], stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL, timeout=30, check=False)
    if completed.returncode or len(completed.stdout) > 192 * 1024:
        return result("failed", "原生查询失败或输出超过限制")
    data = json.loads(completed.stdout)
    if action == "custom.queue-status":
        # Keep the native freshness/status indicators alongside the summary.
        data = {key: data.get(key) for key in ("status", "reason", "age_ms", "observed_at", "summary")}
    return result("succeeded", "原生查询完成", data)


if __name__ == "__main__":
    try:
        response = invoke(Path("/home/pypto-tools/pto-task"), json.load(sys.stdin))
    except (OSError, ValueError, subprocess.TimeoutExpired) as error:
        response = result("failed", "查询不可用：" + type(error).__name__)
    print(json.dumps(response))
