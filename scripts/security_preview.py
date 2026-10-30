"""Security preview for the Bluebox collector install, from rendered manifests.

Usage: security_preview.py <rendered.yaml> [<rendered.yaml> ...] [--json]

Prints, per release: RBAC (cluster-wide and namespaced), hostPaths,
capabilities, run-as user, the Pod Security Admission level the namespace
needs, kubelet TLS mode, and the egress host:port list. The wizard shows this
before the user runs the install command (research §8.2). The same analysis
backs tests/static_checks.py.
"""
import json
import sys
from urllib.parse import urlparse

import yaml

WORKLOAD_KINDS = {"DaemonSet", "Deployment", "StatefulSet", "Job", "ReplicaSet"}


def load(path):
    with open(path, encoding="utf-8") as f:
        return [d for d in yaml.safe_load_all(f) if d]


def pod_specs(docs):
    """Yield (owner, podSpec) for every workload and bare Pod."""
    for d in docs:
        kind = d.get("kind")
        name = f"{kind}/{d.get('metadata', {}).get('name')}"
        if kind in WORKLOAD_KINDS:
            yield name, d["spec"]["template"]["spec"]
        elif kind == "Pod":
            yield name, d["spec"]


def containers(spec):
    return list(spec.get("initContainers") or []) + list(spec.get("containers") or [])


def eff(spec, c, key):
    """Container securityContext value, falling back to the pod level."""
    v = (c.get("securityContext") or {}).get(key)
    return v if v is not None else (spec.get("securityContext") or {}).get(key)


