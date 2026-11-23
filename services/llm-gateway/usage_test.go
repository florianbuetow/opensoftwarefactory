package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const pendingListPath = "/list/gateway-measurements-pending"

func TestNormalizeUsagePreservesZeroCacheAndMissing(t *testing.T) {
	c := usageContext{ID: "id", RunID: "run", ExecutionID: "exec", Vendor: "v", Model: "resolved", RequestedModel: "asked", Effort: "low", OccurredAt: time.Now(), Modes: map[string]string{}}
	m := normalizeUsage(&c, json.RawMessage(`{"prompt_tokens":0,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0},"provider_cost":0.01}`), true)
	if m.InputTokens == nil || *m.InputTokens != 0 || m.CachedInputTokens == nil || *m.CachedInputTokens != 0 || m.UncachedInputTokens == nil || *m.UncachedInputTokens != 0 || m.OutputTokens == nil || *m.OutputTokens != 2 {
		t.Fatalf("measurement %+v", m)
	}
	if m.ReasoningTokens != nil || !strings.Contains(strings.Join(m.Missing, ","), "reasoning_tokens/provider usage omitted") || !strings.Contains(string(m.RawUsage), "provider_cost") {
		t.Fatalf("missing/raw %+v %s", m.Missing, m.RawUsage)
	}
	missing := normalizeUsage(&c, nil, false)
	if missing.Status != statusMissing || missing.InputTokens != nil || !strings.Contains(strings.Join(missing.Missing, ","), "raw_usage/provider omitted usage") {
		t.Fatalf("missing %+v", missing)
	}
}

