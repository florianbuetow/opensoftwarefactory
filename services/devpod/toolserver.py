"""Tool server of the `work` container of a dev pod (port 8080).

GET /healthz, and POST /op with {"op": "<name>", "args": {...}} which answers 200
with {"ok": true, "result": ...} or {"ok": false, "error": "<message for the agent>"}.
Standard library only. It holds no credential.
"""

import json
import os
import signal
import stat
import subprocess
import sys
import tempfile
import threading
from collections.abc import Callable
from typing import IO, cast

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from podhttp import JsonObject, RefusalError, required_env, serve, take_token, text_list  # noqa: E402

WORKSPACE = os.path.realpath(required_env("DEVPOD_WORKSPACE"))
OUTPUT_LIMIT = 20000
DEFAULT_TIMEOUT = 120
MAX_TIMEOUT = 600
MAX_MATCHES = 100
MAX_ENTRIES = 1000
READ_LIMIT = 100000
MATCH_TEXT_LIMIT = 500
SEARCH_TIMEOUT = 30


# ---------------------------------------------------------------- paths


def resolve(path: object, must_exist: bool = False) -> str:
    """Return the real absolute path of `path` inside the workspace, or refuse."""
    if not isinstance(path, str) or path == "":
        raise RefusalError("A path is required. Give a path relative to /workspace, for example src/panel.py.")
    if "\x00" in path:
        raise RefusalError("The path contains a character that is not allowed.")
    joined = path if os.path.isabs(path) else os.path.join(WORKSPACE, path)
    if os.path.isabs(path) and path.startswith("/workspace") and WORKSPACE != "/workspace":
        joined = os.path.join(WORKSPACE, os.path.relpath(path, "/workspace"))
    real = os.path.realpath(joined)
    if real != WORKSPACE and not real.startswith(WORKSPACE + os.sep):
        raise RefusalError(f"The path {path} is outside /workspace. Only files inside /workspace can be used.")
    relative = os.path.relpath(real, WORKSPACE)
    parts = [] if relative == "." else relative.split(os.sep)
    lexical = os.path.normpath(joined).split(os.sep)
    if ".git" in parts or ".git" in lexical:
        raise RefusalError(f"The path {path} is inside .git. Use git commands through run_command instead.")
    if must_exist and not os.path.lexists(real):
        raise RefusalError(f"{display(real)} does not exist. Use list_files to see what exists.")
    return real


def display(real: str) -> str:
    """The path as the agent sees it: relative to the workspace, with `/`."""
    relative = os.path.relpath(real, WORKSPACE)
    return relative.replace(os.sep, "/")


def optional_text_arg(args: JsonObject, name: str) -> str | None:
    """The text of the argument `name`, or None when it is missing."""
    value = args.get(name)
    if value is None:
        return None
    if not isinstance(value, str):
        raise RefusalError(f"The argument {name} must be text.")
    return value


def text_arg(args: JsonObject, name: str) -> str:
    """The text of the required argument `name`."""
    value = optional_text_arg(args, name)
    if value is None:
        raise RefusalError(f"The argument {name} is required.")
    return value


def int_arg(args: JsonObject, name: str) -> int | None:
    """The whole number of the argument `name`, or None when it is missing or empty."""
    value = args.get(name)
    if value is None or value == "":
        return None
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        raise RefusalError(f"The argument {name} must be a whole number.")
    try:
        return int(value)
    except ValueError as error:
        raise RefusalError(f"The argument {name} must be a whole number.") from error


# ---------------------------------------------------------------- file operations


