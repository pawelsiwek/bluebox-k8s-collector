#!/bin/sh
# Redaction + topology test (C1, AC "no env value, command argument or probe
# header reaches the exporter"; AC "the Dynatrace-image variant produces a
# K8S_CLUSTER record without custom components").
#
#   VARIANT=bluebox   (default) values as rendered, Bluebox image:
#                     transform/redact -> transform/topology -> dtk8stopology -> transform/finalize
#   VARIANT=dynatrace overlays/dynatrace-image, stock Dynatrace image:
#                     transform/redact -> transform/topology -> transform/workload_ownerref -> transform/finalize
#                     plus logs/cluster (kube-system Namespace -> K8S_CLUSTER)
#
# Every processor block is taken VERBATIM from the rendered cluster release
# (ConfigMap + inline --config=yaml: args, scripts/effective_config.sh) and
# run in the image that release deploys (scripts/collector_image.sh), over one
# fixture per pulled kind. error_mode: propagate would drop a batch on a failing
# statement, so the test requires a record per kind and no collector error.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
engine=${CONTAINER_ENGINE:-$(command -v podman || command -v docker)}
variant=${VARIANT:-bluebox}
work=$(mktemp -d)
. "$here/tests/lib_sink.sh"
trap 'sink_stop; rm -rf "$work"' EXIT
mkdir -p "$work/in"
sink_start "$work"

case "$variant" in
  bluebox)   overlays="" ; procs='"transform/redact","transform/topology","dtk8stopology","transform/finalize"' ;;
  dynatrace) overlays="dynatrace-image" ; procs='"transform/redact","transform/topology","transform/workload_ownerref","transform/finalize"' ;;
  *) echo "unknown VARIANT=$variant" >&2; exit 2 ;;
esac
"$here/scripts/render.sh" cluster "$work/rendered.yaml" $overlays ${OVERLAYS:-}
"$here/scripts/effective_config.sh" "$work/rendered.yaml" "$work/relay.yaml" >/dev/null
image=$("$here/scripts/collector_image.sh" "$work/rendered.yaml")

# One receiver per fixture file; the k8s.resource.name is the file stem before "_".
recv=""
{
  echo "receivers:"
  for f in "$here"/tests/fixtures/*.json; do
    stem=$(basename "$f" .json)
    [ "${stem%%_*}" = cluster ] && continue
    jq -c . "$f" > "$work/in/$stem.json"
    printf '  file_log/%s:\n    include: [/in/%s.json]\n    start_at: beginning\n' "$stem" "$stem"
    printf '    operators:\n      - type: json_parser\n        parse_to: body\n'
    printf '      - type: add\n        field: attributes["k8s.resource.name"]\n        value: %s\n' "${stem%%_*}"
  done
  sink_exporter_yaml
  printf 'service:\n  telemetry:\n    metrics:\n      level: none\n  pipelines:\n    logs:\n      exporters: [otlp_http/sink]\n'
} > "$work/test.yaml"
for f in "$work"/in/*.json; do recv="$recv\"file_log/$(basename "$f" .json)\","; done
yq -i ".service.pipelines.logs.receivers = [${recv%,}] | .service.pipelines.logs.processors = [$procs]" "$work/test.yaml"
for p in $(echo "$procs" | tr -d '"' | tr ',' ' '); do
  yq -i ".processors.\"$p\" = load(\"$work/relay.yaml\").processors.\"$p\"" "$work/test.yaml"
done
if [ "$variant" = dynatrace ]; then
  jq -c . "$here/tests/fixtures/cluster_kube-system.json" > "$work/in/cluster.json"
  yq -i ".receivers.\"file_log/cluster\" = load(\"$work/test.yaml\").receivers.\"file_log/pods\" |
          .receivers.\"file_log/cluster\".include = [\"/in/cluster.json\"] |
          .receivers.\"file_log/cluster\".operators[1].value = \"namespaces\" |
          .processors.\"transform/cluster_record\" = load(\"$work/relay.yaml\").processors.\"transform/cluster_record\" |
          .service.pipelines.\"logs/cluster\" = {\"receivers\": [\"file_log/cluster\"], \"processors\": [\"transform/cluster_record\"], \"exporters\": [\"otlp_http/sink\"]}" "$work/test.yaml"
fi
chmod -R a+rX "$work"

name=bluebox-redaction-$$
"$engine" run ${CONTAINER_RUN_ARGS:-} -d --name "$name" --user "$(id -u):$(id -g)" \
  -v "$work/in:/in:ro" -v "$SINK_CERTS:/certs:ro" -v "$work/test.yaml:/cfg/test.yaml:ro" \
  -e K8S_CLUSTER_UID=0a1b2c3d-1111-2222-3333-444455556666 -e K8S_CLUSTER_NAME=ci \
  "$image" --config=/cfg/test.yaml >/dev/null
i=0
while [ $i -lt 30 ]; do
  sink_logs_export > "$work/out.json"
  [ "$(jq -s '[.[].resourceLogs[].scopeLogs[].logRecords[]] | length' "$work/out.json" 2>/dev/null || echo 0)" -ge 12 ] && break
  sleep 1; i=$((i + 1))
done
sleep 3
sink_logs_export > "$work/out.json"
"$engine" logs "$name" > "$work/collector.log" 2>&1 || true
"$engine" rm -f "$name" >/dev/null
if [ ! -s "$work/out.json" ]; then
  echo "FAIL: collector produced no output" >&2; cat "$work/collector.log" >&2; exit 1
fi
if grep -q '"level":"error"\|	error	' "$work/collector.log"; then
  echo "FAIL: collector logged errors (a statement failed on some kind):" >&2
  grep '"level":"error"\|	error	' "$work/collector.log" >&2; exit 1
fi
python3 -I "$here/tests/assert_redaction.py" "$work/out.json" "$here/tests/fixtures/secrets.txt" "$variant"
