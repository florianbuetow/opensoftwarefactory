import json
from pathlib import Path
from types import ModuleType

import factory
import httpx
import pytest
from server import load_tools

TOOLS_FILE = Path(__file__).parent.parent / "tools" / "tools.py"

# What a fake blobstore keeps of each PUT: the content, the creator and the trace ID, by object ID.
type Stored = dict[str, tuple[str, str, str]]


@pytest.fixture
def tools() -> ModuleType:
    module = load_tools(TOOLS_FILE)
    assert module is not None
    return module


def fake_blobstore(objects: dict[str, str], stored: Stored | None = None) -> factory.Blobstore:
    def handle(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if request.method == "PUT" and path.startswith("/blobs/"):
            assert stored is not None
            stored[path.removeprefix("/blobs/")] = (
                request.content.decode(),
                request.headers["X-Creator"],
                request.headers["X-Trace-Id"],
            )
            return httpx.Response(204)
        if path.startswith("/blobs/") and path.removeprefix("/blobs/") in objects:
            return httpx.Response(200, text=objects[path.removeprefix("/blobs/")])
        if path == "/list/runs":
            return httpx.Response(200, json={"prefix": "runs", "directories": ["42"], "objects": []})
        return httpx.Response(404, text="object not found")

    return factory.Blobstore("http://blobstore", transport=httpx.MockTransport(handle))


def test_assess_short_text(tools: ModuleType) -> None:
    result = tools._assess("Add a button. It must be blue.")
    assert result == {
        "words": 7,
        "sentences": 2,
        "requirements": 1,
        "components": 0,
        "score": 1,
        "reasons": ["short text with few requirements and components"],
    }


def test_assess_complex_text(tools: ModuleType) -> None:
    names = "Alpha Bravo Charlie Delta Echo Foxtrot Golf Hotel India Juliet"
    lines = [f"The service must call {name} and should log it." for name in names.split()]
    text = "\n".join(lines) + "\n" + "word " * 300
    result = tools._assess(text)
    assert result["requirements"] == 10
    assert result["components"] == 10
    assert result["words"] > 300
    assert result["score"] == 4
    assert len(result["reasons"]) == 3


def test_assess_ignores_sentence_initial_capitals(tools: ModuleType) -> None:
    assert tools._assess("The API needs Redis. Then it shall stop.")["components"] == 2


def test_analyze_task_complexity_reads_artifact(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tools, "blobstore", fake_blobstore({"runs/1/task.md": "We need a Kafka consumer."}))
    result = tools.analyze_task_complexity("runs/1/task.md")
    assert result["object_id"] == "runs/1/task.md"
    assert result["requirements"] == 1
    assert result["components"] == 1


def test_artifact_round_trip(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    stored: Stored = {}
    monkeypatch.setattr(tools, "blobstore", fake_blobstore({"a/b": "hello"}, stored))
    assert tools.get_artifact("a/b") == "hello"
    assert tools.put_artifact("a/c", "text", "agent", "t1") == "a/c"
    assert stored == {"a/c": ("text", "agent", "t1")}
    assert tools.list_artifacts("runs/")["directories"] == ["42"]
    with pytest.raises(factory.NotFoundError):
        tools.get_artifact("missing")


@pytest.mark.parametrize("object_id", ["", "../x", "a//b", ".hidden", "a/b c"])
def test_invalid_object_ids_are_rejected(object_id: str) -> None:
    with pytest.raises(ValueError):
        fake_blobstore({}).get(object_id)


def test_get_policy(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    def handle(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/policies/coding/latest":
            body = {"name": "coding", "version": "v2", "created": "", "creator": "", "files": {"policy.md": "# Rules"}}
            return httpx.Response(200, content=json.dumps(body))
        return httpx.Response(404)

    registry = factory.PolicyRegistry("http://registry", transport=httpx.MockTransport(handle))
    monkeypatch.setattr(tools, "policies", registry)
    assert tools.get_policy("coding") == "# Rules"
    assert "does not exist" in tools.get_policy("other", "v1")


SESSION = "user-sessions/t1/step-a"


def fake_session_store(files: dict[str, str], stored: Stored | None = None, session: str = SESSION) -> factory.Blobstore:
    prefix = f"{session}/files"

    def handle(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if request.method == "PUT" and path.startswith(f"/blobs/{prefix}/"):
            assert stored is not None
            stored[path.removeprefix("/blobs/")] = (
                request.content.decode(),
                request.headers["X-Creator"],
                request.headers["X-Trace-Id"],
            )
            return httpx.Response(204)
        if path.startswith(f"/blobs/{prefix}/") and path.removeprefix(f"/blobs/{prefix}/") in files:
            return httpx.Response(200, text=files[path.removeprefix(f"/blobs/{prefix}/")])
        if path == f"/list/{prefix}" and files:
            objects = [{"id": f"{prefix}/{name}", "size": len(text.encode())} for name, text in files.items()]
            return httpx.Response(200, json={"prefix": prefix, "directories": [], "objects": objects})
        return httpx.Response(404, text="object not found")

    return factory.Blobstore("http://blobstore", transport=httpx.MockTransport(handle))


def test_list_session_files(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tools, "blobstore", fake_session_store({"notes.md": "héllo", "data.json": "{}"}))
    assert tools.list_session_files(SESSION) == {
        "session": SESSION,
        "files": [{"name": "notes.md", "size": 6}, {"name": "data.json", "size": 2}],
    }


def test_list_session_files_empty_session(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tools, "blobstore", fake_session_store({}))
    assert tools.list_session_files(SESSION) == {"session": SESSION, "files": []}


def test_read_session_file_numbers_lines(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tools, "blobstore", fake_session_store({"notes.md": "one\ntwo\nthree\nfour\n"}))
    assert tools.read_session_file(SESSION, "notes.md") == "     1\tone\n     2\ttwo\n     3\tthree\n     4\tfour"
    assert tools.read_session_file(SESSION, "notes.md", 2, 3) == "     2\ttwo\n     3\tthree"
    assert tools.read_session_file(SESSION, "notes.md", 3) == "     3\tthree\n     4\tfour"
    assert tools.read_session_file(SESSION, "notes.md", 0, 1) == "     1\tone"
    assert tools.read_session_file(SESSION, "notes.md", 4, 99) == "     4\tfour"


def test_read_session_file_range_outside_file(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tools, "blobstore", fake_session_store({"notes.md": "one\ntwo\n", "empty.txt": ""}))
    assert tools.read_session_file(SESSION, "notes.md", 3) == "Line 3 is outside notes.md, which has 2 lines."
    assert "end_line 1 is before start_line 2" in tools.read_session_file(SESSION, "notes.md", 2, 1)
    assert "must be 0 or greater" in tools.read_session_file(SESSION, "notes.md", -1)
    assert tools.read_session_file(SESSION, "empty.txt") == "empty.txt is empty."
    with pytest.raises(factory.NotFoundError):
        tools.read_session_file(SESSION, "missing.md")


def test_search_session_files(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    files = {"notes.md": "Alpha\nbeta 12\n", "data.json": '{"beta": 7}\n'}
    monkeypatch.setattr(tools, "blobstore", fake_session_store(files))
    assert tools.search_session_files(SESSION, r"beta\W+\d") == {
        "matches": [
            {"file": "notes.md", "line": 2, "text": "beta 12"},
            {"file": "data.json", "line": 1, "text": '{"beta": 7}'},
        ],
        "truncated": False,
    }
    assert tools.search_session_files(SESSION, "(?i)ALPHA", "notes.md")["matches"] == [{"file": "notes.md", "line": 1, "text": "Alpha"}]
    assert tools.search_session_files(SESSION, "beta", "data.json")["matches"][0]["file"] == "data.json"
    assert tools.search_session_files(SESSION, "nothing") == {"matches": [], "truncated": False}


def test_search_session_files_caps_matches(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tools, "blobstore", fake_session_store({"a.txt": "x\n" * 200, "b.txt": "x\n"}))
    exact = tools.search_session_files(SESSION, "x", "a.txt")
    assert len(exact["matches"]) == 200
    assert exact["truncated"] is False
    capped = tools.search_session_files(SESSION, "x")
    assert len(capped["matches"]) == 200
    assert capped["truncated"] is True
    assert capped["matches"][-1] == {"file": "a.txt", "line": 200, "text": "x"}


def test_search_session_files_invalid_regex(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tools, "blobstore", fake_session_store({"a.txt": "x"}))
    with pytest.raises(ValueError, match="invalid regular expression"):
        tools.search_session_files(SESSION, "(unclosed")


@pytest.mark.parametrize("pattern", ["a{99999999999}", "(" * 5000 + ")" * 5000])
def test_search_session_files_rejects_patterns_that_do_not_compile(
    tools: ModuleType, monkeypatch: pytest.MonkeyPatch, pattern: str
) -> None:
    monkeypatch.setattr(tools, "blobstore", fake_session_store({"a.txt": "x"}))
    with pytest.raises(ValueError, match="invalid regular expression"):
        tools.search_session_files(SESSION, pattern)


def test_write_session_file(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    stored: Stored = {}
    session = "user-sessions/t1/route/step-a"
    monkeypatch.setattr(tools, "blobstore", fake_session_store({}, stored, session))
    assert tools.write_session_file(session, "plan.md", "# Plan") == f"{session}/files/plan.md"
    assert stored == {f"{session}/files/plan.md": ("# Plan", "tool-gateway", "t1")}


@pytest.mark.parametrize(
    "session",
    [
        "",
        "user-sessions",
        "user-sessions/t1",
        "user-sessions/t1/",
        "/user-sessions/t1/step-a",
        "user-sessions/t1/step-a/",
        "user-sessions//step-a",
        "user-sessions/../runs/step-a",
        "user-sessions/t1/..",
        "runs/t1/step-a",
        "user-sessions-x/t1/step-a",
        "user-sessions/t1/step a",
        "user-sessions/t1/step-a\n",
        "user-sessions/t1\n/step-a",
    ],
)
def test_session_tools_reject_invalid_session(tools: ModuleType, monkeypatch: pytest.MonkeyPatch, session: str) -> None:
    stored: Stored = {}
    monkeypatch.setattr(tools, "blobstore", fake_session_store({"a.md": "x"}, stored))
    for call in (
        lambda: tools.list_session_files(session),
        lambda: tools.read_session_file(session, "a.md"),
        lambda: tools.search_session_files(session, "x"),
        lambda: tools.search_session_files(session, "x", "a.md"),
        lambda: tools.write_session_file(session, "a.md", "x"),
    ):
        with pytest.raises(ValueError, match="invalid session"):
            call()
    assert stored == {}


@pytest.mark.parametrize(
    "name",
    ["", "a", "a.yaml", "a.png", ".hidden.md", "-a.md", "../a.md", "sub/a.md", "a b.md", "a.md/", "a.MD", "a" * 101 + ".md", "a.md\n"],
)
def test_session_tools_reject_invalid_file_name(tools: ModuleType, monkeypatch: pytest.MonkeyPatch, name: str) -> None:
    stored: Stored = {}
    monkeypatch.setattr(tools, "blobstore", fake_session_store({"a.md": "x"}, stored))
    with pytest.raises(ValueError, match="invalid file name"):
        tools.read_session_file(SESSION, name)
    with pytest.raises(ValueError, match="invalid file name"):
        tools.write_session_file(SESSION, name, "x")
    if name:
        with pytest.raises(ValueError, match="invalid file name"):
            tools.search_session_files(SESSION, "x", name)
    assert stored == {}


def test_search_session_files_stops_a_runaway_pattern(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tools, "blobstore", fake_session_store({"a.txt": "a" * 60 + "b\n"}))
    monkeypatch.setattr(tools, "SEARCH_SECONDS", 0.5)
    with pytest.raises(ValueError, match="took too long"):
        tools.search_session_files(SESSION, "(a+)+$")
    # A normal search still works afterwards.
    assert tools.search_session_files(SESSION, "b$")["matches"][0]["line"] == 1


TEAM = "teams/t1/build"
TEAM_INFO: dict[str, object] = {
    "lead": "lead",
    "members": [
        {"name": "lead", "kind": "agent", "ref": "demo/lead@v1", "description": "Leads the team."},
        {"name": "coder", "kind": "agent", "ref": "demo/coder@v1", "description": "Writes the code."},
        {"name": "tester", "kind": "agent", "ref": "demo/tester@v1", "description": "Writes the tests."},
    ],
    "channels": [
        {"id": "coder--lead", "members": ["coder", "lead"]},
        {"id": "lead--tester", "members": ["lead", "tester"]},
        {"id": "lead--workflow", "members": ["lead", "workflow"]},
    ],
    "trace_id": "t1",
    "step_id": "build",
}


class TeamStore(factory.Blobstore):
    """A blobstore that also records the creator and the trace ID of each PUT, by object ID."""

    creators: dict[str, tuple[str, str]]


def fake_team_store(objects: dict[str, object], team: str = TEAM) -> tuple[TeamStore, dict[str, str]]:
    """A blobstore holding one team; objects maps paths below the team to text or JSON values."""
    blobs = {
        f"{team}/{path}": value if isinstance(value, str) else json.dumps(value)
        for path, value in {"team.json": TEAM_INFO, **objects}.items()
    }
    creators: dict[str, tuple[str, str]] = {}

    def handle(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if request.method == "PUT" and path.startswith("/blobs/"):
            blobs[path.removeprefix("/blobs/")] = request.content.decode()
            creators[path.removeprefix("/blobs/")] = (request.headers["X-Creator"], request.headers["X-Trace-Id"])
            return httpx.Response(204)
        if path.startswith("/blobs/") and path.removeprefix("/blobs/") in blobs:
            return httpx.Response(200, text=blobs[path.removeprefix("/blobs/")])
        if path.startswith("/list/"):
            prefix = path.removeprefix("/list/")
            found = [{"id": key, "size": len(text.encode())} for key, text in blobs.items() if key.startswith(prefix + "/")]
            if found:
                return httpx.Response(200, json={"prefix": prefix, "directories": [], "objects": found})
        return httpx.Response(404, text="object not found")

    store = TeamStore("http://blobstore", transport=httpx.MockTransport(handle))
    store.creators = creators
    return store, blobs


def queue_blob(channel: str, sender: str, to: str, *messages: tuple[str, str]) -> dict[str, object]:
    """The queue of one sender; each message is (text, seconds of its send time)."""
    stored = [
        {"seq": seq, "text": text, "sent": f"2026-01-01T00:00:{second}.000000Z"} for seq, (text, second) in enumerate(messages, start=1)
    ]
    return {"channel": channel, "from": sender, "to": to, "messages": stored}


def test_send_message_appends_to_the_senders_own_queue(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    store, blobs = fake_team_store({})
    monkeypatch.setattr(tools, "blobstore", store)
    assert tools.send_message(TEAM, "lead", "coder--lead", "Write main.py") == {
        "channel": "coder--lead",
        "seq": 1,
        "to": "coder",
    }
    lead_queue = blobs[f"{TEAM}/channels/coder--lead/lead.json"]
    # Each sender has its own queue with its own seq; a send never touches the other side's queue.
    assert tools.send_message(TEAM, "coder", "coder--lead", "Done") == {"channel": "coder--lead", "seq": 1, "to": "lead"}
    assert tools.send_message(TEAM, "coder", "coder--lead", "Tested") == {"channel": "coder--lead", "seq": 2, "to": "lead"}
    assert blobs[f"{TEAM}/channels/coder--lead/lead.json"] == lead_queue
    assert f"{TEAM}/channels/coder--lead.json" not in blobs
    stored = json.loads(lead_queue)
    assert {key: stored[key] for key in ("channel", "from", "to")} == {"channel": "coder--lead", "from": "lead", "to": "coder"}
    assert [(m["seq"], m["text"]) for m in stored["messages"]] == [(1, "Write main.py")]
    stored = json.loads(blobs[f"{TEAM}/channels/coder--lead/coder.json"])
    assert set(stored) == {"channel", "from", "to", "messages"}
    assert (stored["from"], stored["to"]) == ("coder", "lead")
    assert [(m["seq"], m["text"]) for m in stored["messages"]] == [(1, "Done"), (2, "Tested")]
    for message in stored["messages"]:
        assert set(message) == {"seq", "text", "sent"}
        assert tools.re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}Z", message["sent"])
    assert store.creators[f"{TEAM}/channels/coder--lead/coder.json"] == ("tool-gateway", "t1")
    assert [m["text"] for m in tools.read_messages(TEAM, "lead", "coder--lead")["messages"]] == ["Write main.py", "Done", "Tested"]


@pytest.mark.parametrize("text", ["", "  \n", "x" * 100001])
def test_send_message_refuses_empty_and_oversized_text(tools: ModuleType, monkeypatch: pytest.MonkeyPatch, text: str) -> None:
    store, blobs = fake_team_store({})
    monkeypatch.setattr(tools, "blobstore", store)
    with pytest.raises(ValueError, match="text"):
        tools.send_message(TEAM, "lead", "coder--lead", text)
    assert f"{TEAM}/channels/coder--lead/lead.json" not in blobs
    assert tools.send_message(TEAM, "lead", "coder--lead", "x" * 100000)["seq"] == 1


def test_team_tools_refuse_unknown_member_and_foreign_channel(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    store, blobs = fake_team_store({"files/main.py": "x"})
    monkeypatch.setattr(tools, "blobstore", store)
    before = dict(blobs)
    for call in (
        lambda: tools.list_channels(TEAM, "ghost"),
        lambda: tools.send_message(TEAM, "ghost", "coder--lead", "hi"),
        lambda: tools.read_messages(TEAM, "ghost", "coder--lead"),
        lambda: tools.finish_team(TEAM, "ghost", ["main.py"], "done"),
        # "workflow" is on a channel but is not a member.
        lambda: tools.send_message(TEAM, "workflow", "lead--workflow", "hi"),
    ):
        with pytest.raises(ValueError, match="not a member of this team.*'lead', 'coder', 'tester'"):
            call()
    for call in (
        lambda: tools.send_message(TEAM, "coder", "lead--tester", "hi"),
        lambda: tools.read_messages(TEAM, "coder", "lead--tester"),
        lambda: tools.send_message(TEAM, "coder", "coder--tester", "hi"),
        lambda: tools.read_messages(TEAM, "coder", "../team"),
    ):
        with pytest.raises(ValueError, match=r"not a channel of 'coder': use one of \['coder--lead'\].*list_channels"):
            call()
    assert blobs == before


@pytest.mark.parametrize(
    "team",
    ["", "teams", "teams/t1", "teams/t1/", "teams//build", "teams/../build", "teams/t1/build\n", "user-sessions/t1/build", "runs/t1/build"],
)
def test_team_tools_reject_invalid_team(tools: ModuleType, monkeypatch: pytest.MonkeyPatch, team: str) -> None:
    store, blobs = fake_team_store({})
    monkeypatch.setattr(tools, "blobstore", store)
    before = dict(blobs)
    for call in (
        lambda: tools.list_channels(team, "lead"),
        lambda: tools.send_message(team, "lead", "coder--lead", "hi"),
        lambda: tools.read_messages(team, "lead", "coder--lead"),
        lambda: tools.finish_team(team, "lead", ["main.py"], "done"),
    ):
        with pytest.raises(ValueError, match="invalid team"):
            call()
    assert blobs == before


def test_team_tools_report_a_team_that_does_not_exist(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    store, _ = fake_team_store({})
    monkeypatch.setattr(tools, "blobstore", store)
    with pytest.raises(ValueError, match="does not exist"):
        tools.list_channels("teams/t1/other", "lead")


def test_list_channels_counts_unread_from_the_other_side(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    store, _ = fake_team_store(
        {
            "channels/coder--lead/lead.json": queue_blob("coder--lead", "lead", "coder", ("a", "01"), ("d", "04")),
            "channels/coder--lead/coder.json": queue_blob("coder--lead", "coder", "lead", ("b", "02"), ("c", "03"), ("e", "05")),
            "channels/lead--workflow/workflow.json": queue_blob("lead--workflow", "workflow", "lead", ("task", "00")),
            # The marker counts in the other side's queue: the lead was given the coder's first message.
            "members/lead.json": {"member": "lead", "agent": "demo/lead@v1", "read": {"coder--lead": 1}, "messages": []},
        }
    )
    monkeypatch.setattr(tools, "blobstore", store)
    assert tools.list_channels(TEAM, "lead") == {
        "team": TEAM,
        "member": "lead",
        "channels": [
            {"id": "coder--lead", "with": "coder", "description": "Writes the code.", "unread": 2, "direction": "both"},
            {"id": "lead--tester", "with": "tester", "description": "Writes the tests.", "unread": 0, "direction": "both"},
            {"id": "lead--workflow", "with": "workflow", "description": "", "unread": 1, "direction": "both"},
        ],
    }
    # No members file: the marker is 0, so everything the lead sent is unread.
    assert tools.list_channels(TEAM, "coder") == {
        "team": TEAM,
        "member": "coder",
        "channels": [{"id": "coder--lead", "with": "lead", "description": "Leads the team.", "unread": 2, "direction": "both"}],
    }


def test_read_messages_merges_both_queues_by_send_time(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    store, _ = fake_team_store(
        {
            "channels/coder--lead/lead.json": queue_blob("coder--lead", "lead", "coder", ("a", "01"), ("c", "03")),
            "channels/coder--lead/coder.json": queue_blob("coder--lead", "coder", "lead", ("b", "02"), ("d", "04")),
        }
    )
    monkeypatch.setattr(tools, "blobstore", store)
    history = [
        {"from": sender, "text": text, "sent": f"2026-01-01T00:00:{second}.000000Z"}
        for sender, text, second in (("lead", "a", "01"), ("coder", "b", "02"), ("lead", "c", "03"), ("coder", "d", "04"))
    ]
    assert tools.read_messages(TEAM, "coder", "coder--lead") == {"channel": "coder--lead", "messages": history}
    assert tools.read_messages(TEAM, "lead", "coder--lead", 1)["messages"] == history[1:]
    assert tools.read_messages(TEAM, "lead", "coder--lead", 4) == {"channel": "coder--lead", "messages": []}
    assert tools.read_messages(TEAM, "lead", "lead--tester") == {"channel": "lead--tester", "messages": []}


def test_finish_team_writes_finished_json(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    store, blobs = fake_team_store({"files/main.py": "x", "files/test_main.py": "y"})
    monkeypatch.setattr(tools, "blobstore", store)
    result = tools.finish_team(TEAM, "lead", ["main.py", "test_main.py"], "Calculator with tests.")
    assert isinstance(result, str) and "main.py, test_main.py" in result
    finished = json.loads(blobs[f"{TEAM}/finished.json"])
    assert set(finished) == {"summary", "files", "finished_at"}
    assert finished["summary"] == "Calculator with tests."
    assert finished["files"] == ["main.py", "test_main.py"]
    assert tools.re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}Z", finished["finished_at"])
    assert store.creators[f"{TEAM}/finished.json"] == ("tool-gateway", "t1")


def test_finish_team_refusals(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    store, blobs = fake_team_store({"files/main.py": "x"})
    monkeypatch.setattr(tools, "blobstore", store)
    with pytest.raises(ValueError, match="only the lead 'lead' can finish"):
        tools.finish_team(TEAM, "coder", ["main.py"], "done")
    with pytest.raises(ValueError, match="files is empty"):
        tools.finish_team(TEAM, "lead", [], "done")
    with pytest.raises(ValueError, match=r"do not exist in the workspace: \['missing.py'\]. Existing files: \['main.py'\]"):
        tools.finish_team(TEAM, "lead", ["main.py", "missing.py"], "done")
    with pytest.raises(ValueError, match="invalid file name"):
        tools.finish_team(TEAM, "lead", ["../team.json"], "done")
    assert f"{TEAM}/finished.json" not in blobs


def test_session_file_tools_accept_a_team_workspace(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    store, blobs = fake_team_store({"files/notes.md": "plan\n", "files/image.png": "x"})
    monkeypatch.setattr(tools, "blobstore", store)
    assert tools.write_session_file(TEAM, "main.py", "print(1)\n") == f"{TEAM}/files/main.py"
    assert blobs[f"{TEAM}/files/main.py"] == "print(1)\n"
    assert store.creators[f"{TEAM}/files/main.py"] == ("tool-gateway", "t1")
    assert tools.list_session_files(TEAM) == {
        "session": TEAM,
        "files": [{"name": "notes.md", "size": 5}, {"name": "main.py", "size": 9}],
    }
    assert tools.read_session_file(TEAM, "main.py") == "     1\tprint(1)"
    assert tools.search_session_files(TEAM, "print")["matches"] == [{"file": "main.py", "line": 1, "text": "print(1)"}]
    assert tools.search_session_files(TEAM, "plan", "notes.md")["matches"][0]["file"] == "notes.md"


@pytest.mark.parametrize(
    "extension",
    [
        "md",
        "txt",
        "json",
        "py",
        "go",
        "js",
        "ts",
        "tsx",
        "jsx",
        "java",
        "kt",
        "rs",
        "c",
        "h",
        "cpp",
        "cs",
        "rb",
        "php",
        "sh",
        "sql",
        "html",
        "css",
        "yaml",
        "yml",
        "toml",
        "xml",
    ],
)
def test_team_workspace_extensions(tools: ModuleType, monkeypatch: pytest.MonkeyPatch, extension: str) -> None:
    store, _ = fake_team_store({})
    monkeypatch.setattr(tools, "blobstore", store)
    assert tools.write_session_file(TEAM, f"a.{extension}", "x") == f"{TEAM}/files/a.{extension}"
    if extension not in ("md", "txt", "json"):
        with pytest.raises(ValueError, match="invalid file name"):
            tools.write_session_file(SESSION, f"a.{extension}", "x")


@pytest.mark.parametrize("name", ["", "a", "a.png", "a.exe", ".hidden.py", "../a.py", "sub/a.py", "a b.py", "a.PY", "a.py\n"])
def test_team_workspace_rejects_invalid_file_name(tools: ModuleType, monkeypatch: pytest.MonkeyPatch, name: str) -> None:
    store, blobs = fake_team_store({})
    monkeypatch.setattr(tools, "blobstore", store)
    before = dict(blobs)
    with pytest.raises(ValueError, match="invalid file name"):
        tools.read_session_file(TEAM, name)
    with pytest.raises(ValueError, match="invalid file name"):
        tools.write_session_file(TEAM, name, "x")
    assert blobs == before


@pytest.mark.parametrize("team", ["teams/t1", "teams/t1/", "teams/../build", "teams/t1/build\n"])
def test_session_file_tools_reject_invalid_team_path(tools: ModuleType, monkeypatch: pytest.MonkeyPatch, team: str) -> None:
    store, blobs = fake_team_store({})
    monkeypatch.setattr(tools, "blobstore", store)
    before = dict(blobs)
    for call in (
        lambda: tools.list_session_files(team),
        lambda: tools.read_session_file(team, "a.py"),
        lambda: tools.write_session_file(team, "a.py", "x"),
    ):
        with pytest.raises(ValueError, match="invalid (team|session)"):
            call()
    assert blobs == before


def test_a_one_way_channel_refuses_the_receiver_as_sender(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    channels = [{"id": "coder--lead", "members": ["coder", "lead"], "from": "lead"}]
    store, blobs = fake_team_store({"team.json": {**TEAM_INFO, "channels": channels}})
    monkeypatch.setattr(tools, "blobstore", store)
    assert tools.send_message(TEAM, "lead", "coder--lead", "Write main.py")["to"] == "coder"
    with pytest.raises(ValueError, match="only carries messages from 'lead' to you"):
        tools.send_message(TEAM, "coder", "coder--lead", "Done")
    assert f"{TEAM}/channels/coder--lead/coder.json" not in blobs
    assert [c["direction"] for c in tools.list_channels(TEAM, "lead")["channels"]] == ["send"]
    assert [c["direction"] for c in tools.list_channels(TEAM, "coder")["channels"]] == ["receive"]


def test_list_channels_names_a_two_way_channel_both(tools: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    store, _ = fake_team_store({})
    monkeypatch.setattr(tools, "blobstore", store)
    assert {c["direction"] for c in tools.list_channels(TEAM, "lead")["channels"]} == {"both"}
