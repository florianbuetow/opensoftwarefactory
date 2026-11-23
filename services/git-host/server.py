"""Git host: serves bare repositories over HTTP with git http-backend.

Standard library only. At start it optionally runs a seed script mounted outside
the service image, then it serves /<name>.git/... for clone, fetch and push.
Push needs no credential: this host is for the course exercises only.

GET /healthz answers "ok"; GET / and /browse/<name>.git provide a read-only
repository browser for people. Git's smart HTTP endpoints stay unchanged.
"""

import html
import io
import os
import re
import subprocess
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, unquote, urlencode


def required_env(name: str) -> str:
    """Return the value of an environment variable; report it and exit with code 1 when it is missing or empty."""
    value = os.environ.get(name)
    if not value:
        sys.stderr.write(f"The environment variable {name} is missing or empty.\n")
        sys.exit(1)
    return value


DATA = required_env("GIT_HOST_DATA")
SEED_SCRIPT = os.path.join(required_env("SEED_DIR"), "seed.sh")
PATH = required_env("PATH")
HOME = required_env("HOME")
MAX_BODY = 256 * 1024 * 1024
REPOSITORY_PATH = re.compile(r"/[A-Za-z0-9][A-Za-z0-9._-]*\.git/[A-Za-z0-9._/-]+")
BROWSE_PATH = re.compile(r"/browse/([A-Za-z0-9][A-Za-z0-9._-]*)\.git")
MAX_PREVIEW = 512 * 1024


def git(repo: str, *args: str) -> bytes:
    """Run a git command in a repository and return its output."""
    return subprocess.run(["git", "-C", repo, *args], capture_output=True, check=True).stdout


def safe_path(path: str) -> bool:
    """Tell whether a path stays inside the repository tree."""
    return len(path) <= 1024 and bool(path) and not path.startswith("/") and all(part not in ("", ".", "..") for part in path.split("/"))


def link(base: str, **params: str) -> str:
    """Build an HTML-escaped link with query parameters."""
    query = urlencode(params)
    return html.escape(base + ("?" + query if query else ""), quote=True)


def page(title: str, body: str, home: str = "../") -> str:
    """Wrap a body in the HTML page layout."""
    return (
        '<!doctype html><html lang="en"><meta charset="utf-8">'
        '<meta name="viewport" content="width=device-width,initial-scale=1">'
        f"<title>{html.escape(title)}</title><style>"
        ":root{color-scheme:light dark;font:14px/1.45 -apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif}"
        "*{box-sizing:border-box}body{margin:0;background:#f4f6f8;color:#161b24}"
        "main{max-width:1100px;margin:auto;padding:32px 24px}header{background:#eceff3;border-bottom:1px solid #d9dee6;padding:16px 24px}"
        "header a{font-weight:600}h1{font-size:24px;line-height:1.25;margin:12px 0 24px}h2{font-size:16px;margin:24px 0 12px}"
        "a{color:#2f6fde;text-decoration:none}a:hover{text-decoration:underline}"
        "table{width:100%;border-collapse:collapse;background:#fff;border:1px solid #d9dee6}"
        "th,td{text-align:left;padding:10px 16px;border-bottom:1px solid #d9dee6;vertical-align:top}th{font-size:12px;color:#5b6675}"
        "tr:last-child td{border-bottom:0}.muted{color:#5b6675}.mono,pre{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}"
        "nav{display:flex;gap:16px;margin-bottom:16px}nav .current{font-weight:600;color:inherit}"
        "pre{background:#fff;border:1px solid #d9dee6;padding:16px;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere}"
        "aside{display:flex;gap:8px;flex-wrap:wrap;margin:16px 0}"
        "aside a{padding:5px 10px;border:1px solid #d9dee6;border-radius:6px;background:#fff}"
        "aside .current{border-color:#2f6fde}"
        "@media(prefers-color-scheme:dark){body{background:#12161c;color:#e7ebf2}header{background:#151a21;border-color:#2a3140}"
        "table,pre,aside a{background:#1a1f27;border-color:#2a3140}th,td{border-color:#2a3140}th,.muted{color:#9aa5b5}a{color:#6b9cf5}}"
        f'</style><header><a href="{home}">Git host</a></header><main>' + body + "</main></html>"
    )


