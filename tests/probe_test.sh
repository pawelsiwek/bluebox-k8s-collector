#!/bin/sh
# helm test probe (C4): runs the rendered probe.sh in the pinned curl image
# against a local HTTPS sink that also fakes the kube-apiserver list API.
# Cases: 200 -> ready, 401 -> unauthorized, unresolvable host -> dns.
# Asserts the exact C4 key set and that the token never appears in output.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
engine=${CONTAINER_ENGINE:-$(command -v podman || command -v docker)}
host=${HOST_ALIAS:-host.containers.internal}
port=${SINK_PORT:-18444}
work=$(mktemp -d)
cleanup() { [ -n "${sink_pid:-}" ] && kill "$sink_pid" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT
mkdir -p "$work/certs" "$work/sa" "$work/token" "$work/probe"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$host" \
  -addext "subjectAltName=DNS:$host" \
  -keyout "$work/certs/key.pem" -out "$work/certs/ca.crt" >/dev/null 2>&1
cp "$work/certs/ca.crt" "$work/sa/ca.crt"
printf 'fake-sa-token' > "$work/sa/token"
token=fake-probe-token-do-not-print
printf '%s\n' "$token" > "$work/token/token"
"$here/scripts/render.sh" agent "$work/rendered.yaml"
yq 'select(.kind == "ConfigMap" and .metadata.name == "bluebox-otelcol-probe") | .data."probe.sh"' "$work/rendered.yaml" > "$work/probe/probe.sh"
image=$(yq '.global.bluebox.probe.image' "$here/values/common.yaml")
chmod -R a+rX "$work"

run_case() {  # $1 = sink POST status, $2 = endpoint host, $3 = expected exit, $4.. = expected lines
  status=$1 ep_host=$2 want_rc=$3; shift 3
  [ -n "${sink_pid:-}" ] && kill "$sink_pid" 2>/dev/null && wait "$sink_pid" 2>/dev/null || true
  python3 -I "$here/tests/otlp_sink.py" "$port" "$work/certs/ca.crt" "$work/certs/key.pem" "$work/sink.jsonl" "$status" &
  sink_pid=$!
  sleep 1
  set +e
  "$engine" run ${CONTAINER_RUN_ARGS:-} --rm --user 65532:0 --read-only --tmpfs /tmp \
    -v "$work/probe:/probe:ro" -v "$work/token:/var/run/secrets/bluebox:ro" \
    -v "$work/sa:/var/run/secrets/kubernetes.io/serviceaccount:ro" \
    -e BLUEBOX_ENDPOINT="https://$ep_host:$port/api/v2/otlp" -e TOKEN_SCHEME=Bearer \
    -e COLLECTOR_VERSION=0.1.0 -e K8S_CLUSTER_UID=0a1b2c3d-1111-2222-3333-444455556666 \
    -e CA_FILE=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt \
    -e KUBE_API="https://$host:$port" \
    "$image" /bin/sh ${PROBE_SH_FLAGS:-} /probe/probe.sh > "$work/out.txt" 2>&1
  rc=$?
  set -e
  echo "--- case POST=$status host=$ep_host (exit $rc)"; cat "$work/out.txt"
  [ "$rc" = "$want_rc" ] || { echo "FAIL: exit $rc, want $want_rc"; exit 1; }
  keys=$(cut -d= -f1 "$work/out.txt" | tr '\n' ' ')
  [ "$keys" = "otlp_status cause ready cluster_uid nodes namespaces pods collector_version " ] \
    || { echo "FAIL: key set/order: $keys"; exit 1; }
  for line in "$@"; do grep -qx "$line" "$work/out.txt" || { echo "FAIL: missing '$line'"; exit 1; }; done
  if grep -q "$token" "$work/out.txt"; then echo "FAIL: token printed"; exit 1; fi
}

run_case 200 "$host" 0 otlp_status=200 cause=ok ready=true nodes=3 namespaces=5 pods=1 collector_version=0.1.0
grep -q "\"authorization\": \"Bearer $token\"" "$work/sink.jsonl" || { echo "FAIL: token not sent as Bearer header"; exit 1; }
run_case 401 "$host" 1 otlp_status=401 cause=unauthorized ready=false
run_case 403 "$host" 1 otlp_status=403 cause=forbidden ready=false
run_case 200 does-not-exist.invalid 1 otlp_status=000 cause=dns ready=false
echo "PASS probe"
