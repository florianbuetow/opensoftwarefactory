"""Tools served by the tool gateway.

Every public function defined here (name not starting with "_") becomes an MCP
tool. Its docstring is the description the model sees and its type hints are
the input schema. Saving this file restarts the gateway with the new tools.
"""

import json
import multiprocessing
import re
import threading
from collections.abc import Callable
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any, cast

from factory import NotFoundError, blobstore, connectors, evidence, policies, valid_id

SEARCH_SECONDS = 5.0

# The return type of the tools that answer with a JSON object. At run time it is the bare
# dict: the MCP library derives an output schema from dict[str, Any] and then validates the
# answer and adds structured content, while a bare dict keeps the answer as text only.
if TYPE_CHECKING:
    JsonObject = dict[str, Any]
else:
    JsonObject = dict


def read_trace(run_id: str, execution_id: str, trace_id: str = "", cursor: str = "", limit: int = 100) -> JsonObject:
    """Read target workflow execution spans and artifact references without modifying them.

    Supply the business run UUID and target execution ID from the workflow event.
    trace_id is the optional distributed trace ID, not the business run UUID.
    Follow next_cursor to read more. query_complete describes pagination only;
    source_status explicitly reports unknown or unavailable trace evidence.
    This tool cannot change traces, submit SQL, or access another HTTP endpoint.
    """
    return evidence.read(run_id, execution_id, trace_id, cursor, limit)


def read_trace_artifact(run_id: str, execution_id: str, artifact_id: str) -> JsonObject:
    """Read an artifact only within the target workflow execution's evidence scope.

    Use an artifact_id returned by read_trace. The factory verifies its run and
    execution ownership and bounds returned content, reporting truncation.
    This operation cannot read arbitrary blob namespaces or modify artifacts.
    """
    return evidence.artifact(run_id, execution_id, artifact_id)


REQUIREMENT = re.compile(r"\b(must|should|shall|needs?)\b", re.IGNORECASE)
WORD = re.compile(r"[A-Za-z0-9][A-Za-z0-9'_-]*")
CAPITALIZED = re.compile(r"^[A-Z][A-Za-z0-9_-]+$")
SENTENCE_END = re.compile(r"(?<=[.!?])\s+")
SESSION_ROOT = "user-sessions"
SESSION_FILE_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,99}\.(md|txt|json)$")
TEAM_ROOT = "teams"
TEAM_FILE_NAME = re.compile(
    r"^[A-Za-z0-9][A-Za-z0-9._-]{0,99}\.(md|txt|json"
    r"|py|go|js|ts|tsx|jsx|java|kt|rs|c|h|cpp|cs|rb|php|sh|sql|html|css|yaml|yml|toml|xml)$"
)
MAX_MESSAGE_TEXT = 100000
# A queue holds the messages of one sender in one channel; its append is a read-modify-write,
# so each queue has its own lock. Only this process writes the queues of agent members.
QUEUE_LOCKS: dict[str, threading.Lock] = {}
QUEUE_LOCKS_GUARD = threading.Lock()
MAX_SEARCH_MATCHES = 200
MAX_MATCH_TEXT = 500


def get_policy(policy_id: str, version: str = "latest") -> str:
    """Return the Markdown text of a policy from the policy registry.

    policy_id is the policy name, for example "coding-standards". version is a
    version such as "v3", or "latest" for the newest one. If the policy or
    version does not exist, a message saying so is returned instead.
    """
    try:
        policy = policies.get(policy_id, version)
    except NotFoundError:
        return f"Policy {policy_id!r} version {version!r} does not exist."
    text: str = policy["files"]["policy.md"]
    return text


def get_artifact(object_id: str) -> str:
    """Return the text content of an artifact stored in the blobstore.

    object_id is a slash-separated path such as "runs/42/task.md". Each segment
    may contain letters, digits, ".", "_" and "-", and must not start with ".".
    Fails if the artifact does not exist.
    """
    return blobstore.get(object_id)


def put_artifact(object_id: str, content: str, creator: str = "tool-gateway", trace_id: str = "") -> str:
    """Store text content in the blobstore under object_id and return the object_id.

    object_id is a slash-separated path such as "runs/42/plan.md"; an existing
    artifact with that ID is replaced. creator names who produced the content
    and trace_id links it to a workflow run; both are stored as metadata.
    """
    blobstore.put(object_id, content, creator, trace_id)
    return object_id


