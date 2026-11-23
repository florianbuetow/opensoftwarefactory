package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func skillRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	files := map[string]string{
		"SKILL.md":           "---\nname: git\ndescription: Git procedures.\n---\n",
		"release/SKILL.md":   "---\nname: release\ndescription: Release.\n---\n",
		"scripts/check.sh":   "#!/bin/sh\necho check\n",
		"scripts/cut.sh":     "#!/bin/sh\necho cut\n",
		"scripts/timeout.sh": "#!/bin/sh\nsleep 20\n",
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/skills/git/v3" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "git", "version": "v3", "files": files})
	}))
}

func configureSkills(w *world, registry string) {
	w.s.skillRegistry = registry
	w.s.cfg.skillTimeout = 180
	w.s.cfg.skillOutputLines = 1000
	w.s.cfg.skillOutputBytes = 10240
	w.s.cfg.skillArgsChars = 1000
}

func (w *world) skill(body map[string]any) (bool, any, string) {
	w.t.Helper()
	status, out := w.call("POST", "/api/skills/tool", body)
	if status != 200 {
		w.t.Fatalf("skill tool: status %d: %v", status, out)
	}
	message, _ := out["error"].(string)
	return out["ok"].(bool), out["result"], message
}

func TestReadSkillReturnsOnlyStoredFilesAndNeedsNoPod(t *testing.T) {
	w := newWorld(t)
	registry := skillRegistry(t)
	defer registry.Close()
	configureSkills(w, registry.URL)
	base := map[string]any{"tool": "read_skill", "pod": "", "agent": builder, "run": "run-1",
		"skill": "git", "version": "v3", "args": map[string]any{"file": "SKILL.md"}}
	ok, result, message := w.skill(base)
	if !ok || !strings.Contains(result.(string), "Git procedures") || message != "" {
		t.Fatalf("ok=%v result=%v message=%q", ok, result, message)
	}
	base["args"] = map[string]any{"file": "../secret"}
	if ok, _, message = w.skill(base); ok || !strings.Contains(message, "relative segments") {
		t.Fatalf("invalid path: ok=%v message=%q", ok, message)
	}
	base["args"] = map[string]any{"file": "missing.txt"}
	if ok, _, message = w.skill(base); ok || !strings.Contains(message, "scripts/check.sh") {
		t.Fatalf("missing file: ok=%v message=%q", ok, message)
	}
}

func TestRunSkillScriptChecksPodPassesFilesAndReturnsWarnings(t *testing.T) {
	w := newWorld(t)
	registry := skillRegistry(t)
	defer registry.Close()
	configureSkills(w, registry.URL)
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	body := map[string]any{"tool": "run_skill_script", "pod": id, "agent": builder, "run": "run-1",
		"skill": "git", "version": "v3", "args": map[string]any{"script": "scripts/cut.sh", "args": []string{"a b"}, "timeout_seconds": 4}}
	w.advance(2 * time.Minute)
	ok, result, message := w.skill(body)
	if !ok || message != "" {
		t.Fatalf("ok=%v result=%v message=%q", ok, result, message)
	}
	got := result.(map[string]any)
	if got["exit_code"] != 0.0 || !strings.Contains(got["warning"].(string), "Standard output") {
		t.Fatalf("result=%v", got)
	}
	op := w.pod.ops[len(w.pod.ops)-1]
	if op["timeout_seconds"] != 4.0 || op["max_lines"] != 1000.0 || op["max_bytes"] != 10240.0 {
		t.Fatalf("pod args=%v", op)
	}
	if files := op["files"].(map[string]any); !strings.HasPrefix(files["scripts/check.sh"].(string), "#!") {
		t.Fatalf("files=%v", files)
	}
	p, _ := w.s.pod(id)
	if p.LastActivity.Sub(p.Created) != 2*time.Minute {
		t.Fatalf("skill script did not count as activity: %v", p.LastActivity)
	}

	body["args"] = map[string]any{"script": "scripts/timeout.sh"}
	if ok, _, message = w.skill(body); ok || !strings.Contains(message, "180 second time limit") || !strings.Contains(message, "skill output") {
		t.Fatalf("timeout: ok=%v message=%q", ok, message)
	}
}

