"""PyTestArch smoke test for the Factory's Python services."""

from pathlib import Path

import pytest
from pytestarch import EvaluableArchitecture, get_evaluable_architecture


@pytest.fixture(scope="module")
def evaluable() -> EvaluableArchitecture:
    """Build the Python services import graph."""
    services = Path(__file__).resolve().parents[3] / "services"
    return get_evaluable_architecture(
        str(services),
        str(services),
        ("*__pycache__", "*.venv*"),
    )


def test_evaluable_is_configured(evaluable: EvaluableArchitecture) -> None:
    """Verify that PyTestArch can scan the Factory's Python services."""
    assert evaluable is not None
