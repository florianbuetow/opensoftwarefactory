package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
)

// Services of dev pods: a Linux sandbox with a clone of a Git repository on a work branch.
const (
	// DevPodService marks the step that creates the run's dev pod.
	DevPodService = "devpod"
	// DevPodStopService marks a step that deletes the run's dev pod.
	DevPodStopService = "devpod-stop"
	// CommandService marks a step that runs one command in the run's dev pod without a model.
	CommandService = "command"
)

// Activity names of the orchestrator's own activities for dev pods and rounds.
const (
	CreateDevPodActivity    = "CreateDevPod"
	RunCommandActivity      = "RunCommand"
	StopDevPodActivity      = "StopDevPod"
	ReleaseDevPodActivity   = "ReleaseDevPod"
	PublishRequestsActivity = "PublishRequests"
	DecidePublishActivity   = "DecidePublish"
	KeepRoundsActivity      = "KeepRounds"
)

const (
	// DefaultCommandTimeout bounds a command step that sets no timeout.
	DefaultCommandTimeout = 2 * time.Minute
	// MaxCommandTimeout is the longest a command may run in a dev pod.
	MaxCommandTimeout = 10 * time.Minute
	// RunPlaceholder in a work branch stands for the first eight characters of the run ID.
	RunPlaceholder = "{run}"
	// PullRequestFile is the name of the pull request record the connector service stores
	// under the run prefix.
	PullRequestFile = "pull-request.json"
)

// Rule blocks a tool call in a dev pod with a message.
type Rule struct {
	Tool    string `yaml:"tool" json:"tool"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty"`
	Matches string `yaml:"matches,omitempty" json:"matches,omitempty"`
	Message string `yaml:"message" json:"message"`
}

// DevPod names the dev pod of a run.
type DevPod struct {
	ID           string `json:"id"`
	Repository   string `json:"repository"`
	SourceBranch string `json:"source_branch"`
	WorkBranch   string `json:"work_branch"`
}

// DevPodOutput is the output document of a devpod step.
type DevPodOutput struct {
	Pod          string `json:"pod"`
	Repository   string `json:"repository"`
	SourceBranch string `json:"source_branch"`
	WorkBranch   string `json:"work_branch"`
	StartCommit  string `json:"start_commit"`
	Reused       bool   `json:"reused"`
}

// CommandOutput is the output document of a command step.
type CommandOutput struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// CreatePod is the body of POST /api/devpods.
type CreatePod struct {
	Repository   string   `json:"repository"`
	SourceBranch string   `json:"source_branch"`
	WorkBranch   string   `json:"work_branch"`
	Name         string   `json:"name,omitempty"`
	Run          string   `json:"run"`
	Protected    []string `json:"protected,omitempty"`
	Rules        []Rule   `json:"rules,omitempty"`
}

// CreatedPod is the answer to POST /api/devpods.
type CreatedPod struct {
	ID          string `json:"id"`
	StartCommit string `json:"start_commit,omitempty"`
	Reused      bool   `json:"reused"`
}

// PodExec is the body of POST /api/devpods/{id}/exec.
type PodExec struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// PodExecResult is the answer to POST /api/devpods/{id}/exec.
type PodExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Publish request kinds and states.
const (
	PublishPush        = "push"
	PublishPullRequest = "pull_request"
	PublishPending     = "pending"
)

