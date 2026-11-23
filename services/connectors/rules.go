package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

var ruleTools = map[string]bool{"write_file": true, "edit_file": true, toolRunCommand: true}

func (r rule) check() error {
	if !ruleTools[r.Tool] {
		return errors.New("tool must be write_file, edit_file or run_command")
	}
	if strings.TrimSpace(r.Message) == "" {
		return errors.New("message is required: it tells the agent which rule blocked the call, why, and what to do instead")
	}
	if r.Matches != "" {
		if _, err := regexp.Compile(r.Matches); err != nil {
			return fmt.Errorf("matches is not a regular expression: %v", err)
		}
	}
	return nil
}

// applies reports whether the rule blocks a call of tool on a path (empty for run_command)
// with the given new content or command.
func (r rule) applies(tool, file, text string) bool {
	if r.Tool != tool {
		return false
	}
	if r.Path != "" && tool != toolRunCommand && !globMatch(r.Path, file) {
		return false
	}
	if r.Matches != "" {
		re, err := regexp.Compile(r.Matches)
		return err == nil && re.MatchString(text)
	}
	return true
}

// workspacePath turns a path an agent gave into one relative to the workspace.
func workspacePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/workspace")
	p = path.Clean("/" + p)
	return strings.TrimPrefix(p, "/")
}

// globMatch reports whether a path, or a folder it lies in, matches a pattern. "*" and "?"
// stay within one path segment, "**" crosses segments: "tests/**" covers everything under
// tests, "verify.sh" the one file, "docs" the folder docs and all it holds.
func globMatch(pattern, file string) bool {
	pattern = strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(pattern)), "/")
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	for candidate := file; candidate != "" && candidate != "."; candidate = path.Dir(candidate) {
		if re.MatchString(candidate) {
			return true
		}
	}
	return false
}

// protectedBy returns the pattern that protects a path, or "".
func protectedBy(patterns []string, file string) string {
	for _, pattern := range patterns {
		if globMatch(pattern, file) {
			return pattern
		}
	}
	return ""
}

// commandMatches reports whether a command matches a pattern completely. "*" stands for any
// text; every other character stands for itself.
func commandMatches(pattern, command string) bool {
	parts := strings.Split(pattern, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	re, err := regexp.Compile("(?s)^" + strings.Join(parts, ".*") + "$")
	return err == nil && re.MatchString(command)
}

// hasShellOperator reports whether a command chains, pipes or substitutes commands.
func hasShellOperator(command string) bool {
	return strings.ContainsAny(command, ";|`\n\r") || strings.Contains(command, "&&") || strings.Contains(command, "$(")
}

// commandAllowed checks a command against an agent's allowed command patterns. A command
// with a shell operator passes only when a pattern is exactly that text; otherwise "git log*"
// would also allow "git log; rm -rf .".
func commandAllowed(patterns []string, command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return errors.New(`refused: run_command needs a command. Pass the command to run, for example "git status"`)
	}
	allowed := "This agent has no allowed commands."
	if len(patterns) > 0 {
		allowed = "Allowed commands (* stands for any text): " + strings.Join(patterns, ", ") + "."
	}
	if hasShellOperator(command) {
		for _, pattern := range patterns {
			if strings.TrimSpace(pattern) == command {
				return nil
			}
		}
		return fmt.Errorf("refused: the command %q joins several commands with ;, &&, ||, |, backticks, $( or a line break, and that is only allowed when an allowed command is exactly this text. Run one allowed command per call. %s If the task cannot be done this way, call report_blocked with the reason", command, allowed)
	}
	for _, pattern := range patterns {
		if commandMatches(strings.TrimSpace(pattern), command) {
			return nil
		}
	}
	return fmt.Errorf("refused: the command %q is not one of this agent's allowed commands. %s Run an allowed command instead. If the task cannot be done with them, call report_blocked with the reason", command, allowed)
}

type cachedCommands struct {
	read     time.Time
	patterns []string
}

// allowedCommands reads the commands setting of an agent's agent.json from the agent
// registry. A pinned version never changes, so it is read once; an unpinned agent is read
// again after half a minute.
func (s *service) allowedCommands(ctx context.Context, agent string) (patterns []string, err error) {
	name, version, pinned := strings.Cut(agent, "@")
	if name == "" {
		return nil, errors.New("the tool call does not say which agent makes it")
	}
	if !pinned || version == "" {
		version = "latest"
	}
	s.mu.Lock()
	cached, ok := s.commands[agent]
	s.mu.Unlock()
	if ok && (pinned || s.now().Sub(cached.read) < 30*time.Second) {
		return cached.patterns, nil
	}
	segments := strings.Split(name, "/")
	for i, part := range segments {
		segments[i] = url.PathEscape(part)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.registry+"/agents/"+strings.Join(segments, "/")+"/"+url.PathEscape(version), http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, errors.New("the agent registry is not reachable")
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("the agent registry does not know the agent %s", agent)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("the agent registry answered %s", resp.Status)
	}
	var entry struct {
		Files struct {
			Definition struct {
				Commands []string `json:"commands"`
			} `json:"agent.json"`
		} `json:"files"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		return nil, errors.New("the agent registry returned an agent.json that cannot be read")
	}
	s.mu.Lock()
	s.commands[agent] = cachedCommands{patterns: entry.Files.Definition.Commands, read: s.now()}
	s.mu.Unlock()
	return entry.Files.Definition.Commands, nil
}
