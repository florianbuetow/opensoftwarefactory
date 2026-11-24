package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRecalculationReportsProjectionPendingThenComplete(t *testing.T) {
	f, blob, _ := newFixture(t)
	orchestrator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/measurement-recalculations" {
			writeJSON(w, 202, map[string]string{"workflow_id": "measurement-recalculation/job"})
			return
		}
		http.NotFound(w, r)
	}))
	defer orchestrator.Close()
	occurred := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	zero := int64(0)
	usage := Measurement{SchemaVersion: 1, ID: "recalc-u", Kind: "usage", RunID: runA, ExecutionID: "exec", OccurredAt: occurred, Vendor: "v", Model: "m", Effort: "none", Modes: &map[string]string{}, InputTokens: &zero, CachedInputTokens: &zero, UncachedInputTokens: &zero, OutputTokens: &zero, Status: "reported", Missing: []string{}, RawUsage: json.RawMessage(`{}`)}
	price := Price{ID: "p", Vendor: "v", Model: "m", Effort: "none", Modes: map[string]string{}, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Currency: "EUR", UnitTokens: 1000, InputUncached: "1", InputCached: "1", Output: "1"}
	f.mu.Lock()
	f.objects[measurementObject("usage", usage.ID)], _ = json.Marshal(usage)
	f.objects[pricePrefix+"/p.json"], _ = json.Marshal(price)
	f.mu.Unlock()
	s := &service{blobs: blob.URL, orchestrator: orchestrator.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	code, data := request(t, http.MethodPost, server.URL+"/api/recalculations", `{"measurement_ids":["recalc-u"],"display_currency":"EUR"}`)
	if code != 202 {
		t.Fatalf("start %d %s", code, data)
	}
	var start struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(data, &start)
	batch := `{"display_currency":"EUR","measurement_ids":["recalc-u"],"from":null,"to":null,"limit":10}`
	code, data = request(t, http.MethodPost, server.URL+"/api/recalculations/"+start.ID+"/batch", batch)
	if code != 200 {
		t.Fatalf("batch %d %s", code, data)
	}
	var pending struct {
		Status            string `json:"status"`
		Error             string `json:"error"`
		Done              bool   `json:"done"`
		ProjectionPending bool   `json:"projection_pending"`
	}
	_ = json.Unmarshal(data, &pending)
	if pending.Done || !pending.ProjectionPending || pending.Status != "projection_pending" || pending.Error != "" {
		t.Fatalf("pending %+v", pending)
	}
	firstResponse := string(data)
	code, data = request(t, http.MethodPost, server.URL+"/api/recalculations/"+start.ID+"/batch", batch)
	if code != 200 || string(data) != firstResponse {
		t.Fatalf("lost-response replay code=%d first=%s retry=%s", code, firstResponse, data)
	}
	jobObject, _ := s.readObject(t.Context(), jobPrefix+"/"+start.ID+".json")
	var job recalculationJob
	_ = json.Unmarshal(jobObject.Data, &job)
	_ = s.projectPending(t.Context())
	manifest := projectionManifest{Stream: projectionStream, Hour: occurred.Truncate(time.Hour), Generation: job.Generation, Phase: "complete", UpdatedAt: time.Now().UTC()}
	_, _ = s.writeObject(t.Context(), projectionObject(occurred), manifest, "*")
	_ = s.completeProjectionOperations(t.Context(), occurred.Truncate(time.Hour), job.Generation)
	batch = `{"display_currency":"EUR","measurement_ids":["recalc-u"],"from":null,"to":null,"cursor":"1","limit":10}`
	code, data = request(t, http.MethodPost, server.URL+"/api/recalculations/"+start.ID+"/batch", batch)
	if code != 200 {
		t.Fatalf("finish %d %s", code, data)
	}
	_ = json.Unmarshal(data, &pending)
	if !pending.Done || pending.ProjectionPending || pending.Status != "complete" {
		t.Fatalf("complete %+v", pending)
	}
}

