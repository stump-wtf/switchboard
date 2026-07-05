"""Scaffold smoke tests for the docs-only bootstrap.

There is no application code yet (it is written fresh from the ADRs + specs in the follow-up
session). These tests assert the scaffold is present and the package imports, which keeps CI green
from day one (ADR-006) and guards against a spec/ADR file going missing. They are replaced/expanded
by real behavioral tests in the code session (signature verification, persistence, MCP responses,
SSE emission — brief §9).
"""

from pathlib import Path

import app

REPO_ROOT = Path(__file__).resolve().parent.parent


def test_app_package_imports() -> None:
    assert app.__doc__


def test_all_adrs_present() -> None:
    adrs = sorted((REPO_ROOT / "docs" / "adr").glob("ADR-*.md"))
    # ADR-000 through ADR-006.
    assert len(adrs) >= 7


def test_specs_present() -> None:
    specs = REPO_ROOT / "docs" / "specs"
    assert (specs / "openapi.yaml").is_file()
    assert (specs / "asyncapi.yaml").is_file()
    assert (specs / "mcp-tools.md").is_file()


def test_no_app_implementation_yet() -> None:
    """Guard the brief's boundary: app/ holds only package placeholders this session."""
    py_files = {p.name for p in (REPO_ROOT / "app").rglob("*.py")}
    assert py_files == {"__init__.py"}
