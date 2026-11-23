package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const backfillPrefix = "measurement-backfills"

type backfillJob struct {
	ID         string    `json:"id"`
	Revision   string    `json:"revision,omitempty"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	RunIDs     []string  `json:"run_ids"`
	Cursor     int       `json:"cursor"`
	Processed  int       `json:"processed"`
	Created    int       `json:"created"`
	Generation int64     `json:"generation"`
}
type backfillStart struct {
	RunIDs []string `json:"run_ids"`
}
type backfillBatch struct {
	Cursor int `json:"cursor"`
	Limit  int `json:"limit"`
}
type legacyGapError struct{ error }

func (s *service) backfillRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/backfills", s.startBackfill)
	mux.HandleFunc("GET /api/backfills/{id}", s.getBackfill)
	mux.HandleFunc("POST /api/backfills/{id}/batch", s.backfillBatch)
}

func (s *service) startBackfill(w http.ResponseWriter, r *http.Request) {
	var in backfillStart
	if err := decodeStrictJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	ids := in.RunIDs
	if len(ids) == 0 {
		found, err := s.runIDs(r.Context())
		if err != nil {
			s.dependencyFailure(w, err)
			return
		}
		for id := range found {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			s.fail(w, 422, errors.New("run_ids must be unique"))
			return
		}
		seen[id] = true
	}
	generation, err := s.allocateGeneration(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	now := time.Now().UTC()
	job := backfillJob{ID: uuid.NewString(), Status: jobPending, RunIDs: ids, Generation: generation, CreatedAt: now, UpdatedAt: now}
	rev, err := s.writeObject(r.Context(), backfillPrefix+"/"+job.ID+".json", job, "*")
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	job.Revision = rev
	writeJSON(w, 202, job)
}

func (s *service) getBackfill(w http.ResponseWriter, r *http.Request) {
	object, err := s.readObject(r.Context(), backfillPrefix+"/"+r.PathValue("id")+".json")
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("backfill not found"))
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	var job backfillJob
	if json.Unmarshal(object.Data, &job) != nil {
		s.dependencyFailure(w, errors.New("stored backfill invalid"))
		return
	}
	job.Revision = object.Revision
	writeJSON(w, 200, job)
}

func (s *service) backfillBatch(w http.ResponseWriter, r *http.Request) {
	var in backfillBatch
	if err := decodeStrictJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	if in.Limit < 1 || in.Limit > 100 {
		s.fail(w, 422, errors.New("limit must be 1..100"))
		return
	}
	object, err := s.readObject(r.Context(), backfillPrefix+"/"+r.PathValue("id")+".json")
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	var job backfillJob
	if json.Unmarshal(object.Data, &job) != nil {
		s.dependencyFailure(w, errors.New("stored backfill invalid"))
		return
	}
	if in.Cursor != job.Cursor {
		s.fail(w, 412, errors.New("backfill cursor changed"))
		return
	}
	start := in.Cursor
	end := start + in.Limit
	if end > len(job.RunIDs) {
		end = len(job.RunIDs)
	}
	created := 0
	for _, runID := range job.RunIDs[start:end] {
		n, e := s.backfillRun(r.Context(), runID, job.Generation)
		if e != nil {
			var gap legacyGapError
			if errors.As(e, &gap) {
				job.Status = jobFailed
			} else {
				job.Status = jobRetrying
			}
			job.Error = e.Error()
			job.UpdatedAt = time.Now().UTC()
			_, _ = s.writeObject(r.Context(), backfillPrefix+"/"+job.ID+".json", job, object.Revision)
			writeJSON(w, 200, map[string]any{"processed": 0, "created": created, "next_cursor": job.Cursor, "done": false, "status": job.Status, "error": job.Error})
			return
		}
		created += n
	}
	job.Cursor = end
	job.Processed = end
	job.Created += created
	job.Error = ""
	done := false
	if end == len(job.RunIDs) {
		done, _ = s.backfillGenerationComplete(r.Context(), job.Generation)
		if done {
			job.Status = jobComplete
		} else {
			job.Status = jobProjectionPending
		}
	} else {
		job.Status = jobRunning
	}
	job.UpdatedAt = time.Now().UTC()
	rev, err := s.writeObject(r.Context(), backfillPrefix+"/"+job.ID+".json", job, object.Revision)
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	job.Revision = rev
	writeJSON(w, 200, map[string]any{"processed": end - start, "created": created, "next_cursor": end, "done": done, "status": job.Status})
}

type legacyRun struct {
	Started            time.Time `json:"started"`
	TraceID            string    `json:"trace_id"`
	ExecutionID        string    `json:"execution_id"`
	ParentExecutionID  string    `json:"parent_execution_id"`
	DistributedTraceID string    `json:"distributed_trace_id"`
	Workflow           string    `json:"workflow"`
	WorkflowVersion    int       `json:"workflow_version"`
}

// legacyEvent is one model call in a run record written before usage
// measurements existed. An aggregate stands in for a record that kept only
// its total usage.
type legacyEvent struct {
	Kind    string          `json:"kind"`
	Started time.Time       `json:"started"`
	Model   string          `json:"model"`
	TraceID string          `json:"trace_id"`
	SpanID  string          `json:"span_id"`
	Usage   json.RawMessage `json:"usage"`
	Turn    int             `json:"turn"`
}

// The kinds of legacy event that carry usage.
const (
	legacyModelCall = "model_call"
	legacyAggregate = "legacy_aggregate"
)

func (s *service) backfillRun(ctx context.Context, runID string, generation int64) (int, error) {
	var run legacyRun
	if err := s.getJSON(ctx, s.blobs, "/blobs/runs/"+runID+"/run.json", &run); err != nil {
		return 0, err
	}
	if run.TraceID == "" {
		run.TraceID = runID
	}
	created := 0
	if run.ExecutionID != "" && !run.Started.IsZero() {
		var metadata struct {
			Finished string `json:"finished"`
			Status   string `json:"status"`
		}
		_ = s.getJSON(ctx, s.blobs, "/blobs/runs/"+runID+"/metadata.json", &metadata)
		measurement := Measurement{SchemaVersion: 1, ID: hashID("legacy-execution", runID, run.ExecutionID), Kind: kindExecution, RunID: runID, ExecutionID: run.ExecutionID, ParentExecutionID: run.ParentExecutionID, Workflow: run.Workflow, TraceID: run.DistributedTraceID, OccurredAt: run.Started, Status: statusMissing, Missing: []string{}}
		if run.WorkflowVersion > 0 {
			measurement.WorkflowVersion = &run.WorkflowVersion
		}
		if finished, err := time.Parse(time.RFC3339Nano, metadata.Finished); err == nil {
			duration := finished.Sub(run.Started).Milliseconds()
			if duration >= 0 {
				measurement.DurationMS = &duration
				measurement.Status = statusDerived
			}
		}
		if measurement.DurationMS == nil {
			measurement.Missing = append(measurement.Missing, "duration_ms/legacy finish unavailable")
		}
		n, err := s.enqueueBackfillMeasurement(ctx, &measurement, generation)
		if err != nil {
			return created, err
		}
		created += n
	}
	var listing struct {
		Objects []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	if err := s.getJSON(ctx, s.blobs, "/trace/"+runID, &listing); err != nil {
		return created, err
	}
	for _, item := range listing.Objects {
		if !strings.HasSuffix(item.ID, ".run.json") || strings.HasSuffix(item.ID, "/run.json") {
			continue
		}
		object, err := s.readObject(ctx, item.ID)
		if err != nil {
			return created, err
		}
		var record struct {
			Agent   string          `json:"agent"`
			Model   string          `json:"model"`
			Effort  string          `json:"effort"`
			Started time.Time       `json:"started"`
			Usage   json.RawMessage `json:"usage"`
			Events  []legacyEvent   `json:"events"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(object.Data)))
		decoder.UseNumber()
		if err := decoder.Decode(&record); err != nil {
			return created, legacyGapError{fmt.Errorf("legacy record %s is malformed: %w", item.ID, err)}
		}
		if len(record.Events) == 0 && len(record.Usage) > 0 {
			record.Events = append(record.Events, legacyEvent{Kind: legacyAggregate, Started: record.Started, Model: record.Model, Usage: record.Usage})
		}
		for index := range record.Events {
			event := &record.Events[index]
			if event.Kind != legacyModelCall && event.Kind != legacyAggregate {
				continue
			}
			measurement := legacyUsageMeasurement(&run, item.ID, record.Agent, record.Effort, event, index)
			if measurement.OccurredAt.IsZero() {
				return created, legacyGapError{fmt.Errorf("legacy record %s event %d has no source timestamp", item.ID, index)}
			}
			n, err := s.enqueueBackfillMeasurement(ctx, &measurement, generation)
			if err != nil {
				return created, err
			}
			created += n
		}
	}
	return created, nil
}

