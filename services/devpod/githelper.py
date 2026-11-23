"""git-helper of a dev pod (port 8081): the only container that sees the credential.

POST /clone, /protect, /push, /patch and /git, each answering 200 with a JSON object
that has "ok" and, when ok is false, "error" with a sentence the caller can act on.
Standard library only.

Safety rules of this program:
- The agent controls /workspace, including .git/config and .git/hooks. Every git
  command that reads /workspace therefore runs as the work user (uid 1000) with an
  environment that holds no credential.
- The credential is used in two places only: the clone into the still empty
  /workspace, and the push, which runs in a private repository of this container
  that received the branch as a bundle. It is passed as a one-off http.extraHeader
  through the environment and is never written to a config file.
"""
import base64
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from podhttp import Refusal, serve, take_token  # noqa: E402

WORKSPACE = os.path.realpath(os.environ.get("DEVPOD_WORKSPACE", "/workspace"))
CREDENTIAL_FILE = os.environ.get("DEVPOD_CREDENTIAL_FILE", "/credential/token")
STATE_DIR = os.environ.get("GITHELPER_STATE_DIR", "/var/lib/githelper")
HELPER_HOME = os.environ.get("GITHELPER_HOME", "/tmp/githelper-home")
WORK_UID = 1000
GUARD_UID = 2000
GIT_TIMEOUT = 300
VIEW_LIMIT = 400000
READ_ONLY = ("log", "diff", "show", "ls-files", "rev-parse")
DEFAULT_AUTHOR = ("Agent", "agent@opensoftwarefactory.local")

_can_demote = None


# ---------------------------------------------------------------- running git

def can_demote():
    """True when this process is root and may start a process as the work user."""
    global _can_demote
    if _can_demote is None:
        if os.geteuid() != 0:
            _can_demote = False
        else:
            try:
                subprocess.run(["git", "--version"], user=WORK_UID, group=WORK_UID, extra_groups=[],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
                _can_demote = True
            except (PermissionError, subprocess.SubprocessError, OSError):
                _can_demote = False
    return _can_demote


def git(args, cwd, as_work, config=(), text=True, timeout=GIT_TIMEOUT):
    """Run git. Returns (exit code, stdout, stderr). `config` is a list of (key, value)."""
    pairs = [("safe.directory", "*")] + list(config)
    env = {"PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"), "HOME": HELPER_HOME,
           "LANG": "C.UTF-8", "GIT_TERMINAL_PROMPT": "0", "GIT_PAGER": "cat", "GIT_ASKPASS": "true",
           "GIT_ALLOW_PROTOCOL": "http:https:file", "GIT_CONFIG_GLOBAL": "/dev/null"}
    kwargs = {}
    if as_work and os.geteuid() == 0:
        if can_demote():
            kwargs = {"user": WORK_UID, "group": WORK_UID, "extra_groups": []}
        else:
            # Root without the right to change user: at least never run code from the repository.
            pairs += [("core.hooksPath", "/dev/null"), ("core.fsmonitor", "false")]
    env["GIT_CONFIG_COUNT"] = str(len(pairs))
    for index, (key, value) in enumerate(pairs):
        env["GIT_CONFIG_KEY_%d" % index] = key
        env["GIT_CONFIG_VALUE_%d" % index] = value
    try:
        done = subprocess.run(["git", "--no-pager"] + list(args), cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, **kwargs)
    except subprocess.TimeoutExpired:
        raise Refusal("git %s took longer than %d seconds and was stopped." % (args[0], timeout))
    out, err = done.stdout, done.stderr
    if text:
        out = out.decode("utf-8", errors="replace")
    return done.returncode, out, err.decode("utf-8", errors="replace")


def credential():
    """The token of the mounted credential, or '' when the repository has none."""
    try:
        with open(CREDENTIAL_FILE, encoding="utf-8") as handle:
            return handle.read().strip()
    except (FileNotFoundError, NotADirectoryError, IsADirectoryError):
        return ""


def auth(url):
    """Config pairs that send the credential to the host of `url` only, and the texts to hide."""
    token = credential()
    parts = urlsplit(url)
    if not token or parts.scheme not in ("http", "https"):
        return [], []
    basic = base64.b64encode(("x-access-token:" + token).encode("utf-8")).decode("ascii")
    key = "http.%s://%s/.extraHeader" % (parts.scheme, parts.netloc)
    return [(key, "Authorization: Basic " + basic)], [token, basic]