def list_files(args: JsonObject) -> JsonObject:
    """List the entries of a folder, without .git."""
    real = resolve(optional_text_arg(args, "path") or ".", must_exist=True)
    if not os.path.isdir(real):
        raise RefusalError(f"{display(real)} is a file, not a folder. Use read_file to read it.")
    entries: list[JsonObject] = []
    for name in sorted(os.listdir(real)):
        if name == ".git":
            continue
        info = os.lstat(os.path.join(real, name))
        if stat.S_ISDIR(info.st_mode):
            entries.append({"name": name, "type": "dir"})
        else:
            entries.append({"name": name, "type": "file", "size": info.st_size})
    return {"path": display(real), "entries": entries[:MAX_ENTRIES], "truncated": len(entries) > MAX_ENTRIES}


def read_text(real: str) -> str:
    """The content of a file as text; bytes that are not UTF-8 are replaced."""
    if os.path.isdir(real):
        raise RefusalError(f"{display(real)} is a folder, not a file. Use list_files to see its content.")
    try:
        with open(real, "rb") as handle:
            return handle.read().decode("utf-8", errors="replace")
    except PermissionError as error:
        raise RefusalError(f"{display(real)} cannot be read: permission denied.") from error


def read_file(args: JsonObject) -> JsonObject:
    """Read a file, or the lines start_line to end_line of it."""
    real = resolve(text_arg(args, "path"), must_exist=True)
    lines = read_text(real).splitlines(keepends=True)
    total = len(lines)
    start = int_arg(args, "start_line")
    end = int_arg(args, "end_line")
    first = 1 if start is None else start
    last = total if end is None else min(end, total)
    if first < 1:
        raise RefusalError("start_line must be 1 or more. Lines are counted from 1.")
    if end is not None and end < first:
        raise RefusalError(f"end_line ({end}) is before start_line ({first}).")
    if total and first > total:
        raise RefusalError(f"start_line {first} is after the end of {display(real)}, which has {total} lines.")
    content = "".join(lines[first - 1 : last])
    result: JsonObject = {
        "path": display(real),
        "content": content,
        "start_line": first if total else 0,
        "end_line": last,
        "total_lines": total,
    }
    if len(content) > READ_LIMIT:
        result["content"] = content[:READ_LIMIT]
        result["note"] = f"The content was cut after {READ_LIMIT} of {len(content)} characters. Read the rest with start_line and end_line."
    return result


def store(real: str, content: str) -> None:
    """Write the file in place, so a protected file is never replaced."""
    try:
        with open(real, "w", encoding="utf-8", newline="") as handle:
            handle.write(content)
    except PermissionError as error:
        raise RefusalError(
            f"{display(real)} is protected and cannot be changed. Do not try another way; "
            "if the task needs this change, report that you are blocked."
        ) from error
    except IsADirectoryError as error:
        raise RefusalError(f"{display(real)} is a folder, not a file.") from error


def write_file(args: JsonObject) -> JsonObject:
    """Write a whole file, creating it and its folders when needed."""
    real = resolve(text_arg(args, "path"))
    content = text_arg(args, "content")
    if real == WORKSPACE:
        raise RefusalError("The path must name a file inside /workspace.")
    created = not os.path.exists(real)
    parent = os.path.dirname(real)
    try:
        os.makedirs(parent, exist_ok=True)
    except PermissionError as error:
        raise RefusalError(f"The folder {display(parent)} is protected. A file cannot be created there.") from error
    except (FileExistsError, NotADirectoryError) as error:
        raise RefusalError(f"{display(real)} cannot be created because a part of its path is a file.") from error
    store(real, content)
    return {"path": display(real), "created": created, "bytes": len(content.encode("utf-8"))}


def edit_file(args: JsonObject) -> JsonObject:
    """Replace the one place where old_text occurs in a file with new_text."""
    real = resolve(text_arg(args, "path"), must_exist=True)
    old = text_arg(args, "old_text")
    new = text_arg(args, "new_text")
    if old == "":
        raise RefusalError("old_text is empty. Give the exact text to replace, or use write_file to write the whole file.")
    if old == new:
        raise RefusalError("old_text and new_text are the same, so nothing would change.")
    content = read_text(real)
    count = content.count(old)
    if count == 0:
        raise RefusalError(
            f"old_text was not found in {display(real)}. Read the file and copy the exact text, including spaces and line breaks."
        )
    if count > 1:
        raise RefusalError(
            f"old_text occurs {count} times in {display(real)}. Add more of the surrounding lines so that it matches exactly one place."
        )
    store(real, content.replace(old, new, 1))
    return {"path": display(real), "replaced": 1}


