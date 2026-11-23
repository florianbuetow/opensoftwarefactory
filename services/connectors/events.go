package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

type factoryEvent struct {
	ID                string    `json:"id"`
	Type              string    `json:"type"`
	RunID             string    `json:"run_id"`
	ExecutionID       string    `json:"execution_id"`
	ParentExecutionID string    `json:"parent_execution_id,omitempty"`
	Workflow          string    `json:"workflow"`
	TraceID           string    `json:"trace_id,omitempty"`
	OccurredAt        time.Time `json:"occurred_at"`
	Status            string    `json:"status"`
	SchemaVersion     int       `json:"schema_version"`
	WorkflowVersion   int       `json:"workflow_version"`
}

func bindingMatches(binding *IntakeBinding, event *factoryEvent) bool {
	if event.SchemaVersion != 1 {
		return false
	}
	if !slices.Contains(binding.SourceWorkflows, event.Workflow) {
		return false
	}
	if event.Type == "workflow.finished" && !slices.Contains(binding.Outcomes, event.Status) {
		return false
	}
	return event.Type == "workflow.finished"
}

func (s *service) submitBinding(ctx context.Context, binding *IntakeBinding, event *factoryEvent, raw []byte) (err error) {
	request := map[string]any{
		"binding_id": binding.ID, "event_id": event.ID, "workflow": binding.Workflow, "workflow_version": binding.WorkflowVersion,
		"event": json.RawMessage(raw), "binding_revision": binding.Revision, "binding": binding,
	}
	body, _ := json.Marshal(request)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.orchestratorURL+"/api/intake/runs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("orchestrator is unavailable: %w", err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	response, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return errors.New("pinned destination workflow is unavailable")
	}
	return fmt.Errorf("orchestrator rejected intake with %s: %s", resp.Status, bytes.TrimSpace(response))
}
