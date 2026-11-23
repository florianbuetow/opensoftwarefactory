package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// searchPath is the OpenObserve endpoint the projector queries.
const searchPath = "/api/default/_search"

func TestProjectorWaitsForRemoteDeleteStatusBeforeReplay(t *testing.T) {
	f, blob, orch := newFixture(t)
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := Measurement{SchemaVersion: 1, ID: "u1", Kind: "usage", RunID: runA, ExecutionID: "e1", OccurredAt: hour.Add(time.Minute), Vendor: "v", Model: "m", Effort: "none", Modes: &map[string]string{}, Status: "reported", Missing: []string{}}
	f.mu.Lock()
	f.objects[measurementObject("usage", "u1")], _ = json.Marshal(m)
	f.mu.Unlock()
	var mu sync.Mutex
	deletes, statuses, ingests, searches := 0, 0, 0, 0
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/schema"):
			writeJSON(w, 200, map[string]any{"stats": map[string]int{"doc_num": 1}})
		case r.Method == http.MethodDelete:
			deletes++
			writeJSON(w, 200, map[string]string{"id": "delete-1"})
		case strings.Contains(r.URL.Path, "/status/"):
			statuses++
			state := "Pending"
			if statuses > 1 {
				state = "Completed"
			}
			writeJSON(w, 200, map[string]any{"id": "delete-1", "status": state, "metadata": []any{}})
		case strings.HasSuffix(r.URL.Path, "/_json"):
			ingests++
			writeJSON(w, 200, map[string]any{"code": 200, "status": []map[string]int{{"successful": 1, "failed": 0}}})
		case strings.Contains(r.URL.Path, "/_search_partition"):
			writeJSON(w, 200, map[string]any{"records": 1, "file_num": 1})
		case strings.Contains(r.URL.Path, "/_search"):
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "COUNT(*)") {
				writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"record_count": 1}}, "total": 1, "is_partial": false})
				return
			}
			searches++
			if searches == 1 {
				writeJSON(w, 200, map[string]any{"hits": []any{}, "total": 0, "is_partial": false})
			} else {
				writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"projection_id": "usage/u1", "generation": 2}}, "total": 1, "is_partial": false})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	manifest := projectionManifest{Stream: projectionStream, Hour: hour, Generation: 1, Phase: "complete", UpdatedAt: time.Now().UTC()}
	if _, err := s.writeObject(t.Context(), projectionObject(hour), manifest, "*"); err != nil {
		t.Fatal(err)
	}
	complete, err := s.advanceProjection(t.Context(), hour, 2)
	if err != nil || complete {
		t.Fatalf("delete request complete=%v err=%v", complete, err)
	}
	complete, err = s.advanceProjection(t.Context(), hour, 2)
	if err != nil || complete {
		t.Fatalf("pending status complete=%v err=%v", complete, err)
	}
	complete, err = s.advanceProjection(t.Context(), hour, 2)
	if err != nil || !complete {
		t.Fatalf("completed replay complete=%v err=%v", complete, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deletes != 1 || statuses != 2 || ingests != 1 {
		t.Fatalf("remote calls delete=%d status=%d ingest=%d", deletes, statuses, ingests)
	}
}

func TestProjectorFailsClosedWhenDeleteIdentityIsAmbiguous(t *testing.T) {
	_, blob, orch := newFixture(t)
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	deletes := 0
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/schema") {
			writeJSON(w, 200, map[string]any{"stats": map[string]int{"doc_num": 1}})
			return
		}
		if strings.Contains(r.URL.Path, "/_search_partition") {
			writeJSON(w, 200, map[string]int{"records": 0})
			return
		}
		if r.URL.Path == searchPath {
			writeJSON(w, 200, map[string]any{"hits": []map[string]int{{"record_count": 0}}, "total": 1, "is_partial": false})
			return
		}
		if r.Method == http.MethodDelete {
			deletes++
			writeJSON(w, 200, map[string]string{"message": "accepted without id"})
			return
		}
		http.NotFound(w, r)
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	manifest := projectionManifest{Stream: projectionStream, Hour: hour, Generation: 1, Phase: "complete", UpdatedAt: time.Now().UTC()}
	if _, err := s.writeObject(t.Context(), projectionObject(hour), manifest, "*"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		complete, err := s.advanceProjection(t.Context(), hour, 2)
		if err != nil || complete {
			t.Fatalf("attempt %d complete=%v err=%v", i, complete, err)
		}
	}
	if deletes != 1 {
		t.Fatalf("ambiguous delete was repeated %d times", deletes)
	}
	object, err := s.readObject(t.Context(), projectionObject(hour))
	if err != nil {
		t.Fatal(err)
	}
	var got projectionManifest
	_ = json.Unmarshal(object.Data, &got)
	if got.Phase != "delete_ambiguous" {
		t.Fatalf("phase %q", got.Phase)
	}
}