def scrub(text, secrets):
    for secret in secrets:
        if secret:
            text = text.replace(secret, "***")
    return text


def last_line(text):
    lines = [line.strip() for line in text.splitlines() if line.strip()]
    return lines[-1] if lines else "git gave no message"


# ---------------------------------------------------------------- checks on input

def text_field(body, name, required=True):
    value = body.get(name)
    if value is None or value == "":
        if required:
            raise Refusal("The field %s is required." % name)
        return ""
    if not isinstance(value, str):
        raise Refusal("The field %s must be text." % name)
    return value


def check_branch(name, field):
    if (not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._/-]*", name) or ".." in name or "//" in name
            or name.endswith(("/", ".", ".lock"))):
        raise Refusal("%s %r is not a valid branch name. Use letters, digits, '.', '_', '-' and '/'." % (field, name))
    return name


def check_url(url):
    parts = urlsplit(url)
    if url.startswith("-") or any(ch.isspace() for ch in url):
        raise Refusal("The repository URL %r is not valid." % url)
    if parts.scheme in ("http", "https"):
        if "@" in parts.netloc:
            raise Refusal("The repository URL contains a user name or token. "
                          "Remove it from the URL and store the token as the repository's credential.")
        return url
    if parts.scheme == "file" or (parts.scheme == "" and url.startswith("/")):
        return url
    raise Refusal("The repository URL %r is not supported. Use an http or https URL." % url)


def check_revision(value, field):
    if value.startswith("-") or any(ch.isspace() or ord(ch) < 32 for ch in value):
        raise Refusal("%s %r is not a valid commit or branch." % (field, value))
    return value


# ---------------------------------------------------------------- state of this pod

def load_state():
    try:
        with open(os.path.join(STATE_DIR, "state.json"), encoding="utf-8") as handle:
            return json.load(handle)
    except (FileNotFoundError, ValueError):
        return {}


def save_state(state):
    os.makedirs(STATE_DIR, mode=0o700, exist_ok=True)
    with open(os.path.join(STATE_DIR, "state.json"), "w", encoding="utf-8") as handle:
        json.dump(state, handle)


def has_clone():
    return os.path.exists(os.path.join(WORKSPACE, ".git"))


def need_clone():
    if not has_clone():
        raise Refusal("The pod has no clone yet. The repository must be cloned first.")


def give_to_work_user(path):
    """Make `path` and everything below it owned by the work user (only root can)."""
    if os.geteuid() != 0:
        return
    os.lchown(path, WORK_UID, WORK_UID)
    for folder, folders, files in os.walk(path):
        for name in folders + files:
            os.lchown(os.path.join(folder, name), WORK_UID, WORK_UID)


def empty_workspace():
    for name in os.listdir(WORKSPACE):
        target = os.path.join(WORKSPACE, name)
        if os.path.isdir(target) and not os.path.islink(target):
            shutil.rmtree(target, ignore_errors=True)
        else:
            try:
                os.unlink(target)
            except OSError:
                pass


# ---------------------------------------------------------------- /clone