func TestRunSkillScriptRefusalsDoNotReachThePod(t *testing.T) {
	w := newWorld(t)
	registry := skillRegistry(t)
	defer registry.Close()
	configureSkills(w, registry.URL)
	base := map[string]any{"tool": "run_skill_script", "pod": "", "agent": builder, "run": "run-1",
		"skill": "git", "version": "v3", "args": map[string]any{"script": "scripts/check.sh"}}
	if ok, _, message := w.skill(base); ok || !strings.Contains(message, "needs a devpod step") {
		t.Fatalf("without pod: ok=%v message=%q", ok, message)
	}
	if len(w.pod.called()) != 0 {
		t.Fatalf("calls=%v", w.pod.called())
	}
}

func TestRunSkillScriptValidatesCallBeforeReadingRegistry(t *testing.T) {
	w := newWorld(t)
	configureSkills(w, "http://127.0.0.1:1")
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	body := map[string]any{"tool": "run_skill_script", "pod": id, "agent": builder, "run": "run-1",
		"skill": "git", "version": "v3", "args": map[string]any{"script": "../bad.sh"}}
	if ok, _, message := w.skill(body); ok || !strings.Contains(message, "relative segments") {
		t.Fatalf("path validation: ok=%v message=%q", ok, message)
	}
	body["args"] = map[string]any{"script": "scripts/check.sh", "args": []string{strings.Repeat("x", 1001)}}
	if ok, _, message := w.skill(body); ok || !strings.Contains(message, "1001 characters") {
		t.Fatalf("argument validation: ok=%v message=%q", ok, message)
	}
}

func TestRunSkillScriptArgumentLimitBoundary(t *testing.T) {
	w := newWorld(t)
	registry := skillRegistry(t)
	defer registry.Close()
	configureSkills(w, registry.URL)
	w.s.cfg.skillArgsChars = 1000
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	body := map[string]any{"tool": "run_skill_script", "pod": id, "agent": builder, "run": "run-1",
		"skill": "git", "version": "v3", "args": map[string]any{"script": "scripts/check.sh", "args": []string{strings.Repeat("é", 1000)}}}
	before := len(w.pod.called())
	if ok, _, message := w.skill(body); !ok {
		t.Fatalf("1000 characters refused: %s", message)
	}
	body["args"] = map[string]any{"script": "scripts/check.sh", "args": []string{strings.Repeat("é", 1001)}}
	if ok, _, message := w.skill(body); ok || !strings.Contains(message, "1001 characters") {
		t.Fatalf("1001 characters: ok=%v message=%q", ok, message)
	}
	if got := len(w.pod.called()); got != before+1 {
		t.Fatalf("refused arguments reached pod: %v", w.pod.called()[before:])
	}
}

func TestRunSkillScriptRefusesTimeoutThatWouldOverflowDuration(t *testing.T) {
	w := newWorld(t)
	registry := skillRegistry(t)
	defer registry.Close()
	configureSkills(w, registry.URL)
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	body := map[string]any{"tool": "run_skill_script", "pod": id, "agent": builder, "run": "run-1",
		"skill": "git", "version": "v3", "args": map[string]any{"script": "scripts/check.sh", "timeout_seconds": int(^uint(0) >> 1)}}
	before := len(w.pod.called())
	if ok, _, message := w.skill(body); ok || !strings.Contains(message, "timeout") || !strings.Contains(message, "too large") {
		t.Fatalf("overflow timeout: ok=%v message=%q", ok, message)
	}
	if got := len(w.pod.called()); got != before {
		t.Fatalf("overflow timeout reached pod: %v", w.pod.called()[before:])
	}
}

func TestSkillConfigDefaults(t *testing.T) {
	t.Setenv("SKILL_SCRIPT_TIMEOUT_SECONDS", "")
	t.Setenv("SKILL_OUTPUT_MAX_LINES", "")
	t.Setenv("SKILL_OUTPUT_MAX_BYTES", "")
	t.Setenv("SKILL_ARGS_MAX_CHARS", "")
	cfg := configFromEnv()
	got := []int{cfg.skillTimeout, cfg.skillOutputLines, cfg.skillOutputBytes, cfg.skillArgsChars}
	if want := []int{180, 1000, 10240, 1000}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skill settings=%v want=%v", got, want)
	}
}