func TestProjectorPersistsInflightAndUsesExactHourBoundary(t *testing.T) {
	_, blob, orch := newFixture(t)
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	deletes := 0
	var start, end string
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/schema") {
			writeJSON(w, 200, map[string]any{"stats": map[string]int{"doc_num": 0}})
			return
		}
		if strings.Contains(r.URL.Path, "/_search_partition") {
			writeJSON(w, 200, map[string]int{"records": 0})
			return
		}
		if r.URL.Path == searchPath {
			writeJSON(w, 200, map[string]any{"hits": []map[string]int{{"record_count": 0}}, "total": 1, "is_partial": false})
			return
		}
		if r.Method == http.MethodDelete {
			deletes++
			start = r.URL.Query().Get("start")
			end = r.URL.Query().Get("end")
			writeJSON(w, 200, map[string]string{"id": "job"})
			return
		}
		http.NotFound(w, r)
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	complete, err := s.advanceProjection(t.Context(), hour, 1)
	if err != nil || complete {
		t.Fatalf("complete=%v err=%v", complete, err)
	}
	if deletes != 1 || start != strconv.FormatInt(hour.UnixMicro(), 10) || end != strconv.FormatInt(hour.Add(time.Hour).UnixMicro(), 10) {
		t.Fatalf("delete calls=%d range=%s..%s", deletes, start, end)
	}
	object, _ := s.readObject(t.Context(), projectionObject(hour))
	var manifest projectionManifest
	_ = json.Unmarshal(object.Data, &manifest)
	if manifest.Phase != "delete_requested" || manifest.DeleteJobID != "job" {
		t.Fatalf("manifest %+v", manifest)
	}
}

func TestInterruptedRemoteSubmissionBecomesAmbiguousWithoutRetry(t *testing.T) {
	_, blob, orch := newFixture(t)
	remote := 0
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { remote++; http.Error(w, "must not be called", 500) }))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, phase := range []string{"delete_submitting", "ingestion_submitting"} {
		manifest := projectionManifest{Stream: projectionStream, Hour: hour, Generation: 1, Phase: phase, UpdatedAt: time.Now().UTC()}
		id := projectionObject(hour)
		if current, err := s.readObject(t.Context(), id); err == nil {
			_, _ = s.writeObject(t.Context(), id, manifest, current.Revision)
		} else {
			_, _ = s.writeObject(t.Context(), id, manifest, "*")
		}
		_, _ = s.advanceProjection(t.Context(), hour, 1)
		object, _ := s.readObject(t.Context(), id)
		_ = json.Unmarshal(object.Data, &manifest)
		want := map[string]string{"delete_submitting": "delete_ambiguous", "ingestion_submitting": "ingestion_ambiguous"}[phase]
		if manifest.Phase != want {
			t.Fatalf("%s -> %s", phase, manifest.Phase)
		}
	}
	if remote != 0 {
		t.Fatalf("interrupted submissions retried remotely %d times", remote)
	}
}

func TestIngestionHTTP200WithFailedRowsIsNotComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": []map[string]int{{"successful": 1, "failed": 1}}})
	}))
	defer server.Close()
	s := &service{openobserve: server.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient}
	definitive, err := s.ingestProjection(t.Context(), []map[string]any{{"id": 1}, {"id": 2}})
	var partial partialIngestError
	if err == nil || !definitive || !errors.As(err, &partial) {
		t.Fatalf("partial ingest definitive=%v err=%v", definitive, err)
	}
}

