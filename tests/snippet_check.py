"""Install snippet checks: one bash/zsh subshell with no comment lines, the
token read hidden and passed to kubectl only on stdin, --wait --atomic, and
every referenced values file exists. Fills the placeholders the way the wizard
would for each distribution, and parses the result with bash -n (and
zsh -n when installed).
Usage: snippet_check.py (run from the repository root)"""
import os
import re
import shutil
import subprocess
import sys

root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
tpl = open(os.path.join(root, "install", "snippet.template.txt"), encoding="utf-8").read()
failures = []
BASE = "https://raw.githubusercontent.com/pawelsiwek/bluebox-k8s-collector/<REF>/values"

cases = {
    "eks": ("", "", "", "restricted"),
    "aks": (" -f <BASE>/pod-logs.agent.yaml", "", ' --set "global.bluebox.podLogs.namespaces={shop,payments}"', "privileged"),
    "gke": (" -f <BASE>/overlays/autopilot/agent.yaml", " -f <BASE>/overlays/autopilot/cluster.yaml", "", "restricted"),
    "local": (" -f <BASE>/pod-logs.agent.yaml", "", ' --set "global.bluebox.podLogs.namespaces={*}"', "privileged"),
}
shells = [s for s in ("bash", "zsh") if shutil.which(s)]
for distro, (agent_files, cluster_files, agent_sets, psa) in cases.items():
    body = tpl.split("\n\n", 1)[1]
    s = (body.replace("<AGENT_EXTRA_FILES>", agent_files).replace("<CLUSTER_EXTRA_FILES>", cluster_files)
         .replace("<AGENT_EXTRA_SETS>", agent_sets).replace("<BASE>", BASE).replace("<DISTRO>", distro)
         .replace("<PSA_LEVEL>", psa).replace("<CLUSTER_NAME>", "prod-eu-1")
         .replace("<ENDPOINT>", "https://abc12345.live.dynatrace.com/api/v2/otlp"))
    left = set(re.findall(r"<[A-Z_]+>", s)) - {"<REF>"}
    if left:
        failures.append(f"{distro}: unfilled placeholders {sorted(left)}")
    if not (s.startswith("(\nset -e\n") and s.endswith("\n)\n")):
        failures.append(f"{distro}: the snippet must be one subshell starting with set -e")
    if "\nread -rs BLUEBOX_TOKEN\n" not in s:
        failures.append(f"{distro}: the token must come from a hidden read")
    for line in s.splitlines():
        if line.lstrip().startswith("#"):
            failures.append(f"{distro}: comment line (interactive zsh rejects it): {line}")
        if line.startswith("helm upgrade"):
            for flag in ("--wait", "--atomic", "--version 0.177.0"):
                if flag not in line:
                    failures.append(f"{distro}: missing {flag}")
            for f in re.findall(r"-f (\S+)", line):
                rel = f.replace(BASE + "/", "")
                if not os.path.isfile(os.path.join(root, "values", rel)):
                    failures.append(f"{distro}: values file does not exist: {rel}")
    for sh in shells:
        r = subprocess.run([sh, "-n"], input=s, text=True, capture_output=True, timeout=20)
        if r.returncode != 0:
            failures.append(f"{distro}/{sh}: does not parse: {r.stderr.strip()}")
    token_lines = [l for l in s.splitlines() if "$BLUEBOX_TOKEN" in l and "kubectl" in l]
    if token_lines != [l for l in token_lines if l.startswith("printf '%s' \"$BLUEBOX_TOKEN\" | kubectl create secret generic bluebox-otelcol-token ") and "--from-file=token=/dev/stdin" in l] or not token_lines:
        failures.append(f"{distro}: the token must reach kubectl only on stdin")
    if not s.rstrip().endswith("helm test bluebox-otelcol-agent --namespace bluebox-otelcol --logs\n)"):
        failures.append(f"{distro}: the snippet must end with helm test")

if not shells:
    failures.append("neither bash nor zsh is installed")
if failures:
    print("FAIL snippet:")
    for x in failures:
        print("  -", x)
    sys.exit(1)
print(f"PASS snippet: {len(cases)} distributions in {', '.join(shells)}; parses; hidden token on stdin only, --wait --atomic")
