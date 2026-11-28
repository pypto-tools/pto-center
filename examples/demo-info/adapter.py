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


def main(req):
    root = Path(os.environ["PTO_TOOL_DIR"])
    program = root / "program"
    if req["action"] == "probe":
        data = {"presence": "absent"}
        if program.exists():
            current = version(program)
            data = {"presence": "present", "installedVersion": current, "runningVersion": current}
        return {"status": "succeeded", "data": data}
    if req["action"] != "install":
        return {"status": "failed", "summary": "不支持此操作"}
    # Keep the temporary file on the same filesystem and close it before execution.
    with tempfile.TemporaryDirectory(dir=root) as temp:
        candidate = Path(temp) / "program"
        shutil.copyfile(os.environ["PTO_RELEASE_FILE"], candidate)
        candidate.chmod(0o700)
        if version(candidate) != req["targetVersion"]:
            return {"status": "failed", "summary": "发布包内程序的版本与安装目标不符"}
        os.replace(candidate, program)
    return {"status": "succeeded", "summary": "已安装签名包内的命令行工具"}


if __name__ == "__main__":
    try:
        result = main(json.load(sys.stdin))
    except (OSError, subprocess.SubprocessError) as exc:
        result = {"status": "failed", "summary": str(exc)}
    print(json.dumps(result, ensure_ascii=False))
