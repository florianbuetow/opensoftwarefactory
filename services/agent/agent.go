package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// StepRequest is the activity input every service receives.
type StepRequest struct {
	TraceID           string      `json:"trace_id"`
	Workflow          string      `json:"workflow"`
	StepID            string      `json:"step_id"`
	Agent             string      `json:"agent,omitempty"`
	Inputs            []InputItem `json:"inputs"`
	OutputArtifact    string      `json:"output_artifact"`
	ProducerRunID     string      `json:"producer_run_id,omitempty"`
	TargetRunID       string      `json:"target_run_id,omitempty"`
	TargetExecutionID string      `json:"target_execution_id,omitempty"`
	ExecutionID       string      `json:"execution_id,omitempty"`
	ParentExecutionID string      `json:"parent_execution_id,omitempty"`
	// DevPod is the run's dev pod; empty when the run has none.
	DevPod string `json:"dev_pod,omitempty"`
}

// InputItem is one step input: the artifact that a source produced in one of its rounds.
// The source is a step ID, or "input" for the run input.
type InputItem struct {
	Source   string `json:"source"`
	Artifact string `json:"artifact"`
	Round    int    `json:"round"`
	// Kind marks the items a rejected gate sends back: "rejection" (the gate's decision
	// record), "previous" (the step's own earlier output) or "reviewed" (a document the person
	// was shown). A normal input has none.
	Kind string `json:"kind,omitempty"`
}

// StepResult is the activity output every service returns.
type StepResult struct {
	Artifact string `json:"artifact"`
	// Blocked is the reason the agent gave with report_blocked; empty when it finished its work.
	Blocked string `json:"blocked,omitempty"`
}

// Definition is the agent.json of a pinned agent version.
type Definition struct {
	SchemaVersion int      `json:"schema_version"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Models        []string `json:"models"`
	Effort        string   `json:"effort"`
	Tools         []string `json:"tools"`
	MaxRetries    int      `json:"max_retries"` // model calls after the first one, such as after a tool call; 0 by default
	// Temperature is an optional model setting. It is sent only when set.
	Temperature *float64 `json:"temperature,omitempty"`
	// UnpackInputs is required. With true the first user message holds the content of every
	// input; with false it lists only the references. It is nil when agent.json lacks it.
	UnpackInputs *bool `json:"unpack_inputs"`
	// Commands lists the command patterns the agent may run in a dev pod. The connector service
	// enforces them; here they are only named in the dev pod note.
	Commands       []string        `json:"commands,omitempty"`
	OutputContract *OutputContract `json:"output_contract,omitempty"`
}

// noThinking is the assistant turn that tells a thinking model its thinking is already over.
// The local models take no setting for this, so the request ends with this empty thinking block
// and the model continues with its answer.
const noThinking = "<think>\n\n</think>\n\n"

// maxAnswerTokens limits one model answer. A small local model sometimes does not stop; without
// a limit one such answer holds a step until its timeout.
const maxAnswerTokens = 6000

// chatRequest builds the model request of this agent for the given conversation. With the
// effort "none" the model is asked not to think first: no reasoning_effort is sent and the
// conversation ends with an empty thinking block.
func (d Definition) chatRequest(messages, tools []any) chatRequest {
	var responseFormat map[string]string
	if d.OutputContract != nil {
		responseFormat = map[string]string{"type": "json_object"}
	}
	if d.Effort == "none" {
		ended := append(append(make([]any, 0, len(messages)+1), messages...), map[string]string{"role": "assistant", "content": noThinking})
		return chatRequest{Model: d.Models[0], Messages: ended, Tools: tools, Temperature: d.Temperature, ResponseFormat: responseFormat, MaxTokens: maxAnswerTokens}
	}
	return chatRequest{Model: d.Models[0], Messages: messages, Tools: tools, ReasoningEffort: d.Effort, Temperature: d.Temperature, ResponseFormat: responseFormat, MaxTokens: maxAnswerTokens}
}

// RunRecord is stored next to the output artifact as <artifact>.run.json.
type RunRecord struct {
	Agent                 string            `json:"agent"`
	Model                 string            `json:"model"`
	Effort                string            `json:"effort,omitempty"`
	Temperature           *float64          `json:"temperature,omitempty"`
	Started               time.Time         `json:"started"`
	Finished              time.Time         `json:"finished"`
	ModelCalls            int               `json:"model_calls"`
	Messages              []any             `json:"messages"`
	Usage                 *usage            `json:"usage,omitempty"`
	EstimatedInputTokens  *int64            `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens *int64            `json:"estimated_output_tokens,omitempty"`
	ProcessingTimeMS      *int64            `json:"processing_time_ms,omitempty"`
	EstimationMethod      string            `json:"estimation_method,omitempty"`
	Status                string            `json:"status,omitempty"`
	Error                 string            `json:"error,omitempty"`
	Attempt               int32             `json:"attempt,omitempty"`
	Events                []RunEvent        `json:"events,omitempty"`
	TraceID               string            `json:"trace_id,omitempty"`
	SpanID                string            `json:"span_id,omitempty"`
	RunID                 string            `json:"run_id,omitempty"`
	ExecutionID           string            `json:"execution_id,omitempty"`
	ParentExecutionID     string            `json:"parent_execution_id,omitempty"`
	ActivityID            string            `json:"activity_id,omitempty"`
	ActivityAttempt       int32             `json:"activity_attempt,omitempty"`
	PromptEvidence        *PromptEvidence   `json:"prompt_evidence,omitempty"`
	OutputValidation      string            `json:"output_validation,omitempty"`
	ExtractionObligation  *extractionIntent `json:"extraction_obligation,omitempty"`
}

// PromptEvidence links the exact registry sources and immutable run snapshots used to build
// this attempt's effective system prompt.
type PromptEvidence struct {
	AgentSourceArtifact     string           `json:"agent_source_artifact"`
	SourcePromptArtifact    string           `json:"source_prompt_artifact"`
	EffectivePromptArtifact string           `json:"effective_prompt_artifact,omitempty"`
	Policies                []PolicyEvidence `json:"policies,omitempty"`
}

