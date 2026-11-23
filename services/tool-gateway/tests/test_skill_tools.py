import inspect
import json
from pathlib import Path
from types import ModuleType
from typing import Any

import factory
import httpx
import pytest
from server import load_tools, public_functions, reporting_errors

TOOLS_FILE = Path(__file__).parent.parent / "tools" / "tools.py"

# The calls a fake connector service received: the URL path and the JSON body of each.
type Calls = list[tuple[str, dict[str, Any]]]


@pytest.fixture
def tools() -> ModuleType:
    module = load_tools(TOOLS_FILE)
    assert module is not None
    return module


def fake_connectors(answer: dict[str, object], calls: Calls) -> factory.Connectors:
    def handle(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        calls.append((request.url.path, body))
        return httpx.Response(200, json=answer)

    return factory.Connectors("http://connectors", transport=httpx.MockTransport(handle))


def test_skill_timeout_keeps_both_stream_tails(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    refusal = "Refused: the skill script timed out.\nstdout:\n" + "a" * 3000 + "\nstderr:\n" + "b" * 3000
    monkeypatch.setattr(tools, "connectors", fake_connectors({"ok": False, "error": refusal}, []))
    answer = reporting_errors(tools.run_skill_script)("pod", "agent@v1", "run", "v1", "git", "run.sh")
    assert answer.is_error is True
    assert answer.content[0].text == refusal


def test_skill_tools_have_system_arguments_and_forward_the_contract(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    calls: Calls = []
    monkeypatch.setattr(tools, "connectors", fake_connectors({"ok": True, "result": "text"}, calls))
    functions = {fn.__name__: fn for fn in public_functions(tools)}
    assert list(inspect.signature(functions["read_skill"]).parameters) == ["pod", "agent", "run", "version", "skill", "file"]
    assert list(inspect.signature(functions["run_skill_script"]).parameters) == [
        "pod",
        "agent",
        "run",
        "version",
        "skill",
        "script",
        "args",
        "timeout_seconds",
    ]
    assert tools.read_skill("", "course/builder@v2", "run-1", "v3", "git") == "text"
    tools.run_skill_script("pod1", "course/builder@v2", "run-1", "v3", "git", "release/scripts/tag.sh", ["1.4.0"], 300)
    assert calls == [
        (
            "/api/skills/tool",
            {
                "tool": "read_skill",
                "pod": "",
                "agent": "course/builder@v2",
                "run": "run-1",
                "version": "v3",
                "skill": "git",
                "args": {"file": "SKILL.md"},
            },
        ),
        (
            "/api/skills/tool",
            {
                "tool": "run_skill_script",
                "pod": "pod1",
                "agent": "course/builder@v2",
                "run": "run-1",
                "version": "v3",
                "skill": "git",
                "args": {"script": "release/scripts/tag.sh", "args": ["1.4.0"], "timeout_seconds": 300},
            },
        ),
    ]


def test_skill_refusal_reaches_the_model(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tools, "connectors", fake_connectors({"ok": False, "error": "Refused: the file is not part of this skill."}, []))
    with pytest.raises(ValueError, match="not part"):
        tools.read_skill("", "a@v1", "run-1", "v1", "git", "missing.txt")
