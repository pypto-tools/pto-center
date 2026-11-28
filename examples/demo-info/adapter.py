#!/usr/bin/env python3
"""Install the bundled CLI in the Agent's per-tool directory."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


def version(program):
    return subprocess.check_output([str(program), "--version"], text=True, timeout=5).strip()


def phase(name, state, summary, code=None):
    receipt = {"state": state, "summary": summary}
    if code:
        receipt["code"] = code
    result = {"status": "failed" if state == "failed" else "progress",
              "summary": summary, "phases": {name: receipt}}
    if state == "failed":
        return result
    print(json.dumps(result, ensure_ascii=False), flush=True)


def main(req):
    root = Path(os.environ["PTO_TOOL_DIR"])
    program = root / "program"
    if req["action"] == "probe":
        data = {"presence": "absent"}
        if program.is_file():
            current = version(program)
            data = {"presence": "present", "installedVersion": current, "runningVersion": current}
        return {"status": "succeeded", "data": data}
    if req["action"] != "install":
        return {"status": "failed", "summary": "不支持此操作"}
    phase("preflight", "running", "检查安装目录和发布文件")
    payload = Path(os.environ["PTO_RELEASE_FILE"])
    if program.exists() or program.is_symlink():
        return phase("preflight", "failed", "安装入口已被占用", "install_path_conflict")
    if not os.access(root, os.W_OK | os.X_OK):
        return phase("preflight", "failed", "安装目录不可写", "directory_not_writable")
    if not payload.is_file() or not os.access(payload, os.R_OK) or not payload.stat().st_size:
        return phase("preflight", "failed", "缺少可读的程序文件", "missing_payload")
    phase("preflight", "succeeded", "安装目录和发布文件可用")
    phase("install", "running", "正在校验并替换程序")
    # Keep the temporary file on the same filesystem and close it before execution.
    with tempfile.TemporaryDirectory(dir=root) as temp:
        candidate = Path(temp) / "program"
        shutil.copyfile(payload, candidate)
        candidate.chmod(0o700)
        if version(candidate) != req["targetVersion"]:
            return phase("install", "failed", "发布包内程序的版本与安装目标不符", "payload_version_mismatch")
        os.replace(candidate, program)
    return {"status": "succeeded", "summary": "已安装签名包内的命令行工具",
            "phases": {"install": {"state": "succeeded"}}}


if __name__ == "__main__":
    try:
        result = main(json.load(sys.stdin))
    except (OSError, subprocess.SubprocessError) as exc:
        result = {"status": "failed", "summary": str(exc)}
    print(json.dumps(result, ensure_ascii=False))
