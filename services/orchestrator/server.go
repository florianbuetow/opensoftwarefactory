package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"opensoftwarefactory/orchestrator/pipeline"
)

const maxBodyBytes = 64 << 20

// RunStatus describes one pipeline run. Nodes is the state of its merge and batch steps: a
// pipeline.MergeState or pipeline.BatchState per step ID. It is left out for a run without
// such steps. Status is "Blocked", with the reason in Error, for a run an agent ended with
// report_blocked. DevPod is the run's dev pod and PullRequest the object ID of the pull
// request record the connector service stored for the run, when they exist. Rounds counts
// the runs of every step that ran again after a rejection; the results of the earlier runs
// are stored as <step>.round-<n>.
type RunStatus struct {
	TraceID       string            `json:"trace_id"`
	Workflow      string            `json:"workflow,omitempty"`
	Started       time.Time         `json:"started,omitempty"`
	Finished      time.Time         `json:"finished,omitempty"`
	DurationMS    int64             `json:"duration_ms,omitempty"`
	DurationKnown bool              `json:"duration_known"`
	Status        string            `json:"status"`
	Artifacts     map[string]string `json:"artifacts,omitempty"`
	Nodes         json.RawMessage   `json:"nodes,omitempty"`
	Error         string            `json:"error,omitempty"`
	DevPod        *pipeline.DevPod  `json:"dev_pod,omitempty"`
	PullRequest   string            `json:"pull_request,omitempty"`
	Rounds        map[string]int    `json:"rounds,omitempty"`
}

// RunRecord is stored as runs/<trace>/run.json when a run starts.
type RunRecord struct {
	TraceID            string    `json:"trace_id"`
	DistributedTraceID string    `json:"distributed_trace_id,omitempty"`
	Workflow           string    `json:"workflow"`
	WorkflowVersion    int       `json:"workflow_version"`
	Started            time.Time `json:"started"`
	StartedBy          string    `json:"started_by"`
	InputArtifact      string    `json:"input_artifact"`
	Source             string    `json:"source,omitempty"`
}

// PendingDecision is a gate step of a running workflow that waits for a person.
type PendingDecision struct {
	WorkflowID string `json:"workflow_id"`
	TraceID    string `json:"trace_id"`
	Workflow   string `json:"workflow"`
	pipeline.PendingGate
}

// DecisionRequest is what the UI posts to decide a pending gate.
type DecisionRequest struct {
	WorkflowID string `json:"workflow_id"`
	pipeline.Decision
}

// WaitingSession is an interactive step of a running workflow whose chat session is open.
type WaitingSession struct {
	WorkflowID string `json:"workflow_id"`
	TraceID    string `json:"trace_id"`
	Workflow   string `json:"workflow"`
	pipeline.PendingSession
}

// CompleteRequest is what the UI posts to complete a session with one of its files.
type CompleteRequest struct {
	WorkflowID string `json:"workflow_id"`
	pipeline.Completion
}

// RunningTeam is a team step of a running workflow.
type RunningTeam struct {
	WorkflowID string `json:"workflow_id"`
	TraceID    string `json:"trace_id"`
	Workflow   string `json:"workflow"`
	pipeline.PendingTeam
}

// errRunNotFound reports an unknown trace ID.
var errRunNotFound = errors.New("run not found")

// Runner starts, inspects, decides, completes, and removes pipeline runs.
type Runner interface {
	Start(ctx context.Context, in pipeline.Input) error
	Status(ctx context.Context, traceID string) (RunStatus, error)
	Pending(ctx context.Context) ([]PendingDecision, error)
	Decide(ctx context.Context, workflowID string, d pipeline.Decision) error
	Sessions(ctx context.Context) ([]WaitingSession, error)
	Complete(ctx context.Context, workflowID string, c pipeline.Completion) error
	Teams(ctx context.Context) ([]RunningTeam, error)
	Remove(ctx context.Context, traceID string) error
}

// runObjects is the blobstore access that removing a run needs.
type runObjects interface {
	TraceObjects(ctx context.Context, traceID string) ([]string, error)
	Delete(ctx context.Context, id string) error
}

