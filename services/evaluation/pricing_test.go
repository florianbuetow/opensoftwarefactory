package main

import (
	"log/slog"
	"net/http"
	"testing"
)

func TestMalformedStoredPricingIsDependencyError(t *testing.T) {
	f, blob, orch := newFixture(t)
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	f.mu.Lock()
	f.objects[pricePrefix+"/bad.json"] = []byte(`{bad`)
	f.mu.Unlock()
	if _, err := s.loadPrices(t.Context()); err == nil {
		t.Fatal("malformed stored price was skipped")
	}
	f.mu.Lock()
	delete(f.objects, pricePrefix+"/bad.json")
	f.objects[conversionPrefix+"/bad.json"] = []byte(`{bad`)
	f.mu.Unlock()
	if _, err := s.loadConversions(t.Context()); err == nil {
		t.Fatal("malformed stored conversion was skipped")
	}
}