def _ripgrep(pattern: str, real: str) -> bytes:
    """Run ripgrep for `pattern` below `real` and return its JSON lines."""
    command = [
        "rg",
        "--json",
        "--hidden",
        "--glob",
        "!.git",
        "--max-columns",
        "2000",
        "--regexp",
        pattern,
        "--",
        os.path.relpath(real, WORKSPACE),
    ]
    try:
        process = subprocess.Popen(
            command, cwd=WORKSPACE, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True
        )
    except FileNotFoundError as error:
        raise RefusalError("Search is not available in this pod: ripgrep (rg) is not installed.") from error
    try:
        out, err = process.communicate(timeout=SEARCH_TIMEOUT)
    except subprocess.TimeoutExpired as error:
        stop(process)
        process.communicate()
        raise RefusalError(f"The search took longer than {SEARCH_TIMEOUT} seconds. Search a smaller folder with path.") from error
    if process.returncode not in (0, 1):
        reason = err.decode("utf-8", errors="replace").strip() or f"ripgrep ended with code {process.returncode}"
        raise RefusalError(f"The search failed: {reason[:1000]}")
    return out


def search(args: JsonObject) -> JsonObject:
    """Search the files below a path for a regular expression."""
    pattern = text_arg(args, "pattern")
    if pattern == "":
        raise RefusalError("pattern is empty. Give a regular expression to search for.")
    real = resolve(optional_text_arg(args, "path") or ".", must_exist=True)
    matches: list[JsonObject] = []
    total = 0
    for line in _ripgrep(pattern, real).decode("utf-8", errors="replace").splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("type") != "match":
            continue
        total += 1
        if len(matches) >= MAX_MATCHES:
            continue
        data = event["data"]
        text = (data.get("lines", {}).get("text") or "").rstrip("\n")
        path = data.get("path", {}).get("text") or ""
        matches.append(
            {"path": path[2:] if path.startswith("./") else path, "line": data.get("line_number"), "text": text[:MATCH_TEXT_LIMIT]}
        )
    result: JsonObject = {"matches": matches, "truncated": total > MAX_MATCHES}
    if total > MAX_MATCHES:
        result["note"] = f"Only the first {MAX_MATCHES} of {total} matches are shown. Use a narrower pattern or a path."
    return result


# ---------------------------------------------------------------- commands


def cut(text: str) -> str:
    """Cut long output to OUTPUT_LIMIT characters: the start and the end, with a note between."""
    if len(text) <= OUTPUT_LIMIT:
        return text
    half = OUTPUT_LIMIT // 2
    note = f"\n[output cut: {len(text)} characters in total, showing the first {half} and the last {half}]\n"
    return text[:half] + note + text[-half:]


def stop(process: subprocess.Popen[bytes]) -> None:
    """Kill the process and every process of its session."""
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        process.kill()


def command_environment() -> dict[str, str]:
    """The environment of a command of the agent: without the pod token and without a pager."""
    env = dict(os.environ)
    env.pop("POD_TOKEN", None)
    env.update({"GIT_TERMINAL_PROMPT": "0", "GIT_PAGER": "cat", "PAGER": "cat", "TERM": "dumb"})
    return env


