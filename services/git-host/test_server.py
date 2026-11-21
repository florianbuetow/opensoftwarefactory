"""Read-only repository browser checks against a real bare Git repository."""
import importlib.util
import subprocess
import tempfile
import unittest
from pathlib import Path


spec = importlib.util.spec_from_file_location("git_host_server", Path(__file__).with_name("server.py"))
server = importlib.util.module_from_spec(spec)
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
    def git(source, *args):
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


if __name__ == "__main__":
    unittest.main()
