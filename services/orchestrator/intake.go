package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// intakeClaimPrefix holds one claim per binding and event: intake-claims/<hash>.json, where
// the hash is the SHA-256 of the JSON list [binding id, event id].
const intakeClaimPrefix = "intake-claims"

// intakeClaim is the durable answer to "which run did this binding and event start". It is
// written once, before the run starts, and is never changed or removed, also when the run
// is removed: the same event then still names that run and starts no other.
type intakeClaim struct {
	ClaimedAt      time.Time `json:"claimed_at"`
	RunID          string    `json:"run_id"`
	BindingID      string    `json:"binding_id"`
	EventID        string    `json:"event_id"`
	AcceptanceHash string    `json:"acceptance_hash"`
	SchemaVersion  int       `json:"schema_version"`
}

// intakeClaimStore is the create-only write a claim needs.
type intakeClaimStore interface {
	PutIfAbsent(ctx context.Context, id string, data []byte, meta pipeline.Meta) (revision string, err error)
}

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

// intakeClaimID names the claim of one binding and event.
func intakeClaimID(bindingID, eventID string) string {
	b, _ := json.Marshal([]string{bindingID, eventID})
	sum := sha256.Sum256(b)
	return intakeClaimPrefix + "/" + hex.EncodeToString(sum[:]) + ".json"
}

// claimIntakeRun returns the run ID of the binding and event of req. The first request
// claims a new run ID; every later one reads the ID claimed then, and existing reports
// that. A claim made for another payload is errIntakeConflict.
func (s *server) claimIntakeRun(ctx context.Context, store intakeClaimStore, req *intakeRunRequest, hash string) (runID string, existing bool, err error) {
	id := intakeClaimID(req.BindingID, req.EventID)
	// A second pass reads the claim another request created between this one's read and write.
	for pass := 0; pass < 2; pass++ {
		data, readErr := s.blobs.Get(ctx, id)
		if readErr == nil {
			var claim intakeClaim
			if json.Unmarshal(data, &claim) != nil || claim.SchemaVersion != 1 || uuid.Validate(claim.RunID) != nil || claim.AcceptanceHash == "" || claim.BindingID != req.BindingID || claim.EventID != req.EventID {
				return "", false, fmt.Errorf("intake claim %s is invalid", id)
			}
			if claim.AcceptanceHash != hash {
				return "", false, errIntakeConflict
			}
			return claim.RunID, true, nil
		}
		if !errors.Is(readErr, pipeline.ErrNotFound) {
			return "", false, fmt.Errorf("reading intake claim: %w", readErr)
		}
		runID, err = newRunID()
		if err != nil {
			return "", false, err
		}
		data, _ = json.MarshalIndent(intakeClaim{SchemaVersion: 1, RunID: runID, BindingID: req.BindingID, EventID: req.EventID, AcceptanceHash: hash, ClaimedAt: time.Now().UTC()}, "", "  ")
		_, err = store.PutIfAbsent(ctx, id, data, pipeline.Meta{Creator: "orchestrator"})
		if err == nil {
			return runID, false, nil
		}
		if !errors.Is(err, pipeline.ErrPreconditionFailed) {
			return "", false, fmt.Errorf("writing intake claim: %w", err)
		}
	}
	return "", false, fmt.Errorf("intake claim %s exists but cannot be read", id)
}

// startedIntakeRun reads the receipt the run wrote when it started, intake-receipts/<run
// id>.json. The receipt outlives the run: found tells that the run did start once, also
// when it has since ended or was removed.
func (s *server) startedIntakeRun(ctx context.Context, runID string) (receipt pipeline.IntakeReceipt, found bool, err error) {
	data, err := s.blobs.Get(ctx, "intake-receipts/"+runID+".json")
	if errors.Is(err, pipeline.ErrNotFound) {
		return receipt, false, nil
	}
	if err != nil {
		return receipt, false, fmt.Errorf("reading intake receipt: %w", err)
	}
	if json.Unmarshal(data, &receipt) != nil || receipt.TraceID != runID || receipt.ExecutionID == "" {
		return receipt, false, fmt.Errorf("intake receipt of run %s is invalid", runID)
	}
	return receipt, true, nil
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
	claims, ok := s.blobs.(intakeClaimStore)
	if !ok {
		s.fail(w, http.StatusServiceUnavailable, errors.New("intake submission is unavailable: the artifact store has no create-only write"))
		return
	}
	traceID, claimed, err := s.claimIntakeRun(r.Context(), claims, &req, hash)
	if err != nil {
		if errors.Is(err, errIntakeConflict) {
			s.fail(w, http.StatusPreconditionFailed, err)
		} else {
			s.fail(w, http.StatusServiceUnavailable, err)
		}
		return
	}
	if claimed {
		// The run of an earlier claim that left its receipt did start: it is named again and
		// nothing is started, whether it still runs, has ended, or was removed.
		receipt, started, receiptErr := s.startedIntakeRun(r.Context(), traceID)
		if receiptErr != nil {
			s.fail(w, http.StatusServiceUnavailable, receiptErr)
			return
		}
		if started {
			if receipt.AcceptanceHash != hash || receipt.BindingID != req.BindingID || receipt.EventID != req.EventID {
				s.fail(w, http.StatusPreconditionFailed, errIntakeConflict)
				return
			}
			writeJSON(w, http.StatusAccepted, intakeRunResult{TraceID: traceID, Workflow: def.Name, WorkflowVersion: pipeline.DefinitionVersion(&def), ExecutionID: receipt.ExecutionID, Reused: true})
			return
		}
	}
	// A new claim starts its run here. An earlier claim without a receipt does too: the
	// orchestrator stopped between claiming and starting, or the run has started and not
	// yet written its receipt, which StartIntake answers as reused.
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
	if !reused {
		s.syncRunStart(r.Context(), &RunRecord{
			TraceID: traceID, Workflow: def.Name, WorkflowVersion: pipeline.DefinitionVersion(&def),
			Started: time.Now().UTC(), StartedBy: "intake", InputArtifact: pipeline.ArtifactID(pipeline.RunPrefix(traceID), pipeline.InputSource),
		})
	}
	writeJSON(w, http.StatusAccepted, intakeRunResult{TraceID: traceID, Workflow: def.Name, WorkflowVersion: pipeline.DefinitionVersion(&def), ExecutionID: executionID, Reused: reused})
}

var errIntakeConflict = errors.New("this binding and event already claimed a run with a different payload")

// StartIntake starts the run a claim names, under the claimed run ID. Temporal refuses an ID
// it already knows: that happens only when an earlier request for the same claim started the
// run and this one arrives before the run wrote its receipt. The running execution is then
// checked against the claim and answered as reused.
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
