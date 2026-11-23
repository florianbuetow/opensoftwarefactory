import json
from pathlib import Path

from server import build_app, load_tools, public_functions
from starlette.testclient import TestClient

HEADERS = {
    "Content-Type": "application/json",
    "Accept": "application/json, text/event-stream",
    "MCP-Protocol-Version": "2025-06-18",
}

TOOLS = '''
def _helper(text: str) -> str:
    return text.upper()


def shout(text: str) -> str:
    """Return text in upper case."""
    return _helper(text)
'''


def write(tmp_path: Path, source: str) -> Path:
    path = tmp_path / "tools.py"
    path.write_text(source)
    return path


def rpc(client: TestClient, method: str, params: dict | None = None, id: int | None = 1):
    body = {"jsonrpc": "2.0", "method": method, "params": params or {}}
    if id is not None:
        body["id"] = id
    return client.post("/mcp", json=body, headers=HEADERS)


def test_loader_registers_public_functions_only(tmp_path):
    module = load_tools(write(tmp_path, "from pathlib import Path\n" + TOOLS))
    assert [fn.__name__ for fn in public_functions(module)] == ["shout"]


def test_syntax_error_still_serves_http(tmp_path):
    with TestClient(build_app(write(tmp_path, "def broken(:\n"))) as client:
        assert client.get("/healthz").status_code == 200
        assert rpc(client, "tools/list").json()["result"]["tools"] == []


def test_mcp_stateless_json(tmp_path):
    with TestClient(build_app(write(tmp_path, TOOLS))) as client:
        init = rpc(
            client,
            "initialize",
            {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "test", "version": "0"}},
        )
        assert init.status_code == 200
        assert init.headers["content-type"].startswith("application/json")
        assert init.json()["result"]["protocolVersion"] == "2025-06-18"

        assert rpc(client, "notifications/initialized", id=None).status_code == 202

        listed = rpc(client, "tools/list", id=2).json()
        assert [t["name"] for t in listed["result"]["tools"]] == ["shout"]
        tool = listed["result"]["tools"][0]
        assert tool["description"] == "Return text in upper case."
        assert tool["inputSchema"]["required"] == ["text"]

        called = rpc(client, "tools/call", {"name": "shout", "arguments": {"text": "hi"}}, id=3).json()
        assert called["id"] == 3
        assert called["result"]["isError"] is False
        assert called["result"]["content"] == [{"type": "text", "text": "HI"}]


FAILING_TOOLS = '''
def refuse(member: str) -> dict:
    """Refuse the call."""
    raise ValueError(f"only the lead can finish the team, not {member!r}: send your result to the lead")


async def refuse_later(member: str) -> str:
    """Refuse the call from a coroutine."""
    raise ValueError("not now")


def crash(member: str) -> str:
    """Fail unexpectedly."""
    raise RuntimeError("object 'runs/1/x': HTTP 500\\nTraceback (most recent call last): secret")


def works(member: str) -> dict:
    """Return a value."""
    return {"member": member}
'''


def test_a_failed_tool_call_returns_the_message_the_tool_raised(tmp_path, capsys):
    with TestClient(build_app(write(tmp_path, FAILING_TOOLS))) as client:

        def call(name: str, arguments: dict) -> dict:
            return rpc(client, "tools/call", {"name": name, "arguments": arguments}).json()["result"]

        refused = call("refuse", {"member": "coder"})
        assert refused["isError"] is True
        assert refused["content"] == [
            {"type": "text", "text": "only the lead can finish the team, not 'coder': send your result to the lead"}
        ]
        assert call("refuse_later", {"member": "coder"})["content"][0]["text"] == "not now"

        # An unexpected exception gives its type and first line, and no traceback.
        crashed = call("crash", {"member": "coder"})
        assert crashed["isError"] is True
        assert crashed["content"] == [{"type": "text", "text": "RuntimeError: object 'runs/1/x': HTTP 500"}]
        assert "tool failed" in capsys.readouterr().out

        # Arguments that do not match the schema name the argument.
        invalid = call("refuse", {})
        assert invalid["isError"] is True and "member" in invalid["content"][0]["text"]

        # A wrapped tool keeps its schema and its result.
        listed = rpc(client, "tools/list").json()["result"]["tools"]
        assert [tool["inputSchema"]["required"] for tool in listed] == [["member"]] * 4
        ok = call("works", {"member": "lead"})
        assert ok["isError"] is False and json.loads(ok["content"][0]["text"]) == {"member": "lead"}