func legacyUsageMeasurement(run *legacyRun, artifact, agent, effort string, event *legacyEvent, index int) Measurement {
	occurred := event.Started
	if occurred.IsZero() {
		occurred = run.Started
	}
	missing := []string{"vendor/legacy evidence unavailable", "modes/legacy evidence unavailable", "actual_upstream_attempt_id/legacy identity derived"}
	if run.ExecutionID == "" {
		missing = append(missing, "execution_id/legacy attribution unavailable")
	}
	if effort == "" {
		missing = append(missing, "effort/legacy evidence unavailable")
	}
	m := Measurement{SchemaVersion: 1, ID: hashID("legacy-usage", artifact, strconv.Itoa(index)), Kind: kindUsage, RunID: run.TraceID, ExecutionID: run.ExecutionID, ParentExecutionID: run.ParentExecutionID, Agent: agent, OccurredAt: occurred, Model: event.Model, Effort: effort, TraceID: event.TraceID, SpanID: event.SpanID, Status: statusMissing, Missing: missing, RawUsage: event.Usage}
	if m.Model == "" {
		m.Missing = append(m.Missing, "model/legacy evidence unavailable")
	}
	applyLegacyUsage(&m, event.Usage, event.Kind == legacyAggregate)
	return m
}

func applyLegacyUsage(m *Measurement, raw json.RawMessage, aggregate bool) {
	if len(raw) == 0 {
		m.Missing = append(m.Missing, "raw_usage/legacy evidence unavailable", "input_tokens/legacy evidence unavailable", "output_tokens/legacy evidence unavailable", "cached_input_tokens/legacy evidence unavailable")
		return
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return
	}
	known := false
	read := func(keys ...string) *int64 {
		for _, key := range keys {
			if value, ok := values[key]; ok {
				var number json.Number
				if json.Unmarshal(value, &number) == nil {
					n, err := number.Int64()
					if err == nil {
						if aggregate && n == 0 {
							m.Missing = append(m.Missing, key+"/legacy zero ambiguous")
							return nil
						}
						known = true
						return &n
					}
				}
			}
		}
		return nil
	}
	m.InputTokens = read("prompt_tokens", "input_tokens")
	m.OutputTokens = read("completion_tokens", "output_tokens")
	m.CachedInputTokens = read("cached_input_tokens")
	m.ReasoningTokens = read("reasoning_tokens")
	if known {
		if aggregate {
			m.Status = statusDerived
		} else {
			m.Status = statusReported
		}
	}
}

