#!/bin/sh
# Render one release with `helm template`.
#   render.sh <agent|cluster> <out.yaml> [overlay ...]
# Overlay names: aks eks gke autopilot local ai live-read alerting dynatrace-image pod-logs.
# Values layering order is fixed: common -> role -> pod-logs -> overlays (as given).
# CHART (default open-telemetry/opentelemetry-collector) and CHART_VERSION can be overridden.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
role=$1; out=$2; shift 2
chart=${CHART:-open-telemetry/opentelemetry-collector}
version=${CHART_VERSION:-0.177.0}

set -- "$@"
files="-f $here/values/common.yaml -f $here/values/$role.yaml"
for o in "$@"; do
  if [ "$o" = "pod-logs" ]; then
    [ "$role" = agent ] && files="$files -f $here/values/pod-logs.agent.yaml"
    continue
  fi
  f="$here/values/overlays/$o/$role.yaml"
  [ -f "$f" ] && files="$files -f $f"
done

version_flag=""
case "$chart" in /*|./*) ;; *) version_flag="--version $version" ;; esac

# Test identity only: fixed fake UID, name and a non-routable endpoint.
# shellcheck disable=SC2086
helm template "bluebox-otelcol-$role" "$chart" $version_flag \
  --namespace bluebox-otelcol \
  $files \
  --set global.bluebox.clusterUid=${CLUSTER_UID:-0a1b2c3d-1111-2222-3333-444455556666} \
  --set global.bluebox.clusterName=${CLUSTER_NAME:-ci-cluster} \
  --set global.bluebox.endpoint=${ENDPOINT:-https://example.invalid/api/v2/otlp} \
  ${EXTRA_SET:-} \
  > "$out"
