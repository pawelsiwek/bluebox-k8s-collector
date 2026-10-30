#!/bin/sh
# Pod-log namespace allowlist: runs the rendered file_log/pods receiver
# verbatim (allowlist demo,shop) over a fake /var/log/pods tree and asserts
# only allowlisted namespaces are read; kube-system and the collector
# namespace stay excluded even with "*".
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
engine=${CONTAINER_ENGINE:-$(command -v podman || command -v docker)}
work=$(mktemp -d)
name=bluebox-podlogs-$$
. "$here/tests/lib_sink.sh"
trap '"$engine" rm -f "$name" >/dev/null 2>&1 || true; sink_stop; rm -rf "$work"' EXIT
sink_start "$work"

run_case() {  # $1 = --set value for namespaces, $2.. = namespaces expected to be read
  set_ns=$1; shift
  rm -rf "$work/pods"; : > "$SINK_LOG"
  for ns in demo shop other kube-system bluebox-otelcol; do
    d="$work/pods/${ns}_app-1_0000-uid/app"
    mkdir -p "$d"
    : > "$d/0.log"
  done
  EXTRA_SET="--set global.bluebox.podLogs.namespaces=$set_ns" \
    "$here/scripts/render.sh" agent "$work/rendered.yaml" pod-logs ${OVERLAYS:-}
  yq 'select(.kind == "ConfigMap" and .data.relay != null) | .data.relay' "$work/rendered.yaml" > "$work/relay.yaml"
  image=$("$here/scripts/collector_image.sh" "$work/rendered.yaml")
  { sink_exporter_yaml; printf 'service:\n  telemetry:\n    metrics:\n      level: none\n  pipelines:\n    logs:\n      receivers: [file_log/pods]\n      exporters: [otlp_http/sink]\n'; } > "$work/test.yaml"
  yq -i ".receivers.\"file_log/pods\" = load(\"$work/relay.yaml\").receivers.\"file_log/pods\"" "$work/test.yaml"
  chmod -R a+rX "$work"
  "$engine" run ${CONTAINER_RUN_ARGS:-} -d --name "$name" --user "$(id -u):$(id -g)" \
    -v "$work/pods:/var/log/pods:ro" -v "$SINK_CERTS:/certs:ro" -v "$work/test.yaml:/cfg/test.yaml:ro" \
    "$image" --config=/cfg/test.yaml >/dev/null
  sleep 3   # start_at: end -> only lines appended after start are read
  for ns in demo shop other kube-system bluebox-otelcol; do
    printf '2026-10-26T10:00:00.000000000Z stdout F line-from-%s\n' "$ns" >> "$work/pods/${ns}_app-1_0000-uid/app/0.log"
  done
  sleep 4
  "$engine" rm -f "$name" >/dev/null
  for ns in demo shop other kube-system bluebox-otelcol; do
    want=no
    for e in "$@"; do [ "$e" = "$ns" ] && want=yes; done
    got=no
    sink_logs_export | grep -q "line-from-$ns\"" && got=yes
    [ "$want" = "$got" ] || { echo "FAIL ($set_ns): namespace $ns read=$got, want $want"; exit 1; }
  done
  echo "ok: namespaces=$set_ns reads only: $*"
}

run_case "{demo,shop}" demo shop
run_case "{*}" demo shop other
echo "PASS pod-log namespace allowlist"
