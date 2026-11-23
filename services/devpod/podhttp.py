"""Small JSON-over-HTTP server shared by the tool server and the git-helper.

Standard library only. Every request except GET /healthz must carry the header
X-Pod-Token with the value of the POD_TOKEN environment variable. Without a
configured token every call is refused.
"""
import ctypes
import hmac
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MAX_BODY_BYTES = 32 * 1024 * 1024


class Refusal(Exception):
    """A request that is refused with a sentence the caller can act on."""


def take_token():
    """Read POD_TOKEN and remove it from the environment of every child process.

    On Linux the process is also made non-dumpable, so that a command of the
    same user cannot read the token from /proc/<pid>/environ.
    """
    token = os.environ.pop("POD_TOKEN", "")
    if sys.platform.startswith("linux"):
        try:
            ctypes.CDLL(None, use_errno=True).prctl(4, 0, 0, 0, 0)  # PR_SET_DUMPABLE = 0
        except (OSError, AttributeError):
            pass
    return token


def make_handler(token, routes, unbounded_paths=()):
    """Build a request handler. `routes` maps a POST path to a function(body) -> dict."""

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        server_version = "devpod"

        def log_message(self, fmt, *args):
            sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))

        def _send(self, status, payload):
            data = json.dumps(payload).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            if self.path == "/healthz":
                self._send(200, {"ok": True})
            else:
                self._send(404, {"ok": False, "error": "Unknown path %s." % self.path})

        def do_POST(self):
            path = self.path.split("?", 1)[0]
            try:
                length = int(self.headers.get("Content-Length") or 0)
            except ValueError:
                length = -1
            if length < 0 or (path not in unbounded_paths and length > MAX_BODY_BYTES):
                self.close_connection = True
                self._send(413, {"ok": False, "error": "The request body is missing a valid length or is too large."})
                return
            given = self.headers.get("X-Pod-Token") or ""
            if not token or not hmac.compare_digest(given.encode("utf-8"), token.encode("utf-8")):
                self.close_connection = True
                self._send(401, {"ok": False, "error": "The header X-Pod-Token is missing or wrong."})
                return
            route = routes.get(path)
            if route is None:
                self.close_connection = True
                self._send(404, {"ok": False, "error": "Unknown path %s." % self.path})
                return
            try:
                body = json.loads(self.rfile.read(length).decode("utf-8") or "{}")
                if not isinstance(body, dict):
                    raise ValueError("not an object")
            except (ValueError, UnicodeDecodeError):
                self._send(400, {"ok": False, "error": "The request body must be a JSON object."})
                return
            try:
                payload = route(body)
            except Refusal as refusal:
                payload = {"ok": False, "error": str(refusal)}
            except Exception as error:  # a bug or an unexpected system error; never a silent 500
                payload = {"ok": False, "error": "The pod could not do this: %s: %s" % (type(error).__name__, error)}
            self._send(200, payload)

    return Handler


def serve(port, token, routes, host="0.0.0.0", unbounded_paths=()):
    """Create the server (not yet serving). Port 0 picks a free port."""
    server = ThreadingHTTPServer((host, port), make_handler(token, routes, unbounded_paths))
    server.daemon_threads = True
    return server