func TestDefinitivePartialIngestRestartsWholeHourDeletion(t *testing.T) {
	f, blob, orch := newFixture(t)
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m := Measurement{SchemaVersion: 1, ID: "partial", Kind: "execution", RunID: runA, ExecutionID: "exec", OccurredAt: hour, Status: "missing", Missing: []string{"duration_ms/unavailable"}}
	f.mu.Lock()
	f.objects[measurementObject("execution", m.ID)], _ = json.Marshal(m)
	f.mu.Unlock()
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_json") {
			writeJSON(w, 200, map[string]any{"status": []map[string]int{{"successful": 1, "failed": 1}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	chunks, count, err := s.freezeProjectionSnapshot(t.Context(), hour, 1, "partial-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	manifest := projectionManifest{Stream: projectionStream, Hour: hour, Generation: 1, Phase: "replay_ready", SnapshotID: "partial-snapshot", SnapshotChunks: chunks, SnapshotCount: count, UpdatedAt: time.Now().UTC()}
	_, _ = s.writeObject(t.Context(), projectionObject(hour), manifest, "*")
	_, _ = s.advanceProjection(t.Context(), hour, 1)
	object, _ := s.readObject(t.Context(), projectionObject(hour))
	_ = json.Unmarshal(object.Data, &manifest)
	if manifest.Phase != "delete_needed" || manifest.ReplayCursor != 0 || len(manifest.ExpectedIDs) != 0 {
		t.Fatalf("manifest %+v", manifest)
	}
}

func TestProjectionSnapshotIsFrozenAcrossBatchesAndNextGenerationCatchesUp(t *testing.T) {
	f, blob, orch := newFixture(t)
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f.mu.Lock()
	for i := 0; i < 600; i++ {
		duration := int64(i)
		m := Measurement{SchemaVersion: 1, ID: fmt.Sprintf("m-%03d", i), Kind: "execution", RunID: runA, ExecutionID: fmt.Sprintf("e-%03d", i), OccurredAt: hour, DurationMS: &duration, Status: "derived", Missing: []string{}}
		f.objects[measurementObject("execution", m.ID)], _ = json.Marshal(m)
	}
	f.mu.Unlock()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	chunks, count, err := s.freezeProjectionSnapshot(t.Context(), hour, 1, "snapshot-one")
	if err != nil || chunks != 2 || count != 600 {
		t.Fatalf("freeze chunks=%d count=%d err=%v", chunks, count, err)
	}
	insertedDuration := int64(9999)
	inserted := Measurement{SchemaVersion: 1, ID: "m-000a", Kind: "execution", RunID: runA, ExecutionID: "e-new", OccurredAt: hour, DurationMS: &insertedDuration, Status: "derived", Missing: []string{}}
	updated := inserted
	updated.ID = "m-550"
	updated.ExecutionID = "e-550"
	f.mu.Lock()
	f.objects[measurementObject("execution", inserted.ID)], _ = json.Marshal(inserted)
	f.objects[measurementObject("execution", updated.ID)], _ = json.Marshal(updated)
	f.nextRevision++
	f.revisions[measurementObject("execution", updated.ID)] = fmt.Sprintf("r%d", f.nextRevision)
	f.mu.Unlock()
	manifest := projectionManifest{Hour: hour, Generation: 1, SnapshotID: "snapshot-one", SnapshotChunks: chunks, SnapshotCount: count}
	records, ids, more, err := s.projectionSnapshotBatch(t.Context(), &manifest, 0)
	if err != nil || len(records) != 500 || len(ids) != 500 || !more {
		t.Fatalf("first snapshot batch records=%d more=%v err=%v", len(records), more, err)
	}
	records, ids, more, err = s.projectionSnapshotBatch(t.Context(), &manifest, 500)
	if err != nil || len(records) != 100 || len(ids) != 100 || more {
		t.Fatalf("second snapshot batch records=%d more=%v err=%v", len(records), more, err)
	}
	for i, id := range ids {
		if id == "execution/m-550" && int64Value(records[i]["duration_ms"]) == 9999 {
			t.Fatal("old snapshot observed concurrent canonical update")
		}
		if id == "execution/m-000a" {
			t.Fatal("old snapshot observed concurrent insertion")
		}
	}
	_, nextCount, err := s.freezeProjectionSnapshot(t.Context(), hour, 2, "snapshot-two")
	if err != nil || nextCount != 601 {
		t.Fatalf("next generation count=%d err=%v", nextCount, err)
	}
	nextManifest := projectionManifest{Hour: hour, Generation: 2, SnapshotID: "snapshot-two", SnapshotCount: nextCount}
	foundInsert, foundUpdate := false, false
	for cursor := 0; cursor < nextCount; cursor += 500 {
		batch, batchIDs, _, e := s.projectionSnapshotBatch(t.Context(), &nextManifest, cursor)
		if e != nil {
			t.Fatal(e)
		}
		for i, id := range batchIDs {
			if id == "execution/m-000a" {
				foundInsert = true
			}
			if id == "execution/m-550" && int64Value(batch[i]["duration_ms"]) == 9999 {
				foundUpdate = true
			}
		}
	}
	if !foundInsert || !foundUpdate {
		t.Fatalf("next generation insert=%v update=%v", foundInsert, foundUpdate)
	}
}

func TestDeleteWaitsForWALToPersistOwnedHour(t *testing.T) {
	_, blob, orch := newFixture(t)
	diskCount := int64(1)
	deletes := 0
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	uncached, partitionShape := false, false
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			writeJSON(w, 200, map[string]string{"id": "job"})
			return
		}
		if strings.Contains(r.URL.Path, "/_search_partition") {
			var request struct {
				SQL   string `json:"sql"`
				Start int64  `json:"start_time"`
				End   int64  `json:"end_time"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			partitionShape = strings.Contains(request.SQL, "ORDER BY _timestamp") && request.Start == hour.UnixMicro() && request.End == hour.Add(time.Hour).UnixMicro()
			writeJSON(w, 200, map[string]any{"records": diskCount, "file_num": 1})
			return
		}
		uncached = r.URL.Query().Get("use_cache") == "false"
		writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"record_count": int64(2)}}, "total": 1, "is_partial": false})
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	_, _ = s.advanceProjection(t.Context(), hour, 1)
	if deletes != 0 {
		t.Fatal("delete started while WAL-only records remained")
	}
	diskCount = 2
	_, _ = s.advanceProjection(t.Context(), hour, 1)
	if deletes != 1 {
		t.Fatalf("delete calls %d after persistence", deletes)
	}
	if !uncached || !partitionShape {
		t.Fatalf("uncached=%v partition_shape=%v", uncached, partitionShape)
	}
}

func TestPersistenceBarrierRejectsMissingCountsAndPartialSearch(t *testing.T) {
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		full      map[string]any
		partition map[string]any
		name      string
		wantErr   bool
	}{
		{name: "missing-visible", full: map[string]any{"hits": []map[string]any{{}}, "total": 1, "is_partial": false}, partition: map[string]any{"records": 0}, wantErr: true},
		{name: "missing-partition", full: map[string]any{"hits": []map[string]any{{"record_count": 0}}, "total": 1, "is_partial": false}, partition: map[string]any{"file_num": 0}, wantErr: true},
		{name: "partial-visible", full: map[string]any{"hits": []map[string]any{{"record_count": 0}}, "total": 1, "is_partial": true}, partition: map[string]any{"records": 0}, wantErr: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "_search_partition") {
					writeJSON(w, 200, test.partition)
					return
				}
				writeJSON(w, 200, test.full)
			}))
			defer server.Close()
			s := &service{openobserve: server.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient}
			quiescent, _, err := s.projectionQuiescent(t.Context(), hour)
			if quiescent || test.wantErr != (err != nil) {
				t.Fatalf("quiescent=%v err=%v", quiescent, err)
			}
		})
	}
}

func TestMissingStreamRequiresConfirmedSchema404(t *testing.T) {
	for _, test := range []struct {
		name       string
		schema     int
		wantDelete bool
	}{{"absent", 404, false}, {"schema-error", 500, false}} {
		t.Run(test.name, func(t *testing.T) {
			deletes, searches := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/schema"):
					w.WriteHeader(test.schema)
				case r.Method == http.MethodDelete:
					deletes++
					writeJSON(w, 200, map[string]string{"id": "job"})
				case r.URL.Path == searchPath:
					searches++
					http.Error(w, "search missing", 400)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			_, blob, orch := newFixture(t)
			s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: server.URL, openobserveUser: "u", openobservePassword: "p", projectionStream: "smoke_unique", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
			_, _ = s.advanceProjection(t.Context(), time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), 1)
			if (deletes == 1) != test.wantDelete || searches != 0 {
				t.Fatalf("delete=%d search=%d", deletes, searches)
			}
			if !strings.Contains(s.projectionObject(time.Now()), "smoke_unique") {
				t.Fatal("projection state did not use configured stream")
			}
		})
	}
}

func TestPersistedDeletionVerificationRecoversWhenSchemaIsConfirmedAbsent(t *testing.T) {
	f, blob, orch := newFixture(t)
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	measurement := Measurement{SchemaVersion: 1, ID: "recover-absent", Kind: "execution", RunID: runA, ExecutionID: "exec", OccurredAt: hour, Status: "missing", Missing: []string{"duration_ms/unavailable"}}
	f.mu.Lock()
	f.objects[measurementObject("execution", measurement.ID)], _ = json.Marshal(measurement)
	f.mu.Unlock()
	schemaExists := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/schema"):
			if !schemaExists {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, 200, map[string]any{"stats": map[string]int{"doc_num": 1}})
		case strings.HasSuffix(r.URL.Path, "/_json"):
			schemaExists = true
			writeJSON(w, 200, map[string]any{"status": []map[string]int{{"successful": 1, "failed": 0}}})
		case r.URL.Path == searchPath:
			writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"projection_id": "execution/recover-absent", "generation": 1}}, "total": 1, "is_partial": false})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: server.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	chunks, count, err := s.freezeProjectionSnapshot(t.Context(), hour, 1, "upgrade-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	manifest := projectionManifest{Stream: projectionStream, Hour: hour, Generation: 1, Phase: "deletion_verifying", SnapshotID: "upgrade-snapshot", SnapshotChunks: chunks, SnapshotCount: count, UpdatedAt: time.Now().UTC()}
	_, _ = s.writeObject(t.Context(), s.projectionObject(hour), manifest, "*")
	complete, err := s.advanceProjection(t.Context(), hour, 1)
	if err != nil || !complete {
		t.Fatalf("complete=%v err=%v", complete, err)
	}
	object, _ := s.readObject(t.Context(), s.projectionObject(hour))
	_ = json.Unmarshal(object.Data, &manifest)
	if manifest.Phase != "complete" {
		t.Fatalf("manifest %+v", manifest)
	}
}

func TestEvidenceFiltersTargetExecutionSubtreeAndExcludesSibling(t *testing.T) {
	f, blob, orch := newFixture(t)
	f.mu.Lock()
	f.objects["runs/"+runA+"/run.json"] = []byte(`{"execution_id":"target-exec","distributed_trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	f.objects["workflow-executions/target-exec.json"] = []byte(`{"schema_version":1,"execution_id":"target-exec","run_id":"` + runA + `","workflow":"feature","workflow_version":1,"artifact_prefix":"runs/` + runA + `","started_at":"2026-11-01T12:00:00Z","status":"completed"}`)
	f.mu.Unlock()
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/_search") {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"span_id": "root", "span_attributes": map[string]any{"factory.execution_id": "target-exec"}}, {"span_id": "child", "parent_span_id": "root"}, {"span_id": "sibling", "span_attributes": map[string]any{"factory.execution_id": "other-exec"}}, {"span_id": "sibling-child", "parent_span_id": "sibling"}}, "total": 4, "is_partial": false})
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	resp := get(t, server.URL+"/api/evidence?run_id="+runA+"&execution_id=target-exec&from=2026-11-01T11:00:00Z&to=2026-11-01T13:00:00Z")
	defer closeBody(t, resp.Body)
	var result struct {
		SourceStatus string           `json:"source_status"`
		Spans        []map[string]any `json:"spans"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		t.Fatal("decode evidence")
	}
	if resp.StatusCode != 200 || result.SourceStatus != "available" || len(result.Spans) != 2 || result.Spans[0]["span_id"] != "root" || result.Spans[1]["span_id"] != "child" {
		t.Fatalf("evidence status=%d %+v", resp.StatusCode, result)
	}
}
