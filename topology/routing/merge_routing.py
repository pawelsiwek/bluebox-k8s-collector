#!/usr/bin/env python3
"""Merge the Bluebox K8s topology entry into an exported logs-routing singleton.

The logs routing object (builtin:openpipeline.logs.routing) is one per tenant and
`dtctl apply` replaces its whole entry list, so this tool never writes to a
tenant: it reads an export, edits only the entry carrying this workspace's
marker, and writes a single settings object for `dtctl diff` / `dtctl apply`.

Exit codes: 0 ok, 2 bad input, 3 marker conflict, 4 tenant changed since export,
5 verification failed.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import re
import sys
from pathlib import Path

SCHEMA_ID = "builtin:openpipeline.logs.routing"
TEMPLATE = Path(__file__).with_name("logs-routing-entry.json")
UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
# Pipeline objectIds are opaque base64url strings; anything else is a pasted mistake.
PIPELINE_ID_RE = re.compile(r"^[A-Za-z0-9_\-]{8,512}$")


class ToolError(Exception):
    """A refusal with its exit code."""

    def __init__(self, code: int, message: str) -> None:
        super().__init__(message)
        self.code = code


def marker(workspace_id: str) -> str:
    return f"[bluebox:k8s-topology workspace={workspace_id}]"


def load_export(path: str) -> dict:
    """Read a `dtctl get settings` export (JSON, or YAML when PyYAML is present)."""
    text = sys.stdin.read() if path == "-" else Path(path).read_text(encoding="utf-8")
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        try:
            import yaml  # type: ignore[import-untyped]
        except ImportError as exc:
            raise ToolError(2, "export is not JSON; export with `-o json` or install PyYAML") from exc
        data = yaml.safe_load(text)
    if isinstance(data, list):
        if len(data) != 1:
            raise ToolError(2, f"export holds {len(data)} objects; the logs routing singleton must be exactly one")
        data = data[0]
    if not isinstance(data, dict):
        raise ToolError(2, "export is not a settings object")
    obj = {_camel(k): v for k, v in data.items()}
    if obj.get("schemaId") != SCHEMA_ID:
        raise ToolError(2, f"export schemaId is {obj.get('schemaId')!r}, want {SCHEMA_ID}")
    entries = (obj.get("value") or {}).get("routingEntries")
    if not isinstance(entries, list) or not all(isinstance(e, dict) for e in entries):
        raise ToolError(2, "export has no value.routingEntries list")
    return obj


def _camel(key: str) -> str:
    # dtctl's YAML output lowercases field names (objectid); the apply path accepts both.
    return {"objectid": "objectId", "schemaid": "schemaId", "schemaversion": "schemaVersion",
            "externalid": "externalId", "modificationinfo": "modificationInfo"}.get(key, key)


def build_entry(workspace_id: str, pipeline_id: str, template: Path = TEMPLATE) -> dict:
    if not UUID_RE.match(workspace_id):
        raise ToolError(2, "workspace id must be a lowercase UUID")
    if not PIPELINE_ID_RE.match(pipeline_id):
        raise ToolError(2, "pipeline id does not look like a settings objectId")
    entry = json.loads(template.read_text(encoding="utf-8"))
    entry["description"] = entry["description"].replace("${MARKER}", marker(workspace_id))
    entry["pipelineId"] = pipeline_id
    if "${" in json.dumps(entry):
        raise ToolError(2, "template has an unfilled placeholder")
    return entry


def entries_digest(obj: dict) -> str:
    entries = obj["value"]["routingEntries"]
    return hashlib.sha256(json.dumps(entries, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def _same(existing: dict, entry: dict) -> bool:
    # The API may echo extra keys as null on read-back; anything else extra is a real difference.
    extra = {k: v for k, v in existing.items() if k not in entry and v is not None}
    return not extra and all(existing.get(k) == v for k, v in entry.items())


def _ours(entries: list[dict], workspace_id: str) -> list[int]:
    m = marker(workspace_id)
    return [i for i, e in enumerate(entries) if str(e.get("description", "")).startswith(m)]


def _for_apply(obj: dict, entries: list[dict]) -> dict:
    out = {k: copy.deepcopy(obj[k]) for k in ("objectId", "schemaId", "scope") if k in obj}
    value = copy.deepcopy(obj["value"])
    value["routingEntries"] = entries
    out["value"] = value
    return out


def merge(obj: dict, entry: dict, workspace_id: str) -> tuple[dict, str]:
    """Insert entry first; a byte-equal existing entry is left where it is."""
    entries = obj["value"]["routingEntries"]
    found = _ours(entries, workspace_id)
    if len(found) > 1:
        raise ToolError(3, f"marker for workspace {workspace_id} appears {len(found)} times; fix by hand")
    if found:
        if not _same(entries[found[0]], entry):
            raise ToolError(3, f"entry with marker for workspace {workspace_id} exists with different content; refusing to overwrite")
        return _for_apply(obj, copy.deepcopy(entries)), "unchanged"
    return _for_apply(obj, [copy.deepcopy(entry)] + copy.deepcopy(entries)), "inserted"


def remove(obj: dict, entry: dict, workspace_id: str) -> tuple[dict, str]:
    entries = obj["value"]["routingEntries"]
    found = _ours(entries, workspace_id)
    if not found:
        return _for_apply(obj, copy.deepcopy(entries)), "unchanged"
    if len(found) > 1 or not _same(entries[found[0]], entry):
        raise ToolError(3, f"entry with marker for workspace {workspace_id} differs from ours; refusing to remove")
    return _for_apply(obj, [copy.deepcopy(e) for i, e in enumerate(entries) if i != found[0]]), "removed"


def verify(before: dict, after: dict, entry: dict, workspace_id: str, expect_present: bool) -> list[str]:
    """Compare two exports: foreign entries identical and in order; ours present or absent."""
    problems = []
    b, a = before["value"]["routingEntries"], after["value"]["routingEntries"]
    b_ours, a_ours = set(_ours(b, workspace_id)), set(_ours(a, workspace_id))
    foreign_b = [e for i, e in enumerate(b) if i not in b_ours]
    foreign_a = [e for i, e in enumerate(a) if i not in a_ours]
    if foreign_a != foreign_b:
        problems.append("foreign routing entries changed or were reordered")
    if expect_present:
        if len(a_ours) != 1 or not _same(a[min(a_ours)], entry):
            problems.append("our entry is missing or differs from the template")
        elif min(a_ours) != 0:
            problems.append("our entry is not first; a broader matcher above it may claim the logs")
    elif a_ours:
        problems.append("our entry is still present")
    return problems


def _write(obj: dict, out: str) -> None:
    text = json.dumps(obj, indent=2, sort_keys=False) + "\n"
    if out == "-":
        sys.stdout.write(text)
    else:
        Path(out).write_text(text, encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = p.add_subparsers(dest="cmd", required=True)
    for name in ("merge", "remove"):
        s = sub.add_parser(name)
        s.add_argument("--export", required=True, help="dtctl export of the logs routing object (JSON/YAML, - for stdin)")
        s.add_argument("--workspace-id", required=True)
        s.add_argument("--pipeline-id", required=True, help="objectId of the extension's k8s-otel-logs pipeline")
        s.add_argument("--out", required=True, help="file to write the object for dtctl diff/apply (- for stdout)")
    n = sub.add_parser("normalize", help="write an export in the apply shape, for `dtctl diff -f before -f merged`")
    n.add_argument("--export", required=True)
    n.add_argument("--out", required=True)
    c = sub.add_parser("check-unchanged", help="refuse when the tenant changed since the export that was merged")
    c.add_argument("--export", required=True)
    c.add_argument("--sha256", required=True)
    v = sub.add_parser("verify", help="compare the pre-merge export with a fresh export after apply")
    v.add_argument("--before", required=True)
    v.add_argument("--after", required=True)
    v.add_argument("--workspace-id", required=True)
    v.add_argument("--pipeline-id", required=True)
    v.add_argument("--expect-absent", action="store_true")
    args = p.parse_args(argv)

    try:
        if args.cmd in ("merge", "remove"):
            obj = load_export(args.export)
            entry = build_entry(args.workspace_id, args.pipeline_id)
            merged, action = (merge if args.cmd == "merge" else remove)(obj, entry, args.workspace_id)
            _write(merged, args.out)
            n_foreign = len(obj["value"]["routingEntries"]) - len(_ours(obj["value"]["routingEntries"], args.workspace_id))
            print(json.dumps({"action": action, "input_sha256": entries_digest(obj), "foreign_entries": n_foreign}),
                  file=sys.stderr)
            return 0
        if args.cmd == "normalize":
            obj = load_export(args.export)
            _write(_for_apply(obj, copy.deepcopy(obj["value"]["routingEntries"])), args.out)
            return 0
        if args.cmd == "check-unchanged":
            got = entries_digest(load_export(args.export))
            if got != args.sha256:
                raise ToolError(4, "logs routing changed since the export you merged; re-export and merge again")
            return 0
        entry = build_entry(args.workspace_id, args.pipeline_id)
        problems = verify(load_export(args.before), load_export(args.after), entry, args.workspace_id, not args.expect_absent)
        if problems:
            raise ToolError(5, "; ".join(problems))
        return 0
    except ToolError as err:
        print(f"error: {err}", file=sys.stderr)
        return err.code


if __name__ == "__main__":
    sys.exit(main())
