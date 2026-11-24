package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLegacyBackfillIsResumableAndKeepsZeroAmbiguous(t *testing.T) {
	f, blob, orch := newFixture(t)
	started := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	f.mu.Lock()
	f.objects["runs/"+runA+"/run.json"], _ = json.Marshal(map[string]any{"trace_id": runA, "execution_id": "legacy-exec", "workflow": "legacy", "workflow_version": 1, "started": started})
	f.objects["runs/"+runA+"/metadata.json"], _ = json.Marshal(map[string]any{"finished": started.Add(time.Second).Format(time.RFC3339Nano), "status": "completed"})
	f.objects["runs/"+runA+"/step.run.json"], _ = json.Marshal(map[string]any{"agent": "legacy/agent@v1", "model": "legacy-model", "started": started, "usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0}})
	f.mu.Unlock()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	code, data := request(t, http.MethodPost, srv.URL+"/api/backfills", `{"run_ids":["`+runA+`"]}`)
	if code != 202 {
		t.Fatalf("start %d %s", code, data)
	}
	var job backfillJob
	_ = json.Unmarshal(data, &job)
	code, data = request(t, http.MethodPost, srv.URL+"/api/backfills/"+job.ID+"/batch", `{"cursor":0,"limit":1}`)
	if code != 200 {
		t.Fatalf("batch %d %s", code, data)
	}
	_ = s.projectPending(t.Context())
	code, _ = request(t, http.MethodPost, srv.URL+"/api/backfills/"+job.ID+"/batch", `{"cursor":0,"limit":1}`)
	if code != 412 {
		t.Fatalf("stale cursor %d", code)
	}
	deadline := time.Now().Add(3 * time.Second)
	var usage Measurement
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for id, raw := range f.objects {
			if strings.HasPrefix(id, usagePrefix+"/") {
				_ = json.Unmarshal(raw, &usage)
			}
		}
		f.mu.Unlock()
		if usage.ID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if usage.ID == "" {
		t.Fatal("usage was not backfilled")
	}
	if usage.InputTokens != nil || usage.OutputTokens != nil || !hasMissingField(usage.Missing, "prompt_tokens") || !hasMissingField(usage.Missing, "completion_tokens") {
		t.Fatalf("legacy zeros were treated as known: %+v", usage)
	}
	if usage.Status != "missing" {
		t.Fatalf("legacy aggregate status %q", usage.Status)
	}
}

func TestLegacyBackfillDoesNotSilentlyCompleteEvidenceGaps(t *testing.T) {
	records := [][]byte{[]byte(`{bad`), []byte(`{"agent":"a","model":"m","events":[{"kind":"model_call","usage":{"prompt_tokens":1}}]}`)}
	for index, record := range records {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			f, blob, orch := newFixture(t)
			f.mu.Lock()
			f.objects["runs/"+runA+"/run.json"], _ = json.Marshal(map[string]any{"trace_id": runA})
			f.objects["runs/"+runA+"/step.run.json"] = record
			f.mu.Unlock()
			s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
			if _, err := s.backfillRun(t.Context(), runA, 1); err == nil {
				t.Fatal("legacy evidence gap was silently skipped")
			}
		})
	}
}