type PolicyEvidence struct {
	ID               string `json:"id"`
	Version          string `json:"version"`
	SourceArtifact   string `json:"source_artifact"`
	SnapshotArtifact string `json:"snapshot_artifact"`
}

// RunEvent is one externally meaningful operation in an agent attempt.
type RunEvent struct {
	Kind                  string    `json:"kind"`
	Name                  string    `json:"name,omitempty"`
	Status                string    `json:"status"`
	Started               time.Time `json:"started"`
	Finished              time.Time `json:"finished"`
	DurationMS            int64     `json:"duration_ms"`
	ProcessingTimeMS      *int64    `json:"processing_time_ms,omitempty"`
	Usage                 *usage    `json:"usage,omitempty"` // token counts of a model call, when the model reports them
	EstimatedInputTokens  *int64    `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens *int64    `json:"estimated_output_tokens,omitempty"`
	EstimationMethod      string    `json:"estimation_method,omitempty"`
	Error                 string    `json:"error,omitempty"`
	Category              string    `json:"error_category,omitempty"`
	ToolCallID            string    `json:"tool_call_id,omitempty"`
	Model                 string    `json:"model,omitempty"`
	Service               string    `json:"service,omitempty"`
	Turn                  int       `json:"turn,omitempty"`
	TraceID               string    `json:"trace_id,omitempty"`
	SpanID                string    `json:"span_id,omitempty"`
	Inputs                []string  `json:"input_artifacts,omitempty"`
	Output                string    `json:"output_artifact,omitempty"`
	Policies              []string  `json:"policies,omitempty"`
	MeasurementID         string    `json:"measurement_id,omitempty"`
}

type correlation struct {
	traceID, stepID, agentRef, traceparent     string
	executionID, parentExecutionID, activityID string
	activityAttempt                            int32
	turn                                       int
}
type correlationKey struct{}

type categorizedError struct{ category, message string }

func (e categorizedError) Error() string         { return e.message }
func (e categorizedError) ErrorCategory() string { return e.category }

func withCorrelation(ctx context.Context, req StepRequest, ref string, attempt int32) context.Context {
	sum := sha256.Sum256([]byte(req.TraceID + "\x00" + req.OutputArtifact + "\x00" + req.StepID + "\x00" + ref + "\x00" + strconv.Itoa(int(attempt))))
	trace := sha256.Sum256([]byte(req.TraceID))
	c := correlation{
		traceID: req.TraceID, stepID: req.StepID, agentRef: ref,
		traceparent:     fmt.Sprintf("00-%x-%x-01", trace[:16], sum[:8]),
		activityAttempt: attempt,
		executionID:     req.ExecutionID, parentExecutionID: req.ParentExecutionID,
	}
	if activity.IsActivity(ctx) {
		info := activity.GetInfo(ctx)
		c.executionID = info.WorkflowExecution.RunID
		c.activityID = info.ActivityID
	}
	return context.WithValue(ctx, correlationKey{}, c)
}

func withModelTurn(ctx context.Context, turn int) context.Context {
	c, ok := ctx.Value(correlationKey{}).(correlation)
	if !ok {
		return ctx
	}
	c.turn = turn
	return context.WithValue(ctx, correlationKey{}, c)
}

func setCorrelationHeaders(req *http.Request) {
	if c, ok := req.Context().Value(correlationKey{}).(correlation); ok {
		req.Header.Set("X-Trace-Id", c.traceID)
		req.Header.Set("X-Step-Id", c.stepID)
		req.Header.Set("X-Agent-Ref", c.agentRef)
		if c.executionID != "" {
			req.Header.Set("X-Execution-Id", c.executionID)
		}
		if c.parentExecutionID != "" {
			req.Header.Set("X-Parent-Execution-Id", c.parentExecutionID)
		}
		if c.activityID != "" {
			req.Header.Set("X-Activity-Id", c.activityID)
		}
		req.Header.Set("X-Activity-Attempt", strconv.Itoa(int(c.activityAttempt)))
		if c.turn > 0 {
			req.Header.Set("X-Turn", strconv.Itoa(c.turn))
		}
		if req.Header.Get("Traceparent") == "" {
			req.Header.Set("Traceparent", c.traceparent)
		}
	}
	otel.GetTextMapPropagator().Inject(req.Context(), propagation.HeaderCarrier(req.Header))
}

func eventSpan(ctx context.Context, event RunEvent, spanName string, attrs ...attribute.KeyValue) (context.Context, RunEvent, trace.Span) {
	if c, ok := ctx.Value(correlationKey{}).(correlation); ok {
		attrs = append(attrs, attribute.String("factory.run_id", c.traceID))
		if c.executionID != "" {
			attrs = append(attrs, attribute.String("factory.execution_id", c.executionID))
		}
		if c.parentExecutionID != "" {
			attrs = append(attrs, attribute.String("factory.parent_execution_id", c.parentExecutionID))
		}
	}
	ctx, span := otel.Tracer("opensoftwarefactory/agent").Start(ctx, spanName, trace.WithAttributes(attrs...))
	sc := span.SpanContext()
	if sc.IsValid() {
		event.TraceID, event.SpanID = sc.TraceID().String(), sc.SpanID().String()
	}
	return ctx, event, span
}

func finishEventSpan(event RunEvent, span trace.Span, status string, err error) RunEvent {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, errorCategory(err))
	} else {
		span.SetStatus(codes.Ok, status)
	}
	span.End()
	return finishEvent(event, status, err)
}

// inputArtifacts lists the object IDs of the step's inputs in list order.
func inputArtifacts(req StepRequest) []string {
	ids := make([]string, 0, len(req.Inputs))
	for _, item := range req.Inputs {
		ids = append(ids, item.Artifact)
	}
	return ids
}

func promptEvidencePrefix(output string, attempt int32) string {
	parts := strings.Split(output, "/")
	if len(parts) >= 3 && parts[0] == "runs" {
		return strings.Join([]string{parts[0], parts[1], "_evidence", strings.Join(parts[2:], "/"), "attempt-" + strconv.Itoa(int(attempt))}, "/")
	}
	return output + ".evidence/attempt-" + strconv.Itoa(int(attempt))
}

func attemptOf(ctx context.Context) int32 {
	if activity.IsActivity(ctx) {
		return activity.GetInfo(ctx).Attempt
	}
	return 1
}

func errorCategory(err error) string {
	var categorized interface{ ErrorCategory() string }
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case temporal.IsApplicationError(err):
		return "application"
	case errors.As(err, &categorized):
		return categorized.ErrorCategory()
	default:
		return "transport"
	}
}

func finishEvent(event RunEvent, status string, err error) RunEvent {
	event.Finished = time.Now().UTC()
	event.DurationMS = max(0, event.Finished.Sub(event.Started).Milliseconds())
	event.ProcessingTimeMS = int64Ptr(event.DurationMS)
	event.Status = status
	if err != nil {
		event.Error, event.Category = err.Error(), errorCategory(err)
	}
	return event
}

func int64Ptr(value int64) *int64 { return &value }

func addEstimate(total **int64, value int64) {
	if *total == nil {
		*total = int64Ptr(value)
		return
	}
	**total += value
}

func addReported(total **int64, incomplete *bool, value *int64) {
	if *incomplete {
		return
	}
	if value == nil {
		*total = nil
		*incomplete = true
		return
	}
	if *total == nil {
		copied := *value
		*total = &copied
		return
	}
	**total += *value
}

func addUsage(total **usage, current *usage) {
	if *total == nil {
		*total = &usage{}
	}
	if current == nil {
		current = &usage{}
	}
	addReported(&(*total).PromptTokens, &(*total).promptIncomplete, current.PromptTokens)
	addReported(&(*total).CompletionTokens, &(*total).completionIncomplete, current.CompletionTokens)
	addReported(&(*total).TotalTokens, &(*total).totalIncomplete, current.TotalTokens)
	addReported(&(*total).CachedInputTokens, &(*total).cachedIncomplete, current.CachedInputTokens)
	addReported(&(*total).ReasoningTokens, &(*total).reasoningIncomplete, current.ReasoningTokens)
}

// finalModelInputEstimate describes only the initial serialized request represented by the handoff.
func finalModelInputEstimate(events []RunEvent) *int64 {
	for _, event := range events {
		if event.Kind == "model_call" && event.EstimatedInputTokens != nil {
			return event.EstimatedInputTokens
		}
	}
	return nil
}

// Service runs pinned agent definitions as Temporal step activities.
type Service struct {
	Store                 Store
	RegistryURL           string
	PolicyRegistryURL     string
	LLMURL                string
	EvaluationURL         string
	MCPGatewayURL         string
	HTTP                  *http.Client
	Logger                *slog.Logger
	reconcileMu           sync.Mutex
	reconcileDirs         []string
	intentReconcileOffset int
}

var refPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// parseRef splits a pinned reference like "intake@v1" or "intake/triage@v2": the name may
// sit in groups, at most four segments deep, as in the registry.
func parseRef(ref string) (name, version string, err error) {
	name, version, ok := strings.Cut(ref, "@")
	if !ok || !validName(name) || !refPart.MatchString(version) {
		return "", "", fmt.Errorf("agent reference %q must be <name>@<version>", ref)
	}
	return name, version, nil
}

// validName accepts a name of one to four segments separated by "/".
func validName(name string) bool {
	parts := strings.Split(name, "/")
	if len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		if !refPart.MatchString(p) {
			return false
		}
	}
	return true
}

// Process runs the step's agent and stores its final answer and run record.
func (s *Service) Process(ctx context.Context, req StepRequest) (result StepResult, retErr error) {
	executionID := req.ExecutionID
	if activity.IsActivity(ctx) {
		executionID = activity.GetInfo(ctx).WorkflowExecution.RunID
	}
	ctx, processSpan := otel.Tracer("opensoftwarefactory/agent").Start(ctx, "agent.process",
		trace.WithAttributes(attribute.String("factory.run_id", req.TraceID), attribute.String("factory.execution_id", executionID), attribute.String("factory.parent_execution_id", req.ParentExecutionID), attribute.String("factory.step", req.StepID), attribute.String("factory.agent", req.Agent)))
	defer processSpan.End()
	started := time.Now().UTC()
	attempt := attemptOf(ctx)
	record := RunRecord{Agent: req.Agent, Started: started, Attempt: attempt, Status: "running", Messages: []any{}, RunID: req.TraceID}
	record.ExecutionID, record.ParentExecutionID, record.ActivityAttempt = executionID, req.ParentExecutionID, attempt
	if activity.IsActivity(ctx) {
		record.ActivityID = activity.GetInfo(ctx).ActivityID
	}
	ctx = withCorrelation(ctx, req, req.Agent, attempt)
	if sc := processSpan.SpanContext(); sc.IsValid() {
		record.TraceID, record.SpanID = sc.TraceID().String(), sc.SpanID().String()
	}
	meta := Meta{Creator: "agent:" + req.Agent, TraceID: req.TraceID}
	log := s.Logger.With("run_id", req.TraceID, "step", req.StepID, "agent", req.Agent,
		"attempt", attempt, "trace_id", record.TraceID, "span_id", record.SpanID)
	if previous, ok := s.resumeCompletedExtraction(ctx, req, log); ok {
		return previous, nil
	}
	if err := s.persistRunRecord(record, req.OutputArtifact, meta); err != nil {
		return StepResult{}, fmt.Errorf("persisting agent start checkpoint: %w", err)
	}
	defer func() {
		record.Finished = time.Now().UTC()
		record.ProcessingTimeMS = int64Ptr(max(int64(0), record.Finished.Sub(started).Milliseconds()))
		if retErr != nil {
			record.Status = "failed"
			record.Error = retErr.Error()
			if errors.Is(retErr, context.Canceled) {
				record.Status = "canceled"
			}
		} else if result.Blocked != "" {
			record.Status = "blocked"
			record.Error = result.Blocked
		} else {
			record.Status = "completed"
		}
		if err := s.persistRunRecord(record, req.OutputArtifact, meta); err != nil {
			log.Error("persisting agent run record failed", "error", err)
			if retErr == nil {
				retErr = fmt.Errorf("writing run record: %w", err)
				result = StepResult{}
				record.Status = "failed"
				record.Error = retErr.Error()
			}
		}
		if retErr != nil {
			processSpan.RecordError(retErr)
			processSpan.SetStatus(codes.Error, errorCategory(retErr))
			log.Error("agent failed", "status", record.Status, "error", retErr)
		} else {
			processSpan.SetStatus(codes.Ok, record.Status)
			log.Info("agent completed", "status", record.Status, "model_calls", record.ModelCalls)
		}
	}()
	name, version, err := parseRef(req.Agent)
	if err != nil {
		return StepResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidAgent", nil)
	}
	ref := name + "@" + version
	ctx = withCorrelation(ctx, req, ref, attempt)
	ctx = withDevPod(ctx, req.DevPod, ref, req.TraceID)
	record.Agent = ref
	meta.Creator = "agent:" + ref
	log = s.Logger.With("run_id", req.TraceID, "step", req.StepID, "agent", ref,
		"attempt", attempt, "trace_id", record.TraceID, "span_id", record.SpanID)

	prompt, def, err := s.loadAgent(ctx, name, version)
	if err != nil {
		return StepResult{}, err
	}
	record.Model, record.Effort = def.Models[0], def.Effort
	record.Temperature = def.Temperature
	evidencePrefix := promptEvidencePrefix(req.OutputArtifact, attempt)
	record.PromptEvidence = &PromptEvidence{
		AgentSourceArtifact:  "agents/" + name + "/" + version + "/systemprompt.md",
		SourcePromptArtifact: evidencePrefix + "/systemprompt.source.md",
	}
	if err := s.Store.Put(ctx, record.PromptEvidence.SourcePromptArtifact, []byte(prompt), meta); err != nil {
		return StepResult{}, fmt.Errorf("writing source system prompt evidence: %w", err)
	}
	if err := s.checkpointRunRecord(record, req.OutputArtifact, meta, log); err != nil {
		return StepResult{}, err
	}
	policyTokens, policyParseErr := parsePolicyTokens(prompt)
	if policyParseErr != nil || len(policyTokens) > 0 {
		compileCtx, compileEvent, compileSpan := eventSpan(ctx, RunEvent{Kind: "policy_compilation", Name: "compile policies", Status: "running", Started: time.Now().UTC()}, "agent.policy_compilation")
		record.Events = append(record.Events, compileEvent)
		if err := s.checkpointRunRecord(record, req.OutputArtifact, meta, log); err != nil {
			return StepResult{}, err
		}
		var policies []resolvedPolicy
		prompt, policies, err = s.compilePrompt(compileCtx, prompt)
		if err != nil {
			record.Events[len(record.Events)-1] = finishEventSpan(compileEvent, compileSpan, "failed", categorizedError{"policy_registry", err.Error()})
			_ = s.checkpointRunRecord(record, req.OutputArtifact, meta, log)
			log.Error("policy compilation failed", "error", err)
			var required requiredPolicyError
			if errors.As(err, &required) {
				return StepResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidAgent", nil)
			}
			return StepResult{}, err
		}
		provenance := make([]string, 0, len(policies))
		for _, policy := range policies {
			snapshot := evidencePrefix + "/policies/" + policy.ID + "/" + policy.Version + "/policy.md"
			if err := s.Store.Put(compileCtx, snapshot, []byte(policy.Content), meta); err != nil {
				evidenceErr := fmt.Errorf("writing policy evidence %q: %w", policy.ID, err)
				record.Events[len(record.Events)-1] = finishEventSpan(compileEvent, compileSpan, "failed", evidenceErr)
				_ = s.checkpointRunRecord(record, req.OutputArtifact, meta, log)
				return StepResult{}, evidenceErr
			}
			record.PromptEvidence.Policies = append(record.PromptEvidence.Policies, PolicyEvidence{
				ID: policy.ID, Version: policy.Version,
				SourceArtifact:   "policies/" + policy.ID + "/" + policy.Version + "/policy.md",
				SnapshotArtifact: snapshot,
			})
			provenance = append(provenance, policy.ID+"@"+policy.Version)
		}
		compileEvent.Policies = provenance
		record.Events[len(record.Events)-1] = finishEventSpan(compileEvent, compileSpan, "completed", nil)
		record.Events[len(record.Events)-1].Policies = provenance
		if err := s.checkpointRunRecord(record, req.OutputArtifact, meta, log); err != nil {
			return StepResult{}, err
		}
		log.Info("policy compilation completed", "policies", provenance)
	}
	record.PromptEvidence.EffectivePromptArtifact = evidencePrefix + "/systemprompt.effective.md"
	if err := s.Store.Put(ctx, record.PromptEvidence.EffectivePromptArtifact, []byte(prompt), meta); err != nil {
		return StepResult{}, fmt.Errorf("writing effective system prompt evidence: %w", err)
	}
	if err := s.checkpointRunRecord(record, req.OutputArtifact, meta, log); err != nil {
		return StepResult{}, err
	}
	// One model call, plus max_retries more (each tool round needs one).
	maxCalls := 1 + max(def.MaxRetries, 0)

	user, err := s.firstMessage(ctx, req, *def.UnpackInputs)
	if err != nil {
		return StepResult{}, err
	}
	if strings.TrimSpace(req.DevPod) != "" {
		user += devPodNote(def.Commands)
	}

	var tools []any
	routes := map[string]toolRoute{}
	if len(def.Tools) > 0 {
		eventCtx, event, eventTrace := eventSpan(ctx, RunEvent{Kind: "tool_discovery", Name: "discover tools", Status: "running", Started: time.Now().UTC(), Inputs: inputArtifacts(req)}, "agent.tool_discovery")
		record.Events = append(record.Events, event)
		if err := s.checkpointRunRecord(record, req.OutputArtifact, meta, log); err != nil {
			return StepResult{}, err
		}
		if tools, routes, err = s.discoverTools(eventCtx, def.Tools, log); err != nil {
			record.Events[len(record.Events)-1] = finishEventSpan(event, eventTrace, "failed", categorizedError{"mcp_gateway", err.Error()})
			_ = s.checkpointRunRecord(record, req.OutputArtifact, meta, log)
			return StepResult{}, err
		}
		record.Events[len(record.Events)-1] = finishEventSpan(event, eventTrace, "completed", nil)
		if err := s.checkpointRunRecord(record, req.OutputArtifact, meta, log); err != nil {
			return StepResult{}, err
		}
		defer closeToolRoutes(routes, log)
	}

	messages := []any{
		map[string]string{"role": "system", "content": prompt},
		map[string]string{"role": "user", "content": user},
	}
	record.Messages = messages
	var total *usage
	anyReportedUsage := false
	calls := 0
	answer := ""
	done := false
	blocked := ""
	for calls < maxCalls && !done {
		calls++
		record.ModelCalls = calls
		record.Messages = messages
		modelRequest := def.chatRequest(messages, tools)
		inputEstimate := estimateJSONTokens(modelRequest)
		eventCtx, event, eventTrace := eventSpan(ctx, RunEvent{Kind: "model_call", Name: "chat completion", Model: def.Models[0], Turn: calls, Status: "running", Started: time.Now().UTC(), Inputs: inputArtifacts(req), EstimatedInputTokens: &inputEstimate, EstimationMethod: tokenEstimationMethod}, "agent.model_call", attribute.String("gen_ai.request.model", def.Models[0]), attribute.Int("factory.turn", calls))
		record.Events = append(record.Events, event)
		if err := s.checkpointRunRecord(record, req.OutputArtifact, meta, log); err != nil {
			return StepResult{}, err
		}
		c, err := chat(withModelTurn(eventCtx, calls), s.HTTP, s.LLMURL, modelRequest)
		addEstimate(&record.EstimatedInputTokens, inputEstimate)
		record.EstimationMethod = tokenEstimationMethod
		if err != nil {
			record.Events[len(record.Events)-1] = finishEventSpan(event, eventTrace, "failed", err)
			log.Warn("model call completed", "model", def.Models[0], "turn", calls, "status", "failed",
				"estimated_input_tokens", inputEstimate, "estimation_method", tokenEstimationMethod,
				"processing_time_ms", record.Events[len(record.Events)-1].DurationMS, "error_category", errorCategory(err))
			_ = s.checkpointRunRecord(record, req.OutputArtifact, meta, log)
			return StepResult{}, err
		}
		record.Events[len(record.Events)-1] = finishEventSpan(event, eventTrace, "completed", nil)
		record.Events[len(record.Events)-1].MeasurementID = c.MeasurementID
		addUsage(&total, c.Usage)
		if c.Usage != nil {
			anyReportedUsage = true
			record.Events[len(record.Events)-1].Usage = c.Usage
		}
		outputEstimate := estimateTokens(string(c.Message))
		processingMS := record.Events[len(record.Events)-1].DurationMS
		record.Events[len(record.Events)-1].EstimatedOutputTokens = &outputEstimate
		record.Events[len(record.Events)-1].ProcessingTimeMS = &processingMS
		record.Events[len(record.Events)-1].EstimationMethod = tokenEstimationMethod
		addEstimate(&record.EstimatedOutputTokens, outputEstimate)
		log.Info("model call completed", "model", def.Models[0], "turn", calls,
			"status", record.Events[len(record.Events)-1].Status,
			"estimated_input_tokens", inputEstimate, "estimated_output_tokens", outputEstimate,
			"estimation_method", tokenEstimationMethod, "processing_time_ms", processingMS)
		messages = append(messages, c.Message)
		record.Messages, record.ModelCalls = messages, calls
		if anyReportedUsage {
			record.Usage = total
		}
		if err := s.checkpointRunRecord(record, req.OutputArtifact, meta, log); err != nil {
			return StepResult{}, err
		}
		if len(c.ToolCalls) == 0 {
			record.Events[len(record.Events)-1].Output = req.OutputArtifact
			answer, done = c.Content, true
			break
		}
		for _, tc := range c.ToolCalls {
			service := strings.SplitN(tc.Function.Name, "__", 2)[0]
			toolInputEstimate := estimateJSONTokens(map[string]string{"name": tc.Function.Name, "arguments": tc.Function.Arguments})
			eventCtx, event, eventTrace := eventSpan(ctx, RunEvent{Kind: "tool_call", Name: tc.Function.Name, ToolCallID: tc.ID, Service: service, Turn: calls, Status: "running", Started: time.Now().UTC(), Inputs: inputArtifacts(req), EstimatedInputTokens: &toolInputEstimate, EstimationMethod: tokenEstimationMethod}, "agent.tool_call", attribute.String("gen_ai.tool.name", tc.Function.Name), attribute.String("factory.mcp_service", service), attribute.Int("factory.turn", calls))
			record.Events = append(record.Events, event)
			if err := s.checkpointRunRecord(record, req.OutputArtifact, meta, log); err != nil {
				return StepResult{}, err
			}
			text, toolFailed, err := s.runTool(eventCtx, routes, tc)
			if err != nil {
				finished := finishEventSpan(event, eventTrace, "failed", categorizedError{"mcp_gateway", err.Error()})
				record.Events[len(record.Events)-1] = finished
				log.Warn("tool output completed", "tool", tc.Function.Name, "turn", calls, "status", "failed",
					"estimated_input_tokens", toolInputEstimate, "estimation_method", tokenEstimationMethod,
					"processing_time_ms", finished.DurationMS, "error_category", finished.Category)
				_ = s.checkpointRunRecord(record, req.OutputArtifact, meta, log)
				log.Warn("tool call completed", "tool", tc.Function.Name, "turn", calls, "outcome", "failed",
					"duration_ms", finished.DurationMS, "error_category", finished.Category)
				return StepResult{}, err
			}
			status := "completed"
			var toolErr error
			if toolFailed {
				status = "failed"
				toolErr = categorizedError{"tool", text}
			}
			finished := finishEventSpan(event, eventTrace, status, toolErr)
			finished.EstimatedInputTokens = &toolInputEstimate
			finished.EstimationMethod = tokenEstimationMethod
			if err == nil {
				toolOutputEstimate := estimateTokens(text)
				finished.EstimatedOutputTokens = &toolOutputEstimate
			}
			finished.ProcessingTimeMS = int64Ptr(finished.DurationMS)
			record.Events[len(record.Events)-1] = finished
			log.Info("tool output completed", "tool", tc.Function.Name, "turn", calls,
				"status", finished.Status, "estimated_input_tokens", toolInputEstimate,
				"estimated_output_tokens", finished.EstimatedOutputTokens,
				"estimation_method", tokenEstimationMethod, "processing_time_ms", finished.DurationMS)
			if toolFailed {
				log.Warn("tool call completed", "tool", tc.Function.Name, "turn", calls, "outcome", "failed",
					"duration_ms", finished.DurationMS, "error_category", finished.Category)
			} else {
				log.Info("tool call completed", "tool", tc.Function.Name, "turn", calls, "outcome", "success",
					"duration_ms", finished.DurationMS)
			}
			messages = append(messages, map[string]string{"role": "tool", "tool_call_id": tc.ID, "content": text})
			record.Messages = messages
			if err := s.checkpointRunRecord(record, req.OutputArtifact, meta, log); err != nil {
				return StepResult{}, err
			}
			// A successful report_blocked ends the work at once: the reason is the output.
			if reason, ok := blockedReason(ctx); ok {
				record.Events[len(record.Events)-1].Output = req.OutputArtifact
				answer, blocked, done = reason, reason, true
				break
			}
		}
	}
	if !done {
		return StepResult{}, fmt.Errorf("agent %s gave no final answer within %d model calls (max_retries=%d)", ref, maxCalls, def.MaxRetries)
	}

	if len(record.Events) > 0 {
		meta.EstimatedInputTokens = finalModelInputEstimate(record.Events)
		finalOutputEstimate := estimateTokens(answer)
		meta.EstimatedOutputTokens = &finalOutputEstimate
		attemptMS := max(int64(0), time.Since(started).Milliseconds())
		meta.ProcessingTimeMS = &attemptMS
		meta.EstimationMethod = tokenEstimationMethod
	}
	attemptMS := max(int64(0), time.Since(started).Milliseconds())
	record.ProcessingTimeMS = &attemptMS
	if err := s.Store.Put(ctx, req.OutputArtifact, []byte(answer), meta); err != nil {
		return StepResult{}, fmt.Errorf("writing artifact %q: %w", req.OutputArtifact, err)
	}
	if def.OutputContract != nil {
		record.OutputValidation = validateOutput(answer, *def.OutputContract)
		record.ExtractionObligation = makeExtractionIntent(req, ref, *def.OutputContract, record.OutputValidation)
		if record.ExtractionObligation == nil {
			log.Warn("judge extraction not submitted", "reason", "target identity missing")
		} else {
			if err := s.persistRunRecord(record, req.OutputArtifact, meta); err != nil {
				log.Error("persisting extraction obligation in run record failed", "error", err)
			}
			s.deliverExtraction(ctx, record.ExtractionObligation, req.TraceID, log)
		}
	}
	log.Info("agent finished", "model_calls", calls, "artifact", req.OutputArtifact,
		"estimated_input_tokens", meta.EstimatedInputTokens, "estimated_output_tokens", meta.EstimatedOutputTokens,
		"estimation_method", meta.EstimationMethod, "processing_time_ms", meta.ProcessingTimeMS)
	return StepResult{Artifact: req.OutputArtifact, Blocked: blocked}, nil
}

func (s *Service) resumeCompletedExtraction(ctx context.Context, req StepRequest, log *slog.Logger) (StepResult, bool) {
	data, err := s.Store.Get(ctx, req.OutputArtifact+".run.json")
	if err != nil {
		return StepResult{}, false
	}
	var previous RunRecord
	if json.Unmarshal(data, &previous) != nil || previous.Status != "completed" || previous.ExtractionObligation == nil {
		return StepResult{}, false
	}
	if _, err := s.Store.Get(ctx, req.OutputArtifact); err != nil {
		return StepResult{}, false
	}
	if previous.ExtractionObligation.Status == "delivered" {
		return StepResult{Artifact: req.OutputArtifact}, true
	}
	log.Info("resuming durable extraction without rerunning agent", "artifact", req.OutputArtifact)
	s.deliverExtraction(ctx, previous.ExtractionObligation, req.TraceID, log)
	_ = s.persistRunRecord(previous, req.OutputArtifact, Meta{Creator: "agent:" + previous.Agent, TraceID: req.TraceID})
	return StepResult{Artifact: req.OutputArtifact}, true
}

// checkpointRunRecord makes an in-flight operation and the conversation leading to it durable.
// The agent stops before continuing when durability is unavailable, so no external call can
// complete without a record of what was in flight.
func (s *Service) checkpointRunRecord(record RunRecord, output string, meta Meta, log *slog.Logger) error {
	if err := s.persistRunRecord(record, output, meta); err != nil {
		log.Warn("persisting agent checkpoint failed", "error", err)
		return fmt.Errorf("persisting agent checkpoint: %w", err)
	}
	return nil
}

func (s *Service) persistRunRecord(record RunRecord, output string, meta Meta) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	parent := context.Background()
	if traceID, err := trace.TraceIDFromHex(record.TraceID); err == nil {
		if spanID, err := trace.SpanIDFromHex(record.SpanID); err == nil {
			parent = trace.ContextWithRemoteSpanContext(parent, trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true}))
		}
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	attemptID := output + ".attempt-" + strconv.Itoa(int(record.Attempt)) + ".run.json"
	if err = s.Store.Put(ctx, attemptID, data, meta); err != nil {
		return fmt.Errorf("writing attempt record: %w", err)
	}
	if err = s.Store.Put(ctx, output+".run.json", data, meta); err != nil {
		return fmt.Errorf("writing latest record: %w", err)
	}
	return nil
}

// loadAgent fetches systemprompt.md and agent.json from the registry.
func (s *Service) loadAgent(ctx context.Context, name, version string) (string, Definition, error) {
	// A name may contain folders ("translation/en-2-de"); each segment is escaped on its own.
	segments := strings.Split(name, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	u := strings.TrimRight(s.RegistryURL, "/") + "/agents/" + strings.Join(segments, "/") + "/" + url.PathEscape(version)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", Definition{}, err
	}
	resp, err := s.HTTP.Do(hreq)
	if err != nil {
		return "", Definition{}, fmt.Errorf("agent registry: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", Definition{}, fmt.Errorf("agent registry: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", Definition{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("agent %s@%s not found in registry", name, version), "AgentNotFound", nil)
	}
	if resp.StatusCode/100 != 2 {
		return "", Definition{}, fmt.Errorf("agent registry: GET %s: %s", u, resp.Status)
	}
	var entry struct {
		Files struct {
			Prompt     string     `json:"systemprompt.md"`
			Definition Definition `json:"agent.json"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &entry); err != nil {
		return "", Definition{}, fmt.Errorf("agent registry: invalid response: %w", err)
	}
	if len(entry.Files.Definition.Models) == 0 || entry.Files.Definition.Models[0] == "" {
		return "", Definition{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("agent %s@%s: agent.json lists no models", name, version), "InvalidAgent", nil)
	}
	if entry.Files.Definition.UnpackInputs == nil {
		return "", Definition{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("agent %s@%s: agent.json has no \"unpack_inputs\"; it must be true or false", name, version), "InvalidAgent", nil)
	}
	return entry.Files.Prompt, entry.Files.Definition, nil
}