def branch_names(repo: str) -> list[str]:
    """List the branches of a repository."""
    return [
        line.split("\x00", 1)[0]
        for line in git(repo, "for-each-ref", "--format=%(refname:short)%00%(objectname)", "refs/heads")
        .decode("utf-8", "replace")
        .splitlines()
    ]


def commits_view(repo: str, ref: str, commit: str) -> str:
    """Render the table of recent commits."""
    rows: list[str] = []
    for line in git(repo, "log", "-n", "50", "--format=%H%x00%h%x00%aI%x00%an%x00%s", commit).decode("utf-8", "replace").splitlines():
        full, short, date, author, subject = (line.split("\x00", 4) + [""] * 5)[:5]
        rows.append(
            f'<tr><td><a href="{link("", view="commit", ref=ref, commit=full)}">{html.escape(subject)}</a></td>'
            f'<td class="mono" title="{html.escape(full)}">{html.escape(short)}</td>'
            f"<td>{html.escape(author)}</td><td>{html.escape(date[:10])}</td></tr>"
        )
    return (
        "<table><thead><tr><th>Commit</th><th>Hash</th><th>Author</th><th>Date</th></tr></thead><tbody>"
        + "".join(rows)
        + "</tbody></table>"
    )


def commit_view(repo: str, ref: str, commit: str, selected: str) -> str:
    """Render the patch of one recent commit."""
    visible = git(repo, "rev-list", "-n", "50", commit).decode().splitlines()
    if selected not in visible:
        raise ValueError("No such commit in recent history")
    patch = git(repo, "show", "--format=fuller", "--stat", "--patch", "--no-ext-diff", selected)
    body = f'<p><a href="{link("", view="commits", ref=ref)}">Recent commits</a></p>'
    body += "<pre>" + html.escape(patch[:MAX_PREVIEW].decode("utf-8", "replace")) + "</pre>"
    if len(patch) > MAX_PREVIEW:
        body += '<p class="muted">Patch preview truncated.</p>'
    return body


def file_view(repo: str, commit: str, path: str) -> str:
    """Render the preview of one file."""
    if not safe_path(path):
        raise ValueError("Invalid file path")
    oid = git(repo, "rev-parse", "--verify", f"{commit}:{path}").decode().strip()
    if git(repo, "cat-file", "-t", oid).strip() != b"blob":
        raise ValueError("Not a file")
    size = int(git(repo, "cat-file", "-s", oid))
    body = f"<h2>{html.escape(path)}</h2>"
    if size > MAX_PREVIEW:
        return body + '<p class="muted">File is too large to preview.</p>'
    data = git(repo, "cat-file", "blob", oid)
    return body + (
        '<p class="muted">Binary file</p>' if b"\x00" in data else "<pre>" + html.escape(data.decode("utf-8", "replace")) + "</pre>"
    )


def files_view(repo: str, ref: str, commit: str, path: str) -> str:
    """Render the listing of one directory."""
    if path and not safe_path(path):
        raise ValueError("Invalid directory path")
    treeish = commit + (":" + path if path else "")
    entries = git(repo, "ls-tree", "-z", treeish).split(b"\x00")
    body = f'<p><a href="{link("", ref=ref)}">Root</a> / {html.escape(path)}</p>' if path else ""
    rows: list[str] = []
    for entry in entries:
        if not entry:
            continue
        meta, raw_name = entry.split(b"\t", 1)
        _, kind, _ = meta.decode().split(" ", 2)
        filename = raw_name.decode("utf-8", "replace")
        child = path + "/" + filename if path else filename
        target = link("", view="files" if kind == "tree" else "file", ref=ref, path=child)
        rows.append(
            f'<tr><td><a href="{target}">{html.escape(filename)}{"/" if kind == "tree" else ""}</a></td>'
            f'<td class="muted">{"Folder" if kind == "tree" else "File"}</td></tr>'
        )
    return body + "<table><thead><tr><th>Name</th><th>Type</th></tr></thead><tbody>" + "".join(rows) + "</tbody></table>"


