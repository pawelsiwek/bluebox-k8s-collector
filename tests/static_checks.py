"""Static policy checks over every rendered manifest in a directory.

Usage: static_checks.py <dir-with-rendered-yaml>

AC "with pod logs off, the namespace stays PSA restricted": every render
without pod-logs must need only `restricted`. Also: no hostPort anywhere, no
nodes/proxy, no Secret/ConfigMap RBAC, images pinned by digest, the semconv v1
gates on every collector, one leader elector on all cluster receivers, the app
intake Service local to the node, no Service without ports. The kind e2e re-checks PSA with real admission.

A zero placeholder digest is a NOTE, or a failure with REQUIRE_PINNED_DIGESTS=1
(`make pinned`, used by the release workflow).
"""
import glob
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "scripts"))
import security_preview as sp  # noqa: E402

GATES = "--feature-gates=processor.k8sattributes.EmitV1K8sConventions,processor.k8sattributes.DontEmitV0K8sConventions"
ZERO_DIGEST = "@sha256:" + "0" * 64
REQUIRE_PINNED = os.environ.get("REQUIRE_PINNED_DIGESTS") == "1"
failures, notes = [], []


def fail(f, msg):
    failures.append(f"{os.path.basename(f)}: {msg}")


for f in sorted(glob.glob(os.path.join(sys.argv[1], "*.yaml"))):
    docs = sp.load(f)
    a = sp.analyse(f)
    name = os.path.basename(f)
    logs_on = "pod-logs" in name

    # PSA level
    want = "privileged" if logs_on else "restricted"
    if a["namespacePSA"] != want:
        fail(f, f"namespace PSA {a['namespacePSA']}, want {want}: " +
             "; ".join(v for p in a["pods"] for k in ("baseline", "restricted") for v in p["violations"][k]))
    for p in a["pods"]:
        if logs_on and p["workload"].startswith("DaemonSet/"):
            if p["hostPaths"] != ["/var/log/pods (read-only)"]:
                fail(f, f"{p['workload']}: hostPaths {p['hostPaths']}")
            if p["capabilitiesAdded"]:
                fail(f, f"{p['workload']}: capabilities added {p['capabilitiesAdded']}")
        elif p["psa"] != "restricted":
            fail(f, f"{p['workload']}: PSA {p['psa']} {p['violations']}")
        for img in p["images"]:
            if "@sha256:" not in img:
                fail(f, f"{p['workload']}: image not pinned by digest: {img}")
            elif img.endswith(ZERO_DIGEST):
                if REQUIRE_PINNED:
                    fail(f, f"placeholder digest: {img}")
                else:
                    notes.append(f"placeholder digest (pinned at release): {img}")

    # The API server rejects a Service with no ports; helm template does not.
    for d in docs:
        if d and d.get("kind") == "Service" and not (d.get("spec") or {}).get("ports"):
            fail(f, f"Service {d['metadata']['name']} has no spec.ports (API server rejects it)")

    # hostPort / host namespaces anywhere
    for owner, spec in sp.pod_specs(docs):
        for c in sp.containers(spec):
            for port in c.get("ports") or []:
                if port.get("hostPort"):
                    fail(f, f"{owner}: hostPort {port['hostPort']}")
        if spec.get("hostNetwork") or spec.get("hostPID") or spec.get("hostIPC"):
            fail(f, f"{owner}: host namespace")

    # RBAC
    for r in a["rbac"]:
        res = set(r["resources"])
        if "nodes/proxy" in res:
            fail(f, f"{r['role']}: nodes/proxy")
        if res & {"secrets", "configmaps", "*"}:
            fail(f, f"{r['role']}: reads {sorted(res & {'secrets', 'configmaps', '*'})}")
        writes = set(r["verbs"]) - {"get", "list", "watch"}
        if writes and res != {"leases"}:
            fail(f, f"{r['role']}: write verbs {sorted(writes)} on {sorted(res)}")
        if writes and r["scope"] == "cluster":
            fail(f, f"{r['role']}: cluster-wide write verbs")

    # Collector specifics
    cfg = sp.collector_config(docs)
    for d in docs:
        if d.get("kind") in ("DaemonSet", "Deployment") and d["metadata"]["name"].startswith("bluebox-otelcol"):
            c = d["spec"]["template"]["spec"]["containers"][0]
            if GATES not in (c.get("args") or []):
                fail(f, f"{d['metadata']['name']}: semconv v1 feature gates missing")
            want = {"ghcr.io/pawelsiwek/bluebox-otelcol": ["/bluebox-otelcol"],
                    "public.ecr.aws/dynatrace/dynatrace-otel-collector": ["/dynatrace-otel-collector"]}
            repo = c["image"].split("@")[0]
            if want.get(repo) != c.get("command"):
                fail(f, f"{d['metadata']['name']}: image {repo} with command {c.get('command')}")
    rec = cfg.get("receivers") or {}
    if name.startswith("cluster"):
        for r, v in rec.items():
            if r.startswith(("k8s_cluster", "k8s_events", "k8s_objects")) and \
                    (v or {}).get("k8s_leader_elector") != "k8s_leader_elector/cluster":
                fail(f, f"{r} without leader elector")
        for pname, p in cfg["service"]["pipelines"].items():
            if any(r.startswith("k8s_objects") for r in p["receivers"]) and pname != "logs/topology" \
                    and pname != "logs/cluster" and "filter/no_raw_objects" not in p["processors"]:
                fail(f, f"{pname}: k8s_objects receiver without filter/no_raw_objects")
        if topo_last := cfg["service"]["pipelines"]["logs/topology"]["processors"]:
            if "transform/finalize" not in topo_last:
                fail(f, "logs/topology does not replace the raw body (transform/finalize missing)")
        if "k8s_leader_elector/cluster" not in cfg["service"]["extensions"]:
            fail(f, "leader elector extension not enabled")
        topo = cfg["service"]["pipelines"]["logs/topology"]["processors"]
        if topo.index("transform/redact") != 1:
            fail(f, f"transform/redact is not first after memory_limiter: {topo}")
        for o in (rec.get("k8s_objects/topology") or {}).get("objects", []):
            if o.get("mode") != "pull":
                fail(f, f"k8s_objects {o['name']} not pull mode (watch would bypass redaction ordering)")
    else:
        svc = [d for d in docs if d.get("kind") == "Service" and d["metadata"]["name"] == "bluebox-otelcol"]
        if not svc or svc[0]["spec"].get("internalTrafficPolicy") != "Local":
            fail(f, "app OTLP intake Service missing or not internalTrafficPolicy Local")
    for pname, p in (cfg.get("service") or {}).get("pipelines", {}).items():
        connectors = set((cfg.get("connectors") or {}))
        if "otlp_http/bluebox" not in p["exporters"] or set(p["exporters"]) - {"otlp_http/bluebox"} - connectors:
            fail(f, f"pipeline {pname} exports to {p['exporters']}")
        if p["processors"][0] != "memory_limiter":
            fail(f, f"pipeline {pname} does not start with memory_limiter")
    if (cfg.get("extensions") or {}).get("bearertokenauth", {}).get("wait_for_token_file") is not True:
        fail(f, "bearertokenauth wait_for_token_file not set")

for n in sorted(set(notes)):
    print("NOTE", n)
if failures:
    print("FAIL static checks:")
    for x in failures:
        print("  -", x)
    sys.exit(1)
print(f"PASS static checks on {len(glob.glob(os.path.join(sys.argv[1], '*.yaml')))} renders "
      "(PSA restricted without pod logs, no hostPort, no nodes/proxy, pinned images, v1 gates, leader election)")
