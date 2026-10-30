#!/bin/sh
# Validate every collector config (`relay` key) in a rendered manifest with
# `<collector> validate`, using the image the rendered release deploys
# (scripts/collector_image.sh maps the unpublished Bluebox image to a local build).
#   validate.sh <rendered.yaml>
# Needs podman or docker.
set -eu
rendered=$1
here=$(cd "$(dirname "$0")/.." && pwd)
engine=${CONTAINER_ENGINE:-$(command -v podman || command -v docker)}
rc=0; image=$("$here/scripts/collector_image.sh" "$rendered") || rc=$?
if [ "$rc" = 3 ] && [ "${ALLOW_MISSING_BLUEBOX_IMAGE:-0}" = 1 ]; then
  echo "SKIP (Bluebox image not available)"; exit 0
fi
[ "$rc" = 0 ] || exit "$rc"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
yq 'select(.kind == "ConfigMap" and .data.relay != null) | .data.relay' "$rendered" > "$tmp/relay.yaml"
[ -s "$tmp/relay.yaml" ] || { echo "no collector config in $rendered" >&2; exit 1; }
# k8s receivers build an API client during validate: give them a fake
# in-cluster ServiceAccount (throwaway self-signed CA, dummy token).
mkdir -p "$tmp/sa"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=validate \
  -keyout "$tmp/sa/key.pem" -out "$tmp/sa/ca.crt" >/dev/null 2>&1
printf 'validate-dummy' > "$tmp/sa/token"
printf 'bluebox-otelcol' > "$tmp/sa/namespace"
chmod -R a+rX "$tmp"
# Run with the container's exact args (feature gates, inline --config=yaml: sources),
# pointing the ConfigMap path at the extracted copy.
set --
yq -o=json -I=0 'select(.kind == "DaemonSet" or .kind == "Deployment") | select(.metadata.name | test("^bluebox-otelcol")) | .spec.template.spec.containers[0].args[]' "$rendered" > "$tmp/args.jsonl"
while IFS= read -r a; do
  a=$(printf '%s' "$a" | jq -r .)
  [ "$a" = "--config=/conf/relay.yaml" ] && a="--config=/cfg/relay.yaml"
  set -- "$@" "$a"
done < "$tmp/args.jsonl"
"$engine" run --rm -v "$tmp:/cfg:ro" -v "$tmp/sa:/var/run/secrets/kubernetes.io/serviceaccount:ro" \
  -e MY_POD_IP=127.0.0.1 -e OTEL_K8S_NODE_NAME=node -e OTEL_K8S_NODE_IP=127.0.0.1 \
  -e OTEL_K8S_NAMESPACE=bluebox-otelcol -e OTEL_K8S_POD_NAME=pod -e OTEL_K8S_POD_IP=127.0.0.1 \
  -e K8S_CLUSTER_UID=0a1b2c3d-1111-2222-3333-444455556666 -e K8S_CLUSTER_NAME=ci \
  -e BLUEBOX_ENDPOINT=https://example.invalid/api/v2/otlp \
  -e KUBERNETES_SERVICE_HOST=127.0.0.1 -e KUBERNETES_SERVICE_PORT=443 \
  "$image" validate "$@"