def clone(body):
    url = check_url(text_field(body, "url"))
    source = check_branch(text_field(body, "source_branch"), "source_branch")
    work = check_branch(text_field(body, "work_branch"), "work_branch")
    name = text_field(body, "author_name", required=False) or DEFAULT_AUTHOR[0]
    email = text_field(body, "author_email", required=False) or DEFAULT_AUTHOR[1]
    if work == source:
        raise Refusal("The work branch %r is the same as the source branch. "
                      "Choose another name for the work branch." % work)
    if has_clone():
        state = load_state()
        code, head, _ = git(["rev-parse", "HEAD"], WORKSPACE, as_work=True)
        if state.get("url") == url and state.get("work_branch") == work and code == 0:
            return {"ok": True, "start_commit": state.get("start_commit") or head.strip(),
                    "output": "The repository is already cloned and on the work branch %s." % work}
        raise Refusal("The pod already holds a clone of another repository or work branch. "
                      "Use a new pod for this repository and work branch.")
    os.makedirs(WORKSPACE, exist_ok=True)
    if os.listdir(WORKSPACE):
        raise Refusal("/workspace is not empty and holds no clone. Use a new pod.")

    config, secrets = auth(url)
    code, out, err = git(["clone", "--branch", source, "--", url, WORKSPACE], "/", as_work=False, config=config)
    output = scrub(out + err, secrets)
    if code != 0:
        empty_workspace()
        return {"ok": False, "error": clone_error(output, source), "output": output}
    give_to_work_user(WORKSPACE)

    code, _, _ = git(["rev-parse", "--verify", "--quiet", "refs/remotes/origin/%s^{commit}" % work],
                     WORKSPACE, as_work=True)
    existed = code == 0
    checkout = ["checkout", "-b", work, "--track", "origin/" + work] if existed else ["checkout", "-b", work]
    steps = [checkout, ["config", "user.name", name], ["config", "user.email", email]]
    for step in steps:
        code, out, err = git(step, WORKSPACE, as_work=True)
        output += out + err
        if code != 0:
            return {"ok": False, "error": "The clone worked, but git %s failed: %s" % (step[0], last_line(out + err)),
                    "output": output}
    code, head, err = git(["rev-parse", "HEAD"], WORKSPACE, as_work=True)
    if code != 0:
        return {"ok": False, "error": "The clone has no commit: %s" % last_line(err), "output": output}
    start = head.strip()
    save_state({"url": url, "source_branch": source, "work_branch": work, "start_commit": start})
    return {"ok": True, "start_commit": start, "output": output, "work_branch": work,
            "work_branch_existed": existed}


def clone_error(output, source):
    lowered = output.lower()
    if "remote branch" in lowered and "not found" in lowered:
        return "The source branch %r does not exist in the repository. Check the branch name." % source
    if is_refused(lowered):
        return ("The repository refused access. Check the repository URL and its credential on the "
                "Connectors page.")
    if "could not resolve host" in lowered or "failed to connect" in lowered or "couldn't connect" in lowered:
        return "The repository host cannot be reached from the pod: %s" % last_line(output)
    if "not found" in lowered or "does not appear to be a git repository" in lowered or "does not exist" in lowered:
        return "No repository was found at this URL. Check the repository URL on the Connectors page."
    return "The clone failed: %s" % last_line(output)


def is_refused(lowered):
    signs = ("authentication failed", "could not read username", "could not read password", "terminal prompts disabled",
             "returned error: 401", "returned error: 403", "permission to", "access denied")
    return any(sign in lowered for sign in signs)


# ---------------------------------------------------------------- /protect

def normal_pattern(pattern):
    text = pattern.strip()
    while text.startswith("./"):
        text = text[2:]
    return text.lstrip("/")


def glob_regex(pattern):
    """Translate a glob with `**` into a regular expression over a path with `/`."""
    text = normal_pattern(pattern)
    out = []
    index = 0
    while index < len(text):
        if text.startswith("**/", index):
            out.append("(?:.*/)?")
            index += 3
        elif text.startswith("**", index):
            out.append(".*")
            index += 2
        elif text[index] == "*":
            out.append("[^/]*")
            index += 1
        elif text[index] == "?":
            out.append("[^/]")
            index += 1
        else:
            out.append(re.escape(text[index]))
            index += 1
    return re.compile("".join(out) + "/?")


def plan_protection(root, patterns):
    """Decide what `patterns` protect below `root`.

    Returns (folders, files, holders, unmatched), paths relative to root with `/`:
    folders and files become owned by the guard user; holders are the folders above
    them, which get the sticky bit so that their protected entries cannot be renamed
    or deleted. A matched folder is protected with everything in it. `.git` is never
    touched.
    """
    compiled = [(pattern, glob_regex(pattern)) for pattern in patterns]
    used = set()

    def matches(relative, is_dir):
        hit = False
        for pattern, regex in compiled:
            if regex.fullmatch(relative) or (is_dir and regex.fullmatch(relative + "/")):
                used.add(pattern)
                hit = True
        return hit

    folders, files = [], []

    def walk(folder, relative, inside):
        for name in sorted(os.listdir(folder)):
            if name == ".git":
                continue
            path = os.path.join(folder, name)
            rel = name if relative == "" else relative + "/" + name
            is_dir = os.path.isdir(path) and not os.path.islink(path)
            protected = matches(rel, is_dir) or inside
            if is_dir:
                if protected:
                    folders.append(rel)
                walk(path, rel, protected)
            elif protected:
                files.append(rel)

    walk(root, "", False)
    protected_folders = set(folders)
    holders = set()
    for rel in folders + files:
        parent = rel.rsplit("/", 1)[0] if "/" in rel else ""
        while parent not in protected_folders and parent not in holders:
            holders.add(parent)
            if parent == "":
                break
            parent = parent.rsplit("/", 1)[0] if "/" in parent else ""
    unmatched = [pattern for pattern, _ in compiled if pattern not in used]
    return folders, files, sorted(holders), unmatched