// PublishRequest is a push or pull request an agent asked for that waits for a person.
type PublishRequest struct {
	Kind  string `json:"kind"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	State string `json:"state"`
	N     int    `json:"n"`
}

// PublishDecision is the body of POST /api/devpods/{id}/publish-requests/{n}/decision.
type PublishDecision struct {
	Decision  string `json:"decision"`
	Comment   string `json:"comment"`
	DecidedBy string `json:"decided_by"`
}

// Connectors is the part of the connector service the orchestrator calls.
type Connectors interface {
	CreatePod(ctx context.Context, req *CreatePod) (CreatedPod, error)
	Hold(ctx context.Context, pod, run string) error
	Release(ctx context.Context, pod, run string) error
	Exec(ctx context.Context, pod string, req PodExec) (PodExecResult, error)
	DeletePod(ctx context.Context, pod string) error
	PublishRequests(ctx context.Context, pod, run string) ([]PublishRequest, error)
	DecidePublish(ctx context.Context, pod string, n int, d PublishDecision) error
}

// ConnectorError is an answer of the connector service that is not a success.
type ConnectorError struct {
	Message string
	Status  int
}

func (e *ConnectorError) Error() string {
	return fmt.Sprintf("connector service answered %d: %s", e.Status, e.Message)
}

// HTTPConnectors is the client for the connector service's HTTP API.
type HTTPConnectors struct {
	Client  *http.Client
	BaseURL string
}

// NewHTTPConnectorsFromEnv uses CONNECTORS_URL, defaulting to the in-cluster connector service.
func NewHTTPConnectorsFromEnv(client *http.Client) HTTPConnectors {
	if client == nil {
		client = http.DefaultClient
	}
	return HTTPConnectors{BaseURL: strings.TrimRight(env("CONNECTORS_URL", "http://connectors:8080"), "/"), Client: client}
}

// call sends one request. A body is sent as JSON; a successful answer is decoded into out
// when out is set.
func (c HTTPConnectors) call(ctx context.Context, method, path string, body, out any) (err error) {
	var reader io.Reader
	if body != nil {
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return marshalErr
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("the connector service is not reachable: %w", err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		message := strings.TrimSpace(string(data))
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &failure) == nil && failure.Error != "" {
			message = failure.Error
		}
		if message == "" {
			message = resp.Status
		}
		return &ConnectorError{Status: resp.StatusCode, Message: message}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("the connector service answered %s %s with JSON that cannot be read: %w", method, path, err)
	}
	return nil
}

func podPath(pod, rest string) string {
	return "/api/devpods/" + url.PathEscape(pod) + rest
}

// CreatePod asks the connector service for a dev pod and waits until it is cloned and set up.
func (c HTTPConnectors) CreatePod(ctx context.Context, req *CreatePod) (CreatedPod, error) {
	var out CreatedPod
	err := c.call(ctx, http.MethodPost, "/api/devpods", req, &out)
	return out, err
}

// Hold keeps the dev pod alive for the run.
func (c HTTPConnectors) Hold(ctx context.Context, pod, run string) error {
	return c.call(ctx, http.MethodPost, podPath(pod, "/hold"), map[string]string{"run": run}, nil)
}

// Release tells the connector service that the run no longer needs the dev pod.
func (c HTTPConnectors) Release(ctx context.Context, pod, run string) error {
	return c.call(ctx, http.MethodPost, podPath(pod, "/release"), map[string]string{"run": run}, nil)
}

// Exec runs one command in the dev pod.
func (c HTTPConnectors) Exec(ctx context.Context, pod string, req PodExec) (PodExecResult, error) {
	var out PodExecResult
	err := c.call(ctx, http.MethodPost, podPath(pod, "/exec"), req, &out)
	return out, err
}

// DeletePod deletes the dev pod.
func (c HTTPConnectors) DeletePod(ctx context.Context, pod string) error {
	return c.call(ctx, http.MethodDelete, podPath(pod, ""), nil, nil)
}

// PublishRequests lists the publish requests the agents of the run made in the dev pod.
func (c HTTPConnectors) PublishRequests(ctx context.Context, pod, run string) ([]PublishRequest, error) {
	var out []PublishRequest
	err := c.call(ctx, http.MethodGet, podPath(pod, "/publish-requests?run="+url.QueryEscape(run)), nil, &out)
	return out, err
}

// DecidePublish sends a person's decision about the n-th publish request of the dev pod.
func (c HTTPConnectors) DecidePublish(ctx context.Context, pod string, n int, d PublishDecision) error {
	return c.call(ctx, http.MethodPost, podPath(pod, fmt.Sprintf("/publish-requests/%d/decision", n)), d, nil)
}

// WorkBranchName fills the run placeholder of a work branch with the first eight characters
// of the run ID.
func WorkBranchName(template, run string) string {
	if len(run) > 8 {
		run = run[:8]
	}
	return strings.ReplaceAll(template, RunPlaceholder, run)
}

// CommandTimeout is the time a command step lets its command run.
func (s *Step) CommandTimeout() time.Duration {
	if d, err := time.ParseDuration(s.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultCommandTimeout
}

// connectors returns the connector service, or an error that is not retried when the
// orchestrator has none.
func (a *Activities) connectors() (Connectors, error) {
	if a.Connectors == nil {
		return nil, temporal.NewNonRetryableApplicationError("the connector service is not configured", "NoConnectors", nil)
	}
	return a.Connectors, nil
}

// connectorFailure makes an answer that refuses the request final: only a failure to reach
// the connector service, or a failure inside it, is worth another attempt.
func connectorFailure(err error) error {
	var refused *ConnectorError
	if errors.As(err, &refused) && refused.Status < 500 {
		return temporal.NewNonRetryableApplicationError(refused.Message, "ConnectorRefused", err)
	}
	return err
}

// CreateDevPodRequest describes the dev pod a devpod step creates. Metadata names the run's
// metadata object, which receives the pod.
type CreateDevPodRequest struct {
	TraceID      string   `json:"trace_id"`
	Step         string   `json:"step"`
	Repository   string   `json:"repository"`
	SourceBranch string   `json:"source_branch"`
	WorkBranch   string   `json:"work_branch"`
	Name         string   `json:"name,omitempty"`
	Protected    []string `json:"protected,omitempty"`
	Artifact     string   `json:"artifact"`
	Metadata     string   `json:"metadata"`
	Rules        []Rule   `json:"rules,omitempty"`
}

// DevPodResult is the artifact of a devpod step and the pod it created.
type DevPodResult struct {
	Artifact string `json:"artifact"`
	Pod      DevPod `json:"pod"`
}

// CreateDevPod asks the connector service for the dev pod, waits until it is cloned and set
// up, holds it for the run, stores the step's output document, and records the pod in the
// run's metadata.
func (a *Activities) CreateDevPod(ctx context.Context, req *CreateDevPodRequest) (DevPodResult, error) {
	c, err := a.connectors()
	if err != nil {
		return DevPodResult{}, err
	}
	if req.WorkBranch == req.SourceBranch {
		return DevPodResult{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("the work branch %q must differ from the source branch", req.WorkBranch), "InvalidWorkBranch", nil,
		)
	}
	created, err := c.CreatePod(ctx, &CreatePod{
		Repository: req.Repository, SourceBranch: req.SourceBranch, WorkBranch: req.WorkBranch, Name: req.Name,
		Run: req.TraceID, Protected: req.Protected, Rules: req.Rules,
	})
	if err != nil {
		return DevPodResult{}, connectorFailure(fmt.Errorf("creating the dev pod: %w", err))
	}
	if created.ID == "" {
		return DevPodResult{}, temporal.NewNonRetryableApplicationError("the connector service returned a dev pod without an id", "InvalidDevPod", nil)
	}
	if err = c.Hold(ctx, created.ID, req.TraceID); err != nil {
		return DevPodResult{}, connectorFailure(fmt.Errorf("holding dev pod %q: %w", created.ID, err))
	}
	if created.StartCommit == "" {
		// An answer without the commit: ask the pod where its work branch stands.
		if head, execErr := c.Exec(ctx, created.ID, PodExec{Command: "git rev-parse HEAD", TimeoutSeconds: 30}); execErr == nil && head.ExitCode == 0 {
			created.StartCommit = strings.TrimSpace(head.Stdout)
		}
	}
	pod := DevPod{ID: created.ID, Repository: req.Repository, SourceBranch: req.SourceBranch, WorkBranch: req.WorkBranch}
	meta := Meta{Creator: sessionCreator, TraceID: req.TraceID}
	out := DevPodOutput{
		Pod: pod.ID, Repository: pod.Repository, SourceBranch: pod.SourceBranch, WorkBranch: pod.WorkBranch,
		StartCommit: created.StartCommit, Reused: created.Reused,
	}
	err = a.putJSON(ctx, req.Artifact, out, meta)
	if err != nil {
		return DevPodResult{}, err
	}
	err = a.setMetadata(ctx, req.Metadata, "dev_pod", pod, meta)
	if err != nil {
		return DevPodResult{}, err
	}
	return DevPodResult{Artifact: req.Artifact, Pod: pod}, nil
}

// setMetadata sets one field of a run's metadata and keeps every other field. A run without
// metadata is left as it is.
func (a *Activities) setMetadata(ctx context.Context, id, field string, value any, meta Meta) error {
	if id == "" {
		return nil
	}
	data, err := a.Store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %q: %w", id, err)
	}
	fields := map[string]json.RawMessage{}
	if err = json.Unmarshal(data, &fields); err != nil {
		return temporal.NewNonRetryableApplicationError(fmt.Sprintf("%q is not a JSON object", id), "InvalidMetadata", err)
	}
	if fields[field], err = json.Marshal(value); err != nil {
		return err
	}
	if data, err = json.MarshalIndent(fields, "", "  "); err != nil {
		return err
	}
	if err = a.Store.Put(ctx, id, data, meta); err != nil {
		return fmt.Errorf("writing %q: %w", id, err)
	}
	return nil
}

// CommandRequest names the command a command step runs in the run's dev pod.
type CommandRequest struct {
	TraceID        string `json:"trace_id"`
	Step           string `json:"step"`
	Pod            string `json:"pod"`
	Command        string `json:"command"`
	Artifact       string `json:"artifact"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	AllowFailure   bool   `json:"allow_failure,omitempty"`
}