def view_body(repo: str, query: dict[str, list[str]], view: str, ref: str, commit: str) -> str:
    """Render the part of the repository page that depends on the view."""
    if view == "commits":
        return commits_view(repo, ref, commit)
    if view == "commit":
        return commit_view(repo, ref, commit, query.get("commit", [""])[0])
    path = query.get("path", [""])[0]
    if view == "file":
        return file_view(repo, commit, path)
    return files_view(repo, ref, commit, path)


def repository_page(repo: str, name: str, query: dict[str, list[str]]) -> str:
    """Render the browser page of a repository for the requested view."""
    refs = branch_names(repo)
    default = git(repo, "symbolic-ref", "--short", "HEAD").decode().strip()
    ref = query.get("ref", [default])[0]
    if ref not in refs:
        raise ValueError("No such branch")
    view = query.get("view", ["files"])[0]
    if view not in ("files", "commits", "file", "commit"):
        raise ValueError("No such view")
    commit = git(repo, "rev-parse", "--verify", f"refs/heads/{ref}^{{commit}}").decode().strip()
    title = html.escape(name)
    branch_links = "".join(
        f'<a class="{"current" if branch == ref else ""}" href="{link("", ref=branch)}">{html.escape(branch)}</a>' for branch in refs
    )
    tabs = (
        f'<nav><a class="{"current" if view in ("files", "file") else ""}" href="{link("", ref=ref)}">Files</a>'
        f'<a class="{"current" if view in ("commits", "commit") else ""}" href="{link("", view="commits", ref=ref)}">Commits</a></nav>'
    )
    body = f'<a href="../">All repositories</a><h1>{title}</h1><div class="muted mono">{html.escape(commit[:12])}</div>'
    body += f'<aside aria-label="Branches">{branch_links}</aside>{tabs}'
    body += view_body(repo, query, view, ref, commit)
    return page(name + " · Git host", body)


def read_chunked(stream: io.BufferedIOBase, limit: int) -> bytes:
    """Read a request body sent with Transfer-Encoding: chunked."""
    parts: list[bytes] = []
    total = 0
    while True:
        size = int(stream.readline().split(b";", 1)[0].strip() or b"0", 16)
        if size == 0:
            while stream.readline().strip():
                pass
            return b"".join(parts)
        total += size
        if total > limit:
            raise ValueError("body too large")
        parts.append(stream.read(size))
        stream.readline()


