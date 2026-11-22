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