// RunCommand runs the command in the dev pod and stores the command, its exit code, and its
// output as the step's output document. An exit code other than 0 fails the step unless the
// step allows failure; the output document is stored either way.
func (a *Activities) RunCommand(ctx context.Context, req *CommandRequest) (StepResult, error) {
	c, err := a.connectors()
	if err != nil {
		return StepResult{}, err
	}
	if req.Pod == "" {
		return StepResult{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("step %q has no dev pod to run its command in", req.Step), "NoDevPod", nil,
		)
	}
	res, err := c.Exec(ctx, req.Pod, PodExec{Command: req.Command, TimeoutSeconds: req.TimeoutSeconds})
	if err != nil {
		return StepResult{}, connectorFailure(fmt.Errorf("running %q in dev pod %q: %w", req.Command, req.Pod, err))
	}
	out := CommandOutput{Command: req.Command, ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}
	err = a.putJSON(ctx, req.Artifact, out, Meta{Creator: CommandService, TraceID: req.TraceID})
	if err != nil {
		return StepResult{}, err
	}
	if res.ExitCode != 0 && !req.AllowFailure {
		return StepResult{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("command %q ended with exit code %d; its output is in %s", req.Command, res.ExitCode, req.Artifact), "CommandFailed", nil,
		)
	}
	return StepResult{Artifact: req.Artifact}, nil
}

