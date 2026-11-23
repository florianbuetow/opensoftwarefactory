"""MCP tool gateway: serves the functions in a host-editable tools.py as MCP tools.

The streamable HTTP transport runs stateless with JSON responses at POST /mcp,
so every request gets one application/json JSON-RPC reply and no session ID.
GET /healthz returns 200.

At startup the file at TOOLS_PATH (default /tools/tools.py) is imported and
every public module-level function defined in it becomes a tool: the docstring
is the description and the type hints are the input schema. If the import
fails, the error is logged and the server runs with zero tools.

A background thread checks the file's modification time every 2 seconds. When
it changes, the process logs one line and exits with code 0; Kubernetes then
restarts the container, which loads the edited tools.
"""

import functools
import importlib.util
import inspect
import json
import os
import sys
import threading
import time
import traceback
from collections.abc import Callable
from datetime import UTC, datetime
from pathlib import Path
from types import FunctionType, ModuleType
from typing import Any

import uvicorn
from mcp.server.mcpserver import MCPServer
from mcp_types import CallToolResult, TextContent
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import Response

WATCH_INTERVAL = 2.0


def log(level: str, msg: str, **fields: object) -> None:
    """Print one JSON log line with the time, the level, the message and the given fields."""
    record = {"time": datetime.now(UTC).isoformat(), "level": level, "msg": msg, **fields}
    print(json.dumps(record, default=str), flush=True)


def load_tools(path: Path) -> ModuleType | None:
    """Import the tools file by path, or log the error and return None."""
    try:
        spec = importlib.util.spec_from_file_location("tools", path)
        if spec is None or spec.loader is None:
            raise ImportError(f"cannot load {path}")
        module = importlib.util.module_from_spec(spec)
        sys.modules["tools"] = module
        spec.loader.exec_module(module)
        return module
    except Exception as exc:
        sys.modules.pop("tools", None)
        log("ERROR", "loading tools failed", path=str(path), error=f"{type(exc).__name__}: {exc}")
        return None


def public_functions(module: ModuleType) -> list[FunctionType]:
    """Return the functions defined in the module whose names do not start with "_"."""
    return [
        fn for name, fn in vars(module).items() if not name.startswith("_") and inspect.isfunction(fn) and fn.__module__ == module.__name__
    ]


# Long enough for a refusal that names the rule, the reason and what to do instead, with a
# list of allowed commands.
MAX_ERROR_TEXT = 2000


def reporting_errors[**P](fn: Callable[P, Any]) -> Callable[P, Any]:
    """Wrap a tool so that the caller receives the message of the exception the tool raised.

    The wrapper takes the tool's parameters and returns what the tool returns, or
    a CallToolResult that carries the error; it is async when the tool is.

    The MCP server answers any exception other than its own ToolError with only
    "Error executing tool <name>", which drops the text that tells a model how to
    correct its call. A ValueError is how a tool refuses a call, so its message is
    the answer. Any other exception is logged with its traceback and answered
    with its type and first line.
    """

    def failed(exc: Exception) -> CallToolResult:
        if isinstance(exc, ValueError) and str(exc).strip():
            message = str(exc)
            log("INFO", "tool refused the call", tool=fn.__name__, error=message)
        else:
            message = f"{type(exc).__name__}: {exc}".strip().split("\n", 1)[0].removesuffix(":")
            log("ERROR", "tool failed", tool=fn.__name__, error=message, traceback=traceback.format_exc())
        if fn.__name__ not in ("read_skill", "run_skill_script"):
            message = message[:MAX_ERROR_TEXT]
        return CallToolResult(content=[TextContent(type="text", text=message)], is_error=True)

    if inspect.iscoroutinefunction(fn):

        @functools.wraps(fn)
        async def async_tool(*args: P.args, **kwargs: P.kwargs) -> object:
            try:
                return await fn(*args, **kwargs)
            except Exception as exc:
                return failed(exc)

        return async_tool

    @functools.wraps(fn)
    def tool(*args: P.args, **kwargs: P.kwargs) -> object:
        try:
            return fn(*args, **kwargs)
        except Exception as exc:
            return failed(exc)

    return tool


def build_server(tools_path: Path) -> MCPServer:
    """Return an MCP server that serves the tools file's public functions and GET /healthz."""
    mcp = MCPServer("tool-gateway", log_level="WARNING")
    module = load_tools(tools_path)
    names: list[str] = []
    for fn in public_functions(module) if module else []:
        try:
            mcp.add_tool(reporting_errors(fn))
            names.append(fn.__name__)
        except Exception as exc:
            log("ERROR", "registering tool failed", tool=fn.__name__, error=f"{type(exc).__name__}: {exc}")
    log("INFO", "tools registered", path=str(tools_path), tools=names)

    async def healthz(_: Request) -> Response:
        return Response(status_code=200)

    # custom_route declares no return type; used as a decorator it would leave healthz untyped.
    mcp.custom_route("/healthz", methods=["GET"])(healthz)

    return mcp


def build_app(tools_path: Path) -> Starlette:
    """Return the ASGI app: stateless streamable HTTP with JSON responses."""
    return build_server(tools_path).streamable_http_app(stateless_http=True, json_response=True, host="0.0.0.0")


def mtime(path: Path) -> float | None:
    """Return the file's modification time, or None when it cannot be read."""
    try:
        return path.stat().st_mtime
    except OSError:
        return None


def watch(path: Path) -> None:
    """Exit the process with code 0 once the tools file's modification time changes."""
    initial = mtime(path)
    while True:
        time.sleep(WATCH_INTERVAL)
        if mtime(path) != initial:
            log("INFO", "tools file changed, exiting for restart", path=str(path))
            os._exit(0)


def main() -> None:
    """Serve the tools file on PORT and exit for a restart when the file changes."""
    tools_path = Path(os.environ.get("TOOLS_PATH", "/tools/tools.py"))
    port = int(os.environ.get("PORT", "8080"))
    app = build_app(tools_path)
    threading.Thread(target=watch, args=(tools_path,), daemon=True).start()
    log("INFO", "tool gateway listening", address=f"0.0.0.0:{port}", tools_path=str(tools_path))
    uvicorn.run(app, host="0.0.0.0", port=port, access_log=False, log_level="warning")


if __name__ == "__main__":
    main()
