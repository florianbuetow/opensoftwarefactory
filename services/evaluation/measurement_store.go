package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var errPrecondition = errors.New("blob revision precondition failed")

type storedObject struct {
	Data     []byte
	Revision string
	Created  time.Time
}

func (s *service) readObject(ctx context.Context, id string) (storedObject, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.blobs+"/blobs/"+id, nil)
	if err != nil {
		return storedObject{}, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return storedObject{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return storedObject{}, errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return storedObject{}, fmt.Errorf("reading blob %s: %s", id, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	created, _ := time.Parse(time.RFC3339Nano, resp.Header.Get("X-Created"))
	return storedObject{Data: data, Revision: trimETag(resp.Header.Get("ETag")), Created: created}, err
}

func (s *service) readObjectLimited(ctx context.Context, id string, limit int64) (storedObject, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.blobs+"/blobs/"+id, nil)
	if err != nil {
		return storedObject{}, 0, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return storedObject{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return storedObject{}, 0, errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return storedObject{}, 0, fmt.Errorf("reading blob %s: %s", id, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	total, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if total <= 0 {
		total = int64(len(data))
	}
	created, _ := time.Parse(time.RFC3339Nano, resp.Header.Get("X-Created"))
	return storedObject{Data: data, Revision: trimETag(resp.Header.Get("ETag")), Created: created}, total, err
}

func (s *service) writeObject(ctx context.Context, id string, value any, condition string) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.blobs+"/blobs/"+id, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Creator", "evaluation")
	if condition == "*" {
		req.Header.Set("If-None-Match", "*")
	} else if condition != "" {
		req.Header.Set("If-Match", quoteETag(condition))
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return "", errPrecondition
	}
	if resp.StatusCode == http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("invalid conditional blob write: %s", strings.TrimSpace(string(body)))
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("writing blob %s: %s", id, resp.Status)
	}
	return trimETag(resp.Header.Get("ETag")), nil
}

func trimETag(v string) string  { return strings.Trim(strings.TrimSpace(v), `"`) }
func quoteETag(v string) string { return `"` + trimETag(v) + `"` }

type blobListing struct {
	Directories []string `json:"directories"`
	Objects     []struct {
		ID string `json:"id"`
	} `json:"objects"`
}

type executionScope struct {
	SchemaVersion     int        `json:"schema_version"`
	ExecutionID       string     `json:"execution_id"`
	RunID             string     `json:"run_id"`
	ParentExecutionID string     `json:"parent_execution_id,omitempty"`
	Workflow          string     `json:"workflow"`
	WorkflowVersion   int        `json:"workflow_version"`
	ArtifactPrefix    string     `json:"artifact_prefix"`
	StartedAt         time.Time  `json:"started_at"`
	ClosedAt          *time.Time `json:"closed_at,omitempty"`
	Status            string     `json:"status"`
}

func (s *service) executionScope(ctx context.Context, executionID string) (executionScope, error) {
	object, err := s.readObject(ctx, "workflow-executions/"+executionID+".json")
	if err != nil {
		return executionScope{}, err
	}
	var scope executionScope
	if json.Unmarshal(object.Data, &scope) != nil || scope.SchemaVersion != 1 || scope.ExecutionID != executionID || scope.RunID == "" || scope.ArtifactPrefix == "" {
		return executionScope{}, errors.New("invalid workflow execution ledger record")
	}
	return scope, nil
}

func (s *service) executionForArtifact(ctx context.Context, runID, artifact string) (executionScope, error) {
	ids, err := s.listObjects(ctx, "workflow-executions")
	if err != nil {
		return executionScope{}, err
	}
	var matches []executionScope
	for _, id := range ids {
		object, e := s.readObject(ctx, id)
		if e != nil {
			return executionScope{}, e
		}
		var scope executionScope
		if json.Unmarshal(object.Data, &scope) == nil && scope.RunID == runID && artifactInScope(artifact, scope.ArtifactPrefix) {
			matches = append(matches, scope)
		}
	}
	if len(matches) == 0 {
		return executionScope{}, errNotFound
	}
	sort.Slice(matches, func(i, j int) bool { return len(matches[i].ArtifactPrefix) > len(matches[j].ArtifactPrefix) })
	if len(matches) > 1 && len(matches[0].ArtifactPrefix) == len(matches[1].ArtifactPrefix) {
		return executionScope{}, errors.New("artifact execution scope is ambiguous")
	}
	return matches[0], nil
}

func artifactInScope(artifact, prefix string) bool {
	return artifact == prefix || strings.HasPrefix(artifact, strings.TrimRight(prefix, "/")+"/")
}

func (s *service) listObjects(ctx context.Context, prefix string) ([]string, error) {
	var listing blobListing
	if err := s.getJSON(ctx, s.blobs+"/list/"+url.PathEscape(prefix), &listing); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	ids := make([]string, 0, len(listing.Objects))
	for _, object := range listing.Objects {
		ids = append(ids, object.ID)
	}
	return ids, nil
}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, out any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("malformed JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("malformed JSON: multiple values")
	}
	return nil
}