def list_artifacts(prefix: str = "") -> JsonObject:
    """List the immediate children of a prefix in the blobstore.

    prefix is a slash-separated directory such as "runs/42", or "" for the top
    level. Returns {"prefix", "directories": [names], "objects": [{"id", "size",
    "created", "creator", "trace_id"}]}. Fails if the prefix does not exist.
    """
    return blobstore.list(prefix)


def analyze_task_complexity(object_id: str) -> JsonObject:
    """Estimate how complex the task described in a blobstore artifact is.

    The assessment is a deterministic heuristic, not a judgement of meaning:
    - words: number of words.
    - sentences: number of sentences (text split at ".", "!" or "?" and at line breaks).
    - requirements: sentences containing "must", "should", "shall", "need" or "needs".
    - components: distinct capitalized words that do not start a sentence,
      a rough proxy for the number of systems or components mentioned.
    - score from 1 to 5: starts at 1 and adds one point each for more than 200
      words, more than 800 words, 5 or more requirements, and 10 or more components.
    - reasons: one short string per point added, or one saying the task is small.
    """
    return {"object_id": object_id, **_assess(blobstore.get(object_id))}


def list_session_files(session: str) -> JsonObject:
    """List the files of an interactive chat session.

    session is the session path given in your instructions, for example
    "user-sessions/<trace-id>/<step-id>"; pass it unchanged. A team member
    passes its team path, for example "teams/<trace-id>/<step-id>", to list the
    workspace files that all members of the team share. Returns
    {"session", "files": [{"name", "size"}]} with size in bytes. The list is
    empty when the session has no files yet. Use the names with
    read_session_file, search_session_files and write_session_file.
    """
    return {"session": _session(session), "files": _session_files(session)}


def read_session_file(session: str, name: str, start_line: int = 0, end_line: int = 0) -> str:
    """Return a session file as numbered lines, like "cat -n".

    session is the session path (or team path) given in your instructions. name
    is a file name from list_session_files, for example "notes.md". Lines are
    numbered from 1.
    start_line and end_line select an inclusive range: start_line=10,
    end_line=20 returns lines 10 to 20. 0 means open: start_line=0 starts at the
    first line and end_line=0 reads to the last line, so leaving both at 0
    returns the whole file. An end_line past the end of the file stops at the
    last line. Each output line is the line number, a tab, then the text. If the
    range is outside the file, a message with the file's line count is returned
    instead. Fails if the file does not exist.
    """
    lines = blobstore.get(_session_file(session, name)).splitlines()
    total = len(lines)
    if start_line < 0 or end_line < 0:
        return f"Invalid range: start_line and end_line must be 0 or greater. {name} has {total} lines."
    if total == 0:
        return f"{name} is empty."
    first = start_line or 1
    last = min(end_line or total, total)
    if first > total:
        return f"Line {first} is outside {name}, which has {total} lines."
    if last < first:
        return f"Invalid range: end_line {end_line} is before start_line {first}. {name} has {total} lines."
    return "\n".join(f"{number:6d}\t{lines[number - 1]}" for number in range(first, last + 1))


def search_session_files(session: str, pattern: str, name: str = "") -> JsonObject:
    """Search the session files line by line with a regular expression.

    session is the session path (or team path) given in your instructions.
    pattern is a Python "re" regular expression matched anywhere in a line; it is case-sensitive
    unless it starts with "(?i)". name limits the search to one file from
    list_session_files; leave it "" to search every file of the session.
    Returns {"matches": [{"file", "line", "text"}], "truncated"}: line is the
    1-based line number, usable as start_line and end_line of read_session_file,
    and text is the matching line (shortened when very long). At most 200
    matches are returned; "truncated" is true when there were more, so narrow
    the pattern or search a single file. Fails with a message when pattern is
    not a valid regular expression.
    """
    try:
        regex = re.compile(pattern)
    except (re.error, OverflowError, RecursionError) as exc:
        raise ValueError(f"invalid regular expression {pattern!r}: {exc}") from exc
    names = [name] if name else [entry["name"] for entry in _session_files(session)]
    texts = [(file_name, blobstore.get(_session_file(session, file_name))) for file_name in names]
    return _within(SEARCH_SECONDS, lambda: _search(regex, texts), "the search took too long; simplify the pattern")


