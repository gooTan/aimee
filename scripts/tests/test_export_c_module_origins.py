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


EXPECTED_OVERRIDES: dict[str, str] = {}


class ExportCModuleOriginsTest(unittest.TestCase):
    def test_override_set_is_exact(self) -> None:
        self.assertEqual(EXPORTER.MODULE_ORIGIN_OVERRIDES, EXPECTED_OVERRIDES)

    def test_module_remote_is_canonical_for_all_modules(self) -> None:
        # No per-module overrides: every module derives from the single canonical root.
        for module_id in ("config", "delegates", "git", "protocols", "workflows", "roundtable", "vault",
                          "audit", "memory", "sandbox", "economizer"):
            with self.subTest(module=module_id):
                self.assertEqual(
                    EXPORTER.module_remote(module_id),
                    f"{EXPORTER.REMOTE_ROOT}/aimee-module-{module_id}.git",
                )

    def test_core_and_module_root_is_gooTan(self) -> None:
        self.assertEqual(EXPORTER.REMOTE_ROOT, "https://github.com/gooTan")
        self.assertEqual(f"{EXPORTER.REMOTE_ROOT}/aimee-core-c.git",
                         "https://github.com/gooTan/aimee-core-c.git")

    def test_roundtable_shared_sources_include_preset_configs(self) -> None:
        expected = {
            "config/roundtables/default.json",
            "config/roundtables/plan.json",
            "config/roundtables/implementation.json",
            "config/roundtables/documentation.json",
        }
        roundtable_sources = set(EXPORTER.go_process_shared_sources("roundtable"))
        self.assertTrue(expected.issubset(roundtable_sources), roundtable_sources)
        self.assertEqual(len(expected & roundtable_sources), 4)
        for module_id in ("audit", "workflows", "delegates", "git", "config", "memory"):
            with self.subTest(module=module_id):
                self.assertTrue(
                    expected.isdisjoint(set(EXPORTER.go_process_shared_sources(module_id))),
                    f"{module_id} must not include roundtable preset configs",
                )


if __name__ == "__main__":
    unittest.main()