def apply_protection(root, folders, files, holders, chown=os.lchown):
    """Carry out a plan of plan_protection. `chown` is replaceable for tests without root."""
    for rel in holders:
        path = os.path.join(root, rel) if rel else root
        chown(path, GUARD_UID, GUARD_UID)
        os.chmod(path, 0o1777)
    for rel in folders:
        path = os.path.join(root, rel)
        chown(path, GUARD_UID, GUARD_UID)
        os.chmod(path, 0o755)
    for rel in files:
        path = os.path.join(root, rel)
        chown(path, GUARD_UID, GUARD_UID)
        if os.path.islink(path):
            continue
        mode = stat.S_IMODE(os.lstat(path).st_mode)
        new = (mode | 0o444) & ~0o022 & 0o777
        if mode & 0o111:
            new |= 0o111
        os.chmod(path, new)


def protect(body):
    paths = body.get("paths")
    if not isinstance(paths, list) or not all(isinstance(item, str) and item.strip() for item in paths):
        raise Refusal('The field paths must be a list of glob patterns, for example ["tests/**", "verify.sh"].')
    need_clone()
    for pattern in paths:
        if normal_pattern(pattern).split("/")[0] == ".git" or ".." in pattern.split("/"):
            raise Refusal("The pattern %r is not allowed: .git and paths outside /workspace cannot be protected." % pattern)
    if os.geteuid() != 0:
        raise Refusal("The git-helper does not run as root, so it cannot protect files.")
    folders, files, holders, unmatched = plan_protection(WORKSPACE, paths)
    apply_protection(WORKSPACE, folders, files, holders)
    output = "Protected %d files and %d folders." % (len(files), len(folders))
    if unmatched:
        output += " These patterns matched nothing: %s." % ", ".join(unmatched)
    return {"ok": True, "protected": sorted(folders + files), "unmatched": unmatched, "output": output}


# ---------------------------------------------------------------- /push

def push(body):
    branch = check_branch(text_field(body, "branch"), "branch")
    state = load_state()
    url = text_field(body, "url", required=False) or state.get("url", "")
    if not url:
        raise Refusal('The git-helper does not know the repository URL of this pod. Send it as "url".')
    url = check_url(url)
    need_clone()
    if state.get("work_branch") and branch != state["work_branch"]:
        raise Refusal("Only the work branch %r of this pod can be pushed, not %r." % (state["work_branch"], branch))
    if branch == state.get("source_branch"):
        raise Refusal("The source branch %r cannot be pushed. Only the work branch can." % branch)
    ref = "refs/heads/" + branch
    code, head, _ = git(["rev-parse", "--verify", "--quiet", ref + "^{commit}"], WORKSPACE, as_work=True)
    if code != 0:
        raise Refusal("The branch %r does not exist in the pod. Commit on the work branch first." % branch)
    commit = head.strip()
    notes = ""
    code, status, _ = git(["status", "--porcelain"], WORKSPACE, as_work=True)
    changed = len([line for line in status.splitlines() if line.strip()]) if code == 0 else 0
    if changed:
        notes = "\nNote: %d files have changes that are not committed. They were not pushed." % changed

    with tempfile.TemporaryDirectory(prefix="push-") as private:
        # The branch leaves the agent's repository as data; the push runs in a repository
        # whose config and hooks the agent cannot reach.
        code, bundle, err = git(["bundle", "create", "-", ref], WORKSPACE, as_work=True, text=False)
        if code != 0:
            return {"ok": False, "error": "The branch could not be read from the pod: %s" % last_line(err),
                    "output": err}
        bundle_file = os.path.join(private, "branch.bundle")
        with open(bundle_file, "wb") as handle:
            handle.write(bundle)
        repo = os.path.join(private, "repo.git")
        for step in (["init", "--quiet", "--bare", repo], ["-C", repo, "fetch", "--quiet", bundle_file, ref + ":" + ref]):
            code, out, err = git(step, private, as_work=False)
            if code != 0:
                return {"ok": False, "error": "The branch could not be prepared for the push: %s" % last_line(out + err),
                        "output": out + err}
        config, secrets = auth(url)
        code, out, err = git(["-C", repo, "push", url, ref + ":" + ref], private, as_work=False, config=config)
    output = scrub(out + err, secrets)
    if code != 0:
        return {"ok": False, "error": push_error(output, branch), "output": output}
    git(["update-ref", "refs/remotes/origin/" + branch, commit], WORKSPACE, as_work=True)
    return {"ok": True, "output": (output.strip() + notes).strip(), "branch": branch, "commit": commit}


