package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

var skillPathSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var skillVersion = regexp.MustCompile(`^v[1-9][0-9]*$`)

type skillToolInput struct {
	Tool    string                     `json:"tool"`
	Agent   string                     `json:"agent"`
	Run     string                     `json:"run"`
	Pod     string                     `json:"pod"`
	Skill   string                     `json:"skill"`
	Version string                     `json:"version"`
	Args    map[string]json.RawMessage `json:"args"`
}

type skillScriptResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	StdoutCut bool   `json:"stdout_cut"`
	StderrCut bool   `json:"stderr_cut"`
	TimedOut  bool   `json:"timed_out"`
}

func (s *service) skillTool(w http.ResponseWriter, r *http.Request) {
	var in skillToolInput
	if err := readJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	result, err := s.runSkillTool(r.Context(), in)
	if err != nil {
		s.log.Info("skill tool call refused or failed", "tool", in.Tool, "skill", in.Skill,
			"agent", in.Agent, "run", in.Run, "error", err)
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": result})
}

func (s *service) runSkillTool(ctx context.Context, in skillToolInput) (any, error) {
	if !contains(skillTools, in.Tool) {
		return nil, fmt.Errorf("Refused: %q is not a skill tool. The tools are: %s.", in.Tool, strings.Join(skillTools, ", "))
	}
	switch in.Tool {
	case "read_skill":
		if err := validateSkillNameVersion(in.Skill, in.Version); err != nil {
			return nil, err
		}
		file := rawText(in.Args, "file")
		if file == "" {
			file = "SKILL.md"
		}
		if err := validateSkillFile(file); err != nil {
			return nil, err
		}
		files, err := s.skillFiles(ctx, in.Skill, in.Version)
		if err != nil {
			return nil, err
		}
		content, ok := files[file]
		if !ok {
			return nil, missingSkillFile(in.Skill, file, files)
		}
		return content, nil
	case "run_skill_script":
		return s.runSkillScript(ctx, in)
	}
	panic("validated skill tool was not handled")
}

func (s *service) runSkillScript(ctx context.Context, in skillToolInput) (any, error) {
	if in.Pod == "" {
		return nil, errors.New("Refused: this run has no dev pod, so a skill script cannot run. The workflow needs a devpod step before this node.")
	}
	p, ok := s.pod(in.Pod)
	if !ok {
		return nil, errors.New("Refused: the dev pod of this run does not exist any more. A dev pod is deleted when no agent used it for its idle time or when a person destroyed it. Nothing can be done in it; say so in your answer and stop.")
	}
	if p.State != stateReady {
		return nil, errors.New("Refused: the dev pod is still being created. Try again in a moment.")
	}
	if in.Run == "" || !contains(p.Users, in.Run) {
		return nil, fmt.Errorf("Refused: run %q does not use this dev pod, so it may not run a skill script in it. A run can only work in the dev pod its own workflow created.", in.Run)
	}
	if err := validateSkillNameVersion(in.Skill, in.Version); err != nil {
		return nil, err
	}
	script := rawText(in.Args, "script")
	if err := validateSkillFile(script); err != nil {
		return nil, err
	}
	arguments := []string{}
	if raw, ok := in.Args["args"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, errors.New("Refused: skill script args must be a list of text values. Pass each argument as a separate string.")
		}
	}
	characters := 0
	for _, argument := range arguments {
		characters += utf8.RuneCountInString(argument)
	}
	if characters > s.cfg.skillArgsChars {
		return nil, fmt.Errorf("Refused: the skill script arguments contain %d characters; the limit is %d. Pass fewer or shorter arguments.", characters, s.cfg.skillArgsChars)
	}
	files, err := s.skillFiles(ctx, in.Skill, in.Version)
	if err != nil {
		return nil, err
	}
	if _, ok := files[script]; !ok {
		return nil, missingSkillFile(in.Skill, script, files)
	}
	timeout := rawInt(in.Args, "timeout_seconds")
	if timeout <= 0 {
		timeout = s.cfg.skillTimeout
	}
	maxSeconds := int64((time.Duration(1<<63-1) / time.Second) - 30)
	if int64(timeout) > maxSeconds {
		return nil, fmt.Errorf("Refused: the skill script timeout of %d seconds is too large. Configure a shorter timeout.", timeout)
	}
	s.active(ctx, in.Pod)
	defer s.active(ctx, in.Pod)
	raw, err := s.opWithin(ctx, in.Pod, "run_skill_script", map[string]any{
		"files": files, "script": script, "args": arguments, "timeout_seconds": timeout,
		"max_lines": s.cfg.skillOutputLines, "max_bytes": s.cfg.skillOutputBytes,
	}, (time.Duration(timeout)+30)*time.Second)
	if err != nil {
		return nil, err
	}
	var result skillScriptResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("the dev pod returned a skill script result that cannot be read: %s", tail(string(raw), 500))
	}
	if result.TimedOut {
		return nil, fmt.Errorf("Refused: the skill script %q reached its %d second time limit. The retained output was:\nstdout:\n%s\nstderr:\n%s",
			script, timeout, result.Stdout, result.Stderr)
	}
	out := map[string]any{"exit_code": result.ExitCode, "stdout": result.Stdout, "stderr": result.Stderr}
	if warning := skillOutputWarning(result, s.cfg); warning != "" {
		out["warning"] = warning
	}
	return out, nil
}