type server struct {
	configPath string
	blobs      pipeline.Store
	runner     Runner
	// agents reads the settings of a pinned agent for the run metadata. It may be nil.
	agents func(ctx context.Context, ref string) (AgentSettings, error)
	logger *slog.Logger
	mu     sync.Mutex
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/workflows", s.getWorkflows)
	mux.HandleFunc("GET /api/workflows/{name}/versions/{version}", s.getWorkflowVersion)
	mux.HandleFunc("PATCH /api/workflows/{name}/versions/{version}/description", s.patchWorkflowDescription)
	mux.HandleFunc("POST /api/workflows/{name}/versions", s.postWorkflowVersion)
	mux.HandleFunc("POST /api/workflows/{name}/rename", s.renameWorkflow)
	mux.HandleFunc("DELETE /api/workflows/{name}", s.deleteWorkflow)
	mux.HandleFunc("POST /api/workflows/{name}/runs", s.startRun)
	mux.HandleFunc("GET /api/runs/{trace}", s.getRun)
	mux.HandleFunc("DELETE /api/runs/{trace}", s.deleteRun)
	mux.HandleFunc("GET /api/artifacts/{id...}", s.getArtifact)
	mux.HandleFunc("GET /api/decisions", s.getDecisions)
	mux.HandleFunc("POST /api/decisions", s.postDecision)
	mux.HandleFunc("GET /api/sessions", s.getSessions)
	mux.HandleFunc("POST /api/sessions/complete", s.postSessionComplete)
	mux.HandleFunc("GET /api/teams", s.getTeams)
	return mux
}

func (s *server) config() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return loadConfig(s.configPath)
}

