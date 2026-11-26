#!/usr/bin/env python3
"""Manage the two published integer fields in the native resource guard configuration."""
import fcntl
import json
import os
from pathlib import Path
import re
import shutil
import sys
import tempfile


LIMITS = {"HEALTHY_POLL_SECONDS": 3600, "ALERT_COOLDOWN_SECONDS": 86400}
ASSIGN = re.compile(r"""^(?P<prefix>\s*(?P<name>[A-Z_]+)\s*=\s*)(?P<quote>["']?)(?P<number>[0-9]+)(?P=quote)(?P<suffix>[ \t]*(?:#.*)?)$""")


def current(lines):
    fields = {}
    for line in lines:
        match = ASSIGN.fullmatch(line.rstrip("\r\n"))
        if match and match["name"] in LIMITS:
            if match["name"] in fields:
                raise ValueError("重复的托管配置字段")
            fields[match["name"]] = int(match["number"])
    if fields.keys() != LIMITS.keys():
        raise ValueError("现有配置缺少可识别的托管字段")
    return fields


def result(status, summary, data=None):
    return {"status": status, "summary": summary, "data": data}


def invoke(root, req):
    command = root / "app" / "pto-system-doctor"
    path = root / "config" / "resource-guard.conf"
    if req["action"] == "probe":
        presence = "present" if command.is_file() else ("unknown" if root.exists() else "absent")
        return result("succeeded", presence, {"presence": presence})
    if not command.is_file():
        return result("skipped", "未找到已安装的工具")
    if path.is_symlink() or not path.is_file():
        return result("failed", "资源配置必须是本机的常规文件")

    # Coordinate adapter writers and preserve the native file's unrelated settings and metadata.
    with path.open("rb") as source:
        fcntl.flock(source, fcntl.LOCK_EX if req["action"] == "apply-config" else fcntl.LOCK_SH)
        original = source.read()
        lines = original.decode().splitlines(keepends=True)
        actual = current(lines)
        if req["action"] == "read-config":
            return result("succeeded", "已读取托管字段", actual)
        patch = req.get("values", {})
        if not patch or any(k not in LIMITS or type(v) is not int or not 1 <= v <= LIMITS[k]
                            for k, v in patch.items()):
            return result("failed", "字段或取值不受支持")
        if req["action"] == "validate-config":
            return result("succeeded", "候选配置通过")
        if req["action"] != "apply-config":
            return result("failed", "未知操作")
        for key, value in patch.items():
            if actual[key] not in (req.get("expected", {}).get(key), value):
                return result("conflict", key + " 已在本机变更")
        if all(actual[k] == v for k, v in patch.items()):
            return result("pending-activation", "文件已符合目标，运行进程仍待核实")
        temp = None
        try:
            with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as out:
                temp = Path(out.name)
                for line in lines:
                    match = ASSIGN.fullmatch(line.rstrip("\r\n"))
                    if match and match["name"] in patch:
                        start, end = match.span("number")
                        line = line[:start] + str(patch[match["name"]]) + line[end:]
                    out.write(line.encode())
                stat = os.fstat(source.fileno())
                os.fchown(out.fileno(), stat.st_uid, stat.st_gid)
                out.flush()
                shutil.copystat(path, temp)
                os.fsync(out.fileno())
            if not os.path.samestat(os.fstat(source.fileno()), path.stat()) or path.read_bytes() != original:
                return result("conflict", "配置在写入前已变化")
            os.replace(temp, path)
            fd = os.open(path.parent, os.O_DIRECTORY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        finally:
            if temp is not None:
                temp.unlink(missing_ok=True)
    return result("pending-activation", "配置已写入，需核实资源服务的运行配置")


if __name__ == "__main__":
    try:
        response = invoke(Path("/home/pypto-tools/pto-system-doctor"), json.load(sys.stdin))
    except (OSError, ValueError) as error:
        response = result("failed", "配置不可用：" + type(error).__name__)
    print(json.dumps(response))
