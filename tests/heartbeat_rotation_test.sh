#!/bin/sh
# Heartbeat (C1) + token rotation without restart (AC) + v1-only self attrs.
#
# Runs the rendered agent release's self-telemetry path (otlp/self receiver,
# bearertokenauth, resource/bluebox, allowlist filter, otlp_http/bluebox)
# in the pinned otelcol-k8s image against a local HTTPS sink. The token
# directory is laid out the way kubelet projects a Secret (..data symlink
# swap), then rotated in place. Fake tokens only.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
engine=${CONTAINER_ENGINE:-$(command -v podman || command -v docker)}
gates=$(yq '.command.extraArgs[] | select(test("^--feature-gates="))' "$here/values/common.yaml")
port=${SINK_PORT:-18443}
host=${HOST_ALIAS:-host.containers.internal}
work=$(mktemp -d)
name=bluebox-hb-$$
cleanup() {
  "$engine" rm -f "$name" >/dev/null 2>&1 || true
  [ -n "${sink_pid:-}" ] && kill "$sink_pid" 2>/dev/null || true
  "$engine" volume rm -f "${vol:-none}" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$work/certs"

openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$host" \
  -addext "subjectAltName=DNS:$host,DNS:localhost" \
  -keyout "$work/certs/key.pem" -out "$work/certs/ca.crt" >/dev/null 2>&1
: > "$work/sink.jsonl"
python3 -I "$here/tests/otlp_sink.py" "$port" "$work/certs/ca.crt" "$work/certs/key.pem" "$work/sink.jsonl" &
sink_pid=$!

# kubelet AtomicWriter layout: token -> ..data/token, ..data -> ..genN,
# swapped with rename(2). It lives in a container volume and is written by a
# helper container, because inotify does not cross the macOS -> VM file share.
helper=$(yq '.global.bluebox.probe.image' "$here/values/common.yaml")
vol=bluebox-hb-token-$$
"$engine" volume create "$vol" >/dev/null
project_token() {  # $1 = token value, $2 = generation
  "$engine" run --rm --user 0 -v "$vol:/t" "$helper" sh -ec "
    mkdir -p /t/..gen$2
    printf '%s\\n' '$1' > /t/..gen$2/token   # trailing newline on purpose
    chmod -R a+rX /t/..gen$2
    ln -sfn ..gen$2 /t/..data_tmp
    mv -T /t/..data_tmp /t/..data
    # kubelet then deletes the previous generation; bearertokenauth's
    # fsnotify watch follows the symlink target and fires on that removal.
    for old in /t/..gen*; do [ \"\$old\" = /t/..gen$2 ] || rm -rf \"\$old\"; done
    [ -L /t/token ] || ln -s ..data/token /t/token"
}
project_token fake-token-generation-A 1

"$here/scripts/render.sh" agent "$work/rendered.yaml" ${OVERLAYS:-}
image=$("$here/scripts/collector_image.sh" "$work/rendered.yaml")
yq 'select(.kind == "ConfigMap" and .data.relay != null) | .data.relay' "$work/rendered.yaml" > "$work/relay.yaml"
# Keep only the self-telemetry path; point the exporter at the sink.
yq "{
  \"extensions\": {\"bearertokenauth\": .extensions.bearertokenauth},
  \"receivers\": {\"otlp/self\": .receivers.\"otlp/self\"},
  \"processors\": (.processors | with_entries(select(.key == \"resource/bluebox\" or .key == \"transform/drop_empty\" or .key == \"filter/allowlist\" or .key == \"memory_limiter\" or .key == \"cumulativetodelta\" or .key == \"batch\"))),
  \"exporters\": {\"otlp_http/bluebox\": .exporters.\"otlp_http/bluebox\"},
  \"service\": {
    \"extensions\": [\"bearertokenauth\"],
    \"telemetry\": .service.telemetry,
    \"pipelines\": {\"metrics/self\": .service.pipelines.\"metrics/self\"}}
} |
  .processors.batch.timeout = \"1s\" |
  .exporters.\"otlp_http/bluebox\".endpoint = \"https://$host:$port\" |
  .exporters.\"otlp_http/bluebox\".encoding = \"json\" |
  .exporters.\"otlp_http/bluebox\".tls.ca_file = \"/certs/ca.crt\" |
  .service.telemetry.metrics.readers[0].periodic.interval = 2000" "$work/relay.yaml" > "$work/test.yaml"
chmod -R a+rX "$work/certs" "$work/test.yaml"

"$engine" run ${CONTAINER_RUN_ARGS:-} -d --name "$name" --memory 256m \
  -v "$work/test.yaml:/cfg/test.yaml:ro" -v "$work/certs:/certs:ro" \
  -v "$vol:/var/run/secrets/bluebox:ro" \
  -e K8S_CLUSTER_UID=0a1b2c3d-1111-2222-3333-444455556666 -e K8S_CLUSTER_NAME=ci \
  -e BLUEBOX_ENDPOINT=unused -e MY_POD_IP=127.0.0.1 \
  "$image" --config=/cfg/test.yaml "$gates" >/dev/null
started=$("$engine" inspect -f '{{.State.StartedAt}}' "$name")

wait_for() {  # $1 = jq filter over sink lines that must yield true
  i=0
  while [ $i -lt 40 ]; do
    if [ "$(jq -s "$1" "$work/sink.jsonl" 2>/dev/null)" = "true" ]; then return 0; fi
    sleep 1; i=$((i + 1))
  done
  return 1
}

fail() { echo "FAIL: $*" >&2; "$engine" logs "$name" 2>&1 | tail -20 >&2; exit 1; }

wait_for 'any(.[]; .authorization == "Bearer fake-token-generation-A")' \
  || fail "no request with token A (trimmed, Bearer scheme)"
echo "ok: token A sent as 'Bearer <token>' (newline trimmed)"

project_token fake-token-generation-B 2
wait_for 'any(.[]; .authorization == "Bearer fake-token-generation-B")' \
  || fail "rotated token B never used"
[ "$("$engine" inspect -f '{{.State.StartedAt}}' "$name")" = "$started" ] || fail "collector restarted"
[ "$("$engine" inspect -f '{{.State.Running}}' "$name")" = "true" ] || fail "collector not running"
echo "ok: rotated token B used without a restart"

[ -n "${KEEP_SINK:-}" ] && cp "$work/sink.jsonl" "$KEEP_SINK"
python3 -I - "$work/sink.jsonl" <<'EOF'
import json, sys
names, res = set(), {}
for line in open(sys.argv[1]):
    r = json.loads(line)
    for rm in r["body"].get("resourceMetrics", []):
        for a in rm.get("resource", {}).get("attributes", []):
            res[a["key"]] = next(iter(a["value"].values()))
        for sm in rm.get("scopeMetrics", []):
            for m in sm.get("metrics", []):
                names.add(m["name"])
fails = []
if "otelcol_process_uptime" not in names:
    fails.append(f"heartbeat otelcol_process_uptime missing; got {sorted(names)}")
want = {"k8s.cluster.uid": "0a1b2c3d-1111-2222-3333-444455556666", "k8s.cluster.name": "ci",
        "bluebox.collector.role": "agent"}
for k, v in want.items():
    if res.get(k) != v:
        fails.append(f"resource {k}={res.get(k)!r}, want {v!r}")
if not res.get("bluebox.collector.version"):
    fails.append("bluebox.collector.version missing")
dropped = [n for n in names if not (n.startswith("otelcol_process_uptime") or n.startswith("otelcol_exporter_"))]
if dropped:
    fails.append(f"allowlist let through: {sorted(dropped)}")
if fails:
    print("FAIL heartbeat:\n  - " + "\n  - ".join(fails)); sys.exit(1)
print(f"ok: heartbeat metrics {sorted(names)}; resource {sorted(res)}")
EOF
echo "PASS heartbeat + token rotation"
