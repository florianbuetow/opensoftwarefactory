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

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from podhttp import Refusal, serve, take_token  # noqa: E402

WORKSPACE = os.path.realpath(os.environ.get("DEVPOD_WORKSPACE", "/workspace"))
OUTPUT_LIMIT = 20000
DEFAULT_TIMEOUT = 120
MAX_TIMEOUT = 600
MAX_MATCHES = 100
MAX_ENTRIES = 1000
READ_LIMIT = 100000
MATCH_TEXT_LIMIT = 500
SEARCH_TIMEOUT = 30


# ---------------------------------------------------------------- paths

def resolve(path, must_exist=False):
    """Return the real absolute path of `path` inside the workspace, or refuse."""
    if not isinstance(path, str) or path == "":
        raise Refusal("A path is required. Give a path relative to /workspace, for example src/panel.py.")
    if "\x00" in path:
        raise Refusal("The path contains a character that is not allowed.")
    joined = path if os.path.isabs(path) else os.path.join(WORKSPACE, path)
    if os.path.isabs(path) and path.startswith("/workspace") and WORKSPACE != "/workspace":
        joined = os.path.join(WORKSPACE, os.path.relpath(path, "/workspace"))
    real = os.path.realpath(joined)
    if real != WORKSPACE and not real.startswith(WORKSPACE + os.sep):
        raise Refusal("The path %s is outside /workspace. Only files inside /workspace can be used." % path)
    relative = os.path.relpath(real, WORKSPACE)
    parts = [] if relative == "." else relative.split(os.sep)
    lexical = os.path.normpath(joined).split(os.sep)
    if ".git" in parts or ".git" in lexical:
        raise Refusal("The path %s is inside .git. Use git commands through run_command instead." % path)
    if must_exist and not os.path.lexists(real):
        raise Refusal("%s does not exist. Use list_files to see what exists." % display(real))
    return real


def display(real):
    relative = os.path.relpath(real, WORKSPACE)
    return relative.replace(os.sep, "/")


def text_arg(args, name, required=True):
    value = args.get(name)
    if value is None:
        if required:
            raise Refusal("The argument %s is required." % name)
        return None
    if not isinstance(value, str):
        raise Refusal("The argument %s must be text." % name)
    return value


def int_arg(args, name):
    value = args.get(name)
    if value is None or value == "":
        return None
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        raise Refusal("The argument %s must be a whole number." % name)
    try:
        return int(value)
    except ValueError:
        raise Refusal("The argument %s must be a whole number." % name)


# ---------------------------------------------------------------- file operations

def list_files(args):
    real = resolve(text_arg(args, "path", required=False) or ".", must_exist=True)
    if not os.path.isdir(real):
        raise Refusal("%s is a file, not a folder. Use read_file to read it." % display(real))
    entries = []
    for name in sorted(os.listdir(real)):
        if name == ".git":
            continue
        info = os.lstat(os.path.join(real, name))
        if stat.S_ISDIR(info.st_mode):
            entries.append({"name": name, "type": "dir"})
        else:
            entries.append({"name": name, "type": "file", "size": info.st_size})
    result = {"path": display(real), "entries": entries[:MAX_ENTRIES], "truncated": len(entries) > MAX_ENTRIES}
    return result


def read_text(real):
    if os.path.isdir(real):
        raise Refusal("%s is a folder, not a file. Use list_files to see its content." % display(real))
    try:
        with open(real, "rb") as handle:
            return handle.read().decode("utf-8", errors="replace")
    except PermissionError:
        raise Refusal("%s cannot be read: permission denied." % display(real))


