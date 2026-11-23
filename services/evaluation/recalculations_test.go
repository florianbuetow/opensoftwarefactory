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