// rejectionSentence ends a first message that carries a rejection.
const rejectionSentence = "Change your previous result so that the rejection no longer applies. Keep what was not criticised."

// rejectionKind reports whether an input item is one a rejected gate sent back.
func rejectionKind(kind string) bool {
	return kind == "rejection" || kind == "previous" || kind == "reviewed"
}

// firstMessage renders the documented first user message. With unpack it loads every input
// and writes its content. Without it, it lists only the references. Both keep the list order,
// with the items a rejected gate sent back after the normal inputs.
func (s *Service) firstMessage(ctx context.Context, req StepRequest, unpack bool) (string, error) {
	parts := []string{fmt.Sprintf("Trace: %s\nWorkflow: %s\nStep: %s\nOutput artifact: %s",
		req.TraceID, req.Workflow, req.StepID, req.OutputArtifact)}
	var normal, back []InputItem
	for _, item := range req.Inputs {
		if rejectionKind(item.Kind) {
			back = append(back, item)
		} else {
			normal = append(normal, item)
		}
	}
	if !unpack {
		if len(normal) > 0 {
			lines := []string{"Inputs (read each with get_artifact):"}
			for _, item := range normal {
				lines = append(lines, fmt.Sprintf("- \"%s\" round %d: object %s", item.Source, item.Round, item.Artifact))
			}
			parts = append(parts, strings.Join(lines, "\n"))
		}
		if len(back) > 0 {
			lines := []string{"Rejection (read each with get_artifact):"}
			for _, item := range back {
				switch item.Kind {
				case "rejection":
					lines = append(lines, fmt.Sprintf("- rejection from \"%s\" round %d: object %s", item.Source, item.Round, item.Artifact))
				case "previous":
					lines = append(lines, fmt.Sprintf("- your previous result round %d: object %s", item.Round, item.Artifact))
				default:
					lines = append(lines, fmt.Sprintf("- reviewed input \"%s\" round %d: object %s", item.Source, item.Round, item.Artifact))
				}
			}
			parts = append(parts, strings.Join(lines, "\n"), rejectionSentence)
		}
		return strings.Join(parts, "\n\n"), nil
	}
	// The round is written only for a source that appears more than once.
	perSource := map[string]int{}
	for _, item := range normal {
		perSource[item.Source]++
	}
	for _, item := range normal {
		data, err := s.Store.Get(ctx, item.Artifact)
		if err != nil {
			return "", fmt.Errorf("reading artifact %q: %w", item.Artifact, err)
		}
		round := ""
		if perSource[item.Source] > 1 {
			round = fmt.Sprintf(" round %d", item.Round)
		}
		parts = append(parts, fmt.Sprintf("## Input \"%s\"%s (object %s)\n%s", item.Source, round, item.Artifact, data))
	}
	for _, item := range back {
		data, err := s.Store.Get(ctx, item.Artifact)
		if err != nil {
			return "", fmt.Errorf("reading artifact %q: %w", item.Artifact, err)
		}
		switch item.Kind {
		case "rejection":
			// The decision record names the gate and holds the person's reason. A record that
			// cannot be read as such is shown as it is.
			gate, reason := item.Source, string(data)
			var decision struct {
				Step    string  `json:"step"`
				Comment *string `json:"comment"`
			}
			if json.Unmarshal(data, &decision) == nil && decision.Comment != nil {
				reason = *decision.Comment
				if decision.Step != "" {
					gate = decision.Step
				}
			}
			parts = append(parts, fmt.Sprintf("## Rejection from \"%s\" (round %d)\n%s", gate, item.Round, reason))
		case "previous":
			parts = append(parts, fmt.Sprintf("## Your previous result (round %d) (object %s)\n%s", item.Round, item.Artifact, data))
		default:
			parts = append(parts, fmt.Sprintf("## Input \"%s\" round %d (object %s)\n%s", item.Source, item.Round, item.Artifact, data))
		}
	}
	if len(back) > 0 {
		parts = append(parts, rejectionSentence)
	}
	return strings.Join(parts, "\n\n"), nil
}

