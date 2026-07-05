# webhook-mcp — local dev entry points.
# `make ci` runs the same lint/type/security/test gate CI does (ADR-006), so you can reproduce the
# gate before opening a PR. (Gitleaks + Semgrep run in CI only — they need Docker / an extra install.)

.PHONY: install fmt lint types security test ci

install:  ## Install the package + dev tooling (editable)
	python -m pip install --upgrade pip
	pip install -e '.[dev]'
	pre-commit install || true

fmt:  ## Auto-format
	ruff format .
	ruff check --fix .

lint:  ## Lint + format check
	ruff check .
	ruff format --check .

types:  ## Type-check
	mypy app

security:  ## Static security + dependency audit
	bandit -c pyproject.toml -r app
	pip-audit --skip-editable

test:  ## Run the test suite
	pytest

ci: lint types security test  ## The full local gate (mirrors CI)
