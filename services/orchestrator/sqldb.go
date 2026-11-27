package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (s *server) newSQLDBRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	baseURL, err := url.Parse(s.sqldbURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL.String(), body)
	if err != nil {
		return nil, err
	}
	req.URL.Path = strings.TrimRight(baseURL.Path, "/") + path
	return req, nil
}

func (s *server) syncRunStart(ctx context.Context, run *RunRecord) {
	if s.sqldbURL == "" || s.httpClient == nil || run == nil {
		return
	}
	payload := map[string]any{
		"trace_id":         run.TraceID,
		"workflow":         run.Workflow,
		"workflow_version": run.WorkflowVersion,
		"started":          run.Started.UTC().Format(time.RFC3339),
		"status":           "Running",
		"started_by":       run.StartedBy,
		"input_artifact":   run.InputArtifact,
		"source":           run.Source,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := s.newSQLDBRequest(reqCtx, http.MethodPut, "/api/runs/"+url.PathEscape(run.TraceID), bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("syncing started run to sqldb failed", "trace_id", run.TraceID, "error", err)
		}
		return
	}
	_ = resp.Body.Close()
}

func (s *server) syncRunDelete(ctx context.Context, traceID string) {
	if s.sqldbURL == "" || s.httpClient == nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := s.newSQLDBRequest(reqCtx, http.MethodDelete, "/api/runs/"+url.PathEscape(traceID), http.NoBody)
	if err != nil {
		return
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("syncing deleted run to sqldb failed", "trace_id", traceID, "error", err)
		}
		return
	}
	_ = resp.Body.Close()
}
