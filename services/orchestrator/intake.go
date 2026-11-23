package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

import "opensoftwarefactory/orchestrator/pipeline"

var intakeNamespace = uuid.MustParse("cdb9127d-ec92-5fc8-958f-cf638e419896")

type intakeRunRequest struct {
	BindingID       string          `json:"binding_id"`
	EventID         string          `json:"event_id"`
	Workflow        string          `json:"workflow"`
	Event           json.RawMessage `json:"event"`
	BindingRevision string          `json:"binding_revision"`
	Binding         json.RawMessage `json:"binding"`
	WorkflowVersion int             `json:"workflow_version"`
}

type intakeRunResult struct {
	TraceID         string `json:"trace_id"`
	Workflow        string `json:"workflow"`
	ExecutionID     string `json:"execution_id"`
	WorkflowVersion int    `json:"workflow_version"`
	Reused          bool   `json:"reused"`
}

type intakeStarter interface {
	StartIntake(context.Context, *pipeline.Input, string) (executionID string, reused bool, err error)
}

func deterministicIntakeID(bindingID, eventID string) string {
	b, _ := json.Marshal([]string{bindingID, eventID})
	return uuid.NewSHA1(intakeNamespace, b).String()
}

func canonicalIntakeHash(req *intakeRunRequest, def *pipeline.Definition) (string, error) {
	return pipeline.IntakeAcceptanceHash(def, &pipeline.IntakeInput{BindingID: req.BindingID, BindingRevision: req.BindingRevision, EventID: req.EventID, Event: req.Event, Binding: req.Binding})
}

func (s *server) startIntakeRun(w http.ResponseWriter, r *http.Request) {
	var req intakeRunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	req.BindingID, req.EventID, req.Workflow, req.BindingRevision = strings.TrimSpace(req.BindingID), strings.TrimSpace(req.EventID), strings.TrimSpace(req.Workflow), strings.TrimSpace(req.BindingRevision)
	if req.BindingID == "" || req.EventID == "" || req.Workflow == "" || req.BindingRevision == "" || req.WorkflowVersion < 1 {
		s.fail(w, http.StatusUnprocessableEntity, errors.New("binding_id, event_id, binding_revision, workflow, and a positive workflow_version are required"))
		return
	}
	cfg, err := s.config()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	def, ok := cfg.find(req.Workflow, req.WorkflowVersion)
	if !ok {
		s.fail(w, http.StatusNotFound, errors.New("pinned workflow version not found"))
		return
	}
	hash, err := canonicalIntakeHash(&req, &def)
	if err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	starter, ok := s.runner.(intakeStarter)
	if !ok {
		s.fail(w, http.StatusServiceUnavailable, errors.New("intake submission is unavailable"))
		return
	}
	traceID := deterministicIntakeID(req.BindingID, req.EventID)
	in := &pipeline.Input{
		TraceID: traceID, Definition: def, Prefix: pipeline.RunPrefix(traceID),
		Inputs: []pipeline.Item{{Source: pipeline.InputSource, Artifact: pipeline.ArtifactID(pipeline.RunPrefix(traceID), pipeline.InputSource), Round: 1}},
		Intake: &pipeline.IntakeInput{BindingID: req.BindingID, BindingRevision: req.BindingRevision, EventID: req.EventID, Event: req.Event, Binding: req.Binding, AcceptanceHash: hash},
	}
	executionID, reused, err := starter.StartIntake(r.Context(), in, hash)
	if err != nil {
		if errors.Is(err, errIntakeConflict) {
			s.fail(w, http.StatusPreconditionFailed, err)
		} else {
			s.fail(w, http.StatusServiceUnavailable, err)
		}
		return
	}
	if s.events != nil {
		if err = s.events.acceptExecution(r.Context(), &knownOpenExecution{WorkflowID: traceID, ExecutionID: executionID, RunID: traceID, Workflow: def.Name, WorkflowVersion: pipeline.DefinitionVersion(&def), AcceptedAt: time.Now().UTC()}); err != nil {
			s.fail(w, http.StatusServiceUnavailable, fmt.Errorf("recording accepted intake obligation: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusAccepted, intakeRunResult{TraceID: traceID, Workflow: def.Name, WorkflowVersion: pipeline.DefinitionVersion(&def), ExecutionID: executionID, Reused: reused})
}

var errIntakeConflict = errors.New("deterministic intake identity already accepted a different payload")

func (r temporalRunner) StartIntake(ctx context.Context, in *pipeline.Input, hash string) (executionID string, reused bool, err error) {
	run, err := r.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: in.TraceID, TaskQueue: pipeline.TaskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		Memo: map[string]any{
			"workflow": in.Definition.Name, "workflow_version": pipeline.DefinitionVersion(&in.Definition), "trace_id": in.TraceID,
			"artifact_prefix": in.Prefix, "intake_binding_id": in.Intake.BindingID, "intake_event_id": in.Intake.EventID, "intake_hash": hash,
		},
	}, pipeline.WorkflowName, in)
	if err == nil {
		return run.GetRunID(), false, nil
	}
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if !errors.As(err, &already) {
		return "", false, err
	}
	desc, describeErr := r.c.DescribeWorkflowExecution(ctx, in.TraceID, "")
	if describeErr != nil {
		return "", false, describeErr
	}
	fields := desc.GetWorkflowExecutionInfo().GetMemo().GetFields()
	var storedHash, bindingID, eventID string
	_ = converter.GetDefaultDataConverter().FromPayload(fields["intake_hash"], &storedHash)
	_ = converter.GetDefaultDataConverter().FromPayload(fields["intake_binding_id"], &bindingID)
	_ = converter.GetDefaultDataConverter().FromPayload(fields["intake_event_id"], &eventID)
	if storedHash != hash || bindingID != in.Intake.BindingID || eventID != in.Intake.EventID {
		return "", false, errIntakeConflict
	}
	exec := desc.GetWorkflowExecutionInfo().GetExecution()
	if exec == nil || exec.GetRunId() == "" {
		return "", false, errors.New("accepted intake execution has no Temporal Run ID")
	}
	return exec.GetRunId(), true, nil
}