def push_error(output, branch):
    lowered = output.lower()
    if "non-fast-forward" in lowered or "fetch first" in lowered:
        return ("The repository already has other commits on the branch %r, so the push was refused. "
                "A person must look at the branch in the repository." % branch)
    if is_refused(lowered):
        return ("The repository refused the push: it has no credential that may write. A person must store "
                "a read-and-write credential for the repository on the Connectors page. The commits stay in the pod.")
    if "could not resolve host" in lowered or "failed to connect" in lowered or "couldn't connect" in lowered:
        return "The repository host cannot be reached from the pod: %s" % last_line(output)
    return "The push failed: %s" % last_line(output)


# ---------------------------------------------------------------- /patch and /git

def patch(body):
    base = check_revision(text_field(body, "base"), "base")
    need_clone()
    code, _, _ = git(["rev-parse", "--verify", "--quiet", base + "^{commit}"], WORKSPACE, as_work=True)
    if code != 0:
        raise Refusal("The base %r is not a commit or branch of this repository." % base)
    span = base + "..HEAD"
    code, text, err = git(["format-patch", "--stdout", span], WORKSPACE, as_work=True)
    if code != 0:
        return {"ok": False, "error": "The patch could not be made: %s" % last_line(err)}
    code, log, err = git(["log", "--reverse", "--format=%H%x1f%s%x1f%an", span], WORKSPACE, as_work=True)
    if code != 0:
        return {"ok": False, "error": "The commits could not be listed: %s" % last_line(err)}
    commits = []
    for line in log.splitlines():
        fields = line.split("\x1f")
        if len(fields) == 3:
            commits.append({"hash": fields[0], "subject": fields[1], "author": fields[2]})
    return {"ok": True, "patch": text, "commits": commits}


def view(body):
    args = body.get("args")
    if not isinstance(args, list) or not args or not all(isinstance(item, str) for item in args):
        raise Refusal("The field args must be a list of texts that starts with a git subcommand.")
    if args[0] not in READ_ONLY:
        raise Refusal("git %s is not allowed here. Only these read-only subcommands are: %s."
                      % (args[0], ", ".join(READ_ONLY)))
    for item in args[1:]:
        if item.startswith(("--output", "--ext-diff", "--textconv", "--no-index", "--exec")) or "\x00" in item:
            raise Refusal("The option %s is not allowed in a read-only view." % item)
    need_clone()
    command = [args[0]]
    if args[0] in ("log", "diff", "show"):
        command += ["--no-ext-diff", "--no-textconv"]
    code, out, err = git(["--no-optional-locks"] + command + args[1:], WORKSPACE, as_work=True)
    if len(out) > VIEW_LIMIT:
        out = out[:VIEW_LIMIT] + "\n[output cut: showing the first %d of %d characters]\n" % (VIEW_LIMIT, len(out))
    if code != 0:
        return {"ok": False, "error": "git %s failed: %s" % (args[0], last_line(err or out)), "output": out + err}
    return {"ok": True, "output": out}


ROUTES = {"/clone": clone, "/protect": protect, "/push": push, "/patch": patch, "/git": view}


def main():
    token = take_token()
    if not token:
        sys.stderr.write("POD_TOKEN is not set; every call will be refused.\n")
    os.makedirs(HELPER_HOME, exist_ok=True)
    server = serve(int(os.environ.get("PORT", "8081")), token, ROUTES)
    server.serve_forever()


if __name__ == "__main__":
    main()