type configuredTool struct {
	ref, server, name string
}

type toolRoute struct {
	client *mcpClient
	server string // the MCP service the tool belongs to
	name   string
}

func parseConfiguredTool(ref string) (configuredTool, error) {
	server, name, ok := strings.Cut(ref, "/")
	if !ok || !refPart.MatchString(server) || strings.TrimSpace(name) == "" {
		return configuredTool{}, fmt.Errorf("tool reference %q must be <server>/<tool>", ref)
	}
	return configuredTool{ref: ref, server: server, name: name}, nil
}

func modelToolName(ref string, used map[string]string) string {
	var b strings.Builder
	for _, r := range strings.Replace(ref, "/", "__", 1) {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	alias := b.String()
	if len(alias) <= 64 && (used[alias] == "" || used[alias] == ref) {
		return alias
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(ref)))[:8]
	if len(alias) > 55 {
		alias = alias[:55]
	}
	return alias + "_" + sum
}

// discoverTools asks each selected gateway service for its policy-filtered tools.
func (s *Service) discoverTools(ctx context.Context, selected []string, log *slog.Logger) ([]any, map[string]toolRoute, error) {
	byServer := map[string][]configuredTool{}
	seenRef := map[string]bool{}
	for _, ref := range selected {
		tool, err := parseConfiguredTool(ref)
		if err != nil {
			return nil, nil, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidAgent", nil)
		}
		if !seenRef[ref] {
			seenRef[ref] = true
			byServer[tool.server] = append(byServer[tool.server], tool)
		}
	}
	servers := make([]string, 0, len(byServer))
	for server := range byServer {
		servers = append(servers, server)
	}
	sort.Strings(servers)
	var tools []any
	routes := map[string]toolRoute{}
	aliases := map[string]string{}
	clients := []*mcpClient{}
	closeClients := func() {
		for _, client := range clients {
			_ = client.close()
		}
	}
	base := strings.TrimRight(s.MCPGatewayURL, "/")
	for _, server := range servers {
		client := &mcpClient{url: base + "/mcp/" + url.PathEscape(server), http: s.HTTP}
		clients = append(clients, client)
		if err := client.connect(ctx); err != nil {
			closeClients()
			return nil, nil, fmt.Errorf("discovering MCP service %q: %w", server, err)
		}
		offered, err := client.listTools(ctx)
		if err != nil {
			closeClients()
			return nil, nil, fmt.Errorf("listing MCP service %q: %w", server, err)
		}
		available := map[string]mcpTool{}
		for _, tool := range offered {
			if _, exists := available[tool.Name]; !exists {
				available[tool.Name] = tool
			}
		}
		used := false
		for _, selection := range byServer[server] {
			tool, ok := available[selection.name]
			if !ok {
				log.Warn("configured tool not offered by MCP gateway", "tool", selection.ref)
				continue
			}
			alias := modelToolName(selection.ref, aliases)
			aliases[alias] = selection.ref
			params := tool.InputSchema
			if len(params) == 0 || string(params) == "null" {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			if server == devPodService {
				params = trimDevPodSchema(params)
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": alias, "description": tool.Description, "parameters": params,
			}})
			routes[alias] = toolRoute{client: client, server: server, name: selection.name}
			used = true
		}
		if !used {
			_ = client.close()
		}
	}
	return tools, routes, nil
}

