"""Tests of the pod tool server. Run: python3 -m unittest discover -s services/devpod/tests"""

import json
import os
import shutil
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
import podhttp  # noqa: E402
import toolserver  # noqa: E402
from podhttp import JsonObject, RefusalError  # noqa: E402


class WorkspaceCase(unittest.TestCase):
    def setUp(self):
        self.base = os.path.realpath(tempfile.mkdtemp(prefix="devpod-test-"))
        self.addCleanup(shutil.rmtree, self.base, ignore_errors=True)
        self.root = os.path.join(self.base, "workspace")
        os.makedirs(os.path.join(self.root, "src"))
        os.makedirs(os.path.join(self.root, ".git"))
        self.put("src/panel.py", "def status_panel(claim_id):\n    return {}\n")
        self.put(".git/config", "[core]\n")
        home = os.path.join(self.base, "home")
        os.makedirs(home)
        patches = [
            mock.patch.object(toolserver, "WORKSPACE", self.root),
            mock.patch.dict(os.environ, {"HOME": home, "POD_TOKEN": "must-not-leak"}),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def put(self, relative: str, content: str) -> None:
        path = os.path.join(self.root, relative)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(content)

    def get(self, relative: str) -> str:
        with open(os.path.join(self.root, relative), encoding="utf-8") as handle:
            return handle.read()

    def refused(self, function: Callable[[JsonObject], object], args: JsonObject, *words: str) -> str:
        with self.assertRaises(RefusalError) as caught:
            function(args)
        for word in words:
            self.assertIn(word, str(caught.exception))
        return str(caught.exception)


class PathTest(WorkspaceCase):
    def test_parent_folder_is_refused(self):
        self.refused(toolserver.read_file, {"path": "../secret.txt"}, "outside /workspace")
        self.refused(toolserver.write_file, {"path": "src/../../x", "content": "x"}, "outside /workspace")

    def test_absolute_path_outside_is_refused(self):
        self.refused(toolserver.read_file, {"path": "/etc/passwd"}, "outside /workspace")
        self.refused(toolserver.list_files, {"path": "/"}, "outside /workspace")
        self.refused(toolserver.read_file, {"path": "/workspacefoo/x"}, "outside /workspace")

    def test_absolute_path_inside_workspace_is_accepted(self):
        result = toolserver.read_file({"path": "/workspace/src/panel.py"})
        self.assertEqual(result["path"], "src/panel.py")

    def test_git_folder_is_refused_for_every_file_operation(self):
        self.refused(toolserver.read_file, {"path": ".git/config"}, ".git")
        self.refused(toolserver.write_file, {"path": ".git/hooks/pre-commit", "content": "x"}, ".git")
        self.refused(toolserver.edit_file, {"path": ".git/config", "old_text": "a", "new_text": "b"}, ".git")
        self.refused(toolserver.list_files, {"path": ".git"}, ".git")
        self.refused(toolserver.search, {"pattern": "core", "path": ".git"}, ".git")
        self.refused(toolserver.write_file, {"path": "src/.git/x", "content": "x"}, ".git")
        self.assertFalse(os.path.exists(os.path.join(self.root, ".git", "hooks")))

    def test_symlink_that_leaves_the_workspace_is_refused(self):
        outside = os.path.join(self.base, "outside.txt")
        with open(outside, "w") as handle:
            handle.write("secret")
        os.symlink(outside, os.path.join(self.root, "link.txt"))
        os.symlink(self.base, os.path.join(self.root, "up"))
        os.symlink(os.path.join(self.root, ".git"), os.path.join(self.root, "gitlink"))
        self.refused(toolserver.read_file, {"path": "link.txt"}, "outside /workspace")
        self.refused(toolserver.write_file, {"path": "up/new.txt", "content": "x"}, "outside /workspace")
        self.refused(toolserver.read_file, {"path": "gitlink/config"}, ".git")
        self.assertFalse(os.path.exists(os.path.join(self.base, "new.txt")))

    def test_missing_path_is_refused(self):
        self.refused(toolserver.read_file, {}, "path")
        self.refused(toolserver.read_file, {"path": "src/none.py"}, "does not exist")


class FileTest(WorkspaceCase):
    def test_list_files_hides_git(self):
        result = toolserver.list_files({"path": "."})
        names = [entry["name"] for entry in result["entries"]]
        self.assertEqual(names, ["src"])
        self.assertEqual(result["entries"][0]["type"], "dir")
        inner = toolserver.list_files({"path": "src"})
        self.assertEqual(inner["entries"], [{"name": "panel.py", "type": "file", "size": 42}])
        self.assertEqual(toolserver.list_files({})["path"], ".")

    def test_list_files_on_a_file_is_refused(self):
        self.refused(toolserver.list_files, {"path": "src/panel.py"}, "is a file")

    def test_read_file_whole_and_line_range(self):
        self.put("lines.txt", "one\ntwo\nthree\nfour\n")
        whole = toolserver.read_file({"path": "lines.txt"})
        self.assertEqual((whole["content"], whole["total_lines"]), ("one\ntwo\nthree\nfour\n", 4))
        part = toolserver.read_file({"path": "lines.txt", "start_line": 2, "end_line": 3})
        self.assertEqual((part["content"], part["start_line"], part["end_line"]), ("two\nthree\n", 2, 3))
        tail = toolserver.read_file({"path": "lines.txt", "start_line": 4})
        self.assertEqual(tail["content"], "four\n")
        beyond = toolserver.read_file({"path": "lines.txt", "start_line": 3, "end_line": 99})
        self.assertEqual((beyond["content"], beyond["end_line"]), ("three\nfour\n", 4))

    def test_read_file_wrong_ranges_are_refused(self):
        self.put("lines.txt", "one\ntwo\n")
        self.refused(toolserver.read_file, {"path": "lines.txt", "start_line": 0}, "start_line")
        self.refused(toolserver.read_file, {"path": "lines.txt", "start_line": 3, "end_line": 2}, "before")
        self.refused(toolserver.read_file, {"path": "lines.txt", "start_line": 9}, "has 2 lines")
        self.refused(toolserver.read_file, {"path": "lines.txt", "start_line": "x"}, "whole number")
        self.refused(toolserver.read_file, {"path": "src"}, "is a folder")

    def test_write_file_creates_folders_and_overwrites(self):
        first = toolserver.write_file({"path": "docs/new/a.md", "content": "hello\n"})
        self.assertEqual((first["path"], first["created"], first["bytes"]), ("docs/new/a.md", True, 6))
        second = toolserver.write_file({"path": "docs/new/a.md", "content": "changed"})
        self.assertFalse(second["created"])
        self.assertEqual(self.get("docs/new/a.md"), "changed")
        self.refused(toolserver.write_file, {"path": "x.txt"}, "content")

    @unittest.skipIf(os.geteuid() == 0, "root can write everywhere")
    def test_write_to_a_read_only_file_names_protection(self):
        self.put("verify.sh", "#!/bin/sh\n")
        os.chmod(os.path.join(self.root, "verify.sh"), 0o444)
        self.refused(toolserver.write_file, {"path": "verify.sh", "content": "exit 0\n"}, "protected")
        self.refused(toolserver.edit_file, {"path": "verify.sh", "old_text": "#!/bin/sh", "new_text": "x"}, "protected")
        self.assertEqual(self.get("verify.sh"), "#!/bin/sh\n")

    def test_edit_file_replaces_exactly_one_occurrence(self):
        self.put("a.txt", "alpha beta\ngamma beta\n")
        result = toolserver.edit_file({"path": "a.txt", "old_text": "alpha beta", "new_text": "alpha delta"})
        self.assertEqual(result, {"path": "a.txt", "replaced": 1})
        self.assertEqual(self.get("a.txt"), "alpha delta\ngamma beta\n")

    def test_edit_file_says_when_the_text_is_missing_or_repeated(self):
        self.put("a.txt", "alpha beta\ngamma beta\n")
        self.refused(toolserver.edit_file, {"path": "a.txt", "old_text": "omega", "new_text": "x"}, "was not found")
        self.refused(toolserver.edit_file, {"path": "a.txt", "old_text": "beta", "new_text": "x"}, "occurs 2 times")
        self.refused(toolserver.edit_file, {"path": "a.txt", "old_text": "", "new_text": "x"}, "empty")
        self.refused(toolserver.edit_file, {"path": "a.txt", "old_text": "beta", "new_text": "beta"}, "same")
        self.refused(toolserver.edit_file, {"path": "none.txt", "old_text": "a", "new_text": "b"}, "does not exist")
        self.assertEqual(self.get("a.txt"), "alpha beta\ngamma beta\n")


@unittest.skipUnless(shutil.which("rg"), "ripgrep is not installed")
class SearchTest(WorkspaceCase):
    def test_search_finds_lines_and_skips_git(self):
        self.put(".git/notes", "status_panel\n")
        self.put(".hidden/conf", "status_panel hidden\n")
        result = toolserver.search({"pattern": "status_panel"})
        found = sorted((match["path"], match["line"]) for match in result["matches"])
        self.assertEqual(found, [(".hidden/conf", 1), ("src/panel.py", 1)])
        self.assertFalse(result["truncated"])
        self.assertEqual(result["matches"][0]["text"].count("\n"), 0)

    def test_search_in_a_path_and_without_a_match(self):
        self.put("docs/a.md", "status_panel\n")
        result = toolserver.search({"pattern": "status_panel", "path": "docs"})
        self.assertEqual([match["path"] for match in result["matches"]], ["docs/a.md"])
        self.assertEqual(toolserver.search({"pattern": "nothing-here"})["matches"], [])

    def test_search_returns_at_most_100_matches(self):
        self.put("many.txt", "needle\n" * 250)
        result = toolserver.search({"pattern": "needle"})
        self.assertEqual(len(result["matches"]), 100)
        self.assertTrue(result["truncated"])
        self.assertIn("first 100 of 250", result["note"])

    def test_search_with_a_wrong_pattern_is_refused(self):
        self.refused(toolserver.search, {"pattern": "(unclosed"}, "search failed")
        self.refused(toolserver.search, {"pattern": ""}, "empty")


class CommandTest(WorkspaceCase):
    def test_exit_code_output_and_folder(self):
        result = toolserver.run_command({"command": "pwd; echo problem >&2; exit 3"})
        self.assertEqual(result["exit_code"], 3)
        self.assertEqual(result["stdout"].strip().splitlines()[-1], self.root)
        self.assertIn("problem", result["stderr"])
        self.assertNotIn("timed_out", result)

    def test_long_output_is_cut_with_a_note(self):
        result = toolserver.run_command({"command": "head -c 50000 /dev/zero | tr '\\0' 'z'; echo END"})
        self.assertEqual(result["exit_code"], 0)
        self.assertIn("[output cut: 50004 characters in total", result["stdout"])
        kept = result["stdout"].replace("z", "")
        self.assertLessEqual(result["stdout"].count("z"), toolserver.OUTPUT_LIMIT)
        self.assertGreaterEqual(result["stdout"].count("z"), toolserver.OUTPUT_LIMIT - 10)
        self.assertTrue(result["stdout"].endswith("END\n"), kept)

    def test_short_output_is_not_cut(self):
        self.assertEqual(toolserver.cut("x" * toolserver.OUTPUT_LIMIT), "x" * toolserver.OUTPUT_LIMIT)
        self.assertIn("output cut", toolserver.cut("x" * (toolserver.OUTPUT_LIMIT + 1)))

    def test_timeout_stops_the_command(self):
        started = time.monotonic()
        result = toolserver.run_command({"command": "echo before; sleep 20; echo after", "timeout_seconds": 1})
        self.assertLess(time.monotonic() - started, 10)
        self.assertEqual(result["exit_code"], 124)
        self.assertTrue(result["timed_out"])
        self.assertIn("before", result["stdout"])
        self.assertNotIn("after", result["stdout"])
        self.assertIn("stopped after 1 seconds", result["stderr"])

    def test_timeout_limits(self):
        self.refused(toolserver.run_command, {"command": "true", "timeout_seconds": 0}, "between 1 and 600")
        self.refused(toolserver.run_command, {"command": "  "}, "empty")
        with mock.patch.object(toolserver.subprocess, "Popen") as popen:
            popen.return_value.wait.return_value = 0
            toolserver.run_command({"command": "true", "timeout_seconds": 5000})
            popen.return_value.wait.assert_called_with(timeout=600)
            toolserver.run_command({"command": "true"})
            popen.return_value.wait.assert_called_with(timeout=120)

    def test_background_process_does_not_block_the_answer(self):
        started = time.monotonic()
        result = toolserver.run_command({"command": "sleep 15 & echo started"})
        self.assertLess(time.monotonic() - started, 10)
        self.assertIn("started", result["stdout"])

    def test_the_pod_token_is_not_visible_to_commands(self):
        result = toolserver.run_command({"command": "echo token=[${POD_TOKEN:-}]"})
        self.assertIn("token=[]", result["stdout"])


class SkillScriptTest(WorkspaceCase):
    def run_script(
        self,
        script: str,
        files: JsonObject | None = None,
        args: object = None,
        timeout: object = 5,
        max_lines: object = 1000,
        max_bytes: object = 10240,
    ) -> JsonObject:
        skill_files: JsonObject = {"scripts/run.sh": script} if files is None else files
        return toolserver.run_skill_script(
            {
                "files": skill_files,
                "script": "scripts/run.sh",
                "args": args or [],
                "timeout_seconds": timeout,
                "max_lines": max_lines,
                "max_bytes": max_bytes,
            }
        )

    def test_runs_directly_with_arguments_in_workspace_and_cleans_up(self):
        def scratch():
            return {name for name in os.listdir("/tmp") if name.startswith("factory-skill-")}

        before = scratch()
        arguments = ["a b; c", '"quotes"', "$(echo not-expanded)", "line\nbreak"]
        result = self.run_script("#!/bin/sh\nprintf '%s\\n' \"$PWD\"\nprintf '<%s>' \"$@\"\nprintf 'problem' >&2\nexit 7\n", args=arguments)
        self.assertEqual(result["exit_code"], 7)
        self.assertEqual(result["stdout"], self.root + "\n" + "".join(f"<{value}>" for value in arguments))
        self.assertEqual(result["stderr"], "problem")
        self.assertFalse(result["timed_out"])
        self.assertEqual(scratch(), before)

    def test_files_with_shebangs_are_executable_and_other_files_are_available(self):
        result = self.run_script(
            '#!/bin/sh\ncat "$(dirname "$0")/../data.txt"\n',
            {"scripts/run.sh": '#!/bin/sh\ncat "$(dirname "$0")/../data.txt"\n', "data.txt": "hello\n"},
        )
        self.assertEqual(result["stdout"], "hello\n")

    def test_output_keeps_the_last_byte_and_line_windows(self):
        result = self.run_script(
            "#!/bin/sh\nprintf 'first\\nsecond-long\\nthird\\n'\nprintf 'a\\nb\\nc\\n' >&2\n", max_lines=2, max_bytes=13
        )
        self.assertEqual(result["stdout"], "third\n")
        self.assertEqual(result["stderr"], "b\nc\n")
        self.assertTrue(result["stdout_cut"])
        self.assertTrue(result["stderr_cut"])

    def test_one_line_larger_than_byte_limit_keeps_its_end(self):
        result = self.run_script("#!/bin/sh\nprintf 'abcdefghijklmnop'\n", max_bytes=5)
        self.assertEqual(result["stdout"], "lmnop")
        self.assertTrue(result["stdout_cut"])
        result = self.run_script("#!/bin/sh\nprintf 'abcdefghijklmnop\\n'\n", max_bytes=5)
        self.assertEqual(result["stdout"], "mnop\n")
        self.assertTrue(result["stdout_cut"])

    def test_a_cut_inside_a_utf8_character_uses_replacement(self):
        result = self.run_script("#!/bin/sh\nprintf 'xéz'\n", max_bytes=2)
        self.assertEqual(result["stdout"], "\ufffdz")
        self.assertTrue(result["stdout_cut"])

    def test_concurrent_calls_have_isolated_directories_and_remove_them(self):
        script = "#!/bin/sh\nprintf '%s\\n' \"$0\"\nsleep 0.2\n"
        with ThreadPoolExecutor(max_workers=2) as executor:
            paths = [result["stdout"].strip() for result in executor.map(self.run_script, (script, script))]
        self.assertNotEqual(paths[0], paths[1])
        for path in paths:
            self.assertTrue(path.startswith("/tmp/factory-skill-"))
            self.assertFalse(os.path.exists(os.path.dirname(os.path.dirname(path))))

    def test_finishing_the_script_kills_background_children(self):
        result = self.run_script("#!/bin/sh\n(sleep 1; touch unexpected-background-output) &\nprintf 'complete'\n")
        self.assertEqual(result["stdout"], "complete")
        time.sleep(1.1)
        self.assertFalse(os.path.exists(os.path.join(self.root, "unexpected-background-output")))

    def test_timeout_kills_the_process_group_and_keeps_output_ends(self):
        started = time.monotonic()
        result = self.run_script("#!/bin/sh\necho before\nsleep 20 &\nwait\n", timeout=1)
        self.assertLess(time.monotonic() - started, 6)
        self.assertTrue(result["timed_out"])
        self.assertEqual(result["exit_code"], 124)
        self.assertIn("before", result["stdout"])

    def test_missing_shebang_and_invalid_input_are_refused(self):
        self.refused(lambda _: self.run_script("echo no"), {}, "missing a shebang")
        self.refused(
            toolserver.run_skill_script,
            {
                "files": {"../run.sh": "#!/bin/sh\n"},
                "script": "../run.sh",
                "args": [],
                "timeout_seconds": 1,
                "max_lines": 1,
                "max_bytes": 1,
            },
            "invalid",
        )
        self.refused(
            toolserver.run_skill_script,
            {"files": {"run.sh": "#!/bin/sh\n"}, "script": "run.sh", "args": [1], "timeout_seconds": 1, "max_lines": 1, "max_bytes": 1},
            "list of text",
        )


class HttpTest(WorkspaceCase):
    def setUp(self):
        super().setUp()
        self.server = podhttp.serve(
            0,
            "secret-token",
            {"/op": toolserver.handle_op, "/skill-script": toolserver.handle_skill_script},
            host="127.0.0.1",
            unbounded_paths=("/skill-script",),
        )
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"

    def call(self, path: str, body: JsonObject | None = None, token: str | None = None, raw: bytes | None = None) -> tuple[int, JsonObject]:
        data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
        request = urllib.request.Request(self.url + path, data=data)
        if token is not None:
            request.add_header("X-Pod-Token", token)
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as error:
            return error.code, json.loads(error.read())

    def test_health_needs_no_token(self):
        self.assertEqual(self.call("/healthz"), (200, {"ok": True}))

    def test_skill_operation_has_no_regular_request_limit(self):
        body = {
            "op": "run_skill_script",
            "args": {
                "files": {"run.sh": "#!/bin/sh\necho hello"},
                "script": "run.sh",
                "timeout_seconds": 1,
                "max_lines": 2,
                "max_bytes": 16,
            },
        }
        with mock.patch.object(podhttp, "MAX_BODY_BYTES", 32):
            self.assertEqual(self.call("/op", body, token="secret-token")[0], 413)
            self.assertEqual(self.call("/skill-script", body, token="wrong")[0], 401)
            status, response = self.call("/skill-script", body, token="secret-token")
        self.assertEqual(status, 200)
        self.assertEqual(response["result"]["stdout"], "hello\n")

    def test_missing_or_wrong_token_is_refused(self):
        body = {"op": "list_files", "args": {"path": "."}}
        for token in (None, "", "wrong", "secret-token2"):
            status, answer = self.call("/op", body, token=token)
            self.assertEqual(status, 401)
            self.assertFalse(answer["ok"])
            self.assertIn("X-Pod-Token", answer["error"])

    def test_an_empty_configured_token_refuses_everything(self):
        server = podhttp.serve(0, "", {"/op": toolserver.handle_op}, host="127.0.0.1")
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.url = f"http://127.0.0.1:{server.server_address[1]}"
        self.assertEqual(self.call("/op", {"op": "list_files"}, token="")[0], 401)

    def test_op_answers_in_the_contract_shape(self):
        status, answer = self.call("/op", {"op": "read_file", "args": {"path": "src/panel.py"}}, token="secret-token")
        self.assertEqual(status, 200)
        self.assertTrue(answer["ok"])
        self.assertIn("status_panel", answer["result"]["content"])
        status, answer = self.call("/op", {"op": "read_file", "args": {"path": "../x"}}, token="secret-token")
        self.assertEqual((status, answer["ok"]), (200, False))
        self.assertIn("outside /workspace", answer["error"])

    def test_unknown_op_path_and_body(self):
        status, answer = self.call("/op", {"op": "delete_all"}, token="secret-token")
        self.assertEqual((status, answer["ok"]), (200, False))
        self.assertIn("Unknown op", answer["error"])
        self.assertEqual(self.call("/other", {}, token="secret-token")[0], 404)
        self.assertEqual(self.call("/op", raw=b"not json", token="secret-token")[0], 400)

    def test_take_token_removes_it_from_the_environment(self):
        with mock.patch.dict(os.environ, {"POD_TOKEN": "abc"}), mock.patch.object(podhttp.sys, "platform", "test"):
            self.assertEqual(podhttp.take_token(), "abc")
            self.assertNotIn("POD_TOKEN", os.environ)


if __name__ == "__main__":
    unittest.main()