def _search(regex: re.Pattern[str], texts: list[tuple[str, str]]) -> dict[str, Any]:
    matches: list[dict[str, Any]] = []
    for file_name, text in texts:
        for number, line in enumerate(text.splitlines(), start=1):
            if not regex.search(line):
                continue
            if len(matches) == MAX_SEARCH_MATCHES:
                return {"matches": matches, "truncated": True}
            matches.append({"file": file_name, "line": number, "text": line[:MAX_MATCH_TEXT]})
    return {"matches": matches, "truncated": False}


def _within[T](seconds: float, work: Callable[[], T], message: str) -> T:
    """Run work in a child process and stop it after seconds.

    Python's re has no timeout, and a pattern such as (a+)+$ can run for hours on
    one line; a child process is the only way to stop it.
    """
    context = multiprocessing.get_context("fork")
    receiver, sender = context.Pipe(duplex=False)

    def run() -> None:
        try:
            sender.send((True, work()))
        except Exception as exc:  # reported to the caller below
            sender.send((False, str(exc)))

    child = context.Process(target=run, daemon=True)
    child.start()
    sender.close()
    try:
        if not receiver.poll(seconds):
            raise ValueError(message)
        ok, value = receiver.recv()
    finally:
        if child.is_alive():
            child.terminate()
        child.join(1)
        receiver.close()
    if not ok:
        raise ValueError(value)
    result: T = value
    return result


def write_session_file(session: str, name: str, content: str) -> str:
    """Store text as a file of the session and return its object ID.

    session is the session path given in your instructions. name is the file
    name: it starts with a letter or digit, may contain letters, digits, ".",
    "_" and "-", and must end in ".md", ".txt" or ".json", for example
    "summary.md". No folders. content is the complete new text of the file; an
    existing file with that name is replaced, so to change a file read it first
    and write back the whole text.

    When session is a team path such as "teams/<trace-id>/<step-id>", the file
    goes into the workspace shared by all team members, and code files are
    allowed too: .py .go .js .ts .tsx .jsx .java .kt .rs .c .h .cpp .cs .rb .php
    .sh .sql .html .css .yaml .yml .toml .xml, for example
    write_session_file(session="teams/t1/build", name="main.py", content="print(1)").
    """
    object_id = _session_file(session, name)
    blobstore.put(object_id, content, "tool-gateway", session.split("/")[1])
    return object_id


def list_channels(team: str, member: str) -> JsonObject:
    """List your message channels in a team: who is on the other end and how many unread messages wait.

    team is the team path given in your instructions, for example
    "teams/<trace-id>/<step-id>"; pass it unchanged. member is your own member
    name in the team, also given in your instructions, for example "coder".
    Example: list_channels(team="teams/t1/build", member="coder").
    Returns {"team", "member", "channels": [{"id", "with", "description",
    "unread", "direction"}]}. id is the channel ID to pass to send_message and
    read_messages. with is the name of the other member on that channel and
    description says what that member does. unread is the number of messages
    from the other member that you have not been given yet. direction is
    "both" when you and the other member may send, "send" when only you may
    send, and "receive" when only the other member may send. A channel connects
    exactly two members; you can only message members you share a channel with.
    """
    info = _team(team)
    _team_member(info, member)
    descriptions = {entry["name"]: entry.get("description", "") for entry in info.get("members", [])}
    read = _read_markers(team, member)
    channels: list[dict[str, Any]] = []
    for channel in info.get("channels", []):
        if member not in channel["members"]:
            continue
        other = _other(channel, member)
        marker = read.get(channel["id"], 0)
        unread = sum(1 for message in _queue(team, channel["id"], other) if message["seq"] > marker)
        channels.append(
            {
                "id": channel["id"],
                "with": other,
                "description": descriptions.get(other, ""),
                "unread": unread,
                "direction": _direction(channel, member),
            }
        )
    return {"team": team, "member": member, "channels": channels}


