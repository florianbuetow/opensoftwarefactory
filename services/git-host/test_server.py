"""Read-only repository browser checks against a real bare Git repository."""

import http.client
import importlib.util
import io
import os
import subprocess
import tempfile
import threading
import unittest
from http.server import ThreadingHTTPServer
from pathlib import Path
from unittest import mock

spec = importlib.util.spec_from_file_location("git_host_server", Path(__file__).with_name("server.py"))
assert spec is not None
assert spec.loader is not None
server = importlib.util.module_from_spec(spec)
os.environ["GIT_HOST_DATA"] = "/data"
os.environ["SEED_DIR"] = "/seed"
spec.loader.exec_module(server)


class BrowserTest(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        root = Path(self.work.name)
        source = root / "source"
        source.mkdir()
        self.git(source, "init", "-q", "-b", "main")
        self.git(source, "config", "user.name", "Tester")
        self.git(source, "config", "user.email", "tester@example.com")
        (source / "README.md").write_text("Hello <world>\n")
        (source / "src").mkdir()
        (source / "src" / "app.py").write_text("print('ok')\n")
        self.git(source, "add", ".")
        self.git(source, "commit", "-qm", "Initial <commit>")
        self.git(source, "branch", "feature")
        self.repo = root / "demo.git"
        subprocess.run(["git", "clone", "-q", "--bare", str(source), str(self.repo)], check=True)

    @staticmethod
    def git(source: Path, *args: str) -> None:
        subprocess.run(["git", "-C", str(source), *args], check=True)

    def test_browse_files_commits_and_branches(self):
        files = server.repository_page(str(self.repo), "demo", {})
        self.assertIn("README.md", files)
        self.assertIn("src/", files)
        self.assertIn("feature", files)
        self.assertIn("?view=file&amp;ref=main&amp;path=README.md", files)

        directory = server.repository_page(str(self.repo), "demo", {"path": ["src"]})
        self.assertIn("app.py", directory)
        file_page = server.repository_page(str(self.repo), "demo", {"view": ["file"], "path": ["README.md"]})
        self.assertIn("Hello &lt;world&gt;", file_page)
        history = server.repository_page(str(self.repo), "demo", {"view": ["commits"]})
        self.assertIn("Initial &lt;commit&gt;", history)
        self.assertIn("Tester", history)
        commit = server.git(str(self.repo), "rev-parse", "HEAD").decode().strip()
        detail = server.repository_page(str(self.repo), "demo", {"view": ["commit"], "commit": [commit]})
        self.assertIn("+Hello &lt;world&gt;", detail)

    def test_rejects_unknown_branches_and_invalid_paths(self):
        with self.assertRaises(ValueError):
            server.repository_page(str(self.repo), "demo", {"ref": ["missing"]})
        with self.assertRaises(ValueError):
            server.repository_page(str(self.repo), "demo", {"view": ["file"], "path": ["../secret"]})

    def test_external_seed_script_is_optional(self):
        root = Path(self.work.name)
        self.assertFalse(server.run_seed(str(root / "missing.sh")))
        script = root / "seed.sh"
        marker = root / "seeded"
        script.write_text(f'#!/bin/sh\nprintf "%s" "$GIT_HOST_SEED" > "{marker}"\n')
        old = os.environ.pop("GIT_HOST_SEED", None)
        try:
            self.assertTrue(server.run_seed(str(script)))
        finally:
            if old is not None:
                os.environ["GIT_HOST_SEED"] = old
        self.assertEqual(marker.read_text(), str(root / "assets"))

    def test_edge_cases_for_views_env_and_chunked_reads(self):
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("MISSING_TEST_VAR", None)
            with self.assertRaises(SystemExit) as ctx:
                server.required_env("MISSING_TEST_VAR")
            self.assertEqual(ctx.exception.code, 1)

        with self.assertRaises(ValueError):
            server.repository_page(str(self.repo), "demo", {"view": ["unknown"]})
        with self.assertRaises(ValueError):
            server.repository_page(str(self.repo), "demo", {"view": ["commit"], "commit": ["deadbeef"]})
        with self.assertRaises(ValueError):
            server.repository_page(str(self.repo), "demo", {"view": ["files"], "path": ["../bad"]})
        with self.assertRaises(ValueError):
            server.repository_page(str(self.repo), "demo", {"view": ["file"], "path": ["src"]})

        commit = server.git(str(self.repo), "rev-parse", "HEAD").decode().strip()
        with mock.patch.object(server, "MAX_PREVIEW", 4):
            truncated_commit = server.commit_view(str(self.repo), "main", commit, commit)
            self.assertIn("Patch preview truncated.", truncated_commit)
            large_file = server.file_view(str(self.repo), commit, "README.md")
            self.assertIn("File is too large to preview.", large_file)

        chunked = io.BytesIO(b"5;ext=1\r\nhello\r\n6\r\n world\r\n0\r\nX-Trailer: 1\r\n\r\n")
        self.assertEqual(server.read_chunked(chunked, 32), b"hello world")
        with self.assertRaises(ValueError):
            server.read_chunked(io.BytesIO(b"5\r\nhello\r\n0\r\n\r\n"), 4)


class HttpServerTest(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        root = Path(self.work.name)
        source = root / "source"
        source.mkdir()
        subprocess.run(["git", "-C", str(source), "init", "-q", "-b", "main"], check=True)
        subprocess.run(["git", "-C", str(source), "config", "user.name", "Tester"], check=True)
        subprocess.run(["git", "-C", str(source), "config", "user.email", "tester@example.com"], check=True)
        (source / "README.md").write_text("Hello\n")
        (source / "bin.dat").write_bytes(b"\x00\x01\x02")
        subprocess.run(["git", "-C", str(source), "add", "."], check=True)
        subprocess.run(["git", "-C", str(source), "commit", "-qm", "Initial"], check=True)
        self.repo = root / "demo.git"
        subprocess.run(["git", "clone", "-q", "--bare", str(source), str(self.repo)], check=True)

        self.enterContext(mock.patch.object(server, "DATA", str(root)))

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), server.Handler)
        self.port = self.httpd.server_address[1]
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.httpd.server_close)
        self.addCleanup(self.httpd.shutdown)

    def request(self, method: str, path: str, body: bytes = b"", headers: dict[str, str] | None = None) -> tuple[int, bytes]:
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        try:
            conn.request(method, path, body=body, headers=headers or {})
            resp = conn.getresponse()
            return resp.status, resp.read()
        finally:
            conn.close()

    def test_healthz_listing_browse_and_404_routes(self):
        status, body = self.request("GET", "/healthz")
        self.assertEqual((status, body), (200, b"ok\n"))

        status, body = self.request("GET", "/")
        self.assertEqual(status, 200)
        self.assertIn(b"demo.git", body)

        status, body = self.request("GET", "/browse/demo.git?view=file&path=bin.dat")
        self.assertEqual(status, 200)
        self.assertIn(b"Binary file", body)

        self.assertEqual(self.request("GET", "/browse/missing.git")[0], 404)
        self.assertEqual(self.request("GET", "/browse/demo.git?ref=missing")[0], 404)
        self.assertEqual(self.request("GET", "/unknown")[0], 404)
        self.assertEqual(self.request("GET", "/demo.git/../secret")[0], 404)

    def test_smart_http_backend_and_post_handling(self):
        status, body = self.request(
            "GET",
            "/demo.git/info/refs?service=git-upload-pack",
            headers={"Git-Protocol": "version=2"},
        )
        self.assertEqual(status, 200)
        self.assertIn(b"version 2", body)

        status, _ = self.request(
            "POST",
            "/demo.git/git-upload-pack",
            body=b"0\r\n\r\n",
            headers={"Transfer-Encoding": "chunked", "Content-Encoding": "identity"},
        )
        self.assertEqual(status, 415)

        with mock.patch.object(server, "MAX_BODY", 4):
            status, _ = self.request("POST", "/demo.git/git-upload-pack", body=b"12345")
            self.assertEqual(status, 413)

        bad_backend = subprocess.CompletedProcess(["git", "http-backend"], 1, stdout=b"no-headers", stderr=b"boom\n")
        with mock.patch.object(server.subprocess, "run", return_value=bad_backend):
            status, body = self.request("GET", "/demo.git/info/refs")
            self.assertEqual(status, 500)
            self.assertIn(b"no answer", body)


if __name__ == "__main__":
    unittest.main()
