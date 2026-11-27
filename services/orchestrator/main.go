// Command orchestrator serves the workflow API and runs the Temporal worker
// that executes configured workflows, holds gate steps for human decisions,
// and routes work to other workflows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	temporalotel "go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

import "opensoftwarefactory/orchestrator/pipeline"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	seedDir := flag.String("seed", "", "directory of workflow configurations to add when their names are missing")
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	blobs := pipeline.NewHTTPStoreFromEnv()
	blobs.Client = &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	workflows, err := openWorkflowStoreWithRetry(context.Background(), blobs, logger, time.Minute)
	if err != nil {
		return fmt.Errorf("loading workflows: %w", err)
	}
	seeded, err := seedWorkflows(context.Background(), workflows, *seedDir)
	if err != nil {
		return fmt.Errorf("seeding workflows: %w", err)
	}
	if seeded > 0 {
		logger.Info("seeded workflows", "versions", seeded)
	}
	shutdownTelemetry, err := initTelemetry(context.Background(), "orchestrator")
	if err != nil {
		return fmt.Errorf("initializing telemetry: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := shutdownTelemetry(ctx); shutdownErr != nil {
			logger.Warn("telemetry shutdown failed", "error", shutdownErr)
		}
	}()
	tracing, err := temporalotel.NewTracingInterceptor(temporalotel.TracerOptions{})
	if err != nil {
		return fmt.Errorf("creating tracing interceptor: %w", err)
	}
	c, err := pipeline.Dial(tracing)
	if err != nil {
		return fmt.Errorf("connecting to Temporal: %w", err)
	}
	defer c.Close()
	nc, js, err := connectNATS()
	if err != nil {
		return fmt.Errorf("connecting to JetStream: %w", err)
	}
	defer nc.Close()
	w := worker.New(c, pipeline.TaskQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(pipeline.Pipeline, workflow.RegisterOptions{Name: pipeline.WorkflowName})
	w.RegisterWorkflowWithOptions(pipeline.Team, workflow.RegisterOptions{Name: pipeline.TeamWorkflowName})
	w.RegisterWorkflowWithOptions(pipeline.Recalculation, workflow.RegisterOptions{Name: pipeline.RecalculationWorkflowName})
	agents := newAgentRegistry(blobs.Client)
	sqldbURL := strings.TrimRight(envString("SQLDB_URL", ""), "/")
	w.RegisterActivity(&pipeline.Activities{Store: blobs, Conditional: blobs, EvaluationURL: envString("EVALUATION_URL", "http://evaluation:8080"), SQLDBURL: sqldbURL, HTTPClient: blobs.Client, LoadDefinitions: func() ([]pipeline.Definition, error) {
		cfg, snapshotErr := workflows.snapshot()
		return cfg.Workflows, snapshotErr
	}, DescribeAgent: agentDescriber(agents, logger), LoadAgentSettings: func(ctx context.Context, ref string) (pipeline.AgentExecutionSettings, error) {
		settings, settingsErr := agents.settings(ctx, ref)
		if settingsErr != nil {
			var missing unknownSkillVersionError
			if errors.As(settingsErr, &missing) {
				return pipeline.AgentExecutionSettings{}, temporal.NewNonRetryableApplicationError(settingsErr.Error(), "MissingSkillVersion", settingsErr)
			}
			return pipeline.AgentExecutionSettings{}, settingsErr
		}
		return pipeline.AgentExecutionSettings{Skills: settings.Skills, SkillScriptTimeoutSeconds: settings.SkillScriptTimeoutSeconds}, nil
	}, Connectors: pipeline.NewHTTPConnectorsFromEnv(blobs.Client)})
	err = w.Start()
	if err != nil {
		return err
	}
	defer w.Stop()

	s := &server{
		workflows: workflows, blobs: blobs, runner: temporalRunner{c}, agents: agents.settings, logger: logger,
		agentURL: strings.TrimRight(envString("AGENT_URL", "http://agent:8080"), "/"), sqldbURL: sqldbURL, httpClient: blobs.Client,
	}
	httpServer := &http.Server{Addr: *addr, Handler: otelhttp.NewHandler(s.routes(), "orchestrator.http"), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	events := &eventService{temporal: c, store: blobs, js: js, interval: 5 * time.Second, log: logger, http: blobs.Client, evaluationURL: envString("EVALUATION_URL", "http://evaluation:8080")}
	s.events = events
	go events.run(ctx)
	result := make(chan error, 1)
	go func() {
		logger.Info("orchestrator listening", "address", httpServer.Addr)
		result <- httpServer.ListenAndServe()
	}()
	select {
	case err = <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(drain)
	}
	return nil
}

type temporalRunner struct{ c client.Client }

// Start uses the trace ID as the Temporal workflow ID, so each run is
// addressable by its trace ID in the API and the Temporal UI.
func (r temporalRunner) Start(ctx context.Context, in *pipeline.Input) error {
	_, err := r.StartExecution(ctx, in)
	return err
}

type executionStarter interface {
	StartExecution(context.Context, *pipeline.Input) (string, error)
}

func (r temporalRunner) StartExecution(ctx context.Context, in *pipeline.Input) (string, error) {
	run, err := r.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        in.TraceID,
		TaskQueue: pipeline.TaskQueue,
		// A run ID is new for every run: one that Temporal already knows is refused, so a
		// start never continues or repeats an earlier run.
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		Memo:                                     map[string]any{"workflow": in.Definition.Name, "workflow_version": pipeline.DefinitionVersion(&in.Definition), "trace_id": in.TraceID, "artifact_prefix": in.Prefix},
	}, pipeline.WorkflowName, in)
	if err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			return "", fmt.Errorf("run id %s is already in use: %w", in.TraceID, err)
		}
		// A deadline or transport error can be ambiguous: Temporal may have accepted this
		// start even though this client missed the response.
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if desc, describeErr := r.c.DescribeWorkflowExecution(checkCtx, in.TraceID, ""); describeErr == nil && desc.GetWorkflowExecutionInfo().GetExecution().GetRunId() != "" {
			return desc.GetWorkflowExecutionInfo().GetExecution().GetRunId(), nil
		}
		return "", err
	}
	return run.GetRunID(), nil
}