def send_message(team: str, member: str, channel: str, text: str) -> JsonObject:
    """Send a message to the other member of one of your team channels.

    team is the team path and member is your own member name, both given in
    your instructions. channel is a channel ID from list_channels, for example
    "coder--lead". text is the complete message, at most 100000 characters; it
    must not be empty. Put everything the other member needs into the text, or
    name the workspace file that holds it.
    Example: send_message(team="teams/t1/build", member="lead",
    channel="coder--lead", text="Write main.py as described in input-clarify.md.").
    Returns {"channel", "seq", "to"}: seq is the number of your message among
    those you sent on the channel and to is the member who receives it. The message is delivered
    after your turn ends and the reply arrives later as a new message to you,
    so do not wait for it and do not call read_messages in a loop. Fails with a
    message when the channel is not one of yours; call list_channels to see
    your channels.
    """
    info = _team(team)
    _team_member(info, member)
    entry = _team_channel(info, member, channel)
    if not _is_text(text) or not text.strip():
        raise ValueError("text is empty: pass the complete message as text")
    if len(text) > MAX_MESSAGE_TEXT:
        raise ValueError(
            f"text has {len(text)} characters, the limit is {MAX_MESSAGE_TEXT}: write the content to a "
            "workspace file with write_session_file and send a short message naming the file"
        )
    if _direction(entry, member) == "receive":
        raise ValueError(
            f"channel {channel!r} only carries messages from {entry['from']!r} to you: you cannot send on it. "
            "Use another of your channels (call list_channels) or write your result to a workspace file"
        )
    to = _other(entry, member)
    queue_id = _queue_id(team, channel, member)
    with QUEUE_LOCKS_GUARD:
        lock = QUEUE_LOCKS.setdefault(queue_id, threading.Lock())
    with lock:
        messages = _queue(team, channel, member)
        seq = max((message["seq"] for message in messages), default=0) + 1
        messages.append({"seq": seq, "text": text, "sent": _now()})
        stored = {"channel": channel, "from": member, "to": to, "messages": messages}
        blobstore.put(queue_id, json.dumps(stored), "tool-gateway", info.get("trace_id", ""))
    return {"channel": channel, "seq": seq, "to": to}


def read_messages(team: str, member: str, channel: str, after: int = 0) -> JsonObject:
    """Re-read the messages of one of your team channels, both directions, oldest first.

    New messages are given to you automatically at the start of a turn, so use
    this only to look up what was said earlier. team is the team path and
    member is your own member name, both given in your instructions. channel is
    a channel ID from list_channels, for example "coder--lead". after=0 returns
    every message; after=5 skips the first 5 messages of the channel.
    Example: read_messages(team="teams/t1/build", member="coder", channel="coder--lead").
    Returns {"channel", "messages": [{"from", "text", "sent"}]}: from is the
    sender's member name and sent is the UTC time.
    Fails with a message when the channel is not one of yours; call
    list_channels to see your channels.
    """
    info = _team(team)
    _team_member(info, member)
    entry = _team_channel(info, member, channel)
    # The channel is its two queues, one per sender, merged into one history by send time.
    history = [
        {"from": sender, "text": message["text"], "sent": message["sent"]}
        for sender in entry["members"]
        for message in _queue(team, channel, sender)
    ]
    history.sort(key=lambda message: _sent(message["sent"]))
    return {"channel": channel, "messages": history[max(after, 0) :]}


def finish_team(team: str, member: str, files: list[str], summary: str) -> str:
    """Finish the team's work and name the workspace files that are its result. Only the lead may call this.

    team is the team path and member is your own member name, both given in
    your instructions. files is the list of workspace file names that are the
    result, for example ["main.py", "test_main.py"]; it must not be empty and
    every file must already exist in the workspace (check with
    list_session_files, passing the team path as session). summary is a short
    text saying what was built.
    Example: finish_team(team="teams/t1/build", member="lead",
    files=["main.py", "test_main.py"], summary="Calculator with unit tests.").
    Call it once, when all work is done and reviewed; the team stops after it.
    Returns a confirmation. Fails with a message when you are not the lead or
    when a file is missing.
    """
    info = _team(team)
    _team_member(info, member)
    if member != info.get("lead"):
        raise ValueError(f"only the lead {info.get('lead')!r} can finish the team: send your result to the lead with send_message instead")
    if not _is_text_list(files) or not files:
        raise ValueError('files is empty: pass the names of the result files, for example ["main.py"]')
    for name in files:
        _team_file(team, name)
    existing = sorted(entry["name"] for entry in _session_files(team))
    missing = [name for name in files if name not in existing]
    if missing:
        raise ValueError(
            f"these files do not exist in the workspace: {missing}. Existing files: {existing}. "
            "Write them with write_session_file or correct the names, then call finish_team again"
        )
    finished = {"summary": summary, "files": files, "finished_at": _now()}
    blobstore.put(f"{team}/finished.json", json.dumps(finished), "tool-gateway", info.get("trace_id", ""))
    return f"Team finished. Result files: {', '.join(files)}."


