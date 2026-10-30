#!/bin/sh
# kind e2e for the Bluebox collector packaging. Runs in CI
# (.github/workflows/ci.yml); needs kind, kubectl, helm, jq,
# openssl. Fake tokens only; telemetry goes to an in-cluster HTTPS sink.
#
# Proves, against a real API server and real Pod Security Admission:
#   1. both releases install with --wait --atomic into a namespace enforcing
#      PSA `restricted` (pod logs off);
#   2. a pod with a literal env secret, command args and a probe header never
#      reaches the exporter, and valueFrom survives;
#   3. v1 attribute names only;
#   4. with 2 cluster replicas each Kubernetes event is exported once;
#   5. a rotated token Secret is used without a pod restart;
#   6. the helm test probe prints the C4 keys and passes.
set -eu
here=$(cd "$(dirname "$0")/../.." && pwd)
cluster=${KIND_CLUSTER:-bluebox-e2e}
ns=bluebox-otelcol
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
k() { kubectl --context "kind-$cluster" "$@"; }
fail() { echo "FAIL: $*" >&2; k -n "$ns" get pods -o wide >&2 || true; k -n sink logs deploy/otlp-sink --tail=20 >&2 || true; exit 1; }
records() { k -n sink exec deploy/otlp-sink -- cat /data/sink.jsonl > "$work/sink.jsonl"; }

kind get clusters | grep -qx "$cluster" || kind create cluster --name "$cluster" --wait 120s

# --- in-cluster HTTPS OTLP sink (bearertokenauth requires TLS) ---
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=otlp-sink.sink.svc" \
  -addext "subjectAltName=DNS:otlp-sink.sink.svc,DNS:otlp-sink.sink.svc.cluster.local" \
  -keyout "$work/key.pem" -out "$work/ca.crt" >/dev/null 2>&1
k create namespace sink --dry-run=client -o yaml | k apply -f -
k -n sink create secret tls sink-tls --cert "$work/ca.crt" --key "$work/key.pem" --dry-run=client -o yaml | k apply -f -
k -n sink create configmap sink-py --from-file=otlp_sink.py="$here/tests/otlp_sink.py" --dry-run=client -o yaml | k apply -f -
k apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: otlp-sink, namespace: sink}
spec:
  replicas: 1
  selector: {matchLabels: {app: otlp-sink}}
  template:
    metadata: {labels: {app: otlp-sink}}
    spec:
      containers:
        - name: sink
          image: python:3.13-alpine
          command: [python3, -u, /app/otlp_sink.py, "8443", /tls/tls.crt, /tls/tls.key, /data/sink.jsonl]
          volumeMounts:
            - {name: app, mountPath: /app}
            - {name: tls, mountPath: /tls}
            - {name: data, mountPath: /data}
      volumes:
        - {name: app, configMap: {name: sink-py}}
        - {name: tls, secret: {secretName: sink-tls}}
        - {name: data, emptyDir: {}}
---
apiVersion: v1
kind: Service
metadata: {name: otlp-sink, namespace: sink}
spec:
  selector: {app: otlp-sink}
  ports: [{port: 8443, targetPort: 8443}]
EOF
k -n sink rollout status deploy/otlp-sink --timeout=180s

# --- namespace: PSA restricted enforced (pod logs off) ---
k create namespace "$ns" --dry-run=client -o yaml | k apply -f -
k label namespace "$ns" pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/warn=restricted --overwrite
k -n "$ns" create configmap bluebox-otelcol-ca --from-file=ca.crt="$work/ca.crt" --dry-run=client -o yaml | k apply -f -
printf 'apiVersion: v1\nkind: Secret\nmetadata: {name: bluebox-otelcol-token}\nstringData: {token: fake-e2e-token-A}\n' | k -n "$ns" apply -f -

# Image variant: BLUEBOX_IMAGE (a local build; CI passes the image it built) is
# loaded into kind and used for both releases. Without it the test runs the
# stock Dynatrace image, because the published Bluebox image pin is a placeholder.
if [ -n "${BLUEBOX_IMAGE:-}" ]; then
  kind load docker-image "$BLUEBOX_IMAGE" --name "$cluster"
  img_args="--set image.repository=${BLUEBOX_IMAGE%:*} --set image.tag=${BLUEBOX_IMAGE##*:} --set image.digest= --set image.pullPolicy=Never"
  agent_img="" cluster_img=""
else
  img_args=""
  agent_img="-f $here/values/overlays/dynatrace-image/agent.yaml"
  cluster_img="-f $here/values/overlays/dynatrace-image/cluster.yaml"
fi
helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts --force-update >/dev/null
common="--version ${CHART_VERSION:-0.177.0} --namespace $ns --wait --atomic --timeout 6m
  --set global.bluebox.clusterName=kind-e2e --set global.bluebox.endpoint=https://otlp-sink.sink.svc:8443/api/v2/otlp
  --set global.bluebox.caBundle=true
  --set config.exporters.otlp_http/bluebox.encoding=json"
# shellcheck disable=SC2086
helm upgrade --install bluebox-otelcol-agent open-telemetry/opentelemetry-collector $common \
  -f "$here/values/common.yaml" -f "$here/values/agent.yaml" $agent_img -f "$here/values/overlays/local/agent.yaml" $img_args \
  || fail "agent release did not install under PSA restricted"
