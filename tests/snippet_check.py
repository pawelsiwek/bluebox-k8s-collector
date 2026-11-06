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

LIVE_READ_KEY_STEP = (
    "\nprintf 'Paste the Bluebox live-read key (input is hidden), then press Enter: '"
    "\nread -rs BLUEBOX_LIVE_READ_KEY\necho"
    "\n[ -n \"$BLUEBOX_LIVE_READ_KEY\" ] || { echo 'No live-read key entered; nothing installed.' >&2; exit 1; }"
    "\nprintf '%s' \"$BLUEBOX_LIVE_READ_KEY\" | kubectl create secret generic bluebox-edgeconnect --namespace bluebox-otelcol"
    " --from-literal=client-id=dt0s02.EXAMPLE --from-file=client-secret=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -")
LIVE_READ_SETS = (' --set "global.bluebox.liveRead.edgeConnectName=bluebox-k8s-0617b478"'
                  ' --set "global.bluebox.liveRead.host=k8s-mcp-0617b478.bluebox.internal"'
                  ' --set "global.bluebox.liveRead.apiEndpointHost=abc12345.apps.dynatrace.com"'
                  ' --set "global.bluebox.liveRead.oauthResource=urn:dtenvironment:abc12345"'
                  ' --set "global.bluebox.liveRead.oauthEndpoint=https://sso.dynatrace.com/sso/oauth2/token"')
# distro: (agent files, cluster files, agent sets, psa[, cluster sets, live-read key step])
cases = {
    "eks": ("", "", "", "restricted"),
    "eks+live-read": ("", " -f <BASE>/overlays/live-read/cluster.yaml", "", "restricted", LIVE_READ_SETS, LIVE_READ_KEY_STEP),
    "aks": (" -f <BASE>/pod-logs.agent.yaml", "", ' --set "global.bluebox.podLogs.namespaces={shop,payments}"', "privileged"),
    "gke": (" -f <BASE>/overlays/autopilot/agent.yaml", " -f <BASE>/overlays/autopilot/cluster.yaml", "", "restricted"),
    "local": (" -f <BASE>/pod-logs.agent.yaml", "", ' --set "global.bluebox.podLogs.namespaces={*}"', "privileged"),
}
shells = [s for s in ("bash", "zsh") if shutil.which(s)]
for name, case in cases.items():
    agent_files, cluster_files, agent_sets, psa = case[:4]
    cluster_sets, key_step = (case[4], case[5]) if len(case) > 4 else ("", "")
    distro = name.split("+")[0]
    body = tpl.split("\n\n", 1)[1]
    s = (body.replace("<AGENT_EXTRA_FILES>", agent_files).replace("<CLUSTER_EXTRA_FILES>", cluster_files)
         .replace("<AGENT_EXTRA_SETS>", agent_sets).replace("<CLUSTER_EXTRA_SETS>", cluster_sets)
         .replace("<LIVE_READ_KEY_STEP>", key_step).replace("<BASE>", BASE).replace("<DISTRO>", distro)
         .replace("<PSA_LEVEL>", psa).replace("<CLUSTER_NAME>", "prod-eu-1")
         .replace("<ENDPOINT>", "https://abc12345.live.dynatrace.com/api/v2/otlp"))
    left = set(re.findall(r"<[A-Z_]+>", s)) - {"<REF>"}
    if left:
        failures.append(f"{name}: unfilled placeholders {sorted(left)}")
    if not (s.startswith("(\nset -e\n") and s.endswith("\n)\n")):
        failures.append(f"{name}: the snippet must be one subshell starting with set -e")
    if "\nread -rs BLUEBOX_TOKEN\n" not in s:
        failures.append(f"{name}: the token must come from a hidden read")
    for line in s.splitlines():
        if line.lstrip().startswith("#"):
            failures.append(f"{name}: comment line (interactive zsh rejects it): {line}")
        if line.startswith("helm upgrade"):
            for flag in ("--wait", "--atomic", "--version 0.177.0"):
                if flag not in line:
                    failures.append(f"{name}: missing {flag}")
            for f in re.findall(r"-f (\S+)", line):
                rel = f.replace(BASE + "/", "")
                if not os.path.isfile(os.path.join(root, "values", rel)):
                    failures.append(f"{name}: values file does not exist: {rel}")
    for sh in shells:
        r = subprocess.run([sh, "-n"], input=s, text=True, capture_output=True, timeout=20)
        if r.returncode != 0:
            failures.append(f"{name}/{sh}: does not parse: {r.stderr.strip()}")
    token_lines = [l for l in s.splitlines() if "$BLUEBOX_TOKEN" in l and "kubectl" in l]
    if token_lines != [l for l in token_lines if l.startswith("printf '%s' \"$BLUEBOX_TOKEN\" | kubectl create secret generic bluebox-otelcol-token ") and "--from-file=token=/dev/stdin" in l] or not token_lines:
        failures.append(f"{name}: the token must reach kubectl only on stdin")
    if key_step:
        if "\nread -rs BLUEBOX_LIVE_READ_KEY\n" not in s:
            failures.append(f"{name}: the live-read key must come from a hidden read")
        key_lines = [l for l in s.splitlines() if "$BLUEBOX_LIVE_READ_KEY" in l and "kubectl" in l]
        if not key_lines or any("--from-file=client-secret=/dev/stdin" not in l for l in key_lines):
            failures.append(f"{name}: the live-read key must reach kubectl only on stdin")
    if not s.rstrip().endswith("helm test bluebox-otelcol-agent --namespace bluebox-otelcol --logs\n)"):
        failures.append(f"{name}: the snippet must end with helm test")

if not shells:
    failures.append("neither bash nor zsh is installed")
if failures:
    print("FAIL snippet:")
    for x in failures:
        print("  -", x)
    sys.exit(1)
print(f"PASS snippet: {len(cases)} distributions in {', '.join(shells)}; parses; hidden token on stdin only, --wait --atomic")