# The dev pod tools. A dev pod is a Linux sandbox that holds a clone of a Git repository in
# /workspace, on a work branch of its own. Every tool forwards to the connector service,
# which checks the call against the rules of the pod and of the agent. The first three
# arguments (pod, agent, run) are filled in by the system on every call; whatever a model
# passes for them is replaced.


def list_files(pod: str, agent: str, run: str, path: str = ".") -> JsonObject:
    """List the files and folders in one folder of the repository you work on.

    pod, agent and run are filled in by the system; do not pass them.
    path is a folder relative to the repository root, for example "src" or
    "src/panel". Leave it out to list the root. One level is listed, not the
    folders below it; call list_files again with a sub-folder to go deeper.
    Returns {"path", "entries": [{"name", "type": "file" or "dir", "size"}],
    "truncated"}. Fails with a message if the folder does not exist.
    Example: list_files(path="src").
    """
    return _devpod_object(pod, agent, run, "list_files", {"path": path or "."})


def read_file(pod: str, agent: str, run: str, path: str, start_line: int = 0, end_line: int = 0) -> str:
    """Return the text of a file of the repository you work on.

    pod, agent and run are filled in by the system; do not pass them.
    path is the file relative to the repository root, for example
    "src/panel.py". start_line and end_line select an inclusive range of lines,
    counted from 1: start_line=10, end_line=20 returns lines 10 to 20. 0 means
    open, so leaving both at 0 returns the whole file. The text is returned
    exactly as it is in the file, without line numbers, so you can copy from it
    into edit_file. A very long file is cut, with a note at the end that says
    how to read the rest. Fails with a message if the file does not exist.
    Example: read_file(path="src/panel.py").
    """
    args: dict[str, Any] = {"path": path}
    if start_line:
        args["start_line"] = start_line
    if end_line:
        args["end_line"] = end_line
    result = _devpod(pod, agent, run, "read_file", args)
    if not isinstance(result, dict):
        return str(result)
    answer = cast(dict[str, Any], result)
    content: str = answer.get("content", "")
    if not content and not answer.get("total_lines"):
        return f"{answer.get('path', path)} is empty."
    if answer.get("note"):
        content = f"{content}\n[{answer['note']}]"
    return content


def write_file(pod: str, agent: str, run: str, path: str, content: str) -> JsonObject:
    r"""Write a whole file in the repository you work on, creating it or replacing its content.

    pod, agent and run are filled in by the system; do not pass them.
    path is the file relative to the repository root, for example
    "src/panel.py"; missing folders are created. content is the complete new
    text of the file: everything that was in the file before is gone, so to
    change a part of a file use edit_file instead.
    Returns {"path", "created", "bytes"}. The call is refused with a message
    when the file is protected or a rule of this work forbids the content; the
    message says why and what to do instead. Do not try the same change in
    another way.
    Example: write_file(path="notes/plan.md", content="# Plan\\n").
    """
    return _devpod_object(pod, agent, run, "write_file", {"path": path, "content": content})


def edit_file(pod: str, agent: str, run: str, path: str, old_text: str, new_text: str) -> JsonObject:
    """Replace one piece of text in a file of the repository you work on.

    pod, agent and run are filled in by the system; do not pass them.
    path is the file relative to the repository root. old_text is the exact
    text to replace, copied from read_file including spaces and line breaks; it
    must occur exactly once in the file. new_text is what stands there
    afterwards. If old_text is not found or occurs several times, nothing is
    changed and a message says so: read the file again and pass a longer
    old_text that matches one place.
    Returns {"path", "replaced": 1}. The call is refused with a message when
    the file is protected or a rule of this work forbids the new text; the
    message says why and what to do instead.
    Example: edit_file(path="src/panel.py", old_text="total = 0", new_text="total = 1").
    """
    return _devpod_object(pod, agent, run, "edit_file", {"path": path, "old_text": old_text, "new_text": new_text})


def search(pod: str, agent: str, run: str, pattern: str, path: str = "") -> JsonObject:
    """Search the files of the repository you work on for a regular expression.

    pod, agent and run are filled in by the system; do not pass them.
    pattern is a regular expression, for example "def render" or
    "payment_date". path limits the search to one folder or file, for example
    "src"; leave it out to search the whole repository.
    Returns {"matches": [{"path", "line", "text"}], "truncated"} with at most
    100 matches; line is the line number, counted from 1. No match gives an
    empty list.
    Example: search(pattern="payment_date", path="src").
    """
    args: dict[str, Any] = {"pattern": pattern}
    if path:
        args["path"] = path
    return _devpod_object(pod, agent, run, "search", args)


