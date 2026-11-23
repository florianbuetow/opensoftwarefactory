"""Tests of the git-helper. Run: python3 -m unittest discover -s services/devpod/tests"""

import base64
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from collections.abc import Callable
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
import githelper  # noqa: E402
import podhttp  # noqa: E402
from podhttp import JsonObject, RefusalError  # noqa: E402

GIT_ENV = {
    "PATH": os.environ.get("PATH", ""),
    "GIT_CONFIG_GLOBAL": "/dev/null",
    "GIT_CONFIG_NOSYSTEM": "1",
    "GIT_AUTHOR_NAME": "Tester",
    "GIT_AUTHOR_EMAIL": "tester@example.com",
    "GIT_COMMITTER_NAME": "Tester",
    "GIT_COMMITTER_EMAIL": "tester@example.com",
    "HOME": "/nonexistent",
}


def run(*args: str, cwd: str) -> str:
    done = subprocess.run(list(args), cwd=cwd, env=GIT_ENV, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    if done.returncode != 0:
        raise AssertionError("{} failed: {}".format(" ".join(args), done.stdout))
    return done.stdout.strip()


class HelperCase(unittest.TestCase):
    def setUp(self):
        self.base = os.path.realpath(tempfile.mkdtemp(prefix="githelper-test-"))
        self.addCleanup(shutil.rmtree, self.base, ignore_errors=True)
        self.workspace = os.path.join(self.base, "workspace")
        self.origin = os.path.join(self.base, "origin.git")
        os.makedirs(self.workspace)
        seed = os.path.join(self.base, "seed")
        os.makedirs(os.path.join(seed, "src"))
        run("git", "init", "-q", "-b", "main", cwd=seed)
        with open(os.path.join(seed, "src", "panel.py"), "w") as handle:
            handle.write("def status_panel(claim_id):\n    return {}\n")
        with open(os.path.join(seed, "verify.sh"), "w") as handle:
            handle.write("#!/bin/sh\nexit 0\n")
        run("git", "add", "-A", cwd=seed)
        run("git", "commit", "-q", "-m", "First commit", cwd=seed)
        self.first = run("git", "rev-parse", "HEAD", cwd=seed)
        run("git", "clone", "-q", "--bare", seed, self.origin, cwd=self.base)
        values = {
            "WORKSPACE": self.workspace,
            "STATE_DIR": os.path.join(self.base, "state"),
            "CREDENTIAL_FILE": os.path.join(self.base, "credential", "token"),
            "HELPER_HOME": os.path.join(self.base, "home"),
        }
        for name, value in values.items():
            patch = mock.patch.object(githelper, name, value)
            patch.start()
            self.addCleanup(patch.stop)

    def clone(self, **changes: str) -> JsonObject:
        body: JsonObject = {
            "url": self.origin,
            "source_branch": "main",
            "work_branch": "agent/work-1",
            "author_name": "course/feature-builder@v2",
            "author_email": "agent@opensoftwarefactory.local",
        }
        body.update(changes)
        return githelper.clone(body)

    def commit(self, name: str = "notes.txt", content: str = "a note\n", message: str = "Add a note") -> str:
        with open(os.path.join(self.workspace, name), "w") as handle:
            handle.write(content)
        env = {key: value for key, value in GIT_ENV.items() if "AUTHOR" not in key and "COMMITTER" not in key}
        for args in (["git", "add", "-A"], ["git", "commit", "-q", "-m", message]):
            subprocess.run(args, cwd=self.workspace, env=env, check=True, capture_output=True)
        return run("git", "rev-parse", "HEAD", cwd=self.workspace)

    def set_credential(self, token: str = "tok-SECRET-123") -> str:
        os.makedirs(os.path.dirname(githelper.CREDENTIAL_FILE), exist_ok=True)
        with open(githelper.CREDENTIAL_FILE, "w") as handle:
            handle.write(token + "\n")
        return token

    def refused(self, function: Callable[[JsonObject], JsonObject], body: JsonObject, *words: str) -> None:
        with self.assertRaises(RefusalError) as caught:
            function(body)
        for word in words:
            self.assertIn(word, str(caught.exception))


class CloneTest(HelperCase):
    def test_clone_checks_out_a_new_work_branch(self):
        result = self.clone()
        self.assertTrue(result["ok"], result)
        self.assertEqual(result["start_commit"], self.first)
        self.assertFalse(result["work_branch_existed"])
        self.assertEqual(run("git", "rev-parse", "--abbrev-ref", "HEAD", cwd=self.workspace), "agent/work-1")
        self.assertEqual(run("git", "config", "user.name", cwd=self.workspace), "course/feature-builder@v2")
        self.assertEqual(run("git", "config", "user.email", cwd=self.workspace), "agent@opensoftwarefactory.local")
        self.assertTrue(os.path.exists(os.path.join(self.workspace, "src", "panel.py")))

    def test_commit_in_the_pod_carries_the_agent_identity(self):
        self.clone()
        self.commit()
        self.assertEqual(
            run("git", "log", "-1", "--format=%an <%ae>", cwd=self.workspace), "course/feature-builder@v2 <agent@opensoftwarefactory.local>"
        )

    def test_clone_continues_a_work_branch_that_exists_in_the_repository(self):
        self.clone()
        commit = self.commit()
        self.assertTrue(githelper.push({"branch": "agent/work-1"})["ok"])
        shutil.rmtree(self.workspace)
        os.makedirs(self.workspace)
        shutil.rmtree(githelper.STATE_DIR)
        result = self.clone()
        self.assertTrue(result["ok"], result)
        self.assertTrue(result["work_branch_existed"])
        self.assertEqual(result["start_commit"], commit)

    def test_clone_twice_answers_with_the_first_start_commit(self):
        self.clone()
        self.commit()
        again = self.clone()
        self.assertTrue(again["ok"])
        self.assertEqual(again["start_commit"], self.first)
        self.refused(self.clone_other, {}, "already holds a clone")

    def clone_other(self, _: JsonObject) -> JsonObject:
        return self.clone(work_branch="agent/other")

    def test_work_branch_equal_to_source_branch_is_refused(self):
        self.refused(githelper.clone, {"url": self.origin, "source_branch": "main", "work_branch": "main"}, "same as the source branch")
        self.assertEqual(os.listdir(self.workspace), [])

    def test_unknown_source_branch_gives_a_sentence_and_an_empty_workspace(self):
        result = self.clone(source_branch="release")
        self.assertFalse(result["ok"])
        self.assertIn("source branch 'release' does not exist", result["error"])
        self.assertEqual(os.listdir(self.workspace), [])

    def test_unknown_repository_gives_a_sentence(self):
        result = self.clone(url=os.path.join(self.base, "none.git"))
        self.assertFalse(result["ok"])
        self.assertIn("No repository was found", result["error"])

    def test_wrong_input_is_refused(self):
        self.refused(githelper.clone, {"source_branch": "main", "work_branch": "w"}, "url")
        self.refused(self.clone_with, {"work_branch": "-x"}, "not a valid branch name")
        self.refused(self.clone_with, {"work_branch": "a..b"}, "not a valid branch name")
        self.refused(self.clone_with, {"url": "https://user:tok@example.com/a.git"}, "credential")
        self.refused(self.clone_with, {"url": "ext::sh -c id"}, "not valid")
        self.refused(self.clone_with, {"url": "ssh://example.com/a.git"}, "not supported")
        self.refused(self.clone_with, {"url": "--upload-pack=id"}, "not valid")

    def clone_with(self, changes: JsonObject) -> JsonObject:
        return self.clone(**changes)

    def test_workspace_with_other_files_is_refused(self):
        with open(os.path.join(self.workspace, "stray.txt"), "w") as handle:
            handle.write("x")
        self.refused(self.clone_with, {}, "not empty")


class CredentialTest(HelperCase):
    def test_credential_is_scoped_to_the_host_and_absent_without_a_file(self):
        self.assertEqual(githelper.auth("https://github.com/acme/claims.git"), ([], []))
        token = self.set_credential()
        config, secrets = githelper.auth("https://github.com/acme/claims.git")
        expected = base64.b64encode(("x-access-token:" + token).encode()).decode()
        self.assertEqual(config, [("http.https://github.com/.extraHeader", "Authorization: Basic " + expected)])
        self.assertEqual(secrets, [token, expected])
        self.assertEqual(githelper.auth(self.origin), ([], []))
        self.assertEqual(githelper.scrub(f"a {token} b {expected}", secrets), "a *** b ***")

    def test_credential_is_sent_as_a_header_and_never_stored(self):
        token = self.set_credential()
        seen: list[str | None] = []

        class Recorder(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                seen.append(self.headers.get("Authorization"))
                self.send_response(404)
                self.send_header("Content-Length", "0")
                self.end_headers()

            def log_message(self, format: str, *args: object) -> None:
                pass

        server = HTTPServer(("127.0.0.1", 0), Recorder)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        result = self.clone(url=f"http://127.0.0.1:{server.server_address[1]}/claims.git")
        self.assertFalse(result["ok"])
        expected = "Basic " + base64.b64encode(("x-access-token:" + token).encode()).decode()
        self.assertIn(expected, seen)
        self.assertNotIn(token, json.dumps(result))
        self.assertNotIn(expected[6:], json.dumps(result))
        self.assertEqual(os.listdir(self.workspace), [])

    def test_clone_and_push_leave_no_credential_in_the_workspace(self):
        token = self.set_credential()
        self.clone()
        self.commit()
        self.assertTrue(githelper.push({"branch": "agent/work-1"})["ok"])
        encoded = base64.b64encode(("x-access-token:" + token).encode()).decode()
        for folder, _, files in os.walk(self.workspace):
            for name in files:
                with open(os.path.join(folder, name), "rb") as handle:
                    data = handle.read()
                self.assertNotIn(token.encode(), data, name)
                self.assertNotIn(encoded.encode(), data, name)
        with open(os.path.join(self.workspace, ".git", "config")) as handle:
            self.assertNotIn("extraheader", handle.read().lower())


class PushTest(HelperCase):
    def test_push_without_a_credential_file_puts_the_branch_in_the_repository(self):
        self.clone()
        commit = self.commit()
        result = githelper.push({"branch": "agent/work-1"})
        self.assertTrue(result["ok"], result)
        self.assertEqual(result["commit"], commit)
        self.assertEqual(run("git", "rev-parse", "refs/heads/agent/work-1", cwd=self.origin), commit)
        self.assertEqual(run("git", "rev-parse", "refs/heads/main", cwd=self.origin), self.first)
        self.assertEqual(run("git", "rev-parse", "refs/remotes/origin/agent/work-1", cwd=self.workspace), commit)

    def test_push_twice_and_after_more_commits(self):
        self.clone()
        self.commit()
        self.assertTrue(githelper.push({"branch": "agent/work-1"})["ok"])
        self.assertTrue(githelper.push({"branch": "agent/work-1"})["ok"])
        second = self.commit(content="more\n", message="More")
        self.assertTrue(githelper.push({"branch": "agent/work-1"})["ok"])
        self.assertEqual(run("git", "rev-parse", "refs/heads/agent/work-1", cwd=self.origin), second)

    def test_only_the_work_branch_can_be_pushed(self):
        self.clone()
        self.commit()
        self.refused(githelper.push, {"branch": "main"}, "Only the work branch")
        run("git", "branch", "other", cwd=self.workspace)
        self.refused(githelper.push, {"branch": "other"}, "Only the work branch")
        self.refused(githelper.push, {}, "branch")
        self.assertEqual(run("git", "for-each-ref", "--format=%(refname)", cwd=self.origin), "refs/heads/main")

    def test_hooks_and_config_of_the_workspace_do_not_run_with_the_push(self):
        self.clone()
        self.commit()
        marker = os.path.join(self.base, "hook-ran")
        hook = os.path.join(self.workspace, ".git", "hooks", "pre-push")
        os.makedirs(os.path.dirname(hook), exist_ok=True)
        with open(hook, "w") as handle:
            handle.write(f"#!/bin/sh\ntouch '{marker}'\nexit 1\n")
        os.chmod(hook, 0o755)
        elsewhere = os.path.join(self.base, "elsewhere.git")
        run("git", "init", "-q", "--bare", elsewhere, cwd=self.base)
        run("git", "remote", "set-url", "origin", elsewhere, cwd=self.workspace)
        run("git", "config", f"url.{elsewhere}.insteadOf", self.origin, cwd=self.workspace)
        result = githelper.push({"branch": "agent/work-1"})
        self.assertTrue(result["ok"], result)
        self.assertFalse(os.path.exists(marker))
        self.assertEqual(run("git", "for-each-ref", cwd=elsewhere), "")
        self.assertIn("agent/work-1", run("git", "for-each-ref", cwd=self.origin))

    def test_uncommitted_changes_are_named(self):
        self.clone()
        self.commit()
        with open(os.path.join(self.workspace, "loose.txt"), "w") as handle:
            handle.write("x")
        result = githelper.push({"branch": "agent/work-1"})
        self.assertTrue(result["ok"])
        self.assertIn("1 files have changes that are not committed", result["output"])

    def test_push_that_the_repository_refuses_gives_a_sentence(self):
        self.clone()
        self.commit()
        other = os.path.join(self.base, "other")
        run("git", "clone", "-q", self.origin, other, cwd=self.base)
        run("git", "checkout", "-q", "-b", "agent/work-1", cwd=other)
        with open(os.path.join(other, "theirs.txt"), "w") as handle:
            handle.write("x")
        run("git", "add", "-A", cwd=other)
        run("git", "commit", "-q", "-m", "Theirs", cwd=other)
        run("git", "push", "-q", "origin", "agent/work-1", cwd=other)
        result = githelper.push({"branch": "agent/work-1"})
        self.assertFalse(result["ok"])
        self.assertIn("already has other commits", result["error"])

    def test_push_before_clone_is_refused(self):
        self.refused(githelper.push, {"branch": "agent/work-1"}, "repository URL")
        self.refused(githelper.push, {"branch": "agent/work-1", "url": self.origin}, "no clone yet")


class PatchAndViewTest(HelperCase):
    def test_patch_lists_commits_and_applies_on_a_fresh_clone(self):
        self.clone()
        first = self.commit(message="Show the payment date")
        second = self.commit(name="more.txt", content="more\n", message="Show no payment planned")
        result = githelper.patch({"base": "main"})
        self.assertTrue(result["ok"], result)
        self.assertEqual([item["hash"] for item in result["commits"]], [first, second])
        self.assertEqual(result["commits"][0]["subject"], "Show the payment date")
        self.assertEqual(result["commits"][0]["author"], "course/feature-builder@v2")
        fresh = os.path.join(self.base, "fresh")
        run("git", "clone", "-q", self.origin, fresh, cwd=self.base)
        patch_file = os.path.join(self.base, "work.patch")
        with open(patch_file, "w") as handle:
            handle.write(result["patch"])
        run("git", "am", patch_file, cwd=fresh)
        self.assertEqual(
            run("git", "log", "--format=%s", cwd=fresh).splitlines(), ["Show no payment planned", "Show the payment date", "First commit"]
        )
        self.assertTrue(os.path.exists(os.path.join(fresh, "more.txt")))

    def test_patch_without_commits_is_empty(self):
        self.clone()
        result = githelper.patch({"base": self.first})
        self.assertEqual((result["ok"], result["patch"], result["commits"]), (True, "", []))

    def test_patch_with_a_wrong_base_is_refused(self):
        self.clone()
        self.refused(githelper.patch, {"base": "no-such-branch"}, "not a commit or branch")
        self.refused(githelper.patch, {"base": "--output=/tmp/x"}, "not a valid commit")
        self.refused(githelper.patch, {}, "base")

    def test_read_only_views(self):
        self.clone()
        self.commit(message="Add a note")
        log = githelper.view({"args": ["log", "--format=%s", "main..HEAD"]})
        self.assertEqual((log["ok"], log["output"].strip()), (True, "Add a note"))
        diff = githelper.view({"args": ["diff", "main...HEAD"]})
        self.assertIn("+a note", diff["output"])
        self.assertIn("notes.txt", githelper.view({"args": ["ls-files"]})["output"])
        self.assertIn("Add a note", githelper.view({"args": ["show", "HEAD"]})["output"])
        self.assertEqual(githelper.view({"args": ["rev-parse", "--abbrev-ref", "HEAD"]})["output"].strip(), "agent/work-1")
        bad = githelper.view({"args": ["log", "no-such-branch"]})
        self.assertFalse(bad["ok"])
        self.assertIn("git log failed", bad["error"])

    def test_other_subcommands_and_writing_options_are_refused(self):
        self.clone()
        for args in (["commit", "-m", "x"], ["push"], ["config", "user.name", "x"], ["checkout", "main"], ["-c", "a=b", "log"]):
            self.refused(githelper.view, {"args": args}, "not allowed here")
        self.refused(githelper.view, {"args": ["diff", "--output=/tmp/x"]}, "not allowed in a read-only view")
        self.refused(githelper.view, {"args": ["diff", "--no-index", "/etc/passwd", "/dev/null"]}, "not allowed")
        self.refused(githelper.view, {"args": []}, "args")
        self.refused(githelper.view, {"args": "log"}, "args")


class ProtectTest(HelperCase):
    def tree(self):
        root = os.path.join(self.base, "tree")
        files = [
            "tests/test_a.py",
            "tests/sub/test_b.py",
            "verify.sh",
            "src/panel.py",
            "docs/guide/rules.md",
            "docs/readme.md",
            ".git/config",
            ".git/hooks/pre-commit",
            "checks/check_cl7.py",
        ]
        for relative in files:
            path = os.path.join(root, relative)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "w") as handle:
                handle.write("x\n")
            os.chmod(path, 0o664)
        os.chmod(os.path.join(root, "verify.sh"), 0o775)
        return root

    def test_glob_patterns(self):
        def match(pattern: str, path: str) -> bool:
            return bool(githelper.glob_regex(pattern).fullmatch(path))

        self.assertTrue(match("tests/**", "tests/sub/test_b.py"))
        self.assertTrue(match("tests/**", "tests/"))
        self.assertFalse(match("tests/**", "tests2/a.py"))
        self.assertTrue(match("verify.sh", "verify.sh"))
        self.assertFalse(match("verify.sh", "src/verify.sh"))
        self.assertTrue(match("*.md", "readme.md"))
        self.assertFalse(match("*.md", "docs/readme.md"))
        self.assertTrue(match("**/*.md", "readme.md"))
        self.assertTrue(match("**/*.md", "docs/guide/rules.md"))
        self.assertTrue(match("src/**/panel.py", "src/panel.py"))
        self.assertTrue(match("src/?anel.py", "src/panel.py"))
        self.assertFalse(match("src/?.py", "src/panel.py"))
        self.assertFalse(match("verify.sh", "verifyXsh"))
        self.assertTrue(match("./checks/", "checks/"))

    def test_plan_names_protected_entries_and_the_folders_that_hold_them(self):
        root = self.tree()
        folders, files, holders, unmatched = githelper.plan_protection(
            root, ["tests/**", "verify.sh", "docs/**/rules.md", "checks", "missing/*"]
        )
        self.assertEqual(folders, ["checks", "tests", "tests/sub"])
        self.assertEqual(
            sorted(files), ["checks/check_cl7.py", "docs/guide/rules.md", "tests/sub/test_b.py", "tests/test_a.py", "verify.sh"]
        )
        self.assertEqual(holders, ["", "docs", "docs/guide"])
        self.assertEqual(unmatched, ["missing/*"])

    def test_plan_never_touches_git(self):
        root = self.tree()
        folders, files, holders, _ = githelper.plan_protection(root, ["**"])
        self.assertFalse([path for path in folders + files if path.split("/")[0] == ".git"])
        self.assertIn("src/panel.py", files)
        self.assertEqual(holders, [""])

    def test_apply_sets_owner_sticky_bit_and_modes(self):
        root = self.tree()
        os.chmod(root, 0o755)
        owned: dict[str, tuple[int, int]] = {}
        folders, files, holders, _ = githelper.plan_protection(root, ["tests/**", "verify.sh", "docs/**/rules.md"])
        githelper.apply_protection(
            root, folders, files, holders, chown=lambda path, uid, gid: owned.__setitem__(os.path.relpath(path, root), (uid, gid))
        )

        def mode(relative: str) -> int:
            return stat.S_IMODE(os.lstat(os.path.join(root, relative)).st_mode)

        self.assertEqual(mode("."), 0o1777)
        self.assertEqual(mode("docs"), 0o1777)
        self.assertEqual(mode("docs/guide"), 0o1777)
        self.assertEqual(mode("tests"), 0o755)
        self.assertEqual(mode("tests/sub"), 0o755)
        self.assertEqual(mode("tests/test_a.py"), 0o644)
        self.assertEqual(mode("verify.sh"), 0o755)
        self.assertEqual(mode("docs/guide/rules.md"), 0o644)
        self.assertEqual(mode("src/panel.py"), 0o664)
        self.assertEqual(mode("docs/readme.md"), 0o664)
        self.assertEqual(set(owned.values()), {(2000, 2000)})
        self.assertEqual(
            sorted(owned),
            sorted(
                [
                    ".",
                    "docs",
                    "docs/guide",
                    "docs/guide/rules.md",
                    "tests",
                    "tests/sub",
                    "tests/sub/test_b.py",
                    "tests/test_a.py",
                    "verify.sh",
                ]
            ),
        )

    def test_protect_checks_its_input(self):
        self.refused(githelper.protect, {"paths": "tests/**"}, "list of glob patterns")
        self.refused(githelper.protect, {"paths": ["tests/**"]}, "no clone yet")
        self.clone()
        self.refused(githelper.protect, {"paths": [".git/**"]}, "not allowed")
        self.refused(githelper.protect, {"paths": ["./.git/hooks"]}, "not allowed")
        self.refused(githelper.protect, {"paths": ["../x"]}, "not allowed")

    def test_protect_without_root_says_so(self):
        self.clone()
        with mock.patch.object(githelper.os, "geteuid", return_value=githelper.WORK_UID):
            self.refused(githelper.protect, {"paths": ["verify.sh"]}, "does not run as root")


class HttpTest(HelperCase):
    def test_token_is_checked_and_routes_answer(self):
        server = podhttp.serve(0, "secret-token", githelper.ROUTES, host="127.0.0.1")
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        url = f"http://127.0.0.1:{server.server_address[1]}"

        def call(path: str, body: JsonObject, token: str | None) -> tuple[int, JsonObject]:
            request = urllib.request.Request(url + path, data=json.dumps(body).encode())
            if token:
                request.add_header("X-Pod-Token", token)
            try:
                with urllib.request.urlopen(request, timeout=30) as response:
                    return response.status, json.loads(response.read())
            except urllib.error.HTTPError as error:
                return error.code, json.loads(error.read())

        body = {"url": self.origin, "source_branch": "main", "work_branch": "agent/work-1"}
        self.assertEqual(call("/clone", body, None)[0], 401)
        self.assertEqual(call("/clone", body, "wrong")[0], 401)
        self.assertEqual(os.listdir(self.workspace), [])
        status, answer = call("/clone", body, "secret-token")
        self.assertEqual((status, answer["ok"], answer["start_commit"]), (200, True, self.first))
        self.assertEqual(run("git", "config", "user.email", cwd=self.workspace), "agent@opensoftwarefactory.local")
        status, answer = call("/git", {"args": ["push"]}, "secret-token")
        self.assertEqual((status, answer["ok"]), (200, False))
        self.assertIn("not allowed here", answer["error"])
        self.assertEqual(call("/push", {"branch": "agent/work-1"}, "")[0], 401)


if __name__ == "__main__":
    unittest.main()