func (s *service) enqueueBackfillMeasurement(ctx context.Context, m *Measurement, generation int64) (int, error) {
	objectID := measurementObject(m.Kind, m.ID)
	if existing, err := s.readObject(ctx, objectID); err == nil {
		raw, _ := json.Marshal(m)
		if jsonEquivalent(existing.Data, raw) {
			return 0, nil
		}
		return 0, errPrecondition
	} else if !errors.Is(err, errNotFound) {
		return 0, err
	}
	raw, _ := json.Marshal(m)
	op := mutationOperation{SchemaVersion: 1, ID: uuid.NewString(), ObjectID: objectID, MeasurementID: m.ID, ExpectedRevision: "*", Replacement: raw, AffectedHour: m.OccurredAt.UTC().Truncate(time.Hour), Generation: generation, Phase: opIntent, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if _, err := s.writeObject(ctx, operationPrefix+"/"+op.ID+".json", op, "*"); err != nil {
		return 0, err
	}
	return 1, nil
}

func (s *service) backfillGenerationComplete(ctx context.Context, generation int64) (bool, error) {
	ids, err := s.listObjects(ctx, operationPrefix)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		object, e := s.readObject(ctx, id)
		if e != nil {
			return false, e
		}
		var op mutationOperation
		if json.Unmarshal(object.Data, &op) == nil && op.Generation == generation && op.Phase != opComplete && op.Phase != opCancelled {
			return false, nil
		}
	}
	return true, nil
}

// resumeBackfills advances one bounded durable batch. API-created imports are
// therefore recoverable across client disconnects and process restarts.
func (s *service) resumeBackfills(ctx context.Context) error {
	ids, err := s.listObjects(ctx, backfillPrefix)
	if err != nil {
		return err
	}
	sort.Strings(ids)
	for _, id := range ids {
		object, e := s.readObject(ctx, id)
		if e != nil {
			return e
		}
		var job backfillJob
		if json.Unmarshal(object.Data, &job) != nil || job.Status == jobComplete || job.Status == jobFailed || job.Status == jobCancelled {
			continue
		}
		start := job.Cursor
		end := start + 10
		if end > len(job.RunIDs) {
			end = len(job.RunIDs)
		}
		created := 0
		for _, runID := range job.RunIDs[start:end] {
			n, runErr := s.backfillRun(ctx, runID, job.Generation)
			if runErr != nil {
				var gap legacyGapError
				if errors.As(runErr, &gap) {
					job.Status = jobFailed
				} else {
					job.Status = jobRetrying
				}
				job.Error = runErr.Error()
				job.UpdatedAt = time.Now().UTC()
				_, _ = s.writeObject(ctx, id, job, object.Revision)
				return nil
			}
			created += n
		}
		job.Cursor = end
		job.Processed = end
		job.Created += created
		job.Error = ""
		if end < len(job.RunIDs) {
			job.Status = jobRunning
		} else if complete, _ := s.backfillGenerationComplete(ctx, job.Generation); complete {
			job.Status = jobComplete
		} else {
			job.Status = jobProjectionPending
		}
		job.UpdatedAt = time.Now().UTC()
		_, e = s.writeObject(ctx, id, job, object.Revision)
		return e
	}
	return nil
}
