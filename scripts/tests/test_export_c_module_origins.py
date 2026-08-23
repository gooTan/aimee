#!/usr/bin/env python3
"""Focused stdlib tests for the explicit mixed-origin module repository policy."""

from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
EXPORTER_SOURCE = ROOT / "scripts" / "export_c_repositories.py"

sys.path.insert(0, str(ROOT / "scripts"))
SPEC = importlib.util.spec_from_file_location("export_c_repositories_under_test", EXPORTER_SOURCE)
EXPORTER = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(EXPORTER)


EXPECTED_OVERRIDES = {
    "config": "https://github.com/gooTan/aimee-module-config.git",
    "delegates": "https://github.com/gooTan/aimee-module-delegates.git",
    "git": "https://github.com/gooTan/aimee-module-git.git",
    "workflows": "https://github.com/gooTan/aimee-module-workflows.git",
    "roundtable": "https://github.com/gooTan/aimee-module-roundtable.git",
}


class ExportCModuleOriginsTest(unittest.TestCase):
    def test_override_set_is_exact(self) -> None:
        self.assertEqual(EXPORTER.MODULE_ORIGIN_OVERRIDES, EXPECTED_OVERRIDES)

    def test_module_remote_uses_overrides(self) -> None:
        for module_id, expected in EXPECTED_OVERRIDES.items():
            with self.subTest(module=module_id):
                self.assertEqual(EXPORTER.module_remote(module_id), expected)

    def test_module_remote_falls_back_to_rakuen(self) -> None:
        self.assertEqual(
            EXPORTER.module_remote("audit"),
            f"{EXPORTER.REMOTE_ROOT}/aimee-module-audit.git",
        )
        self.assertEqual(
            EXPORTER.module_remote("memory"),
            f"{EXPORTER.REMOTE_ROOT}/aimee-module-memory.git",
        )

    def test_core_remote_root_remains_rakuen(self) -> None:
        self.assertEqual(EXPORTER.REMOTE_ROOT, "https://github.com/RakuenSoftware")
        # Core repository itself must not be moved.
        self.assertEqual(f"{EXPORTER.REMOTE_ROOT}/aimee-core-c.git",
                         "https://github.com/RakuenSoftware/aimee-core-c.git")


if __name__ == "__main__":
    unittest.main()
