package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// podError is a refusal or failure the pod's own servers reported. Its text is written for
// whoever made the call, so it is passed on unchanged.
type podError struct{ msg string }

func (e podError) Error() string { return e.msg }

// commit is one commit of the work branch as git-helper reports it.
type commit struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
}

// helperResult is the answer of git-helper; each endpoint fills its own fields.
type helperResult struct {
	Output      string   `json:"output"`
	Error       string   `json:"error"`
	StartCommit string   `json:"start_commit"`
	Patch       string   `json:"patch"`
	Commits     []commit `json:"commits"`
	OK          bool     `json:"ok"`
}

// commandResult is the result of the tool server's run_command.
type commandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// conn finds the pod's address and the tokens of its servers, and remembers them.
func (s *service) conn(ctx context.Context, id string) (podConn, error) {
	s.mu.Lock()
	c, ok := s.conns[id]
	s.mu.Unlock()
	if ok {
		return c, nil
	}
	info, err := s.kube.Pod(ctx, podName(id))
	if errors.Is(err, errNotFound) {
		return c, statusError(404, "dev pod %s no longer exists", id)
	}
	if err != nil {
		return c, err
	}
	if info.IP == "" {
		return c, fmt.Errorf("dev pod %s has no address yet", id)
	}
	tokens, err := s.kube.Secret(ctx, podName(id))
	if err != nil {
		return c, fmt.Errorf("reading the tokens of dev pod %s: %w", id, err)
	}
	c = podConn{ip: info.IP, workToken: tokens["work-token"], helperToken: tokens["helper-token"]}
	s.mu.Lock()
	s.conns[id] = c
	s.mu.Unlock()
	return c, nil
}

func (s *service) forget(id string) {
	s.mu.Lock()
	delete(s.conns, id)
	s.mu.Unlock()
}

