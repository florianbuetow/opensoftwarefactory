package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	// RecalculationWorkflowName is the registered Temporal workflow type that recalculates
	// stored measurements.
	RecalculationWorkflowName = "MeasurementRecalculation"
	// RecalculationBatchActivity is the activity that asks evaluation for one batch.
	RecalculationBatchActivity = "RecalculationBatch"
)

// RecalculationInput names a recalculation job of the evaluation service and where its next
// batch starts.
type RecalculationInput struct {
	ID              string     `json:"id"`
	DisplayCurrency string     `json:"display_currency"`
	From            *time.Time `json:"from,omitempty"`
	To              *time.Time `json:"to,omitempty"`
	Cursor          string     `json:"cursor,omitempty"`
	MeasurementIDs  []string   `json:"measurement_ids,omitempty"`
	Limit           int        `json:"limit,omitempty"`
}

// RecalculationBatchResult is the answer of the evaluation service to one batch.
type RecalculationBatchResult struct {
	NextCursor        string `json:"next_cursor,omitempty"`
	Error             string `json:"error,omitempty"`
	Status            string `json:"status,omitempty"`
	Processed         int    `json:"processed"`
	Pending           int    `json:"pending"`
	Done              bool   `json:"done"`
	ProjectionPending bool   `json:"projection_pending,omitempty"`
}

// Recalculation is an ordinary Temporal workflow. Its only unit of work is the bounded,
// deterministic evaluation batch endpoint; progress is durable in Temporal and evaluation.
func Recalculation(ctx workflow.Context, in *RecalculationInput) error {
	if in.Limit <= 0 {
		in.Limit = 100
	}
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue: TaskQueue, StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 0},
	})
	for {
		var batch RecalculationBatchResult
		if err := workflow.ExecuteActivity(activityCtx, RecalculationBatchActivity, in).Get(activityCtx, &batch); err != nil {
			return err
		}
		if batch.Status == "failed" || batch.Status == "cancelled" {
			return temporal.NewNonRetryableApplicationError(batch.Error, "RecalculationFailed", nil)
		}
		if batch.Done && !batch.ProjectionPending {
			return nil
		}
		if batch.NextCursor != "" {
			in.Cursor = batch.NextCursor
		}
		if batch.ProjectionPending || batch.Error != "" {
			if err := workflow.Sleep(ctx, 5*time.Second); err != nil {
				return err
			}
		}
	}
}

// RecalculationBatch posts one batch of a recalculation job to the evaluation service.
func (a *Activities) RecalculationBatch(ctx context.Context, in *RecalculationInput) (out RecalculationBatchResult, err error) {
	base := strings.TrimRight(a.EvaluationURL, "/")
	if base == "" {
		return RecalculationBatchResult{}, temporal.NewNonRetryableApplicationError("evaluation service is not configured", "EvaluationUnavailable", nil)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return RecalculationBatchResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/recalculations/"+in.ID+"/batch", bytes.NewReader(body))
	if err != nil {
		return RecalculationBatchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := a.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return RecalculationBatchResult{}, err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return RecalculationBatchResult{}, err
	}
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusPreconditionFailed {
			return RecalculationBatchResult{}, temporal.NewNonRetryableApplicationError("recalculation batch pins conflict with the stored job", "RecalculationConflict", nil)
		}
		return RecalculationBatchResult{}, fmt.Errorf("evaluation batch answered %s: %s", resp.Status, bytes.TrimSpace(data))
	}
	err = json.Unmarshal(data, &out)
	return out, err
}