def psa_violations(spec):
    """Return {'baseline': [...], 'restricted': [...]} reasons this pod fails."""
    base, restr = [], []
    if spec.get("hostNetwork") or spec.get("hostPID") or spec.get("hostIPC"):
        base.append("host namespaces")
    for v in spec.get("volumes") or []:
        if "hostPath" in v:
            base.append(f"hostPath {v['hostPath'].get('path')}")
    allowed_vol = {"configMap", "csi", "downwardAPI", "emptyDir", "ephemeral", "persistentVolumeClaim", "projected", "secret"}
    for v in spec.get("volumes") or []:
        kinds = set(v) - {"name"}
        if not kinds <= allowed_vol and "hostPath" not in v:
            restr.append(f"volume type {sorted(kinds)}")
    for c in containers(spec):
        sc = c.get("securityContext") or {}
        n = c.get("name")
        if sc.get("privileged"):
            base.append(f"{n}: privileged")
        for p in c.get("ports") or []:
            if p.get("hostPort"):
                base.append(f"{n}: hostPort {p['hostPort']}")
        caps = (sc.get("capabilities") or {})
        added = set(caps.get("add") or [])
        baseline_ok = {"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD",
                       "NET_BIND_SERVICE", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT"}
        if added - baseline_ok:
            base.append(f"{n}: capabilities {sorted(added - baseline_ok)}")
        if added - {"NET_BIND_SERVICE"}:
            restr.append(f"{n}: adds {sorted(added)}")
        if "ALL" not in (caps.get("drop") or []):
            restr.append(f"{n}: capabilities not dropping ALL")
        if eff(spec, c, "allowPrivilegeEscalation") is not False and sc.get("allowPrivilegeEscalation") is not False:
            restr.append(f"{n}: allowPrivilegeEscalation not false")
        if eff(spec, c, "runAsNonRoot") is not True:
            restr.append(f"{n}: runAsNonRoot not true")
        if eff(spec, c, "runAsUser") == 0:
            restr.append(f"{n}: runAsUser 0")
        seccomp = (eff(spec, c, "seccompProfile") or {}).get("type")
        if seccomp not in ("RuntimeDefault", "Localhost"):
            restr.append(f"{n}: seccompProfile {seccomp}")
    return {"baseline": base, "restricted": restr}


def psa_level(spec):
    v = psa_violations(spec)
    if v["baseline"]:
        return "privileged"
    if v["restricted"]:
        return "baseline"
    return "restricted"


def collector_config(docs):
    for d in docs:
        if d.get("kind") == "ConfigMap" and (d.get("data") or {}).get("relay"):
            return yaml.safe_load(d["data"]["relay"])
    return {}


def values_endpoint(docs):
    for _, spec in pod_specs(docs):
        for c in containers(spec):
            for e in c.get("env") or []:
                if e.get("name") == "BLUEBOX_ENDPOINT" and e.get("value"):
                    return e["value"]
    return ""


def env_value(docs, name):
    for _, spec in pod_specs(docs):
        for c in containers(spec):
            for e in c.get("env") or []:
                if e.get("name") == name and e.get("value"):
                    return e["value"]
    return ""


def rbac(docs):
    out = []
    for d in docs:
        if d.get("kind") in ("ClusterRole", "Role"):
            scope = "cluster" if d["kind"] == "ClusterRole" else f"namespace {d['metadata'].get('namespace')}"
            for r in d.get("rules") or []:
                out.append({"role": d["metadata"]["name"], "scope": scope,
                            "apiGroups": r.get("apiGroups", []), "resources": r.get("resources", []),
                            "verbs": r.get("verbs", [])})
    return out


def analyse(path):
    docs = load(path)
    cfg = collector_config(docs)
    pods = []
    for owner, spec in pod_specs(docs):
        hostpaths = []
        mounts = {}
        for c in containers(spec):
            for m in c.get("volumeMounts") or []:
                mounts.setdefault(m["name"], []).append(bool(m.get("readOnly")))
        for v in spec.get("volumes") or []:
            if "hostPath" in v:
                ro = all(mounts.get(v["name"], [False]))
                hostpaths.append(f"{v['hostPath']['path']} ({'read-only' if ro else 'READ-WRITE'})")
        caps_added = sorted({cap for c in containers(spec)
                             for cap in ((c.get("securityContext") or {}).get("capabilities") or {}).get("add") or []})
        users = sorted({str(eff(spec, c, "runAsUser")) for c in containers(spec)})
        pods.append({"workload": owner, "psa": psa_level(spec), "hostPaths": hostpaths,
                     "capabilitiesAdded": caps_added, "runAsUser": users,
                     "images": [c.get("image") for c in containers(spec)],
                     "violations": psa_violations(spec)})
    egress = set()
    ep = values_endpoint(docs)
    if ep:
        u = urlparse(ep)
        egress.add(f"{u.hostname}:{u.port or 443}")
    proxy = env_value(docs, "HTTPS_PROXY")
    if proxy:
        u = urlparse(proxy)
        egress.add(f"{u.hostname}:{u.port or 3128} (proxy)")
    ec_host = env_value(docs, "EDGE_CONNECT_API_ENDPOINT_HOST")
    if any(p["workload"].endswith("bluebox-edgeconnect") for p in pods):
        egress.add(f"{ec_host or '<env>.apps.dynatrace.com'}:443 (EdgeConnect)")
        egress.add(f"{urlparse(env_value(docs, 'EDGE_CONNECT_OAUTH__ENDPOINT') or 'https://sso.dynatrace.com').hostname}:443 (EdgeConnect OAuth)")
    kubelet = (cfg.get("receivers") or {}).get("kubelet_stats")
    tls = None
    if kubelet is not None:
        tls = "UNVERIFIED (insecure_skip_verify)" if kubelet.get("insecure_skip_verify") else "verified"
    levels = [p["psa"] for p in pods]
    need = "privileged" if "privileged" in levels else "baseline" if "baseline" in levels else "restricted"
    return {"file": path, "namespacePSA": need, "pods": pods, "rbac": rbac(docs),
            "kubeletTLS": tls, "egress": sorted(egress)}


def render_text(a):
    lines = [f"== {a['file']}", f"Namespace Pod Security level required: {a['namespacePSA']}"]
    if a["kubeletTLS"]:
        lines.append(f"Kubelet TLS: {a['kubeletTLS']}")
    lines.append("Egress: " + (", ".join(a["egress"]) or "none (in-cluster only)"))
    for p in a["pods"]:
        lines.append(f"- {p['workload']}: PSA {p['psa']}, runAsUser {','.join(p['runAsUser'])}, "
                     f"capabilities added: {', '.join(p['capabilitiesAdded']) or 'none (ALL dropped)'}, "
                     f"hostPaths: {', '.join(p['hostPaths']) or 'none'}")
    lines.append("RBAC:")
    for r in a["rbac"]:
        lines.append(f"  [{r['scope']}] {r['role']}: {','.join(r['verbs'])} on "
                     f"{','.join(r['resources'])} ({','.join(g or 'core' for g in r['apiGroups'])})")
    return "\n".join(lines)


def main(argv):
    as_json = "--json" in argv
    files = [a for a in argv if a != "--json"]
    if not files:
        print(__doc__)
        return 2
    res = [analyse(f) for f in files]
    if as_json:
        print(json.dumps(res, indent=2))
    else:
        print("\n\n".join(render_text(a) for a in res))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
