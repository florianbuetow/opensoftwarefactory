import inspect
import json
from pathlib import Path

import factory
import httpx
import pytest
from server import build_app, load_tools, public_functions
from starlette.testclient import TestClient

TOOLS_FILE = Path(__file__).parent.parent / "tools" / "tools.py"
DEVPOD_TOOLS = [
    "list_files",
    "read_file",
    "write_file",
    "edit_file",
    "search",
    "run_command",
    "report_blocked",
    "push_branch",
    "create_pull_request",
    "update_pull_request",
    "get_pull_request",
    "comment_on_pull_request",
]
WHO = ("pod1", "course/builder@v1", "run-1")


@pytest.fixture
def tools():
    return load_tools(TOOLS_FILE)


def fake_connectors(answer, calls: list) -> factory.Connectors:
    """A connector service that records every call and answers with answer(body), or with answer itself."""

    def handle(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        calls.append((request.url.path, body))
        result = answer(body) if callable(answer) else answer
        if isinstance(result, httpx.Response):
            return result
        return httpx.Response(200, json=result)

    return factory.Connectors("http://connectors", transport=httpx.MockTransport(handle))


def test_every_dev_pod_tool_exists_with_the_identity_arguments_first(tools):
    functions = {fn.__name__: fn for fn in public_functions(tools)}
    for name in DEVPOD_TOOLS:
        assert name in functions, name
        parameters = list(inspect.signature(functions[name]).parameters)
        assert parameters[:3] == ["pod", "agent", "run"], name
        assert "pod, agent and run are filled in by the system" in functions[name].__doc__, name
        assert "Example:" in functions[name].__doc__, name


def test_each_tool_forwards_its_arguments_with_pod_agent_and_run(tools, monkeypatch):
    calls: list = []
    monkeypatch.setattr(tools, "connectors", fake_connectors({"ok": True, "result": {"done": True}}, calls))
    tools.list_files(*WHO)
    tools.list_files(*WHO, path="src")
    tools.read_file(*WHO, path="src/panel.py")
    tools.read_file(*WHO, path="src/panel.py", start_line=3, end_line=9)
    tools.write_file(*WHO, path="a.txt", content="hello")
    tools.edit_file(*WHO, path="a.txt", old_text="hello", new_text="bye")
    tools.search(*WHO, pattern="pay")
    tools.search(*WHO, pattern="pay", path="src")
    tools.run_command(*WHO, command="git status")
    tools.run_command(*WHO, command="./verify.sh", timeout_seconds=300)
    tools.report_blocked(*WHO, reason="The test service is down.")
    tools.push_branch(*WHO)
    tools.create_pull_request(*WHO, title="T", body="B")
    tools.update_pull_request(*WHO, body="B2")
    tools.get_pull_request(*WHO)
    tools.comment_on_pull_request(*WHO, body="Checks pass.")
    assert {path for path, _ in calls} == {"/api/devpods/pod1/tool"}
    assert all(body["agent"] == "course/builder@v1" and body["run"] == "run-1" for _, body in calls)
    assert [(body["tool"], body["args"]) for _, body in calls] == [
        ("list_files", {"path": "."}),
        ("list_files", {"path": "src"}),
        ("read_file", {"path": "src/panel.py"}),
        ("read_file", {"path": "src/panel.py", "start_line": 3, "end_line": 9}),
        ("write_file", {"path": "a.txt", "content": "hello"}),
        ("edit_file", {"path": "a.txt", "old_text": "hello", "new_text": "bye"}),
        ("search", {"pattern": "pay"}),
        ("search", {"pattern": "pay", "path": "src"}),
        ("run_command", {"command": "git status"}),
        ("run_command", {"command": "./verify.sh", "timeout_seconds": 300}),
        ("report_blocked", {"reason": "The test service is down."}),
        ("push_branch", {}),
        ("create_pull_request", {"title": "T", "body": "B"}),
        ("update_pull_request", {"body": "B2"}),
        ("get_pull_request", {}),
        ("comment_on_pull_request", {"body": "Checks pass."}),
    ]


def test_results_are_returned_as_the_connector_service_gave_them(tools, monkeypatch):
    results = {
        "list_files": {"path": ".", "entries": [{"name": "src", "type": "dir"}], "truncated": False},
        "run_command": {"exit_code": 1, "stdout": "", "stderr": "FAILED"},
        "write_file": {"path": "a.txt", "created": True, "bytes": 5},
        "report_blocked": "Recorded. The work ends here as blocked with your reason. Do not call any more tools.",
        "push_branch": "Recorded. A person must approve before this is published.",
        "create_pull_request": {"pull_request": "runs/run-1/pull-request.json", "commits": 1},
    }
    monkeypatch.setattr(
        tools, "connectors", fake_connectors(lambda body: {"ok": True, "result": results[body["tool"]]}, [])
    )
    assert tools.list_files(*WHO) == results["list_files"]
    # A command that fails is a result, not an error: the agent reads the exit code.
    assert tools.run_command(*WHO, command="./verify.sh") == results["run_command"]
    assert tools.write_file(*WHO, path="a.txt", content="hello") == results["write_file"]
    assert tools.report_blocked(*WHO, reason="Down.") == results["report_blocked"]
    assert tools.push_branch(*WHO) == "Recorded. A person must approve before this is published."
    assert json.loads(tools.create_pull_request(*WHO, title="T", body="B")) == results["create_pull_request"]


def test_read_file_returns_the_text_without_line_numbers(tools, monkeypatch):
    files = {
        "a.py": {"path": "a.py", "content": "one\ntwo\n", "start_line": 1, "end_line": 2, "total_lines": 2},
        "empty.txt": {"path": "empty.txt", "content": "", "start_line": 0, "end_line": 0, "total_lines": 0},
        "long.txt": {"path": "long.txt", "content": "x" * 10, "total_lines": 1, "note": "The content was cut."},
    }
    monkeypatch.setattr(
        tools, "connectors", fake_connectors(lambda body: {"ok": True, "result": files[body["args"]["path"]]}, [])
    )
    assert tools.read_file(*WHO, path="a.py") == "one\ntwo\n"
    assert tools.read_file(*WHO, path="empty.txt") == "empty.txt is empty."
    assert tools.read_file(*WHO, path="long.txt") == "xxxxxxxxxx\n[The content was cut.]"


def test_a_refusal_is_raised_with_the_message_for_the_agent(tools, monkeypatch):
    calls: list = []
    refusal = 'Refused: the command "rm -rf tests" is not one of this agent\'s allowed commands. Run an allowed command.'
    monkeypatch.setattr(tools, "connectors", fake_connectors({"ok": False, "error": refusal}, calls))
    with pytest.raises(ValueError) as raised:
        tools.run_command(*WHO, command="rm -rf tests")
    assert str(raised.value) == refusal
    for call in (
        lambda: tools.write_file(*WHO, path="tests/a.py", content="x"),
        lambda: tools.edit_file(*WHO, path="tests/a.py", old_text="a", new_text="b"),
        lambda: tools.push_branch(*WHO),
        lambda: tools.report_blocked(*WHO, reason=""),
    ):
        with pytest.raises(ValueError, match="Refused"):
            call()


def test_a_run_without_a_dev_pod_is_told_so_and_nothing_is_sent(tools, monkeypatch):
    calls: list = []
    monkeypatch.setattr(tools, "connectors", fake_connectors({"ok": True, "result": "x"}, calls))
    for pod in ("", "../other", "a/b"):
        with pytest.raises(ValueError, match="this run has no dev pod"):
            tools.read_file(pod, "course/builder@v1", "run-1", path="a.py")
    assert calls == []


def test_a_connector_service_that_fails_is_an_error_not_a_refusal(tools, monkeypatch):
    monkeypatch.setattr(tools, "connectors", fake_connectors(httpx.Response(503, text="still starting"), []))
    with pytest.raises(RuntimeError, match="HTTP 503: still starting"):
        tools.list_files(*WHO)


def test_a_long_refusal_reaches_the_model_whole(monkeypatch):
    refusal = "Refused: the command joins several commands. " + "Allowed: git status*. " * 40 + "Call report_blocked."
    assert 500 < len(refusal) < 2000
    monkeypatch.setattr(factory, "connectors", fake_connectors({"ok": False, "error": refusal}, []))
    headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    with TestClient(build_app(TOOLS_FILE)) as client:
        client.post(
            "/mcp",
            headers=headers,
            json={
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "t", "version": "0"}},
            },
        )
        listed = client.post("/mcp", headers=headers, json={"jsonrpc": "2.0", "id": 2, "method": "tools/list"}).json()
        by_name = {tool["name"]: tool for tool in listed["result"]["tools"]}
        assert set(DEVPOD_TOOLS) <= set(by_name)
        assert by_name["run_command"]["inputSchema"]["required"] == ["pod", "agent", "run", "command"]
        called = client.post(
            "/mcp",
            headers=headers,
            json={
                "jsonrpc": "2.0",
                "id": 3,
                "method": "tools/call",
                "params": {
                    "name": "run_command",
                    "arguments": {"pod": "pod1", "agent": "course/builder@v1", "run": "run-1", "command": "a; b"},
                },
            },
        ).json()["result"]
    assert called["isError"] is True
    assert called["content"][0]["text"] == refusal