// StopDevPodRequest names the dev pod a devpod-stop step deletes.
type StopDevPodRequest struct {
	TraceID  string `json:"trace_id"`
	Pod      string `json:"pod"`
	Artifact string `json:"artifact"`
}

// StopDevPod deletes the dev pod. A pod that is already gone counts as deleted.
func (a *Activities) StopDevPod(ctx context.Context, req StopDevPodRequest) (StepResult, error) {
	c, err := a.connectors()
	if err != nil {
		return StepResult{}, err
	}
	if req.Pod == "" {
		return StepResult{}, temporal.NewNonRetryableApplicationError("the run has no dev pod to stop", "NoDevPod", nil)
	}
	var refused *ConnectorError
	if err = c.DeletePod(ctx, req.Pod); err != nil && !(errors.As(err, &refused) && refused.Status == http.StatusNotFound) {
		return StepResult{}, connectorFailure(fmt.Errorf("deleting dev pod %q: %w", req.Pod, err))
	}
	out := map[string]any{"pod": req.Pod, "stopped": true}
	err = a.putJSON(ctx, req.Artifact, out, Meta{Creator: sessionCreator, TraceID: req.TraceID})
	if err != nil {
		return StepResult{}, err
	}
	return StepResult{Artifact: req.Artifact}, nil
}

