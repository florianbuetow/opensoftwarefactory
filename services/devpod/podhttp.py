"""Small JSON-over-HTTP server shared by the tool server and the git-helper.

Standard library only. Every request except GET /healthz must carry the header
X-Pod-Token with the value of the POD_TOKEN environment variable. Without a
configured token every call is refused.
"""

import contextlib
import ctypes
import hmac
import json
import os
import sys
from collections.abc import Callable, Collection, Mapping
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, cast

MAX_BODY_BYTES = 32 * 1024 * 1024

type JsonObject = dict[str, Any]
type Route = Callable[[JsonObject], JsonObject]


class RefusalError(Exception):
    """A request that is refused with a sentence the caller can act on."""


def required_env(name: str) -> str:
    """Return the value of an environment variable; report it and exit with code 1 when it is missing or empty."""
    value = os.environ.get(name)
    if not value:
        sys.stderr.write(f"The environment variable {name} is missing or empty.\n")
        sys.exit(1)
    return value


def text_list(value: object) -> list[str] | None:
    """The value as a list of texts, or None when it is not one."""
    if not isinstance(value, list):
        return None
    items = cast(list[object], value)
    texts = [item for item in items if isinstance(item, str)]
    return texts if len(texts) == len(items) else None


def take_token() -> str:
    """Read POD_TOKEN and remove it from the environment of every child process.

    On Linux the process is also made non-dumpable, so that a command of the
    same user cannot read the token from /proc/<pid>/environ.
    """
    token = required_env("POD_TOKEN")
    del os.environ["POD_TOKEN"]
    if sys.platform.startswith("linux"):
        with contextlib.suppress(OSError, AttributeError):
            ctypes.CDLL(None, use_errno=True).prctl(4, 0, 0, 0, 0)  # PR_SET_DUMPABLE = 0
    return token


class PodHandler(BaseHTTPRequestHandler):
    """Answer GET /healthz and the POST routes of one pod server."""

    protocol_version = "HTTP/1.1"
    server_version = "devpod"
    token: str = ""
    routes: Mapping[str, Route] = {}
    unbounded_paths: Collection[str] = ()

    def log_message(self, format: str, *args: object) -> None:
        """Write the access log line to stderr."""
        sys.stderr.write(f"{self.address_string()} {format % args}\n")

    def _send(self, status: int, payload: JsonObject) -> None:
        data = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _length(self) -> int:
        """Return the declared body length, or -1 when it is not a number."""
        try:
            return int(self.headers.get("Content-Length") or 0)
        except ValueError:
            return -1

    def _rejection(self, path: str, length: int) -> tuple[int, str] | None:
        """Return the status and message that refuse this request before its body is read, or None."""
        if length < 0 or (path not in self.unbounded_paths and length > MAX_BODY_BYTES):
            return 413, "The request body is missing a valid length or is too large."
        given = self.headers.get("X-Pod-Token") or ""
        if not self.token or not hmac.compare_digest(given.encode("utf-8"), self.token.encode("utf-8")):
            return 401, "The header X-Pod-Token is missing or wrong."
        if path not in self.routes:
            return 404, f"Unknown path {self.path}."
        return None

    def _body(self, length: int) -> JsonObject | None:
        """Read the body as a JSON object, or return None when it is not one."""
        try:
            body: object = json.loads(self.rfile.read(length).decode("utf-8") or "{}")
        except (ValueError, UnicodeDecodeError):
            return None
        return cast(JsonObject, body) if isinstance(body, dict) else None

    def do_GET(self) -> None:
        """Answer the health check; every other path is unknown."""
        if self.path == "/healthz":
            self._send(200, {"ok": True})
        else:
            self._send(404, {"ok": False, "error": f"Unknown path {self.path}."})

    def do_POST(self) -> None:
        """Check the request, call its route and answer with the route's JSON object."""
        path = self.path.split("?", 1)[0]
        length = self._length()
        rejection = self._rejection(path, length)
        if rejection is not None:
            self.close_connection = True
            self._send(rejection[0], {"ok": False, "error": rejection[1]})
            return
        body = self._body(length)
        if body is None:
            self._send(400, {"ok": False, "error": "The request body must be a JSON object."})
            return
        try:
            payload = self.routes[path](body)
        except RefusalError as refusal:
            payload = {"ok": False, "error": str(refusal)}
        except Exception as error:  # a bug or an unexpected system error; never a silent 500
            payload = {"ok": False, "error": f"The pod could not do this: {type(error).__name__}: {error}"}
        self._send(200, payload)


def make_handler(token: str, routes: Mapping[str, Route], unbounded_paths: Collection[str] = ()) -> type[PodHandler]:
    """Build a request handler. `routes` maps a POST path to a function(body) -> dict."""

    class Handler(PodHandler):
        """The handler of one server, bound to its token and routes."""

    Handler.token = token
    Handler.routes = routes
    Handler.unbounded_paths = unbounded_paths
    return Handler


# The server runs in a pod and must be reachable from other pods, so it listens on all interfaces.
def serve(
    port: int,
    token: str,
    routes: Mapping[str, Route],
    host: str = "0.0.0.0",  # nosec B104
    unbounded_paths: Collection[str] = (),
) -> ThreadingHTTPServer:
    """Create the server (not yet serving). Port 0 picks a free port."""
    server = ThreadingHTTPServer((host, port), make_handler(token, routes, unbounded_paths))
    server.daemon_threads = True
    return server
