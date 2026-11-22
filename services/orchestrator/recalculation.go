package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"opensoftwarefactory/orchestrator/pipeline"
)

type recalculationStarter interface {
	StartRecalculation(context.Context, pipeline.RecalculationInput) (string, error)
}

func (s *server) startMeasurementRecalculation(w http.ResponseWriter, r *http.Request) {
	var in pipeline.RecalculationInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		s.fail(w, 400, err)
		return
	}
	in.ID, in.DisplayCurrency = strings.TrimSpace(in.ID), strings.TrimSpace(in.DisplayCurrency)
	if !intakeIDToken(in.ID) || (in.DisplayCurrency != "EUR" && in.DisplayCurrency != "USD") {
		s.fail(w, 422, errors.New("id and display_currency EUR or USD are required"))
		return
	}
	if in.From != nil && in.To != nil && !in.From.Before(*in.To) {
		s.fail(w, 422, errors.New("from must be before to"))
		return
	}
	starter, ok := s.runner.(recalculationStarter)
	if !ok {
		s.fail(w, 503, errors.New("recalculation workflow is unavailable"))
		return
	}
	workflowID, err := starter.StartRecalculation(r.Context(), in)
	if err != nil {
		s.fail(w, 503, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"workflow_id": workflowID})
}

func intakeIDToken(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func (r temporalRunner) StartRecalculation(ctx context.Context, in pipeline.RecalculationInput) (string, error) {
	id := "measurement-recalculation/" + in.ID
	run, err := r.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: pipeline.TaskQueue, WorkflowExecutionTimeout: 30 * 24 * time.Hour}, pipeline.RecalculationWorkflowName, in)
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &already) {
		return id, nil
	}
	if err != nil {
		return "", err
	}
	return run.GetID(), nil
}
