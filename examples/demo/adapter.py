#!/usr/bin/env python3
"""File-backed demonstration service. All writes stay in PTO_TOOL_DIR."""
import fcntl
import json
import os
import sys
import tempfile
import time
from pathlib import Path


ROOT = Path(os.environ["PTO_TOOL_DIR"])
STATE = ROOT / "service.json"
FIELDS = {"maxConcurrent", "label"}


def report(status, summary, data=None):
    print(json.dumps({"status": status, "summary": summary, "data": data}), flush=True)


def save(state):
    with tempfile.NamedTemporaryFile(mode="w", dir=ROOT, delete=False) as temp:
        json.dump(state, temp)
        temp.flush()
        os.fsync(temp.fileno())
    os.replace(temp.name, STATE)
    fd = os.open(ROOT, os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def managed(state):
    return {key: state["config"][key] for key in FIELDS}


def valid(values):
    return (set(values) == FIELDS and type(values["maxConcurrent"]) is int
            and 1 <= values["maxConcurrent"] <= 64 and isinstance(values["label"], str)
            and len(values["label"]) <= 60)


def main(req):
    op = req["action"]
    state = json.loads(STATE.read_text()) if STATE.exists() else None
    if op == "probe":
        observed = {"presence": "present" if state else "absent"}
        if state:
            observed.update(installedVersion=state["version"], runningVersion=state["running"],
                            writtenRevision=state.get("written", 0), effectiveRevision=state.get("active", 0))
        return report("succeeded", "盘点完成", observed)
    if op == "install" and state is None:
        state = {"version": req["targetVersion"], "running": req["targetVersion"],
                 "config": {"maxConcurrent": 4, "label": "local", "unmanaged": "node-local"}}
        save(state)
        return report("succeeded", "服务已安装")
    if state is None:
        return report("skipped", "未安装")
    if op == "read-config":
        return report("succeeded", "已读取", managed(state))
    if op == "validate-config":
        return report("succeeded" if valid(req["values"]) else "failed", "候选配置校验")
    if op == "apply-config":
        patch, expected = req["values"], req.get("expected", {})
        for field, value in patch.items():
            if field not in FIELDS or state["config"].get(field) not in (expected.get(field), value):
                return report("conflict", "本机配置已有修改")
        if not valid({**managed(state), **patch}):
            return report("failed", "候选配置不兼容")
        state["config"].update(patch)
        state["written"] = req["configRevision"]
        if (ROOT / "defer-activation").exists():
            save(state)
            return report("pending-activation", "已写入，等待激活")
        state["active"] = state["written"]
    elif op in {"update", "rollback"}:
        if (ROOT / "busy").exists():
            report("progress", "等待空闲")
            while (ROOT / "busy").exists():
                time.sleep(0.1)
        state["version"] = req["targetVersion"]
        if (ROOT / "defer-activation").exists():
            save(state)
            return report("pending-activation", "已安装，等待激活")
        state["running"] = state["version"]
    elif op == "activate":
        state["running"] = state["version"]
        state["active"] = state.get("written", 0)
    elif op == "custom.status":
        return report("succeeded", "服务状态", {"runningVersion": state["running"]})
    elif op == "custom.sample":
        days = req["params"]["days"]
        return report("succeeded", "查询窗口", {"days": days, "label": state["config"]["label"]})
    else:
        return report("failed", "不支持的操作")
    save(state)
    report("succeeded", "已生效")


if __name__ == "__main__":
    with (ROOT / ".lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        main(json.load(sys.stdin))
