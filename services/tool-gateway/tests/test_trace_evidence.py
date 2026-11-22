import httpx
import pytest
from factory import TraceEvidence

RUN = "12345678-1234-1234-1234-123456789abc"


def test_read_only_scoped_request_and_unknown_evidence():
    seen = []

    def handle(request):
        seen.append(request)
        return httpx.Response(200, json={"spans": [], "source_status": "unknown", "query_complete": True, "next_cursor": "page2"})

    client = TraceEvidence("http://evaluation", httpx.MockTransport(handle))
    result = client.read(RUN, "child-execution", limit=50)
    assert seen[0].method == "GET"
    assert seen[0].url.path == "/api/evidence"
    assert seen[0].url.params["execution_id"] == "child-execution"
    assert result["source_status"] == "unknown"
    assert result["next_cursor"] == "page2"


@pytest.mark.parametrize("args", [dict(run_id="bad", execution_id="x"), dict(run_id=RUN, execution_id=""), dict(run_id=RUN, execution_id="x", limit=501), dict(run_id=RUN, execution_id="x", trace_id="not-a-trace")])
def test_invalid_scope_never_reaches_backend(args):
    def unexpected(request):
        raise AssertionError("invalid request reached backend")
    client = TraceEvidence("http://evaluation", httpx.MockTransport(unexpected))
    with pytest.raises(ValueError):
        client.read(**args)


def test_artifact_reads_use_scoped_endpoint_and_preserve_denial():
    seen = []

    def handle(request):
        seen.append(request)
        return httpx.Response(403, text="artifact outside target execution")

    client = TraceEvidence("http://evaluation", httpx.MockTransport(handle))
    with pytest.raises(RuntimeError, match="outside target execution"):
        client.artifact(RUN, "child-execution", "runs/another/result.json")
    assert seen[0].url.path == "/api/evidence/artifact"
    assert seen[0].url.params["execution_id"] == "child-execution"
