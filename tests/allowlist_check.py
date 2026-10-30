"""Every metric allowlist alternative must match at least one metric the
receivers actually emit, so a typo or upstream rename cannot silently drop
data (and turn the C4 `metrics` probe into `missing`).

Emitted = metadata.yaml metrics of kubeletstats / k8s_cluster v0.163.0 that
are enabled by default or enabled in our config, plus k8s_cluster's dynamic
names (k8s.node.allocatable_<type>, k8s.node.condition_<type>), plus the
collector self-telemetry heartbeat names.
Usage: allowlist_check.py <rendered-dir> (run from the repository root)"""
import json
import os
import re
import sys

import yaml

root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
fx = os.path.join(root, "tests", "fixtures", "receiver-metrics")
out = sys.argv[1]


def relay(path):
    for d in yaml.safe_load_all(open(path)):
        if d and d.get("kind") == "ConfigMap" and (d.get("data") or {}).get("relay"):
            return yaml.safe_load(d["data"]["relay"])
    return {}


def snake(s):
    return re.sub(r"(?<!^)(?=[A-Z])", "_", s).lower()


agent = relay(os.path.join(out, "agent_.yaml"))
cluster = relay(os.path.join(out, "cluster_.yaml"))
kc = cluster["receivers"]["k8s_cluster"]
enabled_extra = {k for k, v in (kc.get("metrics") or {}).items() if v.get("enabled")}
enabled_extra |= {k for k, v in (agent["receivers"]["kubelet_stats"].get("metrics") or {}).items() if v.get("enabled")}

emitted = set()
for f in ("kubelet-v0.163.0.json", "k8scluster-v0.163.0.json"):
    for m in json.load(open(os.path.join(fx, f))):
        if m["enabled"] or m["name"] in enabled_extra:
            emitted.add(m["name"])
emitted |= {"k8s.node.allocatable_" + snake(t) for t in kc.get("allocatable_types_to_report", [])}
emitted |= {"k8s.node.condition_" + snake(t) for t in kc.get("node_conditions_to_report", [])}
emitted |= {"otelcol_process_uptime", "otelcol_exporter_sent_metric_points", "otelcol_exporter_sent_log_records",
            "otelcol_exporter_send_failed_metric_points", "otelcol_exporter_send_failed_log_records"}

base = yaml.safe_load(open(os.path.join(root, "values", "common.yaml")))
allow = base["global"]["bluebox"]["metrics"]["allow"]
failures = []
for pat in allow:
    rx = re.compile("^(?:" + pat + ")$")
    if not any(rx.match(n) for n in emitted):
        failures.append(f"allowlist entry matches nothing emitted: {pat}")

# The C4 `metrics` probe needs at least these to survive the filter.
must = ["k8s.pod.cpu.usage", "k8s.node.memory.working_set", "k8s.pod.phase", "k8s.container.restarts",
        "k8s.node.condition", "k8s.container.status.reason", "otelcol_process_uptime"]
full = re.compile("^(?:" + "|".join(allow) + ")$")
for n in must:
    if n not in emitted:
        failures.append(f"required metric not emitted by config: {n}")
    if not full.match(n):
        failures.append(f"required metric filtered out: {n}")

if failures:
    print("FAIL allowlist:")
    for x in failures:
        print("  -", x)
    sys.exit(1)
kept = sorted(n for n in emitted if full.match(n))
print(f"PASS allowlist: {len(allow)} entries all match emitted metrics; {len(kept)} of {len(emitted)} emitted names kept")