func TestNormalizeUsageKeepsUnknownEffortAbsent(t *testing.T) {
	c := usageContext{ID: "id", Vendor: "v", Model: "m", RequestedModel: "asked", OccurredAt: time.Now(), Modes: map[string]string{}}
	m := normalizeUsage(&c, json.RawMessage(`{"prompt_tokens":1,"completion_tokens":1}`), true)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"effort"`) || !strings.Contains(strings.Join(m.Missing, ","), "effort/request omitted") {
		t.Fatalf("measurement %s missing=%v", data, m.Missing)
	}
}

func TestNativeUsageObserverIsIncremental(t *testing.T) {
	o := newUsageObserver(true)
	parts := []string{"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n", `data: {"choices":[],"usage":{"prompt_tokens":4,`, "\"completion_tokens\":2}}\n\n", "data: [DONE]\n\n"}
	for _, part := range parts {
		if _, err := o.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
		if len(o.pending) > 4<<20 {
			t.Fatal("observer buffered stream")
		}
	}
	if got := string(o.Usage()); !strings.Contains(got, `"prompt_tokens":4`) {
		t.Fatalf("usage %q", got)
	}
}

func TestGatewayPersistsOutboxBeforeEvaluation(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	blobs := map[string][]byte{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		durableBeforeProvider := len(blobs) >= 2
		mu.Unlock()
		if !durableBeforeProvider {
			t.Error("provider called before durable attempt intent")
		}
		if _, err := io.WriteString(w, `{"id":"c","object":"chat.completion","model":"resolved","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3}}}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		if r.Method == http.MethodPut {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			blobs[id] = data
			order = append(order, "blob")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodDelete {
			delete(blobs, id)
			order = append(order, "delete")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer blob.Close()
	var measured usageMeasurement
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "evaluation")
		if len(blobs) == 0 {
			t.Error("evaluation before outbox")
		}
		if r.Header.Get("If-None-Match") != "*" {
			t.Errorf("condition %q", r.Header.Get("If-None-Match"))
		}
		if err := json.NewDecoder(r.Body).Decode(&measured); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer eval.Close()
	c := Config{BlobstoreURL: blob.URL, EvaluationURL: eval.URL, Providers: []Provider{{Name: "vendor", BaseURL: upstream.URL, Auth: Auth{Type: "none"}, Streaming: true, Models: map[string]string{"public": "resolved"}}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer g.transport.CloseIdleConnections()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public","messages":[],"reasoning_effort":"low"}`))
	req.Header.Set("X-Trace-Id", "run")
	req.Header.Set("X-Execution-Id", "exec")
	req.Header.Set("X-Agent-Ref", "judge@v2")
	req.Header.Set("X-Turn", "1")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Usage-Measurement-Id") == "" {
		t.Fatalf("response %d %v", rec.Code, rec.Header())
	}
	if measured.ID != rec.Header().Get("X-Usage-Measurement-Id") || measured.Vendor != "vendor" || measured.Model != "resolved" || measured.RequestedModel != "public" || measured.Effort != "low" || measured.AgentVersion == nil || *measured.AgentVersion != 2 || measured.CachedInputTokens == nil || *measured.CachedInputTokens != 3 {
		t.Fatalf("measurement %+v", measured)
	}
	if measured.TraceID != "" || measured.SpanID != "" || !strings.Contains(strings.Join(measured.Missing, ","), "trace_id/trace context unavailable") {
		t.Fatalf("invalid trace evidence %+v", measured)
	}
	mu.Lock()
	defer mu.Unlock()
	var evidence usageOutbox
	for id, data := range blobs {
		if strings.HasPrefix(id, usageOutboxPrefix+"/") {
			if err := json.Unmarshal(data, &evidence); err != nil {
				t.Error(err)
			}
		}
	}
	if len(measured.Modes) != 0 || evidence.Attribution.RequestSettings["stream"] != "none" || evidence.Attribution.AgentRef != "judge@v2" {
		t.Fatalf("modes/attribution %+v %+v", measured.Modes, evidence.Attribution)
	}
	if strings.Join(order, ",") != "blob,blob,blob,evaluation,delete" {
		t.Fatalf("order %v", order)
	}
}

func TestGatewayRejectsMalformedEffortBeforeProvider(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()
	g := testGateway(t, upstream, true, Auth{Type: "none"})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[],"reasoning_effort":42}`)))
	if rec.Code != http.StatusBadRequest || called {
		t.Fatalf("status=%d provider_called=%v", rec.Code, called)
	}
}

func TestGatewayFailsBeforeProviderWhenAttemptIntentIsNotDurable(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "disk unavailable", http.StatusInsufficientStorage)
	}))
	defer blob.Close()
	eval := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("evaluation called") }))
	defer eval.Close()
	c := Config{BlobstoreURL: blob.URL, EvaluationURL: eval.URL, Providers: []Provider{{Name: "v", BaseURL: upstream.URL, Auth: Auth{Type: "none"}, Models: map[string]string{"m": "m"}}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("status=%d provider_called=%v", rec.Code, called)
	}
}

func TestPeriodicReplaySkipsActiveProviderAttempt(t *testing.T) {
	var mu sync.Mutex
	blobs := map[string][]byte{}
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/list/"+usagePendingPrefix {
			objects := []map[string]string{}
			for id := range blobs {
				if strings.HasPrefix(id, usagePendingPrefix+"/") {
					objects = append(objects, map[string]string{"id": id})
				}
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"objects": objects}); err != nil {
				t.Error(err)
			}
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		switch r.Method {
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			blobs[id] = data
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			data, ok := blobs[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if _, err := w.Write(data); err != nil {
				t.Error(err)
			}
		case http.MethodDelete:
			delete(blobs, id)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer blob.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		if _, err := io.WriteString(w, `{"id":"c","object":"chat.completion","model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	var evalCalls atomic.Int32
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { evalCalls.Add(1); w.WriteHeader(http.StatusCreated) }))
	defer eval.Close()
	c := Config{BlobstoreURL: blob.URL, EvaluationURL: eval.URL, Providers: []Provider{{Name: "v", BaseURL: upstream.URL, Auth: Auth{Type: "none"}, Models: map[string]string{"m": "m"}}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`)))
	}()
	<-started
	if err := g.replayPendingUsage(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if evalCalls.Load() != 0 {
		t.Fatalf("active attempt published %d times", evalCalls.Load())
	}
	mu.Lock()
	for id, data := range blobs {
		if strings.HasPrefix(id, usageOutboxPrefix+"/") {
			var out usageOutbox
			if err := json.Unmarshal(data, &out); err != nil {
				t.Error(err)
			}
			if out.Phase != phaseActive || out.Measurement.DurationMS != nil {
				t.Fatalf("active outbox %+v", out)
			}
		}
	}
	mu.Unlock()
	close(release)
	<-done
	if evalCalls.Load() != 1 {
		t.Fatalf("final measurement deliveries=%d", evalCalls.Load())
	}
}

func TestReplayOutboxTreatsMatchingConflictAsDelivered(t *testing.T) {
	measurement := usageMeasurement{SchemaVersion: 1, ID: "attempt", Kind: "usage", RunID: "run", ExecutionID: "exec", OccurredAt: time.Now().UTC(), Vendor: "v", Model: "m", RequestedModel: "asked", Effort: "none", Modes: map[string]string{}, ArtifactID: "gateway-measurements/attempt.json", Status: "missing", Missing: []string{"input_tokens: provider usage omitted"}}
	outbox, err := json.Marshal(usageOutbox{Measurement: measurement})
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == pendingListPath:
			if handlerErr := json.NewEncoder(w).Encode(map[string]any{"objects": []map[string]string{{"id": "gateway-measurements-pending/attempt.json"}}}); handlerErr != nil {
				t.Error(handlerErr)
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "pending"):
			if handlerErr := json.NewEncoder(w).Encode(usagePending{ArtifactID: "gateway-measurements/attempt.json"}); handlerErr != nil {
				t.Error(handlerErr)
			}
		case r.Method == http.MethodGet:
			if _, handlerErr := w.Write(outbox); handlerErr != nil {
				t.Error(handlerErr)
			}
		case r.Method == http.MethodDelete:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer blob.Close()
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		stored, handlerErr := json.Marshal(measurement)
		if handlerErr != nil {
			t.Error(handlerErr)
		}
		var object map[string]any
		if handlerErr = json.Unmarshal(stored, &object); handlerErr != nil {
			t.Error(handlerErr)
		}
		object["revision"] = "r1"
		if handlerErr = json.NewEncoder(w).Encode(object); handlerErr != nil {
			t.Error(handlerErr)
		}
	}))
	defer eval.Close()
	c := Config{BlobstoreURL: blob.URL, EvaluationURL: eval.URL, Providers: []Provider{{Name: "v", BaseURL: "http://provider.invalid", Auth: Auth{Type: "none"}, Models: map[string]string{"m": "m"}}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.replayUsageOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("pending marker not removed")
	}
}

func TestReplayContinuesAfterBadPendingItem(t *testing.T) {
	m := usageMeasurement{SchemaVersion: 1, ID: "good", Kind: "usage", OccurredAt: time.Now(), Vendor: "v", Model: "m", RequestedModel: "m", Modes: map[string]string{}, ArtifactID: "gateway-measurements/good.json", Status: "missing", Missing: []string{"raw_usage: omitted"}}
	outbox, err := json.Marshal(usageOutbox{Measurement: m})
	if err != nil {
		t.Fatal(err)
	}
	delivered := false
	deleted := false
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.URL.Path {
		case pendingListPath:
			if handlerErr := json.NewEncoder(w).Encode(map[string]any{"objects": []map[string]string{{"id": "gateway-measurements-pending/bad.json"}, {"id": "gateway-measurements-pending/good.json"}}}); handlerErr != nil {
				t.Error(handlerErr)
			}
		case "/blobs/gateway-measurements-pending/bad.json":
			if _, handlerErr := w.Write([]byte(`{"artifact_id":"wrong/item"}`)); handlerErr != nil {
				t.Error(handlerErr)
			}
		case "/blobs/gateway-measurements-pending/good.json":
			if handlerErr := json.NewEncoder(w).Encode(usagePending{ArtifactID: "gateway-measurements/good.json"}); handlerErr != nil {
				t.Error(handlerErr)
			}
		case "/blobs/gateway-measurements/good.json":
			if _, handlerErr := w.Write(outbox); handlerErr != nil {
				t.Error(handlerErr)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer blob.Close()
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { delivered = true; w.WriteHeader(http.StatusCreated) }))
	defer eval.Close()
	c := Config{BlobstoreURL: blob.URL, EvaluationURL: eval.URL, Providers: []Provider{{Name: "v", BaseURL: "http://unused.invalid", Auth: Auth{Type: "none"}, Models: map[string]string{"m": "m"}}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.replayUsageOutbox(context.Background()); err == nil {
		t.Fatal("bad item was not reported")
	}
	if !delivered || !deleted {
		t.Fatalf("later item starved: delivered=%v deleted=%v", delivered, deleted)
	}
}

func TestConflictUpgradesSameMissingAttemptWithCAS(t *testing.T) {
	reported := usageMeasurement{SchemaVersion: 1, ID: "attempt", Kind: "usage", RunID: "run", ExecutionID: "exec", OccurredAt: time.Now().UTC(), Vendor: "v", Model: "m", RequestedModel: "m", Modes: map[string]string{}, ArtifactID: "gateway-measurements/attempt.json", Status: statusReported, Missing: []string{}, RawUsage: json.RawMessage(`{"prompt_tokens":1}`)}
	cas := false
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			var existing map[string]any
			data, err := json.Marshal(reported)
			if err != nil {
				t.Error(err)
			}
			if err := json.Unmarshal(data, &existing); err != nil {
				t.Error(err)
			}
			existing["status"] = statusMissing
			delete(existing, "raw_usage")
			existing["missing"] = []string{"raw_usage/provider omitted usage"}
			existing["revision"] = "rev1"
			if err := json.NewEncoder(w).Encode(existing); err != nil {
				t.Error(err)
			}
		case http.MethodPut:
			if r.Header.Get("If-Match") == `"rev1"` {
				cas = true
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusPreconditionFailed)
			}
		}
	}))
	defer eval.Close()
	c := Config{EvaluationURL: eval.URL, Providers: []Provider{{Name: "v", BaseURL: "http://unused.invalid", Auth: Auth{Type: "none"}, Models: map[string]string{"m": "m"}}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.deliverOutbox(context.Background(), &usageOutbox{Phase: phaseFinal, Measurement: reported}); err != nil {
		t.Fatal(err)
	}
	if !cas {
		t.Fatal("missing measurement was not upgraded with CAS")
	}
}

func TestStartupFinalizesAbandonedActiveAttempt(t *testing.T) {
	m := usageMeasurement{SchemaVersion: 1, ID: "abandoned", Kind: "usage", OccurredAt: time.Now(), Vendor: "v", Model: "m", RequestedModel: "m", Modes: map[string]string{}, ArtifactID: "gateway-measurements/abandoned.json", Status: "missing", Missing: []string{"raw_usage/provider omitted usage"}}
	outbox, err := json.Marshal(usageOutbox{Phase: phaseActive, Measurement: m})
	if err != nil {
		t.Fatal(err)
	}
	marker, err := json.Marshal(usagePending{ArtifactID: m.ArtifactID})
	if err != nil {
		t.Fatal(err)
	}
	blobs := map[string][]byte{m.ArtifactID: outbox, "gateway-measurements-pending/abandoned.json": marker}
	var delivered usageMeasurement
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pendingListPath {
			if handlerErr := json.NewEncoder(w).Encode(map[string]any{"objects": []map[string]string{{"id": "gateway-measurements-pending/abandoned.json"}}}); handlerErr != nil {
				t.Error(handlerErr)
			}
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		switch r.Method {
		case http.MethodGet:
			if _, handlerErr := w.Write(blobs[id]); handlerErr != nil {
				t.Error(handlerErr)
			}
		case http.MethodPut:
			stored, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Error(readErr)
			}
			blobs[id] = stored
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			delete(blobs, id)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer blob.Close()
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handlerErr := json.NewDecoder(r.Body).Decode(&delivered); handlerErr != nil {
			t.Error(handlerErr)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer eval.Close()
	c := Config{BlobstoreURL: blob.URL, EvaluationURL: eval.URL, Providers: []Provider{{Name: "v", BaseURL: "http://unused.invalid", Auth: Auth{Type: "none"}, Models: map[string]string{"m": "m"}}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.replayUsageOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if delivered.Status != statusMissing || delivered.DurationMS != nil || !strings.Contains(strings.Join(delivered.Missing, ","), "response/process interrupted") {
		t.Fatalf("delivered %+v", delivered)
	}
	var stored usageOutbox
	if err := json.Unmarshal(blobs[m.ArtifactID], &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Phase != phaseFinal {
		t.Fatalf("stored phase %q", stored.Phase)
	}
}

func TestStartupRecoversMoreThanOnePendingBatch(t *testing.T) {
	const total = 205
	blobs := map[string][]byte{}
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("attempt-%03d", i)
		artifact := usageOutboxPrefix + "/" + id + ".json"
		out, err := json.Marshal(usageOutbox{Phase: phaseActive, Measurement: usageMeasurement{SchemaVersion: 1, ID: id, Kind: "usage", OccurredAt: time.Now(), Vendor: "v", Model: "m", RequestedModel: "m", Modes: map[string]string{}, ArtifactID: artifact, Status: "missing", Missing: []string{"raw_usage/provider omitted usage"}}})
		if err != nil {
			t.Error(err)
		}
		marker, err := json.Marshal(usagePending{ArtifactID: artifact})
		if err != nil {
			t.Error(err)
		}
		blobs[artifact] = out
		blobs[usagePendingPrefix+"/"+id+".json"] = marker
	}
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/list/"+usagePendingPrefix {
			objects := []map[string]string{}
			for id := range blobs {
				if strings.HasPrefix(id, usagePendingPrefix+"/") {
					objects = append(objects, map[string]string{"id": id})
				}
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"objects": objects}); err != nil {
				t.Error(err)
			}
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		switch r.Method {
		case http.MethodGet:
			if _, err := w.Write(blobs[id]); err != nil {
				t.Error(err)
			}
		case http.MethodPut:
			stored, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Error(readErr)
			}
			blobs[id] = stored
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			delete(blobs, id)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer blob.Close()
	delivered := 0
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { delivered++; w.WriteHeader(http.StatusCreated) }))
	defer eval.Close()
	c := Config{BlobstoreURL: blob.URL, EvaluationURL: eval.URL, Providers: []Provider{{Name: "v", BaseURL: "http://unused.invalid", Auth: Auth{Type: "none"}, Models: map[string]string{"m": "m"}}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.replayUsageOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if delivered != total {
		t.Fatalf("recovered %d/%d", delivered, total)
	}
}
