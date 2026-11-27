package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type blobList struct {
	Objects []struct {
		ID string `json:"id"`
	} `json:"objects"`
	Directories []string `json:"directories"`
}

func (s *service) backfillLoop(ctx context.Context) {
	for {
		if err := s.backfill(ctx); err != nil {
			s.log.Warn("initial sqldb backfill incomplete, retrying", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
				continue
			}
		}
		return
	}
}

func (s *service) handleBackfill(w http.ResponseWriter, r *http.Request) {
	if err := s.backfill(r.Context()); err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *service) backfill(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := time.Now()
	presentRuns, err := s.backfillRuns(ctx)
	if err != nil {
		return fmt.Errorf("backfilling runs: %w", err)
	}
	if err := s.backfillExecutions(ctx, presentRuns); err != nil {
		return fmt.Errorf("backfilling executions: %w", err)
	}
	if err := s.backfillConversions(ctx); err != nil {
		return fmt.Errorf("backfilling conversions: %w", err)
	}
	if err := s.backfillMeasurements(ctx); err != nil {
		return fmt.Errorf("backfilling measurements: %w", err)
	}
	if err := s.backfillCosts(ctx); err != nil {
		return fmt.Errorf("backfilling costs: %w", err)
	}
	s.log.Info("sqldb backfill complete", "runs", len(presentRuns), "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func (s *service) newBlobRequest(ctx context.Context, path string) (*http.Request, error) {
	baseURL, err := url.Parse(s.blobs)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL.String(), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.URL.Path = strings.TrimRight(baseURL.Path, "/") + path
	return req, nil
}

func (s *service) listBlobs(ctx context.Context, prefix string) (blobList, error) {
	var out blobList
	req, err := s.newBlobRequest(ctx, "/list/"+prefix)
	if err != nil {
		return out, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return out, nil
	}
	if resp.StatusCode/100 != 2 {
		return out, fmt.Errorf("blobstore list %s: %s", prefix, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func (s *service) readBlob(ctx context.Context, id string) (data []byte, found bool, err error) {
	req, err := s.newBlobRequest(ctx, "/blobs/"+id)
	if err != nil {
		return nil, false, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, false, fmt.Errorf("blobstore read %s: %s", id, resp.Status)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return data, true, err
}

func (s *service) backfillRuns(ctx context.Context) (map[string]bool, error) {
	listing, err := s.listBlobs(ctx, "runs")
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(listing.Directories))
	for _, traceID := range listing.Directories {
		present[traceID] = true
		raw, ok, err := s.readBlob(ctx, "runs/"+traceID+"/run.json")
		if err != nil || !ok {
			continue
		}
		var rec struct {
			TraceID         string `json:"trace_id"`
			Workflow        string `json:"workflow"`
			Started         string `json:"started"`
			StartedBy       string `json:"started_by"`
			InputArtifact   string `json:"input_artifact"`
			Source          string `json:"source"`
			WorkflowVersion int    `json:"workflow_version"`
		}
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		row := RunRow{
			TraceID:         traceID,
			Workflow:        rec.Workflow,
			WorkflowVersion: rec.WorkflowVersion,
			Started:         normalizeRFC3339(rec.Started),
			Status:          "Running",
			StartedBy:       rec.StartedBy,
			InputArtifact:   rec.InputArtifact,
			Source:          rec.Source,
		}
		if metaRaw, metaOK, metaErr := s.readBlob(ctx, "runs/"+traceID+"/metadata.json"); metaErr == nil && metaOK {
			var meta struct {
				Status   string `json:"status"`
				Finished string `json:"finished"`
				Error    string `json:"error"`
			}
			if json.Unmarshal(metaRaw, &meta) == nil {
				if meta.Status != "" {
					row.Status = meta.Status
				}
				if meta.Finished != "" {
					row.Finished = normalizeRFC3339(meta.Finished)
				}
				if meta.Error != "" {
					row.Error = meta.Error
				}
			}
		}
		if err := s.upsertRun(ctx, &row, false); err != nil {
			return nil, err
		}
	}
	return present, nil
}

func (s *service) backfillExecutions(ctx context.Context, presentRuns map[string]bool) error {
	listing, err := s.listBlobs(ctx, "workflow-executions")
	if err != nil {
		return err
	}
	for _, obj := range listing.Objects {
		raw, ok, err := s.readBlob(ctx, obj.ID)
		if err != nil || !ok {
			continue
		}
		var exec struct {
			RunID             string `json:"run_id"`
			TraceID           string `json:"trace_id"`
			ParentExecutionID string `json:"parent_execution_id"`
			Workflow          string `json:"workflow"`
			StartedAt         string `json:"started_at"`
			ClosedAt          string `json:"closed_at"`
			Status            string `json:"status"`
			WorkflowVersion   int    `json:"workflow_version"`
		}
		if json.Unmarshal(raw, &exec) != nil || exec.ParentExecutionID != "" {
			continue
		}
		traceID := exec.TraceID
		if traceID == "" {
			traceID = exec.RunID
		}
		if traceID == "" {
			continue
		}
		row := RunRow{
			TraceID:         traceID,
			Workflow:        exec.Workflow,
			WorkflowVersion: exec.WorkflowVersion,
			Started:         normalizeRFC3339(exec.StartedAt),
			Finished:        normalizeRFC3339(exec.ClosedAt),
			Status:          titleStatus(exec.Status),
		}
		if err := s.upsertRun(ctx, &row, !presentRuns[traceID]); err != nil {
			return err
		}
	}
	return nil
}

func titleStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed":
		return "Completed"
	case "failed":
		return "Failed"
	case "blocked":
		return "Blocked"
	case "cancelled":
		return "Cancelled"
	case "terminated":
		return "Terminated"
	case "timed_out", "timedout":
		return "TimedOut"
	case "running":
		return "Running"
	default:
		return status
	}
}

func (s *service) backfillConversions(ctx context.Context) error {
	listing, err := s.listBlobs(ctx, "measurement-config/conversions")
	if err != nil {
		return err
	}
	for _, obj := range listing.Objects {
		raw, ok, err := s.readBlob(ctx, obj.ID)
		if err != nil || !ok {
			continue
		}
		var c ConversionRow
		if json.Unmarshal(raw, &c) != nil || c.FromCurrency == "" || c.ToCurrency == "" || c.Rate == "" {
			continue
		}
		if err := s.upsertConversion(ctx, &c); err != nil {
			return err
		}
	}
	return nil
}

func (s *service) backfillMeasurements(ctx context.Context) error {
	for _, kind := range []string{"usage", "execution", "judge"} {
		listing, err := s.listBlobs(ctx, "measurements/"+kind)
		if err != nil {
			return err
		}
		for _, obj := range listing.Objects {
			raw, ok, err := s.readBlob(ctx, obj.ID)
			if err != nil || !ok {
				continue
			}
			var m MeasurementRow
			if json.Unmarshal(raw, &m) != nil || m.ID == "" {
				continue
			}
			if m.Kind == "" {
				m.Kind = kind
			}
			if err := s.upsertMeasurement(ctx, &m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *service) backfillCosts(ctx context.Context) error {
	listing, err := s.listBlobs(ctx, "measurement-costs")
	if err != nil {
		return err
	}
	for _, obj := range listing.Objects {
		raw, ok, err := s.readBlob(ctx, obj.ID)
		if err != nil || !ok {
			continue
		}
		var c CostRow
		if json.Unmarshal(raw, &c) != nil || c.UsageID == "" {
			continue
		}
		if err := s.upsertCost(ctx, &c); err != nil {
			return err
		}
	}
	return nil
}