def run_command(pod: str, agent: str, run: str, command: str, timeout_seconds: int = 0) -> JsonObject:
    """Run one shell command in the root of the repository you work on.

    pod, agent and run are filled in by the system; do not pass them.
    command is one command, for example "./verify.sh", "git status" or
    "git commit -m 'Add payment date'". Only the commands your instructions
    allow can be run; any other command is refused with a message that lists
    the allowed ones. Run one command per call: a command that joins several
    with ";", "&&", "||" or "|" is refused. Git is used through this tool: to
    save your work run "git add <files>" and then "git commit -m '<message>'".
    timeout_seconds is the time limit; 0 means the default of 120 seconds, the
    most is 600.
    Returns {"exit_code", "stdout", "stderr"}. exit_code 0 means the command
    succeeded; any other value means it failed, and stdout and stderr say why.
    Example: run_command(command="python3 -m unittest discover").
    """
    args: dict[str, Any] = {"command": command}
    if timeout_seconds:
        args["timeout_seconds"] = timeout_seconds
    return _devpod_object(pod, agent, run, "run_command", args)


def report_blocked(pod: str, agent: str, run: str, reason: str) -> str:
    """Say that you cannot finish the work, and end it as blocked.

    pod, agent and run are filled in by the system; do not pass them.
    Call this when something you may not change stops the work: a tool call
    was refused and the task cannot be done without it, a needed service is
    down, or the task contradicts a rule. Do not work around the obstacle.
    reason says in one or two sentences what stops the work and what would be
    needed to continue.
    After this call your work ends; do not call any more tools.
    Example: report_blocked(reason="The test service is down, so the checks cannot run.").
    """
    return _devpod_text(_devpod(pod, agent, run, "report_blocked", {"reason": reason}))


def push_branch(pod: str, agent: str, run: str) -> str:
    """Publish your commits: push the work branch to the Git host of the repository.

    pod, agent and run are filled in by the system; do not pass them.
    It takes no arguments of your own; the branch is always the work branch
    of this work. Commit first (run_command with "git add" and "git commit");
    only committed changes are pushed.
    Returns what happened. If the answer is "Recorded. A person must approve
    before this is published.", the push happens after a person approved it:
    you are done and need not call push_branch again. Fails with a message
    when the repository cannot be written to.
    Example: push_branch().
    """
    return _devpod_text(_devpod(pod, agent, run, "push_branch", {}))


def create_pull_request(pod: str, agent: str, run: str, title: str, body: str) -> str:
    """Create the pull request that offers your commits on the work branch for review.

    pod, agent and run are filled in by the system; do not pass them.
    title is one short line that says what the change does. body is the
    description in Markdown: what changed, why, and how it was checked.
    Commit your changes first. There is one pull request per work; if it
    exists already the call is refused, and you change it with
    update_pull_request.
    Returns what was created. If the answer is "Recorded. A person must
    approve before this is published.", the pull request is created after a
    person approved it: you are done.
    Example: create_pull_request(title="Show the payment date",
    body="Adds the payment date to the panel. Checked with ./verify.sh.").
    """
    return _devpod_text(_devpod(pod, agent, run, "create_pull_request", {"title": title, "body": body}))


def update_pull_request(pod: str, agent: str, run: str, title: str = "", body: str = "") -> str:
    """Change the title or the description of the pull request of your work.

    pod, agent and run are filled in by the system; do not pass them.
    Pass the new title, the new body, or both; what you leave out stays as it
    is. New commits on the work branch are picked up as well. Fails with a
    message if there is no pull request yet.
    Example: update_pull_request(body="Adds the payment date. Checked with ./verify.sh.").
    """
    args: dict[str, Any] = {}
    if title:
        args["title"] = title
    if body:
        args["body"] = body
    return _devpod_text(_devpod(pod, agent, run, "update_pull_request", args))


def get_pull_request(pod: str, agent: str, run: str) -> str:
    """Return the pull request of your work: title, description, branches and comments.

    pod, agent and run are filled in by the system; do not pass them.
    It takes no arguments of your own. Fails with a message if there is no
    pull request yet.
    Example: get_pull_request().
    """
    return _devpod_text(_devpod(pod, agent, run, "get_pull_request", {}))


