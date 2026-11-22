# Dev pod image

One image, `opensoftwarefactory-devpod:local`, for both containers of a dev pod. The connector
service creates the pods; this image has no deployment of its own. The contracts are in
the service APIs and tests.

| Container | Command | User | Port | Holds |
| --- | --- | --- | --- | --- |
| `work` | `python /opt/devpod/toolserver.py` | 1000 | 8080 | the agent's commands; no credential |
| `git-helper` | `python /opt/devpod/githelper.py` | 0 | 8081 | the credential at `/credential/token` |

Both share `/workspace` and refuse every call without the header `X-Pod-Token: <POD_TOKEN>`
(`GET /healthz` is open). Both use only the Python standard library.

## Tool server (`work`)

`POST /op` with `{"op": "<name>", "args": {...}}` answers 200 with `{"ok": true, "result": ...}`
or `{"ok": false, "error": "<sentence for the agent>"}`.

| Op | Result |
| --- | --- |
| `list_files{path}` | `{path, entries: [{name, type: "file" or "dir", size}], truncated}`; one level, without `.git` |
| `read_file{path, start_line?, end_line?}` | `{path, content, start_line, end_line, total_lines, note?}` |
| `write_file{path, content}` | `{path, created, bytes}` |
| `edit_file{path, old_text, new_text}` | `{path, replaced: 1}`; refused when the text occurs zero or several times |
| `search{pattern, path?}` | `{matches: [{path, line, text}], truncated, note?}`; ripgrep, at most 100 matches |
| `run_command{command, timeout_seconds?}` | `{exit_code, stdout, stderr, timed_out?}`; `bash -lc` in `/workspace`, 120 s by default, 600 s at most; a stopped command has exit code 124 |

Paths are relative to `/workspace`; a path outside it or inside `.git` is refused. Output over
20 000 characters keeps its first and last 10 000 characters with a note between them.

## git-helper

| Request | Answer |
| --- | --- |
| `POST /clone {url, source_branch, work_branch, author_name, author_email}` | `{ok, start_commit, output, work_branch, work_branch_existed}` |
| `POST /protect {paths: [globs]}` | `{ok, protected: [paths], unmatched: [globs], output}` |
| `POST /push {branch, url?}` | `{ok, output, branch, commit}` |
| `POST /patch {base}` | `{ok, patch, commits: [{hash, subject, author}]}` |
| `POST /git {args: [...]}` | `{ok, output}`; only `log`, `diff`, `show`, `ls-files`, `rev-parse` |

A failure answers 200 with `{"ok": false, "error": "<sentence>", "output": "<git output>"}`.

- **Credential.** The token is sent as `Authorization: Basic base64("x-access-token:<token>")`,
  only to the host of the repository URL, through a one-off `http.extraHeader` in the
  environment of one git process. It is never written to a file. Without a credential file
  the helper clones and pushes without one.
- **The agent's repository is not trusted.** The agent can change `.git/config` and
  `.git/hooks`. Every git command that reads `/workspace` runs as user 1000 without the
  credential. A push copies the branch as a bundle into a private repository of the
  git-helper and pushes from there to the URL that the helper remembered at the clone.
  Only the work branch can be pushed.
- **Protected paths.** A glob uses `*` and `?` inside one path part and `**` across parts.
  A matched folder (`tests/**`, `tests/` or `tests`) is protected with everything in it.
  Protected files and folders are owned by user 2000 and not writable by others; every
  folder above them is owned by user 2000 with mode 1777 (sticky bit), so user 1000 can
  still create its own files there but cannot rename or delete a protected entry.
- **Needs in the pod.** The git-helper container runs as user 0 with the capabilities
  `CHOWN`, `FOWNER`, `DAC_OVERRIDE`, `SETUID` and `SETGID`. The credential Secret is mounted
  with mode 0400.

## Tests

```bash
python3 -m unittest discover -s services/devpod/tests
```