def read_file(args):
    real = resolve(text_arg(args, "path"), must_exist=True)
    lines = read_text(real).splitlines(keepends=True)
    total = len(lines)
    start = int_arg(args, "start_line")
    end = int_arg(args, "end_line")
    first = 1 if start is None else start
    last = total if end is None else min(end, total)
    if first < 1:
        raise Refusal("start_line must be 1 or more. Lines are counted from 1.")
    if end is not None and end < first:
        raise Refusal("end_line (%d) is before start_line (%d)." % (end, first))
    if total and first > total:
        raise Refusal("start_line %d is after the end of %s, which has %d lines." % (first, display(real), total))
    content = "".join(lines[first - 1:last])
    result = {"path": display(real), "content": content, "start_line": first if total else 0,
              "end_line": last, "total_lines": total}
    if len(content) > READ_LIMIT:
        result["content"] = content[:READ_LIMIT]
        result["note"] = ("The content was cut after %d of %d characters. "
                          "Read the rest with start_line and end_line." % (READ_LIMIT, len(content)))
    return result


def store(real, content):
    """Write the file in place, so a protected file is never replaced."""
    try:
        with open(real, "w", encoding="utf-8", newline="") as handle:
            handle.write(content)
    except PermissionError:
        raise Refusal("%s is protected and cannot be changed. Do not try another way; "
                      "if the task needs this change, report that you are blocked." % display(real))
    except IsADirectoryError:
        raise Refusal("%s is a folder, not a file." % display(real))


def write_file(args):
    real = resolve(text_arg(args, "path"))
    content = text_arg(args, "content")
    if real == WORKSPACE:
        raise Refusal("The path must name a file inside /workspace.")
    created = not os.path.exists(real)
    parent = os.path.dirname(real)
    try:
        os.makedirs(parent, exist_ok=True)
    except PermissionError:
        raise Refusal("The folder %s is protected. A file cannot be created there." % display(parent))
    except (FileExistsError, NotADirectoryError):
        raise Refusal("%s cannot be created because a part of its path is a file." % display(real))
    store(real, content)
    return {"path": display(real), "created": created, "bytes": len(content.encode("utf-8"))}


def edit_file(args):
    real = resolve(text_arg(args, "path"), must_exist=True)
    old = text_arg(args, "old_text")
    new = text_arg(args, "new_text")
    if old == "":
        raise Refusal("old_text is empty. Give the exact text to replace, or use write_file to write the whole file.")
    if old == new:
        raise Refusal("old_text and new_text are the same, so nothing would change.")
    content = read_text(real)
    count = content.count(old)
    if count == 0:
        raise Refusal("old_text was not found in %s. Read the file and copy the exact text, "
                      "including spaces and line breaks." % display(real))
    if count > 1:
        raise Refusal("old_text occurs %d times in %s. Add more of the surrounding lines "
                      "so that it matches exactly one place." % (count, display(real)))
    store(real, content.replace(old, new, 1))
    return {"path": display(real), "replaced": 1}


def search(args):
    pattern = text_arg(args, "pattern")
    if pattern == "":
        raise Refusal("pattern is empty. Give a regular expression to search for.")
    real = resolve(text_arg(args, "path", required=False) or ".", must_exist=True)
    command = ["rg", "--json", "--hidden", "--glob", "!.git", "--max-columns", "2000",
               "--regexp", pattern, "--", os.path.relpath(real, WORKSPACE)]
    try:
        process = subprocess.Popen(command, cwd=WORKSPACE, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, start_new_session=True)
    except FileNotFoundError:
        raise Refusal("Search is not available in this pod: ripgrep (rg) is not installed.")
    try:
        out, err = process.communicate(timeout=SEARCH_TIMEOUT)
    except subprocess.TimeoutExpired:
        stop(process)
        process.communicate()
        raise Refusal("The search took longer than %d seconds. Search a smaller folder with path." % SEARCH_TIMEOUT)
    if process.returncode not in (0, 1):
        reason = err.decode("utf-8", errors="replace").strip() or "ripgrep ended with code %d" % process.returncode
        raise Refusal("The search failed: %s" % reason[:1000])
    matches = []
    total = 0
    for line in out.decode("utf-8", errors="replace").splitlines():
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
        path = (data.get("path", {}).get("text") or "")
        matches.append({"path": path[2:] if path.startswith("./") else path,
                        "line": data.get("line_number"), "text": text[:MATCH_TEXT_LIMIT]})
    result = {"matches": matches, "truncated": total > MAX_MATCHES}
    if total > MAX_MATCHES:
        result["note"] = ("Only the first %d of %d matches are shown. "
                          "Use a narrower pattern or a path." % (MAX_MATCHES, total))
    return result