# Cluster release with the production replica count, not the local single replica.
# shellcheck disable=SC2086
helm upgrade --install bluebox-otelcol-cluster open-telemetry/opentelemetry-collector $common \
  -f "$here/values/common.yaml" -f "$here/values/cluster.yaml" $cluster_img $img_args --set replicaCount=2 \
  || fail "cluster release did not install under PSA restricted"
echo "ok: both releases admitted with enforce=restricted"
restarts_before=$(k -n "$ns" get pods -o jsonpath='{range .items[*]}{.metadata.name}={.status.containerStatuses[0].restartCount}{"\n"}{end}' | sort)

# --- workload with secrets in its spec; also generates events ---
k create namespace demo --dry-run=client -o yaml | k apply -f -
k -n demo apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata: {name: secretive, labels: {app: secretive}}
spec:
  containers:
    - name: app
      image: registry.k8s.io/pause:3.10
      command: ["/pause"]
      args: ["--password=E2E_ARG_SECRET"]
      env:
        - {name: LITERAL, value: E2E_ENV_SECRET}
        - name: REF
          valueFrom: {fieldRef: {fieldPath: status.podIP}}
      readinessProbe:
        httpGet: {path: /, port: 8080, httpHeaders: [{name: Authorization, value: Bearer E2E_PROBE_SECRET}]}
---
apiVersion: v1
kind: Pod
metadata: {name: bad-image}
spec:
  containers: [{name: x, image: registry.invalid/does-not-exist:1}]
EOF

# Topology pull interval is 120s; wait for the pod's k8s.object.
i=0
until records && jq -e 'select(.path == "/v1/logs") | .body.resourceLogs[].scopeLogs[].logRecords[] | select(any(.attributes[]; .key == "k8s.pod.name" and .value.stringValue == "secretive")) | select(any(.attributes[]; .key == "k8s.object"))' "$work/sink.jsonl" >/dev/null 2>&1; do
  i=$((i + 1)); [ $i -gt 60 ] && fail "no topology record for demo/secretive"; sleep 5
done
for s in E2E_ARG_SECRET E2E_ENV_SECRET E2E_PROBE_SECRET; do
  if grep -q "$s" "$work/sink.jsonl"; then fail "secret $s reached the exporter"; fi
done
jq -e 'select(.path == "/v1/logs") | .body.resourceLogs[].scopeLogs[].logRecords[].attributes[] | select(.key == "k8s.object") | .value.stringValue | fromjson | select(.metadata.name == "secretive") | .spec.containers[0].env[] | select(.name == "REF") | .valueFrom.fieldRef' "$work/sink.jsonl" >/dev/null \
  || fail "valueFrom not kept"
echo "ok: redaction in a live cluster"

# --- v1 attribute names only ---
# The k8sattributes v0 -> v1 renames that apply to this config:
# container.image.tag -> container.image.tags (agent extracts it).
if grep -qF '"key": "container.image.tag"' "$work/sink.jsonl"; then fail "v0 attribute container.image.tag present"; fi
grep -qF '"key": "container.image.tags"' "$work/sink.jsonl" || fail "v1 attribute container.image.tags missing"
jq -e '[.body.resourceMetrics[]?.resource.attributes[]?.key] | index("k8s.cluster.uid")' "$work/sink.jsonl" >/dev/null || fail "k8s.cluster.uid missing on metrics"
echo "ok: v1 attribute names"

# --- event dedup with 2 replicas ---
sleep 60; records
dups=$(jq -r 'select(.path == "/v1/logs") | .body.resourceLogs[].scopeLogs[].logRecords[] | select(any(.attributes[]; .key == "event.domain" and .value.stringValue == "k8s")) | [(.attributes[] | select(.key == "k8s.event.uid") | .value.stringValue), (.attributes[] | select(.key == "k8s.event.count") | .value.intValue // .value.stringValue)] | @tsv' "$work/sink.jsonl" | sort | uniq -d | wc -l)
total=$(jq -r 'select(.path == "/v1/logs") | .body.resourceLogs[].scopeLogs[].logRecords[] | select(any(.attributes[]; .key == "event.domain")) | 1' "$work/sink.jsonl" | wc -l)
[ "$total" -gt 0 ] || fail "no k8s events exported"
[ "$dups" -eq 0 ] || fail "$dups duplicated events with 2 replicas"
echo "ok: $total events, none duplicated across 2 replicas"

# --- token rotation without restart ---
printf 'apiVersion: v1\nkind: Secret\nmetadata: {name: bluebox-otelcol-token}\nstringData: {token: fake-e2e-token-B}\n' | k -n "$ns" apply -f -
i=0
until records && grep -q '"authorization": "Bearer fake-e2e-token-B"' "$work/sink.jsonl"; do
  i=$((i + 1)); [ $i -gt 48 ] && fail "rotated token not used within 4 minutes"; sleep 5
done
restarts_after=$(k -n "$ns" get pods -o jsonpath='{range .items[*]}{.metadata.name}={.status.containerStatuses[0].restartCount}{"\n"}{end}' | sort)
[ "$restarts_before" = "$restarts_after" ] || fail "pods restarted during rotation"
echo "ok: token rotated without restart"

# --- helm test probe (C4) ---
helm test bluebox-otelcol-agent --namespace "$ns" --logs > "$work/test.txt" 2>&1 || { cat "$work/test.txt"; fail "helm test failed"; }
for key in otlp_status=200 cause=ok ready=true nodes=1; do grep -q "^$key$" "$work/test.txt" || fail "probe output missing $key"; done
echo "ok: helm test probe"
echo "PASS kind e2e"