class Handler(BaseHTTPRequestHandler):
    """Serve the repository browser and git's smart HTTP endpoints."""

    protocol_version = "HTTP/1.1"
    server_version = "git-host"

    def log_message(self, format: str, *args: object) -> None:
        """Write one access log line to stderr."""
        sys.stderr.write(f"{self.address_string()} {format % args}\n")

    def plain(self, status: int, text: str) -> None:
        """Send a plain text response."""
        data = text.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def html(self, status: int, text: str) -> None:
        """Send an HTML response."""
        data = text.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self) -> None:
        """Handle a GET request."""
        self.handle_request(b"")

    def do_POST(self) -> None:
        """Read the request body and handle a POST request."""
        try:
            if "chunked" in (self.headers.get("Transfer-Encoding") or "").lower():
                body = read_chunked(self.rfile, MAX_BODY)
            else:
                length = int(self.headers.get("Content-Length") or 0)
                if length < 0 or length > MAX_BODY:
                    raise ValueError("body too large")
                body = self.rfile.read(length)
        except ValueError:
            self.close_connection = True
            self.plain(413, "The request body is not valid or too large.\n")
            return
        self.handle_request(body)

    def handle_request(self, body: bytes) -> None:
        """Route a request to the health check, the browser or git http-backend."""
        path, _, query = self.path.partition("?")
        path = unquote(path)
        if path == "/healthz":
            self.plain(200, "ok\n")
            return
        if path == "/":
            self.repository_list()
            return
        match = BROWSE_PATH.fullmatch(path)
        if match and self.command == "GET":
            self.browse(match.group(1), query)
            return
        if not REPOSITORY_PATH.fullmatch(path) or ".." in path:
            self.plain(404, "No repository at this path. GET / lists the repositories.\n")
            return
        self.git_backend(path, query, body)

    def repository_list(self) -> None:
        """Send the page that lists all repositories."""
        names = sorted(name for name in os.listdir(DATA) if name.endswith(".git"))
        rows = "".join(f'<tr><td><a href="browse/{html.escape(name, quote=True)}">{html.escape(name)}</a></td></tr>' for name in names)
        self.html(
            200,
            page(
                "Git host",
                "<h1>Repositories</h1><table><thead><tr><th>Name</th></tr></thead><tbody>" + rows + "</tbody></table>",
                home="./",
            ),
        )

    def browse(self, name: str, query: str) -> None:
        """Send the browser page of one repository."""
        repo = os.path.join(DATA, name + ".git")
        if not os.path.isdir(repo):
            self.plain(404, "No such repository.\n")
            return
        try:
            self.html(200, repository_page(repo, name, parse_qs(query)))
        except (ValueError, subprocess.CalledProcessError):
            self.plain(404, "No such branch, file, or directory.\n")

    def backend_env(self, path: str, query: str, body: bytes) -> dict[str, str]:
        """Build the CGI environment for git http-backend."""
        env = {
            "PATH": PATH,
            "HOME": HOME,
            "GIT_PROJECT_ROOT": DATA,
            "GIT_HTTP_EXPORT_ALL": "1",
            "GATEWAY_INTERFACE": "CGI/1.1",
            "REQUEST_METHOD": self.command,
            "PATH_INFO": path,
            "QUERY_STRING": query,
            "CONTENT_TYPE": self.headers.get("Content-Type") or "",
            "CONTENT_LENGTH": str(len(body)),
            "REMOTE_ADDR": self.client_address[0],
            "REMOTE_USER": "anonymous",
        }
        if self.headers.get("Content-Encoding"):
            env["HTTP_CONTENT_ENCODING"] = self.headers["Content-Encoding"]
        if self.headers.get("Git-Protocol"):
            env["GIT_PROTOCOL"] = self.headers["Git-Protocol"]
        return env

    def git_backend(self, path: str, query: str, body: bytes) -> None:
        """Run git http-backend for the request and send its answer."""
        done = subprocess.run(["git", "http-backend"], input=body, env=self.backend_env(path, query, body), capture_output=True)
        if done.stderr:
            sys.stderr.write(done.stderr.decode("utf-8", errors="replace"))
        head, separator, payload = done.stdout.partition(b"\r\n\r\n")
        if not separator:
            head, separator, payload = done.stdout.partition(b"\n\n")
        if not separator:
            self.plain(500, "git http-backend gave no answer.\n")
            return
        status = 200
        headers: list[tuple[str, str]] = []
        for line in head.decode("latin-1").splitlines():
            name, _, value = line.partition(":")
            if name.lower() == "status":
                status = int(value.strip().split(" ", 1)[0])
            elif name and name.lower() != "content-length":
                headers.append((name.strip(), value.strip()))
        self.send_response(status)
        for name, value in headers:
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def run_seed(script: str) -> bool:
    """Run an optional externally mounted seed script. Return whether one existed."""
    if os.path.isfile(script):
        seed_env = os.environ.copy()
        seed_env["GIT_HOST_SEED"] = os.path.dirname(script) + "/assets"
        subprocess.run(["bash", script], check=True, env=seed_env)
        return True
    return False


def main() -> None:
    """Seed the repositories and serve them until stopped."""
    if not run_seed(SEED_SCRIPT):
        sys.stderr.write(f"git-host starts without seed data; {SEED_SCRIPT} is not mounted\n")
    # The server runs in a pod and must be reachable from other pods, so it listens on all interfaces.
    server = ThreadingHTTPServer(("0.0.0.0", int(required_env("PORT"))), Handler)  # nosec B104
    server.daemon_threads = True
    sys.stderr.write(f"git-host serves {DATA} on port {server.server_address[1]}\n")
    server.serve_forever()


if __name__ == "__main__":
    main()
