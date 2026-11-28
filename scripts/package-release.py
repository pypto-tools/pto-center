#!/usr/bin/env python3
"""Assemble and verify the two native builds with an offline publisher key."""
import argparse
import gzip
import hashlib
import io
import json
import re
import shutil
import subprocess
import tarfile
import tempfile
from pathlib import Path


def sha(data):
    return hashlib.sha256(data).hexdigest()


def write_bootstrap(dest, images, repo):
    with dest.open("xb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as zipped:
        with tarfile.open(fileobj=zipped, mode="w") as archive:
            for name, data in images.items():
                for filename, contents, mode in (
                    ("bin/" + name, data, 0o755),
                    ("deploy/" + name + ".service", (repo / "deploy" / (name + ".service")).read_bytes(), 0o644),
                ):
                    item = tarfile.TarInfo(filename)
                    item.size, item.mode = len(contents), mode
                    archive.addfile(item, io.BytesIO(contents))


def package(args, repo, out, scratch):
    cli = args.cli.resolve()
    publisher = args.publisher.resolve()
    revision = subprocess.check_output(["git", "-C", repo, "rev-parse", "HEAD"], text=True).strip()
    components = ("pto-center", "pto-agent")
    inputs = {name: [] for name in components}
    for arch, machine in (("amd64", 62), ("arm64", 183)):
        source = args.artifacts / ("linux-" + arch)
        meta = json.loads((source / "build.json").read_text())
        if (meta["version"], meta["platform"], meta["revision"]) != (args.version, "linux/" + arch, revision):
            raise ValueError(f"{arch}: build does not match the selected version, platform and Git commit")
        images = {}
        for name in components:
            file = source / name
            data = file.read_bytes()
            if (sha(data) != meta["hashes"][name] or data[:6] != b"\x7fELF\x02\x01"
                    or int.from_bytes(data[18:20], "little") != machine):
                raise ValueError(f"{arch}/{name}: binary checksum or ELF platform mismatch")
            images[name] = data
            inputs[name].extend(["--binary", file])
        write_bootstrap(out / f"pto-center-{args.version}-linux-{arch}.tar.gz", images, repo)

    pub = out / "release.pub"
    shutil.copyfile(publisher / "release.pub", pub)
    state = scratch / "center"
    subprocess.run([cli, "init", "--state", state, "--trust", pub], check=True)
    for name in components:
        bundle = out / f"{name}-{args.version}.json"
        built = subprocess.check_output([cli, "pack-" + name[4:], "--version", args.version, *inputs[name],
                                         "--signing-key", publisher / "signing.key", "--out", bundle])
        registered = subprocess.check_output([cli, "register", "--state", state, "--file", bundle])
        if built != registered:
            raise ValueError(f"{name}: registered identity differs from the signed package")
        print(registered.decode().strip())
    files = sorted(out.iterdir())
    sums = out / "SHA256SUMS"
    sums.write_text("".join(f"{sha(file.read_bytes())}  {file.name}\n" for file in files))
    subprocess.run([cli, "sign-checksums", "--dir", out, "--trust", pub, "--signing-key", publisher / "signing.key"], check=True)
    subprocess.run(["openssl", "pkeyutl", "-verify", "-pubin", "-inkey", out / "release.pem",
                    "-rawin", "-in", sums, "-sigfile", out / "SHA256SUMS.sig"], check=True)


def main():
    repo = Path(__file__).resolve().parents[1]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--publisher", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--cli", type=Path, default=repo / "bin/pto-center")
    args = parser.parse_args()
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:[.-][0-9A-Za-z.-]+)?", args.version):
        parser.error("invalid release version")
    if args.out.exists():
        parser.error("choose a new output directory for the release")
    args.out.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=args.out.parent) as tmp:
        scratch = Path(tmp)
        out = scratch / "assets"
        out.mkdir()
        package(args, repo, out, scratch)
        out.rename(args.out)


if __name__ == "__main__":
    main()
