"""Assertions for redaction_test.sh.
Usage: assert_redaction.py <out.json> <secrets.txt> [bluebox|dynatrace]"""
import json
import sys

out_path, secrets_path = sys.argv[1], sys.argv[2]
variant = sys.argv[3] if len(sys.argv) > 3 else "bluebox"
raw = open(out_path, encoding="utf-8").read()
secrets = [s for s in open(secrets_path, encoding="utf-8").read().split() if s]
failures = []

# 1. No literal secret anywhere in the exporter output (body, attributes, resource).
for s in secrets:
    if s in raw:
        failures.append(f"secret survived: {s}")

records = []
for line in raw.splitlines():
    if not line.strip():
        continue
    for rl in json.loads(line)["resourceLogs"]:
        for sl in rl["scopeLogs"]:
            records.extend(sl["logRecords"])


def attr(rec, key):
    for a in rec.get("attributes", []):
        if a["key"] == key:
            return next(iter(a["value"].values()))
    return None


by_kind = {}
by_name = {}
derived = []  # K8S_CLUSTER / CONTAINER records: no k8s.object, empty body
for rec in records:
    if attr(rec, "k8s.object") is None:
        derived.append(rec)
        if rec.get("body", {}).get("stringValue", ""):
            failures.append(f"{attr(rec, 'k8s.topo.node.type')}: derived record has a body")
        continue
    obj = json.loads(attr(rec, "k8s.object"))  # must stay valid JSON
    by_name[obj["metadata"]["name"]] = (rec, obj)
    if obj["metadata"]["name"] != "web-abc-xyz12":
        by_kind[obj["kind"]] = (rec, obj)
    if rec["body"].get("stringValue") != attr(rec, "k8s.object"):
        failures.append(f"{obj['kind']}: body is not the redacted k8s.object")
    if attr(rec, "k8s.object.redacted") is not True:
        failures.append(f"{obj['kind']}: k8s.object.redacted marker missing")
    ann = obj.get("metadata", {}).get("annotations", {})
    if "kubectl.kubernetes.io/last-applied-configuration" in ann:
        failures.append("last-applied-configuration annotation not removed")

ALL = {"Namespace", "Node", "Pod", "Service", "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet"}
if set(by_kind) != ALL:
    failures.append(f"missing kinds {sorted(ALL - set(by_kind))}; a statement may have failed on them")
if {"Node", "ReplicaSet"} <= set(by_kind) and (
        by_kind["Node"][1]["status"]["nodeInfo"]["systemUUID"] != "1234-abcd"
        or by_kind["ReplicaSet"][1]["metadata"]["ownerReferences"][0]["name"] != "web"):
    failures.append("identity fields lost on Node/ReplicaSet")
if {"Pod", "Deployment"} <= set(by_kind):
    _, pod = by_kind["Pod"]
    c = pod["spec"]["containers"][0]
    env = {e["name"]: e for e in c["env"]}
    if env["FROM_SECRET"].get("valueFrom") != {"secretKeyRef": {"name": "db-creds", "key": "password"}}:
        failures.append(f"valueFrom secretKeyRef not kept: {env['FROM_SECRET']}")
    if env["FROM_FIELD"].get("valueFrom") != {"fieldRef": {"fieldPath": "status.podIP"}}:
        failures.append("valueFrom fieldRef not kept")
    for name in ("LITERAL", "ESCAPED", "NUMERIC"):
        if env[name].get("value") != "<redacted>":
            failures.append(f"env {name} not redacted: {env[name]}")
    if c["command"] != ["<redacted>"] or c["args"] != ["<redacted>"]:
        failures.append("command/args not redacted")
    hdr = c["livenessProbe"]["httpGet"]["httpHeaders"][0]
    if hdr != {"name": "Authorization", "value": "<redacted>"}:
        failures.append(f"probe header not redacted: {hdr}")
    if c["livenessProbe"]["httpGet"]["path"] != "/healthz":
        failures.append("non-secret probe field lost")
    if pod["metadata"]["name"] != "redaction-test" or c["image"] != "nginx":
        failures.append("identity fields lost")
    if pod["metadata"]["annotations"].get("app.example.com/api-token") != "<redacted>":
        failures.append("token-named annotation not redacted")
    if pod["metadata"]["annotations"].get("example.com/config") != "<redacted>":
        failures.append("JSON annotation not redacted")
    rec, _ = by_kind["Pod"]
    for k, want in (("k8s.pod.name", "redaction-test"), ("k8s.topo.node.type", "K8S_POD"),
                    ("k8s.namespace.name", "demo"), ("event.provider", "KUBERNETES_OTEL_TOPO")):
        if attr(rec, k) != want:
            failures.append(f"topology attribute {k}={attr(rec, k)!r}, want {want!r}")
    _, dep = by_kind["Deployment"]
    dc = dep["spec"]["template"]["spec"]["containers"][0]
    denv = {e["name"]: e for e in dc["env"]}
    if denv["REF"].get("valueFrom") != {"secretKeyRef": {"name": "web-creds", "key": "pw"}}:
        failures.append("deployment valueFrom not kept")
    if dep["spec"]["replicas"] != 2:
        failures.append("deployment replicas lost")

# K8S_CLUSTER: dtk8stopology (bluebox) or the kube-system Namespace pull (dynatrace).
clusters = [r for r in derived if attr(r, "k8s.topo.node.type") == "K8S_CLUSTER"]
if not clusters or any(attr(r, "k8s.cluster.uid") != "0a1b2c3d-1111-2222-3333-444455556666" for r in clusters):
    failures.append(f"no K8S_CLUSTER record with the cluster uid ({variant})")
containers = [r for r in derived if attr(r, "k8s.topo.node.type") == "CONTAINER"]
if variant == "bluebox":
    imgs = {(attr(r, "k8s.pod.name"), attr(r, "k8s.container.name")): attr(r, "k8s.container.image") for r in containers}
    for key, want in {("redaction-test", "app"): "nginx", ("redaction-test", "init"): "busybox",
                      ("web-abc-xyz12", "web"): "nginx:1.27"}.items():
        if imgs.get(key) != want:
            failures.append(f"CONTAINER fan-out {key}: image {imgs.get(key)!r}, want {want!r} (got {sorted(imgs)})")
else:
    if containers:
        failures.append("dynatrace variant produced CONTAINER records (no dtk8stopology expected)")
    rec, _ = by_name.get("web-abc-xyz12", (None, None))
    if rec is None or attr(rec, "k8s.workload.name") != "web" or attr(rec, "k8s.workload.kind") != "deployment":
        failures.append("ownerReferences workload not derived: "
                        f"{rec and (attr(rec, 'k8s.workload.name'), attr(rec, 'k8s.workload.kind'))}")

if failures:
    print("FAIL redaction:")
    for f in failures:
        print("  -", f)
    sys.exit(1)
print(f"PASS redaction ({variant}): {len(records)} records ({len(clusters)} K8S_CLUSTER, {len(containers)} CONTAINER), "
      f"{len(secrets)} secrets absent, valueFrom kept, last-applied removed")