# ---------------------------------------------------------------- commands

def cut(text):
    """Cut long output to OUTPUT_LIMIT characters: the start and the end, with a note between."""
    if len(text) <= OUTPUT_LIMIT:
        return text
    half = OUTPUT_LIMIT // 2
    note = "\n[output cut: %d characters in total, showing the first %d and the last %d]\n" % (len(text), half, half)
    return text[:half] + note + text[-half:]


def stop(process):
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        process.kill()


def command_environment():
    env = dict(os.environ)
    env.pop("POD_TOKEN", None)
    env.update({"GIT_TERMINAL_PROMPT": "0", "GIT_PAGER": "cat", "PAGER": "cat", "TERM": "dumb"})
    env.setdefault("HOME", "/home/dev")
    return env


def run_command(args):
    command = text_arg(args, "command")
    if command.strip() == "":
        raise Refusal("command is empty. Give the command to run.")
    timeout = int_arg(args, "timeout_seconds")
    if timeout is None:
        timeout = DEFAULT_TIMEOUT
    if timeout < 1:
        raise Refusal("timeout_seconds must be between 1 and %d." % MAX_TIMEOUT)
    timeout = min(timeout, MAX_TIMEOUT)
    # Temporary files instead of pipes: a process left in the background cannot block the answer.
    with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
        process = subprocess.Popen(["bash", "-lc", command], cwd=WORKSPACE, stdin=subprocess.DEVNULL,
                                   stdout=out, stderr=err, env=command_environment(), start_new_session=True)
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
    result = {"exit_code": code, "stdout": cut(stdout), "stderr": cut(stderr)}
    if timed_out:
        result["timed_out"] = True
        result["stderr"] = cut(stderr) + ("\n" if stderr and not stderr.endswith("\n") else "") + (
            "[The command was stopped after %d seconds (the time limit). "
            "Run a shorter command or give a larger timeout_seconds, at most %d.]" % (timeout, MAX_TIMEOUT))
    return result


# ---------------------------------------------------------------- skill scripts

class OutputWindow:
    """Keep only the tail of one output stream without ever retaining it in full."""

    def __init__(self, max_bytes):
        self.max_bytes = max_bytes
        self.data = bytearray()
        self.cut = False
        self.starts_midline = False

    def add(self, chunk):
        self.data.extend(chunk)
        if len(self.data) <= self.max_bytes:
            return
        remove = len(self.data) - self.max_bytes
        dropped = self.data[:remove]
        del self.data[:remove]
        self.cut = True
        self.starts_midline = dropped[-1:] != b"\n"

    def text(self, max_lines):
        data = bytes(self.data)
        _, newline, remaining = data.partition(b"\n")
        if self.starts_midline and newline and remaining:
            data = remaining
        lines = data.splitlines(keepends=True)
        if len(lines) > max_lines:
            lines = lines[-max_lines:]
            self.cut = True
        return b"".join(lines).decode("utf-8", errors="replace")


def _read_stream(stream, window):
    try:
        while True:
            chunk = stream.read(8192)
            if not chunk:
                return
            window.add(chunk)
    finally:
        stream.close()