def run_command(args: JsonObject) -> JsonObject:
    """Run a shell command in the workspace and return its exit code and output."""
    command = text_arg(args, "command")
    if command.strip() == "":
        raise RefusalError("command is empty. Give the command to run.")
    timeout = int_arg(args, "timeout_seconds")
    if timeout is None:
        timeout = DEFAULT_TIMEOUT
    if timeout < 1:
        raise RefusalError(f"timeout_seconds must be between 1 and {MAX_TIMEOUT}.")
    timeout = min(timeout, MAX_TIMEOUT)
    # Temporary files instead of pipes: a process left in the background cannot block the answer.
    with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
        process = subprocess.Popen(
            ["bash", "-lc", command],
            cwd=WORKSPACE,
            stdin=subprocess.DEVNULL,
            stdout=out,
            stderr=err,
            env=command_environment(),
            start_new_session=True,
        )
        timed_out = False
        try:
            code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            stop(process)
            process.wait()
            code = 124
        out.seek(0)
        err.seek(0)
        stdout = out.read().decode("utf-8", errors="replace")
        stderr = err.read().decode("utf-8", errors="replace")
    result: JsonObject = {"exit_code": code, "stdout": cut(stdout), "stderr": cut(stderr)}
    if timed_out:
        result["timed_out"] = True
        result["stderr"] = (
            cut(stderr)
            + ("\n" if stderr and not stderr.endswith("\n") else "")
            + f"[The command was stopped after {timeout} seconds (the time limit). "
            + f"Run a shorter command or give a larger timeout_seconds, at most {MAX_TIMEOUT}.]"
        )
    return result


# ---------------------------------------------------------------- skill scripts


class OutputWindow:
    """Keep only the tail of one output stream without ever retaining it in full."""

    def __init__(self, max_bytes: int) -> None:
        self.max_bytes = max_bytes
        self.data = bytearray()
        self.cut = False
        self.starts_midline = False

    def add(self, chunk: bytes) -> None:
        """Append a chunk and drop the oldest bytes beyond the limit."""
        self.data.extend(chunk)
        if len(self.data) <= self.max_bytes:
            return
        remove = len(self.data) - self.max_bytes
        dropped = self.data[:remove]
        del self.data[:remove]
        self.cut = True
        self.starts_midline = not dropped.endswith(b"\n")

    def text(self, max_lines: int) -> str:
        """The kept output as text: whole lines only, and at most the last max_lines."""
        data = bytes(self.data)
        _, newline, remaining = data.partition(b"\n")
        if self.starts_midline and newline and remaining:
            data = remaining
        lines = data.splitlines(keepends=True)
        if len(lines) > max_lines:
            lines = lines[-max_lines:]
            self.cut = True
        return b"".join(lines).decode("utf-8", errors="replace")


def _read_stream(stream: IO[bytes], window: OutputWindow) -> None:
    try:
        while True:
            chunk = stream.read(8192)
            if not chunk:
                return
            window.add(chunk)
    finally:
        stream.close()


def _skill_files(args: JsonObject, root: str) -> str:
    files = args.get("files")
    if not isinstance(files, dict) or not files:
        raise RefusalError("The skill has no files, so its script cannot be run. Fix the stored skill version.")
    script = text_arg(args, "script")
    for path, content in cast(dict[object, object], files).items():
        if (
            not isinstance(path, str)
            or not path
            or os.path.isabs(path)
            or "\x00" in path
            or any(part in ("", ".", "..") for part in path.split("/"))
        ):
            raise RefusalError(f"The skill file path {path!r} is invalid. Fix the stored skill version.")
        if not isinstance(content, str):
            raise RefusalError(f"The skill file {path} is not text. Fix the stored skill version.")
        target = os.path.join(root, *path.split("/"))
        os.makedirs(os.path.dirname(target), exist_ok=True)
        encoded = content.encode("utf-8")
        with open(target, "wb") as handle:
            handle.write(encoded)
        if encoded.startswith(b"#!"):
            os.chmod(target, 0o700)
    return script