def comment_on_pull_request(pod: str, agent: str, run: str, body: str) -> str:
    """Add a comment to the pull request of your work.

    pod, agent and run are filled in by the system; do not pass them.
    body is the text of the comment in Markdown, for example a review finding
    or a note on what was checked. Fails with a message if there is no pull
    request yet.
    Example: comment_on_pull_request(body="All checks pass.").
    """
    return _devpod_text(_devpod(pod, agent, run, "comment_on_pull_request", {"body": body}))


def _devpod(pod: str, agent: str, run: str, tool: str, args: dict[str, Any]) -> object:
    """Forward a dev pod tool call to the connector service; a refusal is raised with its message."""
    return connectors.tool(pod, tool, args, agent, run)


def _devpod_object(pod: str, agent: str, run: str, tool: str, args: dict[str, Any]) -> JsonObject:
    """Forward a dev pod tool call that the connector service answers with a JSON object.

    The answer is passed on as it is, without a check of its type.
    """
    return cast(JsonObject, _devpod(pod, agent, run, tool, args))


def _devpod_text(result: object) -> str:
    """Return a result as text: a sentence as it is, anything else as JSON."""
    if isinstance(result, str):
        return result
    return json.dumps(result, indent=1, ensure_ascii=False)


def read_skill(pod: str, agent: str, run: str, version: str, skill: str, file: str = "SKILL.md") -> str:
    """Read a text file from a skill carried by this agent.

    pod, agent, run and version are filled in by the system; do not pass them.
    skill is the carried skill's name. file is its relative path and defaults to
    the root SKILL.md. Read that file before using a skill, then follow its
    instructions to read any further files it names.
    Example: read_skill(skill="git", file="release/SKILL.md").
    """
    # The connector service answers read_skill with the text of the file.
    return cast(str, connectors.skill(pod, agent, run, version, skill, "read_skill", {"file": file}))


def run_skill_script(
    pod: str, agent: str, run: str, version: str, skill: str, script: str, args: list[str] | None = None, timeout_seconds: int = 0
) -> JsonObject:
    """Run one script from a skill carried by this agent in the run's dev pod.

    pod, agent, run, version and timeout_seconds are filled in by the system; do
    not pass them. skill names the carried skill, script is a relative path in
    it, and args are passed directly as separate arguments without a shell.
    The exit code and the retained ends of standard output and error are returned.
    Example: run_skill_script(skill="git", script="release/scripts/tag.sh", args=["1.4.0"]).
    """
    if args is None:
        args = []
    # The connector service answers run_skill_script with a JSON object.
    return cast(
        JsonObject,
        connectors.skill(
            pod, agent, run, version, skill, "run_skill_script", {"script": script, "args": args, "timeout_seconds": timeout_seconds}
        ),
    )


def _is_text(value: object) -> bool:
    """Report whether an argument is a string; a direct call can pass any value."""
    return isinstance(value, str)


def _is_text_list(value: object) -> bool:
    """Report whether an argument is a list of strings; a direct call can pass any value."""
    return isinstance(value, list) and all(isinstance(item, str) for item in cast(list[object], value))


def _now() -> str:
    """Return the UTC time as RFC 3339 with microseconds, so messages of two queues order by it."""
    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def _sent(value: str) -> datetime:
    try:
        return datetime.fromisoformat(value)
    except ValueError:
        return datetime.min.replace(tzinfo=UTC)


def _is_team(path: str) -> bool:
    return path.startswith(TEAM_ROOT + "/")


def _team_path(team: str) -> str:
    segments = team.split("/")
    if len(segments) < 3 or segments[0] != TEAM_ROOT or "\n" in team or not valid_id(team):
        raise ValueError(f'invalid team {team!r}: use the team path given in your instructions, such as "{TEAM_ROOT}/<trace-id>/<step-id>"')
    return team


def _team_file(team: str, name: str) -> str:
    if not TEAM_FILE_NAME.fullmatch(name):
        raise ValueError(
            f"invalid file name {name!r}: use letters, digits, '.', '_' and '-', start with a letter or "
            "digit, and end in .md, .txt, .json or a code extension such as .py, .go, .js, .ts, .html or .yaml"
        )
    return f"{_team_path(team)}/files/{name}"


