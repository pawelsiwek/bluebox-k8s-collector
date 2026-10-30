#!/bin/sh
# Events pipeline, Bluebox image: a k8s_events-shaped record and a pod MODIFIED
# watch record (OOMKilled, env and args holding secrets) go through the
# rendered transform/events -> dtk8seventsprocessor -> transform/inferred_events
# -> filter/no_raw_objects chain. Asserts: the event is forwarded with
# event.domain=k8s, one KUBERNETES_INFERRED_EVENT is emitted for the OOM kill,
# and nothing from the raw pod body (or any k8s_objects record) is exported.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
engine=${CONTAINER_ENGINE:-$(command -v podman || command -v docker)}
work=$(mktemp -d)
. "$here/tests/lib_sink.sh"
trap 'sink_stop; rm -rf "$work"' EXIT
mkdir -p "$work/in"
sink_start "$work"

"$here/scripts/render.sh" cluster "$work/rendered.yaml"
"$here/scripts/effective_config.sh" "$work/rendered.yaml" "$work/relay.yaml" >/dev/null
image=$("$here/scripts/collector_image.sh" "$work/rendered.yaml")
jq -c . "$here/tests/fixtures/events/pod_watch_oom.json" > "$work/in/pods.json"
jq -c . "$here/tests/fixtures/events/k8s_event.json" > "$work/in/event.json"

procs="transform/events dtk8seventsprocessor transform/inferred_events filter/no_raw_objects"
{
  cat <<'EOF'
receivers:
  file_log/pods:
    include: [/in/pods.json]
    start_at: beginning
    operators:
      - type: json_parser
        parse_to: body
      - type: add
        field: attributes["k8s.resource.name"]
        value: pods
  file_log/event:
    include: [/in/event.json]
    start_at: beginning
    operators:
      - type: json_parser
        parse_to: body
      - type: add
        field: attributes["k8s.event.reason"]
        value: BackOff
      - type: add
        field: resource["k8s.object.kind"]
        value: Pod
      - type: add
        field: resource["k8s.object.name"]
        value: memhog-1
EOF
  sink_exporter_yaml
  printf 'service:\n  telemetry:\n    metrics:\n      level: none\n  pipelines:\n    logs:\n      receivers: [file_log/pods, file_log/event]\n      exporters: [otlp_http/sink]\n'
} > "$work/test.yaml"
list=""
for p in $procs; do
  yq -i ".processors.\"$p\" = load(\"$work/relay.yaml\").processors.\"$p\"" "$work/test.yaml"
  list="$list\"$p\","
done
yq -i ".service.pipelines.logs.processors = [${list%,}]" "$work/test.yaml"
chmod -R a+rX "$work"

name=bluebox-events-$$
"$engine" run ${CONTAINER_RUN_ARGS:-} -d --name "$name" --user "$(id -u):$(id -g)" \
  -v "$work/in:/in:ro" -v "$SINK_CERTS:/certs:ro" -v "$work/test.yaml:/cfg/test.yaml:ro" \
  -e K8S_CLUSTER_UID=0a1b2c3d-1111-2222-3333-444455556666 -e K8S_CLUSTER_NAME=ci \
  "$image" --config=/cfg/test.yaml >/dev/null
sleep 6
"$engine" logs "$name" > "$work/collector.log" 2>&1 || true
"$engine" rm -f "$name" >/dev/null
sink_logs_export > "$work/out.json"
fail() { echo "FAIL events: $*" >&2; cat "$work/collector.log" >&2; exit 1; }
for s in WATCH_ENV_SECRET_20 WATCH_ARG_SECRET_21 OOMKilled; do
  if grep -q "$s" "$work/out.json"; then fail "raw pod watch content exported ($s)"; fi
done
recs=$(jq -c '.resourceLogs[].scopeLogs[].logRecords[] | (.attributes | map({(.key): (.value | to_entries[0].value)}) | add)' "$work/out.json")
[ "$(printf '%s\n' "$recs" | jq -s 'map(select(.["k8s.resource.name"] != null)) | length')" = 0 ] || fail "a k8s_objects record was exported"
printf '%s\n' "$recs" | jq -se 'map(select(.["event.provider"] == "KUBERNETES_EVENT" and .["event.domain"] == "k8s" and .["dt.kubernetes.event.involved_object.name"] == "memhog-1")) | length == 1' >/dev/null \
  || fail "KUBERNETES_EVENT not forwarded with event.domain=k8s and the throttle keys: $recs"
printf '%s\n' "$recs" | jq -se 'map(select(.["event.provider"] == "KUBERNETES_INFERRED_EVENT" and .["event.domain"] == "k8s" and .["k8s.container.name"] == "hog" and .["k8s.pod.name"] == "memhog-1")) | length == 1' >/dev/null \
  || fail "no OOM inferred event: $recs"
echo "PASS events (bluebox): event forwarded, OOM inferred, raw pod watch records dropped"
