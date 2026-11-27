package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

import "opensoftwarefactory/orchestrator/pipeline"

// metadataSchemaVersion is the schema_version of metadata.json.
const metadataSchemaVersion = 1

// WorkflowVersion names one version of a workflow.
type WorkflowVersion struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// AgentSettings are the settings of an agent that shape its output, as the agent registry
// held them when the run started. Temperature is left out when the agent sets none.
type AgentSettings struct {
	Model                     string            `json:"model"`
	Effort                    string            `json:"effort"`
	Temperature               *float64          `json:"temperature,omitempty"`
	Skills                    map[string]string `json:"skills,omitempty"`
	Tools                     []string          `json:"tools"`
	MaxTurns                  int               `json:"max_turns"`
	SkillScriptTimeoutSeconds int               `json:"skill_script_timeout_seconds,omitempty"`
}

// agentError stands in for the settings of an agent that could not be read.
type agentError struct {
	Error string `json:"error"`
}

// RunMetadata is stored as runs/<run id>/metadata.json when a run starts. The pipeline adds
// finished, status, and error when the run ends.
type RunMetadata struct {
	RunID              string          `json:"run_id"`
	ExecutionID        string          `json:"execution_id,omitempty"`
	ParentExecutionID  string          `json:"parent_execution_id,omitempty"`
	DistributedTraceID string          `json:"distributed_trace_id,omitempty"`
	Workflow           WorkflowVersion `json:"workflow"`
	Started            string          `json:"started"`
	StartedBy          string          `json:"started_by"`
	InputArtifact      string          `json:"input_artifact"`
	// Agents maps an agent reference to its AgentSettings, or to an agentError.
	Agents map[string]any `json:"agents"`
	// DevPod is the run's dev pod. The pipeline adds it when a devpod step created the pod.
	DevPod        *pipeline.DevPod  `json:"dev_pod,omitempty"`
	Workflows     []WorkflowVersion `json:"workflows"`
	SchemaVersion int               `json:"schema_version"`
}

// runMetadata collects what produces a run: the workflow version, the nested workflow
// versions it reaches through pinned references, and the settings of every agent they name.
// The workflow a route step starts is chosen during the run and is not listed.
func (s *server) runMetadata(ctx context.Context, c Config, def *pipeline.Definition, record *RunRecord) RunMetadata {
	meta := RunMetadata{
		SchemaVersion: metadataSchemaVersion, RunID: record.TraceID, ExecutionID: record.ExecutionID, ParentExecutionID: record.ParentExecutionID, DistributedTraceID: record.DistributedTraceID,
		Workflow:  WorkflowVersion{Name: def.Name, Version: pipeline.DefinitionVersion(def)},
		Workflows: []WorkflowVersion{}, Started: record.Started.Format(time.RFC3339), StartedBy: record.StartedBy,
		InputArtifact: record.InputArtifact, Agents: map[string]any{},
	}
	// A registry that does not answer must not hold up the start of the run for long.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nested := pipeline.NestedDefinitions(c.Workflows, def)
	for i := range nested {
		meta.Workflows = append(meta.Workflows, WorkflowVersion{Name: nested[i].Name, Version: pipeline.DefinitionVersion(&nested[i])})
	}
	for _, ref := range pipeline.AgentRefs(append([]pipeline.Definition{*def}, nested...)...) {
		if s.agents == nil {
			meta.Agents[ref] = agentError{Error: "the agent registry is not configured"}
			continue
		}
		settings, err := s.agents(ctx, ref)
		if err != nil {
			meta.Agents[ref] = agentError{Error: err.Error()}
			continue
		}
		meta.Agents[ref] = settings
	}
	return meta
}

// agentDefinition is the part of an agent's agent.json the orchestrator reads.
type agentDefinition struct {
	Description string   `json:"description"`
	Models      []string `json:"models"`
	Effort      string   `json:"effort"`
	Temperature *float64 `json:"temperature"`
	Tools       []string `json:"tools"`
	Skills      []string `json:"skills"`
	MaxTurns    int      `json:"max_turns"`
	// MaxRetries is the name the limit had before max_turns: the model calls after the first one.
	MaxRetries                int `json:"max_retries"`
	SkillScriptTimeoutSeconds int `json:"skill_script_timeout_seconds"`
}

// agentRegistry reads pinned agents from the agent registry.
type agentRegistry struct {
	client    *http.Client
	base      string
	skillBase string
}

// newAgentRegistry uses the registry at AGENT_REGISTRY_URL.
func newAgentRegistry(client *http.Client) agentRegistry {
	base := os.Getenv("AGENT_REGISTRY_URL")
	if base == "" {
		base = "http://agent-registry:8080"
	}
	skillBase := os.Getenv("SKILL_REGISTRY_URL")
	if skillBase == "" {
		skillBase = "http://skill-registry:8080"
	}
	return agentRegistry{base: strings.TrimRight(base, "/"), skillBase: strings.TrimRight(skillBase, "/"), client: client}
}

