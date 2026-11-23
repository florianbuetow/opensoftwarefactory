"""HTTP clients for the factory services that tools call: the blobstore and the policy registry."""

import os
import re
from typing import Any
from urllib.parse import quote

import httpx

SEGMENT = re.compile(r"^[A-Za-z0-9_-][A-Za-z0-9._-]*$")
TIMEOUT = 10.0


class NotFoundError(LookupError):
    pass


def valid_id(object_id: str) -> bool:
    """Report whether object_id is a slash-separated ID the blobstore accepts."""
    if not object_id or len(object_id) > 512:
        return False
    return all(SEGMENT.match(segment) for segment in object_id.split("/"))


def _check_id(object_id: str) -> str:
    if not valid_id(object_id):
        raise ValueError(
            f"invalid object id {object_id!r}: use slash-separated segments of letters, digits, '.', '_' and '-'"
        )
    return object_id


def _raise_for_status(response: httpx.Response, what: str) -> None:
    if response.status_code == 404:
        raise NotFoundError(f"{what} not found")
    if response.is_error:
        raise RuntimeError(f"{what}: HTTP {response.status_code}: {response.text.strip()}")


class Blobstore:
    def __init__(self, base_url: str, transport: httpx.BaseTransport | None = None) -> None:
        self.client = httpx.Client(base_url=base_url, timeout=TIMEOUT, transport=transport)

    def get(self, object_id: str) -> str:
        response = self.client.get(f"/blobs/{_check_id(object_id)}")
        _raise_for_status(response, f"object {object_id!r}")
        return response.text

    def put(self, object_id: str, content: str, creator: str, trace_id: str = "") -> None:
        headers = {"X-Creator": creator, "X-Trace-Id": trace_id}
        response = self.client.put(f"/blobs/{_check_id(object_id)}", content=content.encode(), headers=headers)
        _raise_for_status(response, f"object {object_id!r}")

    def list(self, prefix: str = "") -> dict[str, Any]:
        prefix = prefix.strip("/")
        if prefix:
            _check_id(prefix)
        response = self.client.get(f"/list/{prefix}")
        _raise_for_status(response, f"prefix {prefix!r}")
        return response.json()

    def meta(self, object_id: str) -> dict[str, Any]:
        response = self.client.get(f"/meta/{_check_id(object_id)}")
        _raise_for_status(response, f"object {object_id!r}")
        return response.json()


class PolicyRegistry:
    def __init__(self, base_url: str, transport: httpx.BaseTransport | None = None) -> None:
        self.client = httpx.Client(base_url=base_url, timeout=TIMEOUT, transport=transport)

    def get(self, policy_id: str, version: str = "latest") -> dict[str, Any]:
        """Return {"name","version","created","creator","files":{"policy.md": ...}}."""
        if not SEGMENT.match(policy_id) or not SEGMENT.match(version):
            raise ValueError(f"invalid policy id or version: {policy_id!r} {version!r}")
        response = self.client.get(f"/policies/{quote(policy_id)}/{quote(version)}")
        _raise_for_status(response, f"policy {policy_id!r} version {version!r}")
        return response.json()


class Connectors:
    """The connector service, which checks and runs every tool call on a dev pod."""

    # A command in a dev pod may run for ten minutes; the call must outlast it.
    TOOL_TIMEOUT = 660.0

    def __init__(self, base_url: str, transport: httpx.BaseTransport | None = None) -> None:
        self.client = httpx.Client(base_url=base_url, timeout=self.TOOL_TIMEOUT, transport=transport)

    def tool(self, pod: str, tool: str, args: dict[str, Any], agent: str, run: str) -> Any:
        """Run one dev pod tool and return its result.

        A call the connector service refuses raises ValueError with the message
        written for the agent: what was refused, why, and what to do instead.
        """
        if not SEGMENT.match(pod or ""):
            raise ValueError(
                "this run has no dev pod, so the dev pod tools cannot be used: "
                "the workflow needs a devpod step before this step"
            )
        try:
            response = self.client.post(
                f"/api/devpods/{quote(pod)}/tool", json={"tool": tool, "args": args, "agent": agent, "run": run}
            )
        except httpx.HTTPError as exc:
            raise RuntimeError(f"the connector service did not answer {tool}: {type(exc).__name__}") from exc
        if response.is_error:
            raise RuntimeError(f"connector service: HTTP {response.status_code}: {response.text.strip()}")
        answer = response.json()
        if not answer.get("ok"):
            raise ValueError(answer.get("error") or f"{tool} was refused without a reason")
        return answer.get("result")

    def skill(self, pod: str, agent: str, run: str, version: str, skill: str,
              tool: str, args: dict[str, Any]) -> Any:
        """Read or run a carried skill through the connector service."""
        try:
            response = self.client.post(
                "/api/skills/tool",
                json={"tool": tool, "pod": pod, "agent": agent, "run": run,
                      "version": version, "skill": skill, "args": args},
                timeout=None,
            )
        except httpx.HTTPError as exc:
            raise RuntimeError(f"the connector service did not answer {tool}: {type(exc).__name__}") from exc
        if response.is_error:
            raise RuntimeError(f"connector service: HTTP {response.status_code}: {response.text.strip()}")
        answer = response.json()
        if not answer.get("ok"):
            raise ValueError(answer.get("error") or f"{tool} was refused without a reason")
        return answer.get("result")


class TraceEvidence:
    """Read-only, bounded access to execution evidence; never accepts arbitrary URLs or SQL."""

    def __init__(self, base_url: str, transport: httpx.BaseTransport | None = None) -> None:
        self.client = httpx.Client(base_url=base_url, timeout=TIMEOUT, transport=transport)

    def read(self, run_id: str, execution_id: str, trace_id: str = "", cursor: str = "", limit: int = 100) -> dict:
        self._validate_scope(run_id, execution_id)
        if trace_id and not re.fullmatch(r"[0-9a-fA-F]{32}", trace_id):
            raise ValueError("trace_id must be a distributed trace ID")
        if isinstance(limit, bool) or not isinstance(limit, int) or not 1 <= limit <= 500:
            raise ValueError("limit must be between 1 and 500")
        if len(cursor) > 2048:
            raise ValueError("cursor is too long")
        response = self.client.get("/api/evidence", params={"run_id": run_id, "execution_id": execution_id, "trace_id": trace_id, "cursor": cursor, "limit": limit})
        _raise_for_status(response, "trace evidence")
        return response.json()

    @staticmethod
    def _validate_scope(run_id: str, execution_id: str) -> None:
        if not re.fullmatch(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}", run_id):
            raise ValueError("run_id must be a run UUID")
        if not execution_id or len(execution_id) > 255 or any(ord(c) < 32 for c in execution_id):
            raise ValueError("execution_id must identify the target workflow execution")

    def artifact(self, run_id: str, execution_id: str, artifact_id: str) -> dict:
        self._validate_scope(run_id, execution_id)
        _check_id(artifact_id)
        response = self.client.get("/api/evidence/artifact", params={"run_id": run_id, "execution_id": execution_id, "artifact_id": artifact_id})
        _raise_for_status(response, "execution artifact")
        return response.json()


evidence = TraceEvidence(os.environ.get("EVALUATION_URL", "http://evaluation:8080"))
blobstore = Blobstore(os.environ.get("BLOBSTORE_URL", "http://blobstore:8080"))
policies = PolicyRegistry(os.environ.get("POLICY_REGISTRY_URL", "http://policy-registry:8080"))
connectors = Connectors(os.environ.get("CONNECTORS_URL", "http://connectors:8080"))