func TestBackfillGetValidationAndResume(t *testing.T) {
	f, blob, orch := newFixture(t)
	started := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	f.mu.Lock()
	f.objects["runs/"+runA+"/run.json"], _ = json.Marshal(map[string]any{"trace_id": runA, "execution_id": "exec-a", "workflow": "wf", "workflow_version": 1, "started": started})
	f.objects["runs/"+runA+"/step.run.json"], _ = json.Marshal(map[string]any{
		"agent": "a@v1",
		"events": []map[string]any{
			{"kind": "other"},
			{"kind": legacyModelCall, "started": started, "model": "m", "usage": map[string]any{"input_tokens": 5, "output_tokens": 3}},
		},
	})
	f.objects["runs/"+runB+"/run.json"], _ = json.Marshal(map[string]any{"trace_id": runB, "started": started})
	f.objects["runs/"+runB+"/step.run.json"], _ = json.Marshal(map[string]any{"agent": "a@v1", "model": "m", "started": started, "usage": map[string]any{"prompt_tokens": 10}})
	f.mu.Unlock()

	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Validation errors on startBackfill and backfillBatch.
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/backfills", `{bad`); code != http.StatusBadRequest {
		t.Fatalf("bad JSON start = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/backfills", `{"run_ids":["a","a"]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate run_ids = %d", code)
	}
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/backfills/missing", ""); code != http.StatusNotFound {
		t.Fatalf("missing backfill = %d", code)
	}

	// Start with empty run_ids discovers all runs in fixture.
	code, data := request(t, http.MethodPost, srv.URL+"/api/backfills", `{}`)
	if code != http.StatusAccepted {
		t.Fatalf("discover start = %d %s", code, data)
	}
	var discovered backfillJob
	if err := json.Unmarshal(data, &discovered); err != nil {
		t.Fatal(err)
	}
	if len(discovered.RunIDs) != 4 {
		t.Fatalf("discovered runs = %v", discovered.RunIDs)
	}

	// Get backfill returns the stored job.
	code, data = request(t, http.MethodGet, srv.URL+"/api/backfills/"+discovered.ID, "")
	if code != http.StatusOK {
		t.Fatalf("get backfill = %d %s", code, data)
	}

	// Corrupt backfill returns 503.
	f.mu.Lock()
	f.objects[backfillPrefix+"/corrupt.json"] = []byte(`{bad`)
	f.mu.Unlock()
	if got, _ := request(t, http.MethodGet, srv.URL+"/api/backfills/corrupt", ""); got != http.StatusServiceUnavailable {
		t.Fatalf("corrupt get backfill = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/backfills/corrupt/batch", `{"cursor":0,"limit":1}`); got != http.StatusServiceUnavailable {
		t.Fatalf("corrupt batch = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/backfills/"+discovered.ID+"/batch", `{bad`); got != http.StatusBadRequest {
		t.Fatalf("bad JSON batch = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/backfills/"+discovered.ID+"/batch", `{"cursor":0,"limit":0}`); got != http.StatusUnprocessableEntity {
		t.Fatalf("bad limit batch = %d", got)
	}
	f.mu.Lock()
	delete(f.objects, backfillPrefix+"/corrupt.json")
	delete(f.objects, backfillPrefix+"/"+discovered.ID+".json")
	f.mu.Unlock()

	// Test resumeBackfills: complete job is skipped, pending job advances to projection_pending then complete.
	doneJob := backfillJob{ID: "0-done", Status: jobComplete, RunIDs: []string{}}
	activeJob := backfillJob{ID: "1-active", Status: jobPending, RunIDs: []string{runA, runB}, Generation: 10}
	f.mu.Lock()
	f.objects[backfillPrefix+"/0-done.json"], _ = json.Marshal(doneJob)
	f.objects[backfillPrefix+"/1-active.json"], _ = json.Marshal(activeJob)
	f.mu.Unlock()

	if err := s.resumeBackfills(t.Context()); err != nil {
		t.Fatalf("resume active: %v", err)
	}
	code, data = request(t, http.MethodGet, srv.URL+"/api/backfills/1-active", "")
	if code != http.StatusOK {
		t.Fatalf("get resumed = %d", code)
	}
	var resumed backfillJob
	_ = json.Unmarshal(data, &resumed)
	if resumed.Status != jobProjectionPending || resumed.Cursor != 2 || resumed.Created == 0 {
		t.Fatalf("resumed = %+v", resumed)
	}

	// Mark all operations complete and resume again -> jobComplete.
	f.mu.Lock()
	for id, raw := range f.objects {
		if strings.HasPrefix(id, operationPrefix+"/") {
			var op mutationOperation
			if json.Unmarshal(raw, &op) == nil {
				op.Phase = opComplete
				f.objects[id], _ = json.Marshal(op)
			}
		}
	}
	f.mu.Unlock()
	if err := s.resumeBackfills(t.Context()); err != nil {
		t.Fatalf("resume complete: %v", err)
	}
	code, data = request(t, http.MethodGet, srv.URL+"/api/backfills/1-active", "")
	_ = json.Unmarshal(data, &resumed)
	if code != http.StatusOK || resumed.Status != jobComplete {
		t.Fatalf("completed = %d %+v", code, resumed)
	}

	// Resume a job with a legacy gap marks it failed; transient error marks retrying.
	f.mu.Lock()
	f.objects["runs/"+runC+"/step.run.json"] = []byte(`{bad`)
	f.objects[backfillPrefix+"/2-gap.json"], _ = json.Marshal(backfillJob{ID: "2-gap", Status: jobPending, RunIDs: []string{runC}, Generation: 11})
	f.mu.Unlock()
	if err := s.resumeBackfills(t.Context()); err != nil {
		t.Fatalf("resume gap: %v", err)
	}
	code, data = request(t, http.MethodGet, srv.URL+"/api/backfills/2-gap", "")
	_ = json.Unmarshal(data, &resumed)
	if code != http.StatusOK || resumed.Status != jobFailed {
		t.Fatalf("gap job = %d %+v", code, resumed)
	}

	f.mu.Lock()
	f.objects[backfillPrefix+"/3-missing.json"], _ = json.Marshal(backfillJob{ID: "3-missing", Status: jobPending, RunIDs: []string{"nonexistent-run"}, Generation: 12})
	f.mu.Unlock()
	if err := s.resumeBackfills(t.Context()); err != nil {
		t.Fatalf("resume retrying: %v", err)
	}
	code, data = request(t, http.MethodGet, srv.URL+"/api/backfills/3-missing", "")
	_ = json.Unmarshal(data, &resumed)
	if code != http.StatusOK || resumed.Status != jobRetrying {
		t.Fatalf("retrying job = %d %+v", code, resumed)
	}

	// applyLegacyUsage edge cases: empty raw usage and malformed raw usage.
	var emptyUsage Measurement
	applyLegacyUsage(&emptyUsage, nil, false)
	if !hasMissingField(emptyUsage.Missing, "raw_usage") {
		t.Fatalf("empty usage missing = %v", emptyUsage.Missing)
	}
	var badUsage Measurement
	applyLegacyUsage(&badUsage, json.RawMessage(`{bad`), false)
	if badUsage.InputTokens != nil {
		t.Fatalf("bad usage parsed tokens")
	}

	// enqueueBackfillMeasurement idempotent skip and conflict.
	m := Measurement{SchemaVersion: 1, ID: "dup-m", Kind: kindExecution, RunID: runA, ExecutionID: "e", OccurredAt: started, Status: statusMissing, Missing: []string{}}
	f.mu.Lock()
	f.objects[measurementObject(kindExecution, m.ID)], _ = json.Marshal(m)
	f.mu.Unlock()
	if n, err := s.enqueueBackfillMeasurement(t.Context(), &m, 1); err != nil || n != 0 {
		t.Fatalf("idempotent enqueue = %d %v", n, err)
	}
	m.Status = statusDerived
	if _, err := s.enqueueBackfillMeasurement(t.Context(), &m, 1); err == nil {
		t.Fatal("conflicting enqueue succeeded")
	}
}