// ReleaseDevPodRequest names a dev pod and the run that no longer needs it.
type ReleaseDevPodRequest struct {
	Pod string `json:"pod"`
	Run string `json:"run"`
}

// ReleaseDevPod tells the connector service that the run is over. A pod that is already gone
// needs no release.
func (a *Activities) ReleaseDevPod(ctx context.Context, req ReleaseDevPodRequest) error {
	c, err := a.connectors()
	if err != nil {
		return err
	}
	var refused *ConnectorError
	if err = c.Release(ctx, req.Pod, req.Run); errors.As(err, &refused) && refused.Status == http.StatusNotFound {
		return nil
	}
	return connectorFailure(err)
}

// PublishRequestsRequest names a dev pod and the run whose publish requests are asked for.
type PublishRequestsRequest struct {
	Pod string `json:"pod"`
	Run string `json:"run"`
}

// PublishRequests returns the publish requests of the run that wait for a person, in the
// order they were made.
func (a *Activities) PublishRequests(ctx context.Context, req PublishRequestsRequest) ([]PublishRequest, error) {
	c, err := a.connectors()
	if err != nil {
		return nil, err
	}
	all, err := c.PublishRequests(ctx, req.Pod, req.Run)
	if err != nil {
		return nil, connectorFailure(fmt.Errorf("reading the publish requests of dev pod %q: %w", req.Pod, err))
	}
	waiting := []PublishRequest{}
	for _, r := range all {
		if r.State == PublishPending {
			waiting = append(waiting, r)
		}
	}
	sort.SliceStable(waiting, func(i, j int) bool { return waiting[i].N < waiting[j].N })
	return waiting, nil
}

