package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// devpodTools are the tools of the MCP service devpod, in the order the plan names them.
var devpodTools = []string{
	"list_files", "read_file", "write_file", "edit_file", "search", "run_command", "report_blocked",
	"push_branch", "create_pull_request", "update_pull_request", "get_pull_request", "comment_on_pull_request",
}

// toolInput is one tool call of an agent, as the tool gateway forwards it.
type toolInput struct {
	Tool  string                     `json:"tool"`
	Args  map[string]json.RawMessage `json:"args"`
	Agent string                     `json:"agent"`
	Run   string                     `json:"run"`
}

// The names of the tools the service treats in a way of their own.
const (
	toolRunCommand     = "run_command"
	toolRunSkillScript = "run_skill_script"
)

// A reference such as course/builder@v3; it is put into a shell command, so it is checked.
var agentRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@-]{0,199}$`)

// text reads a string argument; a missing one is empty.
func (in toolInput) text(key string) string {
	var v string
	_ = json.Unmarshal(in.Args[key], &v)
	return v
}

func (in toolInput) has(key string) bool {
	raw, ok := in.Args[key]
	return ok && string(raw) != "null"
}

// tool answers every call with 200: ok with the result, or not ok with a message for the
// agent that names what was refused, why, and what to do instead.
func (s *service) tool(w http.ResponseWriter, r *http.Request) {
	var in toolInput
	if err := readJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	result, err := s.runTool(r.Context(), r.PathValue("id"), in)
	if err != nil {
		s.log.Info("tool call refused or failed", "pod", r.PathValue("id"), "tool", in.Tool, "agent", in.Agent, "run", in.Run, "error", err)
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": result})
}

func (s *service) runTool(ctx context.Context, id string, in toolInput) (any, error) {
	p, ok := s.pod(id)
	if !ok {
		return nil, errors.New("Refused: the dev pod of this run does not exist any more. A dev pod is deleted when no agent used it for its idle time or when a person destroyed it. Nothing can be done in it; say so in your answer and stop.")
	}
	if p.State != stateReady {
		return nil, errors.New("Refused: the dev pod is still being created. Try again in a moment.")
	}
	if in.Run == "" || !contains(p.Users, in.Run) {
		return nil, fmt.Errorf("Refused: run %q does not use this dev pod, so it may not call tools on it. A run can only work in the dev pod its own workflow created.", in.Run)
	}
	if !contains(devpodTools, in.Tool) {
		return nil, fmt.Errorf("Refused: %q is not a dev pod tool. The tools are: %s.", in.Tool, strings.Join(devpodTools, ", "))
	}
	s.active(ctx, id)
	defer s.active(ctx, id)
	switch in.Tool {
	case "list_files", "read_file", "search":
		return s.op(ctx, id, in.Tool, in.Args)
	case "write_file", "edit_file":
		if err := checkWrite(&p, in); err != nil {
			return nil, err
		}
		return s.op(ctx, id, in.Tool, in.Args)
	case toolRunCommand:
		return s.agentCommand(ctx, &p, in)
	case "report_blocked":
		return s.reportBlocked(ctx, &p, in)
	}
	return s.gitTool(ctx, &p, in)
}

// active records agent activity; the pod's idle time starts again.
func (s *service) active(ctx context.Context, id string) {
	if _, err := s.change(ctx, id, func(p *devPod) error { p.LastActivity = s.now(); return nil }); err != nil {
		s.log.Warn("recording agent activity", "pod", id, "error", err)
	}
}

// checkWrite refuses a write to a protected path and a write a rule blocks.
func checkWrite(p *devPod, in toolInput) error {
	if strings.TrimSpace(in.text("path")) == "" {
		return fmt.Errorf(`Refused: %s needs a path. Pass the file's path relative to the workspace, for example "src/panel.py".`, in.Tool)
	}
	file := workspacePath(in.text("path"))
	if pattern := protectedBy(p.Protected, file); pattern != "" {
		return fmt.Errorf("Refused: %s on %q. The file is protected in this dev pod (protected: %q), so an agent may not change it. Leave it as it is and change other files. If the task cannot be done without changing it, call report_blocked with the reason.", in.Tool, file, pattern)
	}
	content := in.text("content")
	if in.Tool == "edit_file" {
		content = in.text("new_text")
	}
	for _, rl := range p.Rules {
		if rl.applies(in.Tool, file, content) {
			return fmt.Errorf("Refused: %s on %q was blocked by a rule of this dev pod. %s", in.Tool, file, rl.Message)
		}
	}
	return nil
}

// agentCommand runs a command for an agent when the agent's allowed commands and the pod's
// rules let it.
func (s *service) agentCommand(ctx context.Context, p *devPod, in toolInput) (any, error) {
	command := in.text("command")
	patterns, err := s.allowedCommands(ctx, in.Agent)
	if err != nil {
		return nil, fmt.Errorf("Refused: the command %q was not run, because the allowed commands of the agent %q could not be read: %v. Try again; if it keeps failing, call report_blocked with the reason.", command, in.Agent, err)
	}
	err = commandAllowed(patterns, command)
	if err != nil {
		return nil, err
	}
	for _, rl := range p.Rules {
		if rl.applies(toolRunCommand, "", command) {
			return nil, fmt.Errorf("Refused: the command %q was blocked by a rule of this dev pod. %s", strings.TrimSpace(command), rl.Message)
		}
	}
	s.setAuthor(ctx, p, in.Agent)
	args := map[string]any{"command": command}
	var timeout float64
	if json.Unmarshal(in.Args["timeout_seconds"], &timeout) == nil && timeout > 0 {
		args["timeout_seconds"] = int(min(timeout, 600))
	}
	return s.op(ctx, p.ID, toolRunCommand, args)
}

// setAuthor makes the agent, with its version, the author of the commits it creates.
func (s *service) setAuthor(ctx context.Context, p *devPod, agent string) {
	if agent == p.Author || !agentRef.MatchString(agent) {
		return
	}
	out, err := s.runCommand(ctx, p.ID, fmt.Sprintf("git config user.name '%s' && git config user.email '%s'", agent, authorEmail), 30)
	if err != nil || out.ExitCode != 0 {
		s.log.Warn("setting the commit author", "pod", p.ID, "agent", agent, "error", err, "output", out.Stderr)
		return
	}
	if _, err = s.change(ctx, p.ID, func(p *devPod) error { p.Author = agent; return nil }); err != nil {
		s.log.Warn("recording the commit author", "pod", p.ID, "error", err)
	}
}

func (s *service) reportBlocked(ctx context.Context, p *devPod, in toolInput) (any, error) {
	reason := strings.TrimSpace(in.text("reason"))
	if reason == "" {
		return nil, errors.New("Refused: report_blocked needs a reason. Say in one or two sentences what stops the work and what would be needed to continue.")
	}
	_, err := s.change(ctx, p.ID, func(p *devPod) error {
		p.Blocked = append(p.Blocked, blockedReport{Run: in.Run, Agent: in.Agent, Reason: reason, Time: s.now()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("The blocked report could not be recorded: %v. Call report_blocked again.", err)
	}
	return "Recorded. The work ends here as blocked with your reason. Do not call any more tools.", nil
}
