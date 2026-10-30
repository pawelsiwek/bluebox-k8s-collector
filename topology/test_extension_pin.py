"""The vendored extension must be the pinned upstream release, byte for byte: fixes go upstream."""

from __future__ import annotations

import hashlib
import json
import re
import unittest
from pathlib import Path

EXT = Path(__file__).parent / "extension"


class ExtensionPinTest(unittest.TestCase):
    def test_vendored_files_match_the_upstream_pin(self) -> None:
        pin = json.loads((EXT / "UPSTREAM.json").read_text())
        on_disk = {str(p.relative_to(EXT)) for p in (EXT / "src").rglob("*") if p.is_file()}
        self.assertEqual(on_disk, set(pin["files"]), "vendored file set differs from UPSTREAM.json")
        for rel, digest in pin["files"].items():
            self.assertEqual(hashlib.sha256((EXT / rel).read_bytes()).hexdigest(), digest, f"{rel} was edited")

    def test_extension_yaml_version_matches_the_pin(self) -> None:
        pin = json.loads((EXT / "UPSTREAM.json").read_text())
        text = (EXT / "src" / "extension.yaml").read_text()
        self.assertRegex(text, rf"(?m)^name: {re.escape(pin['extension'])}$")
        self.assertRegex(text, rf"(?m)^version: {re.escape(pin['version'])}$")


if __name__ == "__main__":
    unittest.main()