def _team(team: str) -> dict[str, Any]:
    try:
        info: dict[str, Any] = json.loads(blobstore.get(f"{_team_path(team)}/team.json"))
        return info
    except NotFoundError:
        raise ValueError(f"team {team!r} does not exist: use the team path given in your instructions unchanged") from None


def _team_member(info: dict[str, Any], member: str) -> None:
    names = [entry["name"] for entry in info.get("members", [])]
    if member not in names:
        raise ValueError(f"{member!r} is not a member of this team: pass your own member name as member. Members: {names}")


def _team_channel(info: dict[str, Any], member: str, channel: str) -> dict[str, Any]:
    own: list[dict[str, Any]] = [entry for entry in info.get("channels", []) if member in entry["members"]]
    for entry in own:
        if entry["id"] == channel:
            return entry
    raise ValueError(
        f"{channel!r} is not a channel of {member!r}: use one of {[entry['id'] for entry in own]} "
        "(call list_channels to see who is on each)"
    )


def _direction(channel: dict[str, Any], member: str) -> str:
    """Return "both", or "send" or "receive" for a one-way channel, as seen by member."""
    sender = channel.get("from", "")
    if not sender:
        return "both"
    return "send" if sender == member else "receive"


def _other(channel: dict[str, Any], member: str) -> str:
    return next((name for name in channel["members"] if name != member), member)


def _queue_id(team: str, channel: str, sender: str) -> str:
    return f"{team}/channels/{channel}/{sender}.json"


def _queue(team: str, channel: str, sender: str) -> list[dict[str, Any]]:
    """Return the messages sender sent on the channel; none when sender sent nothing yet."""
    try:
        messages: list[dict[str, Any]] = json.loads(blobstore.get(_queue_id(team, channel, sender))).get("messages") or []
        return messages
    except NotFoundError:
        return []


def _read_markers(team: str, member: str) -> dict[str, int]:
    try:
        markers: dict[str, int] = json.loads(blobstore.get(f"{team}/members/{member}.json")).get("read") or {}
        return markers
    except NotFoundError:
        return {}


def _session(session: str) -> str:
    if _is_team(session):
        return _team_path(session)
    segments = session.split("/")
    # valid_id anchors with "$", which also matches before a trailing newline in a segment.
    if len(segments) < 3 or segments[0] != SESSION_ROOT or "\n" in session or not valid_id(session):
        raise ValueError(
            f'invalid session {session!r}: use the session path given in your instructions, such as "{SESSION_ROOT}/<trace-id>/<step-id>"'
        )
    return session


def _session_file(session: str, name: str) -> str:
    if _is_team(session):
        return _team_file(session, name)
    if not SESSION_FILE_NAME.fullmatch(name):
        raise ValueError(
            f"invalid file name {name!r}: use letters, digits, '.', '_' and '-', "
            "start with a letter or digit, and end in .md, .txt or .json"
        )
    return f"{_session(session)}/files/{name}"


def _session_files(session: str) -> list[dict[str, Any]]:
    try:
        listing = blobstore.list(f"{_session(session)}/files")
    except NotFoundError:
        return []
    files = [{"name": entry["id"].rsplit("/", 1)[-1], "size": entry.get("size")} for entry in listing["objects"]]
    pattern = TEAM_FILE_NAME if _is_team(session) else SESSION_FILE_NAME
    return [entry for entry in files if pattern.match(entry["name"])]


def _sentences(text: str) -> list[str]:
    return [s.strip() for line in text.splitlines() for s in SENTENCE_END.split(line) if s.strip()]


def _assess(text: str) -> dict[str, Any]:
    sentences = _sentences(text)
    words = len(WORD.findall(text))
    requirements = sum(1 for s in sentences if REQUIREMENT.search(s))
    components = {w for s in sentences for w in WORD.findall(s)[1:] if CAPITALIZED.match(w)}
    reasons: list[str] = []
    if words > 200:
        reasons.append(f"more than 200 words ({words})")
    if words > 800:
        reasons.append(f"more than 800 words ({words})")
    if requirements >= 5:
        reasons.append(f"5 or more requirements ({requirements})")
    if len(components) >= 10:
        reasons.append(f"10 or more components mentioned ({len(components)})")
    score = 1 + len(reasons)
    if not reasons:
        reasons.append("short text with few requirements and components")
    return {
        "words": words,
        "sentences": len(sentences),
        "requirements": requirements,
        "components": len(components),
        "score": score,
        "reasons": reasons,
    }