def _skill_files(args, root):
    files = args.get("files")
    if not isinstance(files, dict) or not files:
        raise Refusal("The skill has no files, so its script cannot be run. Fix the stored skill version.")
    script = text_arg(args, "script")
    for path, content in files.items():
        if (not isinstance(path, str) or not path or os.path.isabs(path) or "\x00" in path
                or any(part in ("", ".", "..") for part in path.split("/"))):
            raise Refusal("The skill file path %r is invalid. Fix the stored skill version." % path)
        if not isinstance(content, str):
            raise Refusal("The skill file %s is not text. Fix the stored skill version." % path)
        target = os.path.join(root, *path.split("/"))
        os.makedirs(os.path.dirname(target), exist_ok=True)
        encoded = content.encode("utf-8")
        with open(target, "wb") as handle:
            handle.write(encoded)
        if encoded.startswith(b"#!"):
            os.chmod(target, 0o700)
    return script


def run_skill_script(args):
    timeout = int_arg(args, "timeout_seconds")
    max_lines = int_arg(args, "max_lines")
    max_bytes = int_arg(args, "max_bytes")
    if timeout is None or timeout < 1:
        raise Refusal("timeout_seconds must be a positive whole number.")
    if max_lines is None or max_lines < 1:
        raise Refusal("max_lines must be a positive whole number.")
    if max_bytes is None or max_bytes < 1:
        raise Refusal("max_bytes must be a positive whole number.")
    arguments = args.get("args", [])
    if not isinstance(arguments, list) or any(not isinstance(item, str) for item in arguments):
        raise Refusal("args must be a list of text arguments.")

    with tempfile.TemporaryDirectory(prefix="factory-skill-", dir="/tmp") as root:
        script = _skill_files(args, root)
        executable = os.path.join(root, *script.split("/"))
        if not os.path.isfile(executable):
            raise Refusal("The skill script %s does not exist. Choose a script listed by the skill." % script)
        with open(executable, "rb") as handle:
            if not handle.read(2).startswith(b"#!"):
                raise Refusal("The skill script %s could not be started: missing a shebang line. "
                              "Fix the script and save a new skill version." % script)
        stdout = OutputWindow(max_bytes)
        stderr = OutputWindow(max_bytes)
        try:
            process = subprocess.Popen([executable] + arguments, cwd=WORKSPACE, stdin=subprocess.DEVNULL,
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                       env=command_environment(), start_new_session=True)
        except OSError as error:
            reason = "missing a shebang line" if error.errno == 8 else str(error)
            raise Refusal("The skill script %s could not be started: %s. Fix the script and save a new skill version."
                          % (script, reason))
        readers = [threading.Thread(target=_read_stream, args=(process.stdout, stdout), daemon=True),
                   threading.Thread(target=_read_stream, args=(process.stderr, stderr), daemon=True)]
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
        return {"exit_code": code, "stdout": stdout.text(max_lines), "stderr": stderr.text(max_lines),
                "stdout_cut": stdout.cut, "stderr_cut": stderr.cut, "timed_out": timed_out}


OPS = {"list_files": list_files, "read_file": read_file, "write_file": write_file,
       "edit_file": edit_file, "search": search, "run_command": run_command,
       "run_skill_script": run_skill_script}


def handle_op(body):
    name = body.get("op")
    args = body.get("args")
    if args is None:
        args = {}
    if name not in OPS:
        raise Refusal("Unknown op %r. The ops are: %s." % (name, ", ".join(sorted(OPS))))
    if not isinstance(args, dict):
        raise Refusal("args must be a JSON object.")
    return {"ok": True, "result": OPS[name](args)}


def handle_skill_script(body):
    if body.get("op") != "run_skill_script":
        raise Refusal("This endpoint runs skill scripts. Use /op for other operations.")
    return handle_op(body)


def main():
    token = take_token()
    if not token:
        sys.stderr.write("POD_TOKEN is not set; every call will be refused.\n")
    server = serve(int(os.environ.get("PORT", "8080")), token, {"/op": handle_op, "/skill-script": handle_skill_script},
                   unbounded_paths=("/skill-script",))
    server.serve_forever()


if __name__ == "__main__":
    main()