// startRun stores the request body as the run input, assigns a new trace ID,
// and starts the configured workflow under that ID. The version parameter
// selects a stored version; without it the newest version runs.
func (s *server) startRun(w http.ResponseWriter, r *http.Request) {
	c, err := s.config()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	version := 0
	if v := r.URL.Query().Get("version"); v != "" {
		if version, err = strconv.Atoi(v); err != nil || version < 1 {
			s.fail(w, http.StatusBadRequest, errors.New("version must be a number such as 2"))
			return
		}
	}
	def, ok := c.find(r.PathValue("name"), version)
	if !ok {
		if _, known := c.find(r.PathValue("name"), 0); known {
			s.fail(w, http.StatusNotFound, errors.New("unknown workflow version"))
			return
		}
		s.fail(w, http.StatusNotFound, errors.New("unknown workflow"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	// An existing artifact, such as a predefined task under tasks/, can be
	// the input instead of the request body.
	if source := r.URL.Query().Get("input"); source != "" {
		if !pipeline.ValidID(source) {
			s.fail(w, http.StatusBadRequest, errors.New("input must be a valid object id"))
			return
		}
		if body, err = s.blobs.Get(r.Context(), source); errors.Is(err, pipeline.ErrNotFound) {
			s.fail(w, http.StatusNotFound, fmt.Errorf("input artifact %q not found", source))
			return
		} else if err != nil {
			s.fail(w, http.StatusBadGateway, err)
			return
		}
	}
	traceID := uuid.NewString()
	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(attribute.String("factory.run_id", traceID), attribute.String("factory.workflow", def.Name))
	distributedTraceID := ""
	if sc := span.SpanContext(); sc.IsValid() {
		distributedTraceID = sc.TraceID().String()
	}
	prefix := pipeline.RunPrefix(traceID)
	input := pipeline.ArtifactID(prefix, pipeline.InputSource)
	startedBy := strings.TrimSpace(r.Header.Get("X-Creator"))
	if startedBy == "" {
		startedBy = "orchestrator"
	}
	if err = s.blobs.Put(r.Context(), input, body, pipeline.Meta{Creator: startedBy, TraceID: traceID}); err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	// The run record lets the UI name runs by workflow and time instead of trace ID.
	run := RunRecord{
		TraceID: traceID, Workflow: def.Name, WorkflowVersion: def.Version, Started: time.Now().UTC(), StartedBy: startedBy,
		InputArtifact: input, Source: r.URL.Query().Get("input"), DistributedTraceID: distributedTraceID,
	}
	record, _ := json.MarshalIndent(run, "", "  ")
	if err = s.blobs.Put(r.Context(), pipeline.ArtifactID(prefix, "run.json"), record, pipeline.Meta{Creator: startedBy, TraceID: traceID}); err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	// The metadata records what produces the run, so the run can be evaluated later.
	metadata, _ := json.MarshalIndent(s.runMetadata(r.Context(), c, def, run), "", "  ")
	if err = s.blobs.Put(r.Context(), pipeline.ArtifactID(prefix, pipeline.MetadataFile), metadata, pipeline.Meta{Creator: startedBy, TraceID: traceID}); err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	runInputs := []pipeline.Item{{Source: pipeline.InputSource, Artifact: input, Round: 1}}
	if err = s.runner.Start(r.Context(), pipeline.Input{TraceID: traceID, Definition: def, Inputs: runInputs, Prefix: prefix}); err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	s.logger.Info("run started", "workflow", def.Name, "version", def.Version, "trace_id", traceID)
	writeJSON(w, http.StatusAccepted, map[string]any{"trace_id": traceID, "workflow": def.Name, "workflow_version": def.Version, "input_artifact": input})
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	trace := r.PathValue("trace")
	if uuid.Validate(trace) != nil {
		s.fail(w, http.StatusBadRequest, errors.New("trace id must be a UUID"))
		return
	}
	st, err := s.runner.Status(r.Context(), trace)
	if errors.Is(err, errRunNotFound) {
		// The run history is kept only for a while; a finished run is then answered from its metadata.
		if stored, ok := s.storedStatus(r.Context(), trace); ok {
			s.addPod(r.Context(), &stored)
			writeJSON(w, http.StatusOK, stored)
			return
		}
		s.fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	s.addPod(r.Context(), &st)
	writeJSON(w, http.StatusOK, st)
}

// addPod adds the run's dev pod, which the metadata records, and the object ID of its pull
// request record, when the connector service stored one.
func (s *server) addPod(ctx context.Context, st *RunStatus) {
	prefix := pipeline.RunPrefix(st.TraceID)
	if data, err := s.blobs.Get(ctx, pipeline.ArtifactID(prefix, pipeline.MetadataFile)); err == nil {
		var meta struct {
			DevPod *pipeline.DevPod `json:"dev_pod"`
		}
		if json.Unmarshal(data, &meta) == nil && meta.DevPod != nil && meta.DevPod.ID != "" {
			st.DevPod = meta.DevPod
		}
	}
	record := pipeline.ArtifactID(prefix, pipeline.PullRequestFile)
	if _, err := s.blobs.Get(ctx, record); err == nil {
		st.PullRequest = record
	}
}

// deleteRun removes a run: its history (a running run is terminated first) and every
// artifact under runs/<trace>/.
func (s *server) deleteRun(w http.ResponseWriter, r *http.Request) {
	trace := r.PathValue("trace")
	if uuid.Validate(trace) != nil {
		s.fail(w, http.StatusBadRequest, errors.New("trace id must be a UUID"))
		return
	}
	if err := s.runner.Remove(r.Context(), trace); err != nil && !errors.Is(err, errRunNotFound) {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	if objects, ok := s.blobs.(runObjects); ok {
		ids, err := objects.TraceObjects(r.Context(), trace)
		if err != nil {
			s.fail(w, http.StatusBadGateway, err)
			return
		}
		prefix := pipeline.RunPrefix(trace) + "/"
		for _, id := range ids {
			if !strings.HasPrefix(id, prefix) {
				continue
			}
			if err = objects.Delete(r.Context(), id); err != nil {
				s.fail(w, http.StatusBadGateway, err)
				return
			}
		}
	}
	s.logger.Info("run removed", "trace_id", trace)
	w.WriteHeader(http.StatusNoContent)
}

// getArtifact proxies a blobstore object so the UI can show run results.
func (s *server) getArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !pipeline.ValidID(id) {
		s.fail(w, http.StatusBadRequest, errors.New("invalid object id"))
		return
	}
	data, err := s.blobs.Get(r.Context(), id)
	if errors.Is(err, pipeline.ErrNotFound) {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// getDecisions lists the gate steps of running workflows that wait for a person.
func (s *server) getDecisions(w http.ResponseWriter, r *http.Request) {
	pending, err := s.runner.Pending(r.Context())
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	if pending == nil {
		pending = []PendingDecision{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": pending})
}

// postDecision delivers a person's decision to a waiting gate step.
func (s *server) postDecision(w http.ResponseWriter, r *http.Request) {
	var req DecisionRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	switch {
	case req.WorkflowID == "":
		s.fail(w, http.StatusBadRequest, errors.New("workflow_id is required"))
		return
	case req.Step == "":
		s.fail(w, http.StatusBadRequest, errors.New("step is required"))
		return
	case req.DecidedBy == "":
		s.fail(w, http.StatusBadRequest, errors.New("decided_by is required"))
		return
	case req.Decision.Decision != pipeline.Approved && req.Decision.Decision != pipeline.Rejected:
		s.fail(w, http.StatusBadRequest, errors.New("decision must be approved or rejected"))
		return
	}
	// A gate with on_reject sends the reason back to an earlier step, so a rejection needs one.
	if req.Decision.Decision == pipeline.Rejected && strings.TrimSpace(req.Comment) == "" {
		pending, err := s.runner.Pending(r.Context())
		if err != nil {
			s.fail(w, http.StatusBadGateway, err)
			return
		}
		for _, gate := range pending {
			if gate.WorkflowID == req.WorkflowID && gate.Step == req.Step && gate.OnReject != "" {
				s.fail(w, http.StatusBadRequest, errors.New("a rejection at this gate needs a comment with the reason; it goes back to the step that runs again"))
				return
			}
		}
	}
	if err := s.runner.Decide(r.Context(), req.WorkflowID, req.Decision); err != nil {
		if errors.Is(err, errRunNotFound) {
			s.fail(w, http.StatusNotFound, err)
			return
		}
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	s.logger.Info("decision recorded", "workflow_id", req.WorkflowID, "step", req.Step, "decision", req.Decision.Decision, "decided_by", req.DecidedBy)
	writeJSON(w, http.StatusAccepted, req)
}

// getSessions lists the interactive steps of running workflows whose session is open.
func (s *server) getSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.runner.Sessions(r.Context())
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	if sessions == nil {
		sessions = []WaitingSession{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// getTeams lists the team steps of running workflows.
func (s *server) getTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := s.runner.Teams(r.Context())
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	if teams == nil {
		teams = []RunningTeam{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": teams})
}

// postSessionComplete ends a waiting interactive step with one of its session files as output.
func (s *server) postSessionComplete(w http.ResponseWriter, r *http.Request) {
	var req CompleteRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	switch {
	case req.WorkflowID == "":
		s.fail(w, http.StatusBadRequest, errors.New("workflow_id is required"))
		return
	case req.Step == "":
		s.fail(w, http.StatusBadRequest, errors.New("step is required"))
		return
	case req.CompletedBy == "":
		s.fail(w, http.StatusBadRequest, errors.New("completed_by is required"))
		return
	case !pipeline.SessionFilePattern.MatchString(req.File):
		s.fail(w, http.StatusBadRequest, errors.New("file must be the name of a .md, .txt, or .json session file"))
		return
	}
	sessions, err := s.runner.Sessions(r.Context())
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	session := ""
	for _, open := range sessions {
		if open.WorkflowID == req.WorkflowID && open.Step == req.Step {
			session = open.Session
		}
	}
	if session == "" {
		s.fail(w, http.StatusNotFound, errors.New("no such waiting session"))
		return
	}
	if _, err = s.blobs.Get(r.Context(), session+"/files/"+req.File); errors.Is(err, pipeline.ErrNotFound) {
		s.fail(w, http.StatusNotFound, fmt.Errorf("session file %q not found", req.File))
		return
	} else if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	if err = s.runner.Complete(r.Context(), req.WorkflowID, req.Completion); err != nil {
		if errors.Is(err, errRunNotFound) {
			s.fail(w, http.StatusNotFound, err)
			return
		}
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	s.logger.Info("session completed", "workflow_id", req.WorkflowID, "step", req.Step, "file", req.File, "completed_by", req.CompletedBy)
	writeJSON(w, http.StatusAccepted, req)
}

func (s *server) fail(w http.ResponseWriter, status int, err error) {
	if status >= 500 {
		s.logger.Error("request failed", "status", status, "error", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
