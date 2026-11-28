#!/usr/bin/env bash
# Local-only demonstration using isolated state and two independent Agent processes.
set -euo pipefail
umask 077
cd -- "$(dirname -- "$0")/.."
sandbox_dir=$(mktemp -d)
admin_port=${PTO_DEMO_ADMIN_PORT:-8080}
node_port=${PTO_DEMO_NODE_PORT:-8081}
pids=()
cleanup() {
    for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
    for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
    rm -rf -- "$sandbox_dir"
}
trap cleanup EXIT
trap 'exit 0' INT TERM
bin/pto-center keygen --dir "$sandbox_dir/keys"
bin/pto-center init --state "$sandbox_dir/center" --trust "$sandbox_dir/keys/release.pub"
bin/pto-center pack --dir examples/demo-info --version v1 --payload demo-info \
    --signing-key "$sandbox_dir/keys/signing.key" --out "$sandbox_dir/demo-info.json"
bin/pto-center register --state "$sandbox_dir/center" --file "$sandbox_dir/demo-info.json"
for v in v1 v2; do
    bin/pto-center pack --dir examples/demo --version "$v" --signing-key "$sandbox_dir/keys/signing.key" --out "$sandbox_dir/$v.json"
    bin/pto-center register --state "$sandbox_dir/center" --file "$sandbox_dir/$v.json"
done
for node in demo-a demo-b; do
    bin/pto-center invite --state "$sandbox_dir/center" --node "$node" --group demo > "$sandbox_dir/$node.invite"
done
bin/pto-center serve --local --state "$sandbox_dir/center" --admin "127.0.0.1:$admin_port" --nodes "127.0.0.1:$node_port" &
pids+=("$!")
for node in demo-a demo-b; do
    bin/pto-agent --local --node "$node" --state "$sandbox_dir/$node" --center "http://127.0.0.1:$node_port" \
        --trust "$sandbox_dir/keys/release.pub" --invite-file "$sandbox_dir/$node.invite" &
    pids+=("$!")
done
printf '管理页面 http://127.0.0.1:%s\n账号 admin，密码见 %s/center/admin.token\n退出时按 Ctrl-C，演示状态随后清理。\n' "$admin_port" "$sandbox_dir"
wait -n
