"""Validate every values file's global.bluebox block against values.schema.json,
plus known-good and known-bad snippet inputs.
Usage: schema_check.py (run from the repository root)"""
import copy
import glob
import json
import os
import sys

import jsonschema
import yaml

root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
schema = json.load(open(os.path.join(root, "values", "values.schema.json")))
validator = jsonschema.Draft202012Validator(schema)
failures = []

files = sorted(glob.glob(os.path.join(root, "values", "*.yaml")) +
               glob.glob(os.path.join(root, "values", "overlays", "*", "*.yaml")))
for f in files:
    doc = yaml.safe_load(open(f)) or {}
    for err in validator.iter_errors({"global": doc.get("global", {})}):
        failures.append(f"{os.path.relpath(f, root)}: {err.json_path}: {err.message}")

base = yaml.safe_load(open(os.path.join(root, "values", "common.yaml")))
good = copy.deepcopy(base)
good["global"]["bluebox"].update({
    "clusterUid": "3c428181-ee86-4a26-8f17-97cc00736650",
    "clusterName": "prod-eu-1",
    "endpoint": "https://abc12345.live.dynatrace.com/api/v2/otlp",
    "podLogs": {"namespaces": ["shop", "payments"]},
})
for err in validator.iter_errors(good):
    failures.append(f"known-good input rejected: {err.json_path}: {err.message}")

bad_cases = {
    "uid not a UUID": {"clusterUid": "kube-system"},
    "http endpoint": {"endpoint": "http://abc.live.dynatrace.com/api/v2/otlp"},
    "endpoint without otlp path": {"endpoint": "https://abc.live.dynatrace.com"},
    "glob in namespace": {"podLogs": {"namespaces": ["kube-*"]}},
    "brace in namespace": {"podLogs": {"namespaces": ["a,b}"]}},
    "unknown scheme": {"tokenScheme": "Basic"},
    "unpinned probe image": {"probe": {"image": "curlimages/curl:latest"}},
    "quote in allowlist": {"metrics": {"allow": ["a\")|true|(\""]}},
    "template in cluster name": {"clusterName": "{{ .Values }}"},
    "comma in cluster name (breaks --set)": {"clusterName": "a,b"},
    "backslash in cluster name": {"clusterName": "a\\b"},
    "dollar in cluster name (shell)": {"clusterName": "a$b"},
    "unknown key": {"extra": True},
}
for label, patch in bad_cases.items():
    doc = copy.deepcopy(good)
    doc["global"]["bluebox"].update(patch)
    if validator.is_valid(doc):
        failures.append(f"known-bad input accepted: {label}")

if failures:
    print("FAIL schema:")
    for x in failures:
        print("  -", x)
    sys.exit(1)
print(f"PASS schema: {len(files)} values files valid, {len(bad_cases)} bad inputs rejected")