// definition reads the agent.json of a pinned agent such as demo/coder@v1.
func (r agentRegistry) definition(ctx context.Context, ref string) (def agentDefinition, err error) {
	name, version, ok := strings.Cut(ref, "@")
	if !ok {
		return agentDefinition{}, fmt.Errorf("agent %q is not pinned to a version", ref)
	}
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	baseURL, err := url.Parse(r.base)
	if err != nil {
		return agentDefinition{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL.String(), http.NoBody)
	if err != nil {
		return agentDefinition{}, err
	}
	req.URL.Path = strings.TrimRight(baseURL.Path, "/") + "/agents/" + strings.Join(segments, "/") + "/" + url.PathEscape(version)
	resp, err := r.client.Do(req)
	if err != nil {
		return agentDefinition{}, errors.New("the agent registry is not reachable")
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode == http.StatusNotFound {
		return agentDefinition{}, errors.New("the agent registry does not know this agent")
	}
	if resp.StatusCode/100 != 2 {
		return agentDefinition{}, fmt.Errorf("the agent registry answered %s", resp.Status)
	}
	var entry struct {
		Files struct {
			Definition agentDefinition `json:"agent.json"`
		} `json:"files"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		return agentDefinition{}, errors.New("the agent registry returned an agent.json that cannot be read")
	}
	return entry.Files.Definition, nil
}

// settings returns the settings of a pinned agent for the run metadata.
func (r agentRegistry) settings(ctx context.Context, ref string) (AgentSettings, error) {
	def, err := r.definition(ctx, ref)
	if err != nil {
		return AgentSettings{}, err
	}
	// The turns the agent service gives the agent: max_turns, or for a version stored before
	// it, one more than max_retries.
	turns := def.MaxTurns
	if turns <= 0 {
		turns = 1 + max(def.MaxRetries, 0)
	}
	out := AgentSettings{
		Effort: def.Effort, Temperature: def.Temperature, Tools: def.Tools, MaxTurns: turns,
		SkillScriptTimeoutSeconds: def.SkillScriptTimeoutSeconds,
	}
	if len(def.Models) > 0 {
		out.Model = def.Models[0]
	}
	if out.Tools == nil {
		out.Tools = []string{}
	}
	if len(def.Skills) > 0 {
		out.Skills = make(map[string]string, len(def.Skills))
	}
	for _, ref := range def.Skills {
		name, version, ok := strings.Cut(ref, "@")
		if !ok || name == "" || version == "" {
			return AgentSettings{}, fmt.Errorf("skill %q is not pinned to a version", ref)
		}
		if err := r.skillExists(ctx, name, version); err != nil {
			return AgentSettings{}, err
		}
		out.Skills[name] = version
	}
	return out, nil
}

// unknownSkillVersionError marks a pinned skill version that cannot be loaded. A workflow must
// stop instead of retrying because retrying cannot repair an immutable missing version.
type unknownSkillVersionError struct{ ref string }

func (e unknownSkillVersionError) Error() string {
	return fmt.Sprintf("the skill registry does not know %q", e.ref)
}

func (r agentRegistry) skillExists(ctx context.Context, name, version string) (err error) {
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	u := r.skillBase + "/skills/" + strings.Join(segments, "/") + "/" + url.PathEscape(version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return errors.New("the skill registry is not reachable")
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode == http.StatusNotFound {
		return unknownSkillVersionError{ref: name + "@" + version}
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the skill registry answered %s", resp.Status)
	}
	return nil
}

// storedStatus answers a run's status from runs/<run id>/metadata.json, for a finished run
// whose history is no longer kept. It reports false when the metadata has no outcome.
func (s *server) storedStatus(ctx context.Context, trace string) (RunStatus, bool) {
	data, err := s.blobs.Get(ctx, pipeline.ArtifactID(pipeline.RunPrefix(trace), "metadata.json"))
	if err != nil {
		return RunStatus{}, false
	}
	var meta struct {
		Started  time.Time       `json:"started"`
		Finished time.Time       `json:"finished"`
		Status   string          `json:"status"`
		Error    string          `json:"error"`
		Workflow WorkflowVersion `json:"workflow"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return RunStatus{}, false
	}
	// The names are those the run history gives a finished run.
	status, ok := map[string]string{"completed": "Completed", "failed": "Failed", "cancelled": "Canceled", "blocked": "Blocked"}[meta.Status]
	if !ok {
		return RunStatus{}, false
	}
	st := RunStatus{TraceID: trace, Workflow: meta.Workflow.Name, Started: meta.Started, Finished: meta.Finished, Status: status, Error: meta.Error}
	if !meta.Started.IsZero() && !meta.Finished.IsZero() {
		st.DurationMS, st.DurationKnown = meta.Finished.Sub(meta.Started).Milliseconds(), true
	}
	return st, true
}
