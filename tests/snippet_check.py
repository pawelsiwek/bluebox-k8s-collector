"""Install snippet checks: shell-agnostic commands, Secret only via stdin,
--wait --atomic, and every referenced values file exists. Fills the
placeholders the way the wizard would for each distribution.
Usage: snippet_check.py (run from the repository root)"""
import os
import re
import sys

root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
tpl = open(os.path.join(root, "install", "snippet.template.txt"), encoding="utf-8").read()
failures = []
BASE = "https://raw.githubusercontent.com/pawelsiwek/bluebox-k8s-collector/<REF>/values"

cases = {
    "eks": ("", "", "", "restricted"),
    "aks": (" -f <BASE>/pod-logs.agent.yaml", "", ' --set "global.bluebox.podLogs.namespaces={shop,payments}"', "privileged"),
    "gke": (" -f <BASE>/overlays/autopilot/agent.yaml", " -f <BASE>/overlays/autopilot/cluster.yaml", "", "restricted"),
    "local": (" -f <BASE>/pod-logs.agent.yaml", "", "", "privileged"),
}
for distro, (agent_files, cluster_files, agent_sets, psa) in cases.items():
    s = (tpl.replace("<AGENT_EXTRA_FILES>", agent_files).replace("<CLUSTER_EXTRA_FILES>", cluster_files)
         .replace("<AGENT_EXTRA_SETS>", agent_sets).replace("<BASE>", BASE).replace("<DISTRO>", distro)
         .replace("<PSA_LEVEL>", psa).replace("<CLUSTER_NAME>", "prod-eu-1")
         .replace("<ENDPOINT>", "https://abc12345.live.dynatrace.com/api/v2/otlp")
         .replace("<TOKEN>", "dt0s16.FAKE"))
    in_secret = False
    for line in s.splitlines():
        if not line.strip() or line.startswith("#"):
            continue
        if line.startswith("apiVersion: v1"):
            in_secret = True
        if in_secret:
            if line.startswith(("helm ", "kubectl ")):
                in_secret = False
            else:
                continue
        cmd = line
        for bad, why in (("$", "variable"), ("`", "backtick"), ("|", "pipe"), ("&&", "chaining"),
                         (";", "chaining"), ("'", "single quote (cmd.exe)"), ("\\", "continuation/escape")):
            if bad in cmd:
                failures.append(f"{distro}: {why} in: {cmd}")
        if not cmd.startswith(("helm ", "kubectl ")):
            failures.append(f"{distro}: not a helm/kubectl command: {cmd}")
        if "token" in cmd.lower() and "secret" in cmd.lower() and "-f -" not in cmd:
            failures.append(f"{distro}: secret not from stdin: {cmd}")
        if cmd.startswith("helm upgrade"):
            for flag in ("--wait", "--atomic", "--version 0.177.0"):
                if flag not in cmd:
                    failures.append(f"{distro}: missing {flag}")
            for f in re.findall(r"-f (\S+)", cmd):
                rel = f.replace(BASE + "/", "")
                if not os.path.isfile(os.path.join(root, "values", rel)):
                    failures.append(f"{distro}: values file does not exist: {rel}")
            if "dt0s16" in cmd:
                failures.append(f"{distro}: token on a helm command line")
    if "<" in re.sub(r"<(REF)>", "", s.split("\n\n", 1)[1]):
        left = set(re.findall(r"<[A-Z_]+>", s)) - {"<REF>"}
        if left:
            failures.append(f"{distro}: unfilled placeholders {sorted(left)}")

if failures:
    print("FAIL snippet:")
    for x in failures:
        print("  -", x)
    sys.exit(1)
print(f"PASS snippet: {len(cases)} distributions, shell-agnostic, Secret via stdin, --wait --atomic")