// callPod sends one request to a server of the pod. The context limits its time.
func (s *service) callPod(ctx context.Context, id string, port int, method, path string, in any) (status int, data []byte, err error) {
	c, err := s.conn(ctx, id)
	if err != nil {
		return 0, nil, err
	}
	var body io.Reader
	if in != nil {
		encoded, marshalErr := json.Marshal(in)
		if marshalErr != nil {
			return 0, nil, marshalErr
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.podURL(c.ip, port)+path, body)
	if err != nil {
		return 0, nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	token := c.workToken
	if port == helperPort {
		token = c.helperToken
	}
	req.Header.Set("X-Pod-Token", token)
	resp, err := s.podHTTP.Do(req)
	if err != nil {
		// The pod may have been restarted with another address.
		s.forget(id)
		return 0, nil, fmt.Errorf("dev pod %s does not answer: %w", id, err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	var response io.Reader = resp.Body
	if path != skillScriptPath {
		response = io.LimitReader(response, 64<<20)
	}
	data, err = io.ReadAll(response)
	return resp.StatusCode, data, err
}

// op runs one operation of the tool server in the work container.
func (s *service) op(ctx context.Context, id, name string, args any) (json.RawMessage, error) {
	return s.opWithin(ctx, id, name, args, 11*time.Minute)
}

func (s *service) opWithin(ctx context.Context, id, name string, args any, limit time.Duration) (json.RawMessage, error) {
	if args == nil {
		args = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	path := opPath
	if name == toolRunSkillScript {
		path = skillScriptPath
	}
	status, data, err := s.callPod(ctx, id, workPort, http.MethodPost, path, map[string]any{"op": name, "args": args})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
		OK     bool            `json:"ok"`
	}
	if json.Unmarshal(data, &answer) != nil || (status != 200 && answer.Error == "") {
		return nil, fmt.Errorf("the dev pod's tool server answered %s with status %d: %s", name, status, tail(string(data), 500))
	}
	if !answer.OK {
		return nil, podError{answer.Error}
	}
	if answer.Result == nil {
		answer.Result = json.RawMessage("null")
	}
	return answer.Result, nil
}

// runCommand runs a shell command in the work container.
func (s *service) runCommand(ctx context.Context, id, command string, timeoutSeconds int) (commandResult, error) {
	var out commandResult
	args := map[string]any{"command": command}
	if timeoutSeconds > 0 {
		args["timeout_seconds"] = timeoutSeconds
	}
	raw, err := s.op(ctx, id, toolRunCommand, args)
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("the dev pod's tool server returned a command result that cannot be read: %s", tail(string(raw), 500))
	}
	return out, nil
}

// helper calls git-helper, the only container that holds the repository's credential.
func (s *service) helper(ctx context.Context, id, path string, in any) (helperResult, error) {
	var out helperResult
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	status, data, err := s.callPod(ctx, id, helperPort, http.MethodPost, path, in)
	if err != nil {
		return out, err
	}
	if json.Unmarshal(data, &out) != nil {
		return out, fmt.Errorf("the dev pod's git-helper answered %s with status %d: %s", path, status, tail(string(data), 500))
	}
	if !out.OK || status != 200 {
		msg := strings.TrimSpace(out.Error + "\n" + out.Output)
		if msg == "" {
			msg = fmt.Sprintf("git-helper answered %s with status %d", path, status)
		}
		return out, podError{msg}
	}
	return out, nil
}

// podFailure answers a request whose call into the pod failed: 422 when the pod's server
// refused the call, 404 when the pod is gone, 502 otherwise.
func (s *service) podFailure(w http.ResponseWriter, err error) {
	var pe podError
	if errors.As(err, &pe) {
		s.fail(w, 422, err)
		return
	}
	s.fail(w, 502, err)
}

// ready returns the pod of a request when it can be used.
func (s *service) ready(w http.ResponseWriter, r *http.Request) (devPod, bool) {
	p, ok := s.pod(r.PathValue("id"))
	if !ok {
		s.fail(w, 404, errors.New("this dev pod does not exist"))
		return p, false
	}
	if p.State != stateReady {
		s.fail(w, 409, errors.New("this dev pod is still being created"))
		return p, false
	}
	return p, true
}

// execPod runs one command for a command step. No rule is checked, because the workflow's
// author wrote the command, and it does not count as agent activity.
func (s *service) execPod(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := readJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	if strings.TrimSpace(in.Command) == "" {
		s.fail(w, 400, errors.New("command is required"))
		return
	}
	p, ok := s.ready(w, r)
	if !ok {
		return
	}
	out, err := s.runCommand(r.Context(), p.ID, in.Command, in.TimeoutSeconds)
	if err != nil {
		s.podFailure(w, err)
		return
	}
	writeJSON(w, 200, out)
}

// viewFiles and viewFile answer with the tool server's own result of list_files and read_file.
func (s *service) viewFiles(w http.ResponseWriter, r *http.Request) {
	p, ok := s.ready(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "."
	}
	files, err := s.op(r.Context(), p.ID, "list_files", map[string]any{"path": path})
	if err != nil {
		s.podFailure(w, err)
		return
	}
	writeJSON(w, 200, files)
}

func (s *service) viewFile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.ready(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		s.fail(w, 400, errors.New("path is required"))
		return
	}
	content, err := s.op(r.Context(), p.ID, "read_file", map[string]any{"path": path})
	if err != nil {
		s.podFailure(w, err)
		return
	}
	writeJSON(w, 200, content)
}

func (s *service) viewGit(w http.ResponseWriter, r *http.Request, args func(*devPod) []string) {
	p, ok := s.ready(w, r)
	if !ok {
		return
	}
	out, err := s.helper(r.Context(), p.ID, "/git", map[string]any{"args": args(&p)})
	if err != nil {
		s.podFailure(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"output": out.Output})
}

func (s *service) viewLog(w http.ResponseWriter, r *http.Request) {
	s.viewGit(w, r, func(*devPod) []string { return []string{"log", "-n", "200", "--date=iso", "--decorate"} })
}

// viewDiff shows everything the pod changed since it was created from the source branch,
// committed or not.
func (s *service) viewDiff(w http.ResponseWriter, r *http.Request) {
	s.viewGit(w, r, func(p *devPod) []string { return []string{"diff", p.base()} })
}

// base is the commit the work branch started from; the source branch stands in for it when
// the clone did not report one.
func (p *devPod) base() string {
	if p.StartCommit != "" {
		return p.StartCommit
	}
	return p.SourceBranch
}