func (r temporalRunner) Status(ctx context.Context, traceID string) (RunStatus, error) {
	d, err := r.c.DescribeWorkflowExecution(ctx, traceID, "")
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return RunStatus{}, errRunNotFound
	}
	if err != nil {
		return RunStatus{}, err
	}
	info := d.GetWorkflowExecutionInfo()
	st := RunStatus{TraceID: traceID, Status: info.GetStatus().String(), Started: info.GetStartTime().AsTime()}
	if p := info.GetMemo().GetFields()["workflow"]; p != nil {
		_ = converter.GetDefaultDataConverter().FromPayload(p, &st.Workflow)
	}
	// The state of the merge and batch steps is also answered for a run that has ended.
	if value, queryErr := r.c.QueryWorkflow(ctx, traceID, "", pipeline.NodesQuery); queryErr == nil {
		var nodes map[string]json.RawMessage
		if value.Get(&nodes) == nil && len(nodes) > 0 {
			st.Nodes, _ = json.Marshal(nodes)
		}
	}
	// How often the steps that ran again after a rejection have run.
	if value, queryErr := r.c.QueryWorkflow(ctx, traceID, "", pipeline.RoundsQuery); queryErr == nil {
		var rounds map[string]int
		if value.Get(&rounds) == nil && len(rounds) > 0 {
			st.Rounds = rounds
		}
	}
	if st.Status == "Running" {
		st.DurationMS = max(0, time.Since(st.Started).Milliseconds())
		st.DurationKnown = true
		return st, nil
	}
	if closeTime := info.GetCloseTime(); closeTime != nil {
		st.Finished = closeTime.AsTime()
		st.DurationMS = max(0, st.Finished.Sub(st.Started).Milliseconds())
		st.DurationKnown = true
	}
	var res pipeline.Result
	if err = r.c.GetWorkflow(ctx, traceID, "").Get(ctx, &res); err != nil {
		st.Error = err.Error()
	}
	st.Artifacts = res.Artifacts
	if len(res.Rounds) > 0 {
		st.Rounds = res.Rounds
	}
	// A run that an agent ended with report_blocked completed without an error; it is shown
	// as blocked, with the reason.
	if res.Blocked != "" && st.Error == "" {
		st.Status, st.Error = "Blocked", res.Blocked
	}
	return st, nil
}

// Pending queries every running pipeline workflow, including routed child
// workflows, for the gate steps that wait for a decision.
func (r temporalRunner) Pending(ctx context.Context) ([]PendingDecision, error) {
	var out []PendingDecision
	var token []byte
	for {
		page, err := r.c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Query:         fmt.Sprintf("WorkflowType = '%s' AND ExecutionStatus = 'Running'", pipeline.WorkflowName),
			NextPageToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, exec := range page.GetExecutions() {
			id := exec.GetExecution().GetWorkflowId()
			value, err := r.c.QueryWorkflow(ctx, id, "", pipeline.PendingQuery)
			if err != nil {
				continue // the workflow finished between the list and the query
			}
			var state pipeline.PendingState
			err = value.Get(&state)
			if err != nil {
				return nil, err
			}
			for _, g := range state.Gates {
				out = append(out, PendingDecision{WorkflowID: id, TraceID: state.TraceID, Workflow: state.Workflow, PendingGate: g})
			}
		}
		token = page.GetNextPageToken()
		if len(token) == 0 {
			return out, nil
		}
	}
}