func (s *service) skillFiles(ctx context.Context, skill, version string) (map[string]string, error) {
	segments := strings.Split(skill, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	endpoint := s.skillRegistry + "/skills/" + strings.Join(segments, "/") + "/" + url.PathEscape(version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Refused: skill %q version %s could not be read from the skill registry: %v. Try again.", skill, version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("Refused: skill %q version %s does not exist. Fix the agent to use a stored skill version.", skill, version)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return nil, fmt.Errorf("Refused: skill %q version %s could not be read from the skill registry: %s %s. Try again.", skill, version, resp.Status, strings.TrimSpace(string(data)))
	}
	var stored struct {
		Files map[string]string `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stored); err != nil || stored.Files == nil {
		return nil, fmt.Errorf("Refused: skill %q version %s has an unreadable registry response. Fix the stored skill version.", skill, version)
	}
	return stored.Files, nil
}

func validateSkillNameVersion(skill, version string) error {
	parts := strings.Split(skill, "/")
	if len(parts) < 1 || len(parts) > 4 {
		return fmt.Errorf("Refused: skill name %q is invalid. Use one to four slash-separated name segments.", skill)
	}
	for _, part := range parts {
		if !skillPathSegment.MatchString(part) || part == "." || part == ".." {
			return fmt.Errorf("Refused: skill name %q is invalid. Use letters, digits, dots, dashes and underscores in each segment.", skill)
		}
	}
	if !skillVersion.MatchString(version) {
		return fmt.Errorf("Refused: skill version %q is invalid. Use a pinned version such as v3.", version)
	}
	return nil
}

func validateSkillFile(file string) error {
	if file == "" || strings.HasPrefix(file, "/") {
		return fmt.Errorf("Refused: skill file path %q is invalid. Use a relative path such as SKILL.md or scripts/check.sh.", file)
	}
	for _, part := range strings.Split(file, "/") {
		if !skillPathSegment.MatchString(part) || part == "." || part == ".." {
			return fmt.Errorf("Refused: skill file path %q is invalid. Use relative segments of letters, digits, dots, dashes and underscores.", file)
		}
	}
	return nil
}

func missingSkillFile(skill, file string, files map[string]string) error {
	listed := make([]string, 0, len(files))
	for name := range files {
		listed = append(listed, name)
	}
	sort.Strings(listed)
	return fmt.Errorf("Refused: %q is not a file of skill %q. Choose one of: %s.", file, skill, strings.Join(listed, ", "))
}

func rawText(args map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(args[key], &value)
	return value
}

func rawInt(args map[string]json.RawMessage, key string) int {
	var value int
	_ = json.Unmarshal(args[key], &value)
	return value
}

func skillOutputWarning(result skillScriptResult, cfg config) string {
	parts := []string{}
	if result.StdoutCut {
		parts = append(parts, fmt.Sprintf("Standard output was longer than the limit. Only the last %d lines or %d bytes are shown.", cfg.skillOutputLines, cfg.skillOutputBytes))
	}
	if result.StderrCut {
		parts = append(parts, fmt.Sprintf("Standard error was longer than the limit. Only the last %d lines or %d bytes are shown.", cfg.skillOutputLines, cfg.skillOutputBytes))
	}
	return strings.Join(parts, " ")
}