func TestRecalculationGetValidationAndErrorPaths(t *testing.T) {
	f, blob, _ := newFixture(t)
	orchStatus := http.StatusAccepted
	orchBody := `{"workflow_id":"wf-1"}`
	orchestrator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/measurement-recalculations" {
			w.WriteHeader(orchStatus)
			_, _ = w.Write([]byte(orchBody))
			return
		}
		http.NotFound(w, r)
	}))
	defer orchestrator.Close()

	occurred := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	zero := int64(0)
	usage1 := Measurement{SchemaVersion: 1, ID: "u1", Kind: kindUsage, RunID: runA, ExecutionID: "e", OccurredAt: occurred, Vendor: "v", Model: "m", Effort: "none", Modes: &map[string]string{}, InputTokens: &zero, CachedInputTokens: &zero, UncachedInputTokens: &zero, OutputTokens: &zero, Status: statusReported, Missing: []string{}, RawUsage: json.RawMessage(`{}`)}
	usage2 := usage1
	usage2.ID = "u2"
	f.mu.Lock()
	f.objects[measurementObject(kindUsage, "u1")], _ = json.Marshal(usage1)
	f.objects[measurementObject(kindUsage, "u2")], _ = json.Marshal(usage2)
	f.mu.Unlock()

	s := &service{blobs: blob.URL, orchestrator: orchestrator.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Start validation errors.
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations", `{bad`); code != http.StatusBadRequest {
		t.Fatalf("bad JSON start = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations", `{"display_currency":"GBP"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad currency = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations", `{"display_currency":"EUR","from":"2026-11-02T00:00:00Z","to":"2026-11-01T00:00:00Z"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad range = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations", `{"display_currency":"EUR","measurement_ids":["u1","u1"]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate measurement_ids = %d", code)
	}

	// Orchestrator failures produce 503 with start_failed.
	orchStatus = http.StatusInternalServerError
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations", `{"display_currency":"EUR"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("orch 500 = %d", code)
	}
	orchStatus = http.StatusAccepted
	orchBody = `{}`
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations", `{"display_currency":"EUR"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("orch missing workflow_id = %d", code)
	}

	// Successful start and GET recalculation.
	orchBody = `{"workflow_id":"wf-ok"}`
	code, data := request(t, http.MethodPost, srv.URL+"/api/recalculations", `{"display_currency":"EUR","measurement_ids":["u1","u2"]}`)
	if code != http.StatusAccepted {
		t.Fatalf("start = %d %s", code, data)
	}
	var started struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(data, &started)

	code, data = request(t, http.MethodGet, srv.URL+"/api/recalculations/"+started.ID, "")
	if code != http.StatusOK || !json.Valid(data) {
		t.Fatalf("get recalc = %d %s", code, data)
	}
	if got, _ := request(t, http.MethodGet, srv.URL+"/api/recalculations/missing", ""); got != http.StatusNotFound {
		t.Fatalf("missing recalc = %d", got)
	}
	f.mu.Lock()
	f.objects[jobPrefix+"/corrupt.json"] = []byte(`{bad`)
	f.mu.Unlock()
	if got, _ := request(t, http.MethodGet, srv.URL+"/api/recalculations/corrupt", ""); got != http.StatusServiceUnavailable {
		t.Fatalf("corrupt get recalc = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations/corrupt/batch", `{"display_currency":"EUR","limit":1}`); got != http.StatusServiceUnavailable {
		t.Fatalf("corrupt batch = %d", got)
	}

	// Batch validation errors.
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations/"+started.ID+"/batch", `{bad`); got != http.StatusBadRequest {
		t.Fatalf("bad JSON batch = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations/"+started.ID+"/batch", `{"display_currency":"EUR","limit":0}`); got != http.StatusUnprocessableEntity {
		t.Fatalf("bad limit batch = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations/missing/batch", `{"display_currency":"EUR","limit":1}`); got != http.StatusNotFound {
		t.Fatalf("missing job batch = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations/"+started.ID+"/batch", `{"display_currency":"USD","measurement_ids":["u1","u2"],"limit":1}`); got != http.StatusPreconditionFailed {
		t.Fatalf("mismatched currency batch = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations/"+started.ID+"/batch", `{"display_currency":"EUR","measurement_ids":["u1","u2"],"cursor":"abc","limit":1}`); got != http.StatusBadRequest {
		t.Fatalf("bad cursor batch = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/recalculations/"+started.ID+"/batch", `{"display_currency":"EUR","measurement_ids":["u1","u2"],"cursor":"5","limit":1}`); got != http.StatusPreconditionFailed {
		t.Fatalf("wrong offset batch = %d", got)
	}

	// Multi-batch run: limit 1 leaves jobRunning.
	code, data = request(t, http.MethodPost, srv.URL+"/api/recalculations/"+started.ID+"/batch", `{"display_currency":"EUR","measurement_ids":["u1","u2"],"limit":1}`)
	if code != http.StatusOK {
		t.Fatalf("first batch = %d %s", code, data)
	}
	var batchRes recalculationBatchResult
	_ = json.Unmarshal(data, &batchRes)
	if batchRes.Status != jobRunning || batchRes.NextCursor != "1" {
		t.Fatalf("first batch result = %+v", batchRes)
	}

	// Helper coverage for sameStrings and sameTime.
	if sameStrings([]string{"a"}, []string{"a", "b"}) || sameStrings([]string{"a"}, []string{"b"}) {
		t.Fatal("sameStrings false positive")
	}
	t1 := occurred
	t2 := occurred.Add(time.Minute)
	if !sameTime(&t1, &t1) || sameTime(&t1, &t2) || sameTime(&t1, nil) {
		t.Fatal("sameTime mismatch")
	}
}