// Remove deletes a run's history; Temporal terminates a running run first.
func (r temporalRunner) Remove(ctx context.Context, traceID string) error {
	namespace := os.Getenv("TEMPORAL_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}
	_, err := r.c.WorkflowService().DeleteWorkflowExecution(ctx, &workflowservice.DeleteWorkflowExecutionRequest{
		Namespace:         namespace,
		WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: traceID},
	})
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return errRunNotFound
	}
	return err
}

// Sessions queries every running pipeline workflow, including routed child
// workflows, for the interactive steps whose session is open.
func (r temporalRunner) Sessions(ctx context.Context) ([]WaitingSession, error) {
	var out []WaitingSession
	var token []byte
	for {
		page, err := r.c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Query:         fmt.Sprintf("WorkflowType = '%s' AND ExecutionStatus = 'Running'", pipeline.WorkflowName),
			NextPageToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, exec := range page.GetExecutions() {
			id := exec.GetExecution().GetWorkflowId()
			value, err := r.c.QueryWorkflow(ctx, id, "", pipeline.PendingQuery)
			if err != nil {
				continue // the workflow finished between the list and the query
			}
			var state pipeline.PendingState
			err = value.Get(&state)
			if err != nil {
				return nil, err
			}
			for i := range state.Sessions {
				out = append(out, WaitingSession{WorkflowID: id, TraceID: state.TraceID, Workflow: state.Workflow, PendingSession: state.Sessions[i]})
			}
		}
		token = page.GetNextPageToken()
		if len(token) == 0 {
			return out, nil
		}
	}
}

// Teams queries every running pipeline workflow, including nested child workflows, for the
// team steps that are running.
func (r temporalRunner) Teams(ctx context.Context) ([]RunningTeam, error) {
	var out []RunningTeam
	var token []byte
	for {
		page, err := r.c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Query:         fmt.Sprintf("WorkflowType = '%s' AND ExecutionStatus = 'Running'", pipeline.WorkflowName),
			NextPageToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, exec := range page.GetExecutions() {
			id := exec.GetExecution().GetWorkflowId()
			value, err := r.c.QueryWorkflow(ctx, id, "", pipeline.PendingQuery)
			if err != nil {
				continue // the workflow finished between the list and the query
			}
			var state pipeline.PendingState
			err = value.Get(&state)
			if err != nil {
				return nil, err
			}
			for _, team := range state.Teams {
				out = append(out, RunningTeam{WorkflowID: id, TraceID: state.TraceID, Workflow: state.Workflow, PendingTeam: team})
			}
		}
		token = page.GetNextPageToken()
		if len(token) == 0 {
			return out, nil
		}
	}
}

// Complete signals the workflow that holds the interactive step.
func (r temporalRunner) Complete(ctx context.Context, workflowID string, c pipeline.Completion) error {
	err := r.c.SignalWorkflow(ctx, workflowID, "", pipeline.CompleteSignal, c)
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return errRunNotFound
	}
	return err
}

func (r temporalRunner) FailSession(ctx context.Context, workflowID string, failure pipeline.SessionFailure) error {
	err := r.c.SignalWorkflow(ctx, workflowID, "", pipeline.SessionFailureSignal, failure)
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return errRunNotFound
	}
	return err
}

// Decide signals the workflow that holds the gate step.
func (r temporalRunner) Decide(ctx context.Context, workflowID string, d pipeline.Decision) error {
	err := r.c.SignalWorkflow(ctx, workflowID, "", pipeline.DecisionSignal, d)
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return errRunNotFound
	}
	return err
}

// agentDescriber returns the lookup of an agent's description in the agent registry. The
// description only informs the other members of a team, so a failed lookup yields ""
// instead of failing the team.
func agentDescriber(agents agentRegistry, logger *slog.Logger) func(context.Context, string) string {
	return func(ctx context.Context, ref string) string {
		def, err := agents.definition(ctx, ref)
		if err != nil {
			logger.Warn("agent description lookup failed", "agent", ref, "error", err)
			return ""
		}
		return def.Description
	}
}
