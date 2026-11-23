package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"opensoftwarefactory/orchestrator/pipeline"
)

type acceptedFlakyStore struct {
	*memStore
	runWrites int
}

func (s *acceptedFlakyStore) Put(ctx context.Context, id string, data []byte, meta pipeline.Meta) error {
	if strings.HasSuffix(id, "/run.json") {
		s.runWrites++
		if s.runWrites == 2 {
			return errors.New("injected metadata outage")
		}
	}
	return s.memStore.Put(ctx, id, data, meta)
}

func TestManualStartReturnsAcceptedIdentityAndLeavesRecoveryObligation(t *testing.T) {
	workflows := storeWorkflows(t, sampleConfig)
	store := &acceptedFlakyStore{memStore: newMemStore()}
	runner := &fakeRunner{}
	s := &server{workflows: workflows, blobs: store, runner: runner, logger: slog.New(slog.DiscardHandler)}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/sample/runs", strings.NewReader("hello"))
	req.SetPathValue("name", sampleWorkflow)
	rec := httptest.NewRecorder()
	s.startRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"execution_id":"execution-`) {
		t.Fatalf("accepted identity missing: %s", rec.Body.String())
	}
	found := false
	for id := range store.data {
		if strings.HasPrefix(id, runStartObligationPrefix+"/") {
			found = true
		}
	}
	if !found {
		t.Fatal("accepted run lost its recovery obligation")
	}
}
