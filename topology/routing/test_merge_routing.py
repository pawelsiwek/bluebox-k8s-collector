"""Tests for merge_routing.py against a recorded logs-routing export with foreign entries.

Run: cd topology/routing && python3 -m unittest -v test_merge_routing
"""

from __future__ import annotations

import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

import merge_routing as mr

HERE = Path(__file__).parent
EXPORT_JSON = HERE / "testdata" / "logs-routing.export.json"
EXPORT_YAML = HERE / "testdata" / "logs-routing.export.yaml"
WS = "0f6c2a8e-1b2c-4d5e-8f90-a1b2c3d4e5f6"
OTHER_WS = "11111111-2222-4333-8444-555555555555"
PIPELINE = "K8S-OTEL-LOGS-PIPELINE-OBJECT-ID"


def run(*argv: str) -> tuple[int, str]:
    err = io.StringIO()
    with contextlib.redirect_stderr(err):
        code = mr.main(list(argv))
    return code, err.getvalue()


class MergeTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.original = mr.load_export(str(EXPORT_JSON))
        self.foreign = self.original["value"]["routingEntries"]

    def merge_file(self, export: Path, name: str = "merged.json") -> tuple[int, dict, str]:
        out = self.tmp / name
        code, err = run("merge", "--export", str(export), "--workspace-id", WS, "--pipeline-id", PIPELINE, "--out", str(out))
        return code, (json.loads(out.read_text()) if code == 0 else {}), err

    def test_inserts_first_and_keeps_foreign_entries_byte_identical_in_order(self) -> None:
        code, merged, err = self.merge_file(EXPORT_JSON)
        self.assertEqual(code, 0, err)
        entries = merged["value"]["routingEntries"]
        self.assertTrue(entries[0]["description"].startswith(mr.marker(WS)))
        self.assertEqual(entries[0]["pipelineId"], PIPELINE)
        self.assertEqual(entries[1:], self.foreign)
        self.assertEqual(merged["objectId"], "ROUTING-OBJECT-ID-REDACTED")
        self.assertEqual(merged["schemaId"], mr.SCHEMA_ID)
        self.assertEqual(json.loads(err)["action"], "inserted")
        self.assertEqual(json.loads(err)["foreign_entries"], 3)

    def test_output_is_a_single_settings_object_dtctl_apply_updates_in_place(self) -> None:
        # dtctl apply detects a settings object by schemaId + scope + value and updates by objectId;
        # a list or a "type" field would route it elsewhere or apply every element.
        _, merged, _ = self.merge_file(EXPORT_JSON)
        self.assertIsInstance(merged, dict)
        self.assertEqual(set(merged), {"objectId", "schemaId", "scope", "value"})
        out = self.tmp / "norm.json"
        self.assertEqual(run("normalize", "--export", str(EXPORT_JSON), "--out", str(out))[0], 0)
        norm = json.loads(out.read_text())
        self.assertEqual({k: v for k, v in norm.items() if k != "value"}, {k: v for k, v in merged.items() if k != "value"})
        self.assertEqual(norm["value"]["routingEntries"], merged["value"]["routingEntries"][1:])

    def test_rerun_on_the_applied_state_is_a_no_op(self) -> None:
        _, first, _ = self.merge_file(EXPORT_JSON, "first.json")
        code, second, err = self.merge_file(self.tmp / "first.json", "second.json")
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(err)["action"], "unchanged")
        self.assertEqual(second, first)

    def test_read_back_with_null_extra_keys_is_still_ours(self) -> None:
        _, first, _ = self.merge_file(EXPORT_JSON, "first.json")
        first["value"]["routingEntries"][0]["builtinPipelineId"] = None
        (self.tmp / "readback.json").write_text(json.dumps(first))
        code, _, err = self.merge_file(self.tmp / "readback.json", "again.json")
        self.assertEqual(code, 0, err)

    def test_refuses_when_our_marker_exists_with_different_content(self) -> None:
        _, first, _ = self.merge_file(EXPORT_JSON, "first.json")
        first["value"]["routingEntries"][0]["matcher"] = "true"
        (self.tmp / "edited.json").write_text(json.dumps(first))
        code, _, err = self.merge_file(self.tmp / "edited.json", "never.json")
        self.assertEqual(code, 3, err)
        self.assertFalse((self.tmp / "never.json").exists())

    def test_other_workspace_entry_is_foreign(self) -> None:
        other = mr.build_entry(OTHER_WS, PIPELINE)
        obj = json.loads(EXPORT_JSON.read_text())
        obj[0]["value"]["routingEntries"].insert(0, other)
        (self.tmp / "two.json").write_text(json.dumps(obj))
        code, merged, err = self.merge_file(self.tmp / "two.json")
        self.assertEqual(code, 0, err)
        self.assertEqual(merged["value"]["routingEntries"][1], other)

    def test_yaml_export_merges_like_json(self) -> None:
        try:
            import yaml  # noqa: F401
        except ImportError:
            self.skipTest("PyYAML not installed")
        _, from_json, _ = self.merge_file(EXPORT_JSON, "a.json")
        code, from_yaml, err = self.merge_file(EXPORT_YAML, "b.json")
        self.assertEqual(code, 0, err)
        self.assertEqual(from_yaml, from_json)

    def test_rejects_wrong_schema_and_bad_ids(self) -> None:
        obj = json.loads(EXPORT_JSON.read_text())
        obj[0]["schemaid"] = "builtin:openpipeline.metrics.routing"
        (self.tmp / "metrics.json").write_text(json.dumps(obj))
        self.assertEqual(self.merge_file(self.tmp / "metrics.json")[0], 2)
        code, _ = run("merge", "--export", str(EXPORT_JSON), "--workspace-id", "not-a-uuid",
                      "--pipeline-id", PIPELINE, "--out", str(self.tmp / "x.json"))
        self.assertEqual(code, 2)
        code, _ = run("merge", "--export", str(EXPORT_JSON), "--workspace-id", WS,
                      "--pipeline-id", "<resolve me>", "--out", str(self.tmp / "x.json"))
        self.assertEqual(code, 2)

    def test_check_unchanged_detects_a_concurrent_edit(self) -> None:
        _, _, err = self.merge_file(EXPORT_JSON)
        digest = json.loads(err)["input_sha256"]
        self.assertEqual(run("check-unchanged", "--export", str(EXPORT_JSON), "--sha256", digest)[0], 0)
        obj = json.loads(EXPORT_JSON.read_text())
        obj[0]["value"]["routingEntries"][1]["enabled"] = False
        (self.tmp / "raced.json").write_text(json.dumps(obj))
        self.assertEqual(run("check-unchanged", "--export", str(self.tmp / "raced.json"), "--sha256", digest)[0], 4)

    def test_verify_catches_dropped_or_reordered_foreign_entries(self) -> None:
        _, merged, _ = self.merge_file(EXPORT_JSON)
        ok = self.tmp / "ok.json"
        ok.write_text(json.dumps(merged))
        args = ("--before", str(EXPORT_JSON), "--workspace-id", WS, "--pipeline-id", PIPELINE)
        self.assertEqual(run("verify", *args, "--after", str(ok))[0], 0)
        swapped = json.loads(json.dumps(merged))
        e = swapped["value"]["routingEntries"]
        e[1], e[2] = e[2], e[1]
        (self.tmp / "swapped.json").write_text(json.dumps(swapped))
        self.assertEqual(run("verify", *args, "--after", str(self.tmp / "swapped.json"))[0], 5)
        dropped = json.loads(json.dumps(merged))
        del dropped["value"]["routingEntries"][3]
        (self.tmp / "dropped.json").write_text(json.dumps(dropped))
        self.assertEqual(run("verify", *args, "--after", str(self.tmp / "dropped.json"))[0], 5)

    def test_remove_restores_the_original_entries(self) -> None:
        _, merged, _ = self.merge_file(EXPORT_JSON)
        (self.tmp / "applied.json").write_text(json.dumps(merged))
        out = self.tmp / "removed.json"
        code, err = run("remove", "--export", str(self.tmp / "applied.json"), "--workspace-id", WS,
                        "--pipeline-id", PIPELINE, "--out", str(out))
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(out.read_text())["value"]["routingEntries"], self.foreign)
        code, err = run("remove", "--export", str(out), "--workspace-id", WS, "--pipeline-id", PIPELINE,
                        "--out", str(self.tmp / "again.json"))
        self.assertEqual((code, json.loads(err)["action"]), (0, "unchanged"))


if __name__ == "__main__":
    unittest.main()
