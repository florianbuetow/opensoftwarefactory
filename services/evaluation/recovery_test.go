package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIntentRecoveryAppliesCanonicalBeforeProjection(t *testing.T) {
	_, blob, orch := newFixture(t)
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	occurred := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := Measurement{SchemaVersion: 1, ID: "recover", Kind: "execution", RunID: runA, ExecutionID: "exec", OccurredAt: occurred, Status: "missing", Missing: []string{"duration_ms/unavailable"}}
	raw, _ := json.Marshal(m)
	op := mutationOperation{SchemaVersion: 1, ID: "recover-op", ObjectID: measurementObject("execution", m.ID), MeasurementID: m.ID, ExpectedRevision: "*", Replacement: raw, AffectedHour: occurred.Truncate(time.Hour), Generation: 1, Phase: "intent", CreatedAt: occurred, UpdatedAt: occurred}
	if _, err := s.writeObject(t.Context(), operationPrefix+"/"+op.ID+".json", op, "*"); err != nil {
		t.Fatal(err)
	}
	_ = s.projectPending(t.Context())
	if _, err := s.readObject(t.Context(), op.ObjectID); err != nil {
		t.Fatalf("canonical was not recovered: %v", err)
	}
	stored, _ := s.readObject(t.Context(), operationPrefix+"/"+op.ID+".json")
	_ = json.Unmarshal(stored.Data, &op)
	if op.Phase != "projection_pending" {
		t.Fatalf("phase %q", op.Phase)
	}
}

func TestConflictedIntentIsNeverProjectedOrCompleted(t *testing.T) {
	f, blob, orch := newFixture(t)
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f.mu.Lock()
	f.objects[measurementObject("execution", "same")] = []byte(`{"id":"other"}`)
	f.mu.Unlock()
	replacement := json.RawMessage(`{"id":"same"}`)
	op := mutationOperation{SchemaVersion: 1, ID: "conflict-op", ObjectID: measurementObject("execution", "same"), MeasurementID: "same", ExpectedRevision: "*", Replacement: replacement, AffectedHour: hour, Generation: 3, Phase: "intent", CreatedAt: hour, UpdatedAt: hour}
	if _, err := s.writeObject(t.Context(), operationPrefix+"/"+op.ID+".json", op, "*"); err != nil {
		t.Fatal(err)
	}
	_ = s.projectPending(t.Context())
	object, _ := s.readObject(t.Context(), operationPrefix+"/"+op.ID+".json")
	_ = json.Unmarshal(object.Data, &op)
	if op.Phase != "conflict" {
		t.Fatalf("phase %q", op.Phase)
	}
	if _, err := s.readObject(t.Context(), projectionObject(hour)); !errors.Is(err, errNotFound) {
		t.Fatalf("conflicted intent created projection: %v", err)
	}
}

func TestCalculationPendingRetriesDependencyFailure(t *testing.T) {
	f, blob, orch := newFixture(t)
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	price := Price{Vendor: "v", Model: "m", Effort: "none", Modes: map[string]string{}, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Currency: "EUR", UnitTokens: 1000, InputUncached: "1", InputCached: "1", Output: "1"}
	f.mu.Lock()
	f.objects[price.Key().objectID()], _ = json.Marshal(price)
	f.failPriceList = true
	f.mu.Unlock()
	zero := int64(0)
	m := Measurement{SchemaVersion: 1, ID: "usage-retry", Kind: "usage", RunID: runA, ExecutionID: "exec", OccurredAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), Vendor: "v", Model: "m", Effort: "none", Modes: &map[string]string{}, InputTokens: &zero, CachedInputTokens: &zero, UncachedInputTokens: &zero, OutputTokens: &zero, Status: "reported", Missing: []string{}, RawUsage: json.RawMessage(`{}`)}
	raw, _ := json.Marshal(m)
	f.mu.Lock()
	f.objects[measurementObject("usage", m.ID)] = raw
	f.mu.Unlock()
	op := mutationOperation{SchemaVersion: 1, ID: "calc-op", ObjectID: measurementObject("usage", m.ID), MeasurementID: m.ID, ExpectedRevision: "*", Replacement: raw, AffectedHour: m.OccurredAt.Truncate(time.Hour), Generation: 2, Phase: "calculation_pending", CreatedAt: m.OccurredAt, UpdatedAt: m.OccurredAt}
	if _, err := s.writeObject(t.Context(), operationPrefix+"/"+op.ID+".json", op, "*"); err != nil {
		t.Fatal(err)
	}
	_ = s.projectPending(t.Context())
	stored, _ := s.readObject(t.Context(), operationPrefix+"/"+op.ID+".json")
	_ = json.Unmarshal(stored.Data, &op)
	if op.Phase != "calculation_pending" || op.Error == "" {
		t.Fatalf("failure was not retained %+v", op)
	}
	f.mu.Lock()
	f.failPriceList = false
	f.mu.Unlock()
	_ = s.projectPending(t.Context())
	if _, err := s.readObject(t.Context(), costPrefix+"/"+m.ID+".json"); err != nil {
		t.Fatalf("cost retry failed: %v", err)
	}
}

func TestMeasurementHandlerOnlyEnqueuesProjection(t *testing.T) {
	_, blob, orch := newFixture(t)
	remoteCalls := 0
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { remoteCalls++; http.Error(w, "unexpected", 500) }))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	body := `{"schema_version":1,"id":"queued","kind":"execution","run_id":"run","execution_id":"exec","occurred_at":"2026-11-01T12:00:00Z","status":"missing","missing":["duration_ms/unavailable"]}`
	code, data := conditionalPut(t, server.URL+"/api/measurements/queued", body, "If-None-Match", "*")
	if code != 200 {
		t.Fatalf("put %d %s", code, data)
	}
	if remoteCalls != 0 {
		t.Fatalf("handler made %d remote projection calls", remoteCalls)
	}
}