def _start_skill_script(root: str, script: str, arguments: list[str]) -> subprocess.Popen[bytes]:
    """Start the script of a skill that was written below `root`."""
    executable = os.path.join(root, *script.split("/"))
    if not os.path.isfile(executable):
        raise RefusalError(f"The skill script {script} does not exist. Choose a script listed by the skill.")
    with open(executable, "rb") as handle:
        if not handle.read(2).startswith(b"#!"):
            raise RefusalError(
                f"The skill script {script} could not be started: missing a shebang line. Fix the script and save a new skill version."
            )
    try:
        return subprocess.Popen(
            [executable, *arguments],
            cwd=WORKSPACE,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=command_environment(),
            start_new_session=True,
        )
    except OSError as error:
        reason = "missing a shebang line" if error.errno == 8 else str(error)
        raise RefusalError(
            f"The skill script {script} could not be started: {reason}. Fix the script and save a new skill version."
        ) from error


def run_skill_script(args: JsonObject) -> JsonObject:
    """Write the files of a skill to a temporary folder, run one of its scripts and return the tail of its output."""
    timeout = int_arg(args, "timeout_seconds")
    max_lines = int_arg(args, "max_lines")
    max_bytes = int_arg(args, "max_bytes")
    if timeout is None or timeout < 1:
        raise RefusalError("timeout_seconds must be a positive whole number.")
    if max_lines is None or max_lines < 1:
        raise RefusalError("max_lines must be a positive whole number.")
    if max_bytes is None or max_bytes < 1:
        raise RefusalError("max_bytes must be a positive whole number.")
    arguments = text_list(args.get("args", []))
    if arguments is None:
        raise RefusalError("args must be a list of text arguments.")

    with tempfile.TemporaryDirectory(prefix="factory-skill-") as root:
        process = _start_skill_script(root, _skill_files(args, root), arguments)
        stdout = OutputWindow(max_bytes)
        stderr = OutputWindow(max_bytes)
        readers = [
            threading.Thread(target=_read_stream, args=(process.stdout, stdout), daemon=True),
            threading.Thread(target=_read_stream, args=(process.stderr, stderr), daemon=True),
        ]
        for reader in readers:
            reader.start()
        timed_out = False
        try:
            code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            stop(process)
            process.wait()
            code = 124
        finally:
            # A finished script may leave children holding the pipes. Always stop the
            # session so the readers close and no child survives the call.
            stop(process)
        for reader in readers:
            reader.join(timeout=5)
        return {
            "exit_code": code,
            "stdout": stdout.text(max_lines),
            "stderr": stderr.text(max_lines),
            "stdout_cut": stdout.cut,
            "stderr_cut": stderr.cut,
            "timed_out": timed_out,
        }


OPS: dict[str, Callable[[JsonObject], JsonObject]] = {
    "list_files": list_files,
    "read_file": read_file,
    "write_file": write_file,
    "edit_file": edit_file,
    "search": search,
    "run_command": run_command,
    "run_skill_script": run_skill_script,
}


def handle_op(body: JsonObject) -> JsonObject:
    """Run the op that the body names with its args."""
    name = body.get("op")
    args = body.get("args")
    if args is None:
        args = {}
    if name not in OPS:
        raise RefusalError("Unknown op {!r}. The ops are: {}.".format(name, ", ".join(sorted(OPS))))
    if not isinstance(args, dict):
        raise RefusalError("args must be a JSON object.")
    return {"ok": True, "result": OPS[name](cast(JsonObject, args))}


def handle_skill_script(body: JsonObject) -> JsonObject:
    """Run a skill script; this route takes no other op."""
    if body.get("op") != "run_skill_script":
        raise RefusalError("This endpoint runs skill scripts. Use /op for other operations.")
    return handle_op(body)


def main() -> None:
    """Serve the tool server until the container stops."""
    token = take_token()
    required_env("HOME")
    server = serve(
        int(required_env("PORT")),
        token,
        {"/op": handle_op, "/skill-script": handle_skill_script},
        unbounded_paths=("/skill-script",),
    )
    server.serve_forever()


if __name__ == "__main__":
    main()
