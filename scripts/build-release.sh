#!/usr/bin/env bash
set -euo pipefail
version=${1:?usage: build-release.sh VERSION DESTINATION}
out=${2:?destination for native release binaries}
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || { echo 'Invalid release version' >&2; exit 1; }
arch=$(go env GOHOSTARCH)
[[ $(go env GOHOSTOS) == linux && ( $arch == amd64 || $arch == arm64 ) ]]
mkdir -p "$out"
flags="-s -w -X github.com/pypto-tools/pto-center/internal/buildversion.Release=$version"
GOOS=linux GOARCH="$arch" CGO_ENABLED=1 go build -mod=readonly -trimpath -tags netgo,osusergo,sqlite_omit_load_extension \
  -ldflags "$flags -linkmode=external -extldflags=-static" -o "$out/pto-center" ./cmd/pto-center
GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags "$flags" -o "$out/pto-agent" ./cmd/pto-agent
[[ $("$out/pto-center" --version) == "$version" && $("$out/pto-agent" --version) == "$version" ]]
trial=$(mktemp -d)
trap 'rm -rf -- "$trial"' EXIT
"$out/pto-center" keygen --dir "$trial/publisher"
"$out/pto-center" init --state "$trial/center" --trust "$trial/publisher/release.pub"
revision=$(git rev-parse HEAD)
python3 - "$out" "$version" "$arch" "$revision" <<'PY'
import hashlib, json, sys
from pathlib import Path
dest, version, arch, revision = sys.argv[1:]
root = Path(dest)
hashes = {name: hashlib.sha256((root/name).read_bytes()).hexdigest() for name in ('pto-center', 'pto-agent')}
(root/'build.json').write_text(json.dumps(dict(version=version, platform='linux/'+arch, revision=revision, hashes=hashes))+'\n')
PY