// PublishInfo describes the publish request a decision is about. Step is the step whose
// agent asked for it.
type PublishInfo struct {
	Step       string `json:"step"`
	Pod        string `json:"pod"`
	N          int    `json:"n"`
	Kind       string `json:"kind"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
}

// PublishQuestion is the question a person is asked about a publish request.
func PublishQuestion(r PublishRequest, pod DevPod) string {
	if r.Kind == PublishPullRequest {
		return fmt.Sprintf("Publish branch %s of %s and open the pull request '%s'?", pod.WorkBranch, pod.Repository, r.Title)
	}
	return fmt.Sprintf("Publish branch %s of %s?", pod.WorkBranch, pod.Repository)
}

// DecidePublish stores the decision about a publish request as a decision record and sends
// it to the connector service, which publishes on approval.
func (a *Activities) DecidePublish(ctx context.Context, rec *DecisionRecord) (StepResult, error) {
	c, err := a.connectors()
	if err != nil {
		return StepResult{}, err
	}
	if rec.Publish == nil {
		return StepResult{}, temporal.NewNonRetryableApplicationError("the decision names no publish request", "InvalidDecision", nil)
	}
	out, err := a.RecordDecision(ctx, rec)
	if err != nil {
		return StepResult{}, err
	}
	decision := PublishDecision{Decision: rec.Decision, Comment: rec.Comment, DecidedBy: rec.DecidedBy}
	if err = c.DecidePublish(ctx, rec.Publish.Pod, rec.Publish.N, decision); err != nil {
		return StepResult{}, connectorFailure(fmt.Errorf("sending the decision about publish request %d of dev pod %q: %w", rec.Publish.N, rec.Publish.Pod, err))
	}
	return out, nil
}

// RoundName names the kept artifacts of the n-th run of a step that ran again after a rejection.
func RoundName(step string, round int) string {
	return fmt.Sprintf("%s.round-%d", step, round)
}

// evidenceBase is the prefix under which the agent service keeps the prompt evidence of the
// step whose artifact is base: runs/<run>/_evidence/<rest>.
func evidenceBase(base string) string {
	parts := strings.Split(base, "/")
	if len(parts) < 3 || parts[0] != "runs" {
		return ""
	}
	return strings.Join([]string{parts[0], parts[1], "_evidence", strings.Join(parts[2:], "/")}, "/")
}

// roundID maps an object of the step whose artifact is base to the name under which the
// round keeps it: the artifact itself, its records beside it (<base>.run.json and the like),
// the objects of a nested run under it, and its prompt evidence. ok is false for an object
// that does not belong to the step, or that an earlier round already keeps.
func roundID(id, base string, round int) (string, bool) {
	suffix := fmt.Sprintf(".round-%d", round)
	for _, root := range []string{base, evidenceBase(base)} {
		if root == "" {
			continue
		}
		rest, found := strings.CutPrefix(id, root)
		if !found {
			continue
		}
		own := rest == "" || strings.HasPrefix(rest, "/") || (root == base && strings.HasPrefix(rest, "."))
		if !own || strings.HasPrefix(rest, ".round-") {
			continue
		}
		return root + suffix + rest, true
	}
	return "", false
}

// KeptStep names a step that runs again: the artifact name of its run, the round that
// produced what is stored now, and the artifacts it delivered, which stay in place until
// the next run replaces them.
type KeptStep struct {
	Base  string   `json:"base"`
	Keep  []string `json:"keep,omitempty"`
	Round int      `json:"round"`
}

// KeepRoundsRequest lists the steps of a run whose results are kept before they run again.
type KeepRoundsRequest struct {
	TraceID string     `json:"trace_id"`
	Steps   []KeptStep `json:"steps"`
}

// KeepRounds copies what every listed step stored to <step>.round-<n>: its output, its run
// and attempt records, its prompt evidence, and the objects of a nested run. The records are
// then removed from the plain name, so the next run starts clean; the delivered artifacts
// stay until the next run replaces them.
func (a *Activities) KeepRounds(ctx context.Context, req KeepRoundsRequest) error {
	var ids []string
	if lister, ok := a.Store.(interface {
		TraceObjects(ctx context.Context, traceID string) ([]string, error)
	}); ok {
		var err error
		if ids, err = lister.TraceObjects(ctx, req.TraceID); err != nil {
			return fmt.Errorf("listing the objects of run %q: %w", req.TraceID, err)
		}
	}
	remover, canDelete := a.Store.(interface {
		Delete(ctx context.Context, id string) error
	})
	creators, _ := a.Store.(interface {
		Creator(ctx context.Context, id string) (string, error)
	})
	for _, step := range req.Steps {
		// A store that cannot list still keeps the output and the run record.
		candidates := append([]string{step.Base, step.Base + ".run.json"}, step.Keep...)
		candidates = append(candidates, ids...)
		sort.Strings(candidates)
		seen := map[string]bool{}
		for _, id := range candidates {
			target, ok := roundID(id, step.Base, step.Round)
			if !ok || seen[id] {
				continue
			}
			seen[id] = true
			data, err := a.Store.Get(ctx, id)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("reading %q: %w", id, err)
			}
			meta := Meta{Creator: sessionCreator, TraceID: req.TraceID}
			if creators != nil {
				if creator, creatorErr := creators.Creator(ctx, id); creatorErr == nil && creator != "" {
					meta.Creator = creator
				}
			}
			if err = a.Store.Put(ctx, target, data, meta); err != nil {
				return fmt.Errorf("writing %q: %w", target, err)
			}
			if canDelete && !contains(step.Keep, id) {
				if err = remover.Delete(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
					return fmt.Errorf("removing %q: %w", id, err)
				}
			}
		}
	}
	return nil
}