func closeToolRoutes(routes map[string]toolRoute, log *slog.Logger) {
	closed := map[*mcpClient]bool{}
	for _, route := range routes {
		if !closed[route.client] {
			closed[route.client] = true
			if err := route.client.close(); err != nil {
				log.Warn("closing MCP session failed", "error", err)
			}
		}
	}
}

// runTool executes one tool call and returns the text for the tool message.
// Problems the model can fix (unknown tool, bad arguments, tool errors) become
// tool output; transport failures fail the activity.
func (s *Service) runTool(ctx context.Context, routes map[string]toolRoute, tc toolCall) (string, bool, error) {
	alias := tc.Function.Name
	route, ok := routes[alias]
	if !ok {
		return fmt.Sprintf("Error: tool %q is not available", alias), true, nil
	}
	args := json.RawMessage(tc.Function.Arguments)
	if strings.TrimSpace(tc.Function.Arguments) == "" {
		args = json.RawMessage(`{}`)
	}
	var obj map[string]any
	if err := json.Unmarshal(args, &obj); err != nil {
		return fmt.Sprintf("Error: invalid arguments for tool %q: %v", alias, err), true, nil
	}
	devPod := route.server == devPodService
	pod := devPodOf(ctx)
	if devPod {
		// The harness, which knows the run, sets the pod and who is calling, whatever the
		// model passed. The stored tool call stays as the model wrote it.
		if pod == nil || pod.pod == "" {
			return fmt.Sprintf("Error: tool %q works in a dev pod, and this run has none. The workflow needs a devpod step before this step.", alias), true, nil
		}
		if obj == nil {
			obj = map[string]any{}
		}
		obj["pod"], obj["agent"], obj["run"] = pod.pod, pod.agent, pod.run
		data, err := json.Marshal(obj)
		if err != nil {
			return "", false, err
		}
		args = data
	}
	text, isError, err := route.client.callTool(ctx, route.name, args)
	if err != nil {
		return "", false, err
	}
	if isError {
		text = "Error: " + text
	} else if devPod && strings.HasSuffix(route.name, blockedTool) {
		reason, _ := obj["reason"].(string)
		if reason = strings.TrimSpace(reason); reason == "" {
			reason = noBlockedReason
		}
		pod.blocked, pod.reason = true, reason
	}
	return text, isError, nil
}
