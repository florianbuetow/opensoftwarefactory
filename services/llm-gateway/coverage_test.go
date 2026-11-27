package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type errCloser struct {
	err error
}

func (e errCloser) Close() error { return e.err }

type failingResponseWriter struct {
	header    http.Header
	failAfter int
	writes    int
}

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingResponseWriter) WriteHeader(int) {}

func (w *failingResponseWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, errors.New("write failed")
	}
	return len(p), nil
}

func (w *failingResponseWriter) Flush() {}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnvHelper(t *testing.T) {
	t.Setenv("LLM_GATEWAY_TEST_ENV", "custom-value")
	if got := env("LLM_GATEWAY_TEST_ENV", "fallback"); got != "custom-value" {
		t.Fatalf("got %q, want custom-value", got)
	}
	t.Setenv("LLM_GATEWAY_TEST_ENV", "")
	if got := env("LLM_GATEWAY_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("got %q, want fallback", got)
	}
}

func TestCloseAfterJoinsErrors(t *testing.T) {
	baseErr := errors.New("base")
	closeErr := errors.New("close")
	if err := closeAfter(nil, errCloser{}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if err := closeAfter(baseErr, errCloser{}); !errors.Is(err, baseErr) {
		t.Fatalf("got %v, want %v", err, baseErr)
	}
	joined := closeAfter(baseErr, errCloser{err: closeErr})
	if !errors.Is(joined, baseErr) || !errors.Is(joined, closeErr) {
		t.Fatalf("expected joined errors, got %v", joined)
	}
}

func TestAuthorizeBasicBearerAndNone(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com", http.NoBody)
	authorize(req, &Provider{Auth: Auth{Type: "basic", Username: "u", Password: "p"}})
	u, p, ok := req.BasicAuth()
	if !ok || u != "u" || p != "p" {
		t.Fatalf("basic auth = (%q, %q, %v)", u, p, ok)
	}

	req2 := httptest.NewRequest(http.MethodGet, "http://example.com", http.NoBody)
	authorize(req2, &Provider{Auth: Auth{Type: "bearer", Token: "tok"}})
	if got := req2.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("bearer auth = %q", got)
	}

	req3 := httptest.NewRequest(http.MethodGet, "http://example.com", http.NoBody)
	authorize(req3, &Provider{Auth: Auth{Type: "none"}})
	if got := req3.Header.Get("Authorization"); got != "" {
		t.Fatalf("none auth = %q", got)
	}
}

func TestReadCompletionValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"invalid json", `{`},
		{"extra trailing data", `{"id":"1","object":"chat.completion","model":"m","choices":[{"message":{"role":"assistant"}}]}{}`},
		{"missing fields", `{"id":"","object":"chat.completion","model":"m","choices":[]}`},
		{"wrong object", `{"id":"1","object":"other","model":"m","choices":[{"message":{"role":"assistant"}}]}`},
		{"missing message", `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0}]}`},
		{"invalid tool_calls", `{"id":"1","object":"chat.completion","model":"m","choices":[{"message":{"tool_calls":"bad"}}]}`},
		{"null tool_call element", `{"id":"1","object":"chat.completion","model":"m","choices":[{"message":{"tool_calls":[null]}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readCompletion(strings.NewReader(tc.body)); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestWriteCompletionStreamServiceTierAndErrors(t *testing.T) {
	c := completion{
		ID:                "c1",
		Object:            "chat.completion",
		Model:             "m1",
		SystemFingerprint: json.RawMessage(`"fp1"`),
		ServiceTier:       json.RawMessage(`"default"`),
		Usage:             json.RawMessage(`{"prompt_tokens":1}`),
		Choices: []struct {
			Message      map[string]json.RawMessage `json:"message"`
			FinishReason json.RawMessage            `json:"finish_reason"`
			Logprobs     json.RawMessage            `json:"logprobs"`
			Index        int                        `json:"index"`
		}{{
			Index:        0,
			Message:      map[string]json.RawMessage{"role": json.RawMessage(`"assistant"`), "content": json.RawMessage(`"hi"`)},
			FinishReason: json.RawMessage(`"stop"`),
		}},
	}
	rec := httptest.NewRecorder()
	if err := writeCompletionStream(rec, &c, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"service_tier":"default"`) {
		t.Fatalf("missing service_tier in %s", rec.Body.String())
	}

	for failAfter := 0; failAfter < 3; failAfter++ {
		fw := &failingResponseWriter{failAfter: failAfter}
		if err := writeCompletionStream(fw, &c, true); err == nil {
			t.Fatalf("expected error when failing after %d writes", failAfter)
		}
	}
}

func TestServeHTTPValidationAndErrorPaths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"upstream/model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	g := testGateway(t, upstream, false, Auth{Type: "none"})

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
	}{
		{"chat wrong method", http.MethodGet, "/v1/chat/completions", "", http.StatusMethodNotAllowed},
		{"models wrong method", http.MethodPost, "/v1/models", "", http.StatusMethodNotAllowed},
		{"model detail wrong method", http.MethodPost, "/v1/models/public-model", "", http.StatusMethodNotAllowed},
		{"unknown path", http.MethodGet, "/v1/other", "", http.StatusNotFound},
		{"unknown model detail", http.MethodGet, "/v1/models/missing", "", http.StatusNotFound},
		{"invalid json", http.MethodPost, "/v1/chat/completions", `{`, http.StatusBadRequest},
		{"null json", http.MethodPost, "/v1/chat/completions", `null`, http.StatusBadRequest},
		{"trailing json", http.MethodPost, "/v1/chat/completions", `{"model":"public-model"}{}`, http.StatusBadRequest},
		{"missing model", http.MethodPost, "/v1/chat/completions", `{"messages":[]}`, http.StatusBadRequest},
		{"unknown model", http.MethodPost, "/v1/chat/completions", `{"model":"nope","messages":[]}`, http.StatusNotFound},
		{"invalid stream", http.MethodPost, "/v1/chat/completions", `{"model":"public-model","stream":"yes"}`, http.StatusBadRequest},
		{"invalid service_tier type", http.MethodPost, "/v1/chat/completions", `{"model":"public-model","service_tier":123}`, http.StatusBadRequest},
		{"empty service_tier", http.MethodPost, "/v1/chat/completions", `{"model":"public-model","service_tier":"   "}`, http.StatusBadRequest},
		{"invalid modes type", http.MethodPost, "/v1/chat/completions", `{"model":"public-model","modes":"bad"}`, http.StatusBadRequest},
		{"empty modes key", http.MethodPost, "/v1/chat/completions", `{"model":"public-model","modes":{" ":"v"}}`, http.StatusBadRequest},
		{"empty modes value", http.MethodPost, "/v1/chat/completions", `{"model":"public-model","modes":{"k":" "}}`, http.StatusBadRequest},
		{"invalid stream_options", http.MethodPost, "/v1/chat/completions", `{"model":"public-model","stream":true,"stream_options":"bad"}`, http.StatusBadRequest},
		{"valid temperature service_tier modes", http.MethodPost, "/v1/chat/completions", `{"model":"public-model","temperature":0.2,"service_tier":"flex","modes":{"tier":"fast"}}`, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

func TestServeHTTPUpstreamFailureAndStreamAborts(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	gClosed := testGateway(t, closed, true, Auth{Type: "none"})
	rec := httptest.NewRecorder()
	gClosed.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[]}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("closed upstream status = %d, want 502", rec.Code)
	}

	badComp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"bad"}`)
	}))
	defer badComp.Close()
	gBad := testGateway(t, badComp, false, Auth{Type: "none"})
	rec = httptest.NewRecorder()
	gBad.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","stream":true}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("bad synthetic completion status = %d, want 502", rec.Code)
	}

	// Synthetic stream write abort panics with http.ErrAbortHandler.
	goodComp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"upstream/model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer goodComp.Close()
	gSynth := testGateway(t, goodComp, false, Auth{Type: "none"})
	func() {
		defer func() {
			r := recover()
			if r != http.ErrAbortHandler {
				t.Fatalf("expected http.ErrAbortHandler, got %v", r)
			}
		}()
		gSynth.ServeHTTP(&failingResponseWriter{failAfter: 0}, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","stream":true}`)))
	}()

	// Native stream write abort panics with http.ErrAbortHandler.
	gNative := testGateway(t, goodComp, true, Auth{Type: "none"})
	func() {
		defer func() {
			r := recover()
			if r != http.ErrAbortHandler {
				t.Fatalf("expected http.ErrAbortHandler, got %v", r)
			}
		}()
		gNative.ServeHTTP(&failingResponseWriter{failAfter: 0}, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","stream":true}`)))
	}()
}

func TestProviderModelsErrorBranches(t *testing.T) {
	badJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{not-json`)
	}))
	defer badJSON.Close()
	statusErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer statusErr.Close()

	g := testGateway(t, badJSON, true, Auth{Type: "none"})
	ctx := context.Background()

	if cat := g.providerModels(ctx, &Provider{Name: "p1", BaseURL: "://bad-url", ModelDiscovery: discoveryLMStudio}); len(cat.loaded) != 0 {
		t.Fatalf("expected empty catalog for invalid URL, got %+v", cat)
	}
	if cat := g.providerModels(ctx, &Provider{Name: "p2", BaseURL: badJSON.URL, ModelDiscovery: discoveryLMStudio}); len(cat.loaded) != 0 {
		t.Fatalf("expected empty catalog for bad LMStudio JSON, got %+v", cat)
	}
	if cat := g.providerModels(ctx, &Provider{Name: "p3", BaseURL: badJSON.URL}); len(cat.loaded) != 0 {
		t.Fatalf("expected empty catalog for bad OpenAI JSON, got %+v", cat)
	}
	if cat := g.providerModels(ctx, &Provider{Name: "p4", BaseURL: statusErr.URL}); len(cat.loaded) != 0 {
		t.Fatalf("expected empty catalog for 500 status, got %+v", cat)
	}
}

func TestUsageObserverEdgeCasesAndOverflow(t *testing.T) {
	// nextSSEFrame with CRLFCRLF before LFLF.
	if end, sep := nextSSEFrame([]byte("a\r\n\r\nb\n\n")); end != 1 || sep != 4 {
		t.Fatalf("nextSSEFrame CRLF = (%d, %d)", end, sep)
	}

	// Non-streaming overflow > 4MiB.
	o := newUsageObserver(false)
	big := bytes.Repeat([]byte("a"), (4<<20)+10)
	if n, err := o.Write(big); err != nil || n != len(big) {
		t.Fatalf("Write big = (%d, %v)", n, err)
	}
	if n, err := o.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("Write after overflow = (%d, %v)", n, err)
	}
	if o.Usage() != nil {
		t.Fatalf("expected nil usage after overflow")
	}

	// Streaming overflow without separator -> enters discarding, then recovers after separator.
	so := newUsageObserver(true)
	if _, err := so.Write(big); err != nil {
		t.Fatal(err)
	}
	if !so.discarding {
		t.Fatal("expected discarding=true")
	}
	if _, err := so.Write([]byte("still-discarding")); err != nil {
		t.Fatal(err)
	}
	if _, err := so.Write([]byte("\n\nevent: message\ndata: not-json\ndata: {\"usage\":null}\ndata: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n")); err != nil {
		t.Fatal(err)
	}
	if got := string(so.Usage()); !strings.Contains(got, `"prompt_tokens":7`) {
		t.Fatalf("recovered usage = %q", got)
	}

	// Streaming overflow where separator is inside the overflowing chunk.
	so2 := newUsageObserver(true)
	chunkWithSep := append(bytes.Repeat([]byte("x"), (4<<20)+10), []byte("\n\ndata: {\"usage\":{\"prompt_tokens\":9}}\n\n")...)
	if _, err := so2.Write(chunkWithSep); err != nil {
		t.Fatal(err)
	}
	if got := string(so2.Usage()); !strings.Contains(got, `"prompt_tokens":9`) {
		t.Fatalf("recovered usage 2 = %q", got)
	}
}

func TestNormalizeUsageBranches(t *testing.T) {
	c := usageContext{
		ID: "id1", Vendor: "v", Model: "m", RequestedModel: "m",
		Agent: "coder", AgentRef: "coder@invalid", OccurredAt: time.Now(), Modes: map[string]string{},
	}
	inv := normalizeUsage(&c, json.RawMessage(`{invalid`), true)
	if inv.Status != "invalid" {
		t.Fatalf("expected invalid status, got %+v", inv)
	}

	respStyle := normalizeUsage(&c, json.RawMessage(`{"input_tokens":10,"output_tokens":5,"cached_input_tokens":2,"uncached_input_tokens":8,"completion_tokens_details":{"reasoning_tokens":4}}`), false)
	if respStyle.InputTokens == nil || *respStyle.InputTokens != 10 ||
		respStyle.OutputTokens == nil || *respStyle.OutputTokens != 5 ||
		respStyle.CachedInputTokens == nil || *respStyle.CachedInputTokens != 2 ||
		respStyle.UncachedInputTokens == nil || *respStyle.UncachedInputTokens != 8 ||
		respStyle.ReasoningTokens == nil || *respStyle.ReasoningTokens != 4 {
		t.Fatalf("respStyle = %+v", respStyle)
	}
	missingJoined := strings.Join(respStyle.Missing, ",")
	if !strings.Contains(missingJoined, "agent_version/agent reference is not vN") || !strings.Contains(missingJoined, "response/interrupted") {
		t.Fatalf("missing = %v", respStyle.Missing)
	}

	empty := normalizeUsage(&c, json.RawMessage(`{}`), true)
	emptyMissing := strings.Join(empty.Missing, ",")
	if !strings.Contains(emptyMissing, "input_tokens/provider usage omitted") || !strings.Contains(emptyMissing, "output_tokens/provider usage omitted") {
		t.Fatalf("empty missing = %v", empty.Missing)
	}
}

func TestUsageOutboxAndReplayErrorPaths(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Partial configuration fails beginUsageAttempt.
	gPartial := &Gateway{blobstoreURL: "http://blob", log: logger, evidenceClient: &http.Client{}}
	if err := gPartial.beginUsageAttempt(context.Background(), &usageContext{ID: "a"}); err == nil {
		t.Fatal("expected error for partial usage config")
	}

	// Empty configuration returns nil for replayUsageOutbox and replayPendingUsage.
	gEmpty := &Gateway{log: logger, evidenceClient: &http.Client{}}
	if err := gEmpty.replayUsageOutbox(context.Background()); err != nil {
		t.Fatalf("replayUsageOutbox empty: %v", err)
	}
	if err := gEmpty.replayPendingUsage(context.Background(), true); err != nil {
		t.Fatalf("replayPendingUsage empty: %v", err)
	}

	// replayUsageOutboxLoop exits cleanly on context cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gEmpty.replayUsageOutboxLoop(ctx)

	// HTTP 500 error paths for storeOutbox, storePending, deletePending, deliverOutbox, getBlobJSON, pendingUsageCount.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "notfound") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer errSrv.Close()

	gErr := &Gateway{blobstoreURL: errSrv.URL, evaluationURL: errSrv.URL, log: logger, evidenceClient: errSrv.Client()}
	ob := &usageOutbox{Phase: phaseFinal, Measurement: usageMeasurement{ID: "m1", ArtifactID: usageOutboxPrefix + "/m1.json"}}
	if err := gErr.storeOutbox(context.Background(), ob, true); err == nil {
		t.Fatal("expected storeOutbox error")
	}
	if err := gErr.storePending(context.Background(), usagePending{ArtifactID: ob.Measurement.ArtifactID}, true); err == nil {
		t.Fatal("expected storePending error")
	}
	if err := gErr.deletePending(context.Background(), "m1"); err == nil {
		t.Fatal("expected deletePending 500 error")
	}
	if err := gErr.deletePending(context.Background(), "notfound"); err != nil {
		t.Fatalf("deletePending 404 should succeed: %v", err)
	}
	if err := gErr.deliverOutbox(context.Background(), ob); err == nil {
		t.Fatal("expected deliverOutbox error")
	}
	var dst map[string]any
	if err := gErr.getBlobJSON(context.Background(), "k", &dst); err == nil {
		t.Fatal("expected getBlobJSON error")
	}
	if _, err := gErr.pendingUsageCount(context.Background()); err == nil {
		t.Fatal("expected pendingUsageCount error")
	}
	if err := gErr.replayUsageOutbox(context.Background()); err == nil {
		t.Fatal("expected replayUsageOutbox error")
	}
	if err := gErr.replayPendingUsage(context.Background(), true); err == nil {
		t.Fatal("expected replayPendingUsage error")
	}
	// completeUsage logs and returns when storeOutbox fails.
	gErr.completeUsage(context.Background(), &usageContext{ID: "m1", OccurredAt: time.Now(), Modes: map[string]string{}}, nil, false)
}

func TestCompareExistingMeasurementAndCAS(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	base := usageMeasurement{
		SchemaVersion: 1, ID: "a1", Kind: "usage", RunID: "r1", ExecutionID: "e1",
		OccurredAt: now, Vendor: "v", Model: "m", RequestedModel: "m", Modes: map[string]string{},
		ArtifactID: "gateway-measurements/a1.json", Status: statusReported, Missing: []string{},
	}
	expectedBytes, _ := json.Marshal(base)

	// 1. GET returns 500.
	g := &Gateway{log: logger, evidenceClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error", Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}}
	if err := g.compareExistingMeasurement(context.Background(), "http://eval/api/measurements/a1", expectedBytes); err == nil {
		t.Fatal("expected error on GET 500")
	}

	// 2. GET returns invalid JSON.
	g.evidenceClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("{bad")), Header: make(http.Header)}, nil
	})}
	if err := g.compareExistingMeasurement(context.Background(), "http://eval/api/measurements/a1", expectedBytes); err == nil {
		t.Fatal("expected error on invalid JSON")
	}

	// 3. Different identity fails.
	diff := base
	diff.Model = "other-model"
	diffBytes, _ := json.Marshal(diff)
	g.evidenceClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(diffBytes)), Header: make(http.Header)}, nil
	})}
	if err := g.compareExistingMeasurement(context.Background(), "http://eval/api/measurements/a1", expectedBytes); err == nil {
		t.Fatal("expected error on identity mismatch")
	}

	// 4. CAS recovery from statusMissing -> statusReported succeeds, then fails on non-2xx.
	var storedMap map[string]any
	_ = json.Unmarshal(expectedBytes, &storedMap)
	storedMap["status"] = statusMissing
	storedMap["revision"] = "rev-1"
	storedMissingBytes, _ := json.Marshal(storedMap)

	var putStatus atomic.Int32
	putStatus.Store(http.StatusOK)
	g.evidenceClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(storedMissingBytes)), Header: make(http.Header)}, nil
		}
		if r.Header.Get("If-Match") != `"rev-1"` {
			t.Errorf("If-Match = %q", r.Header.Get("If-Match"))
		}
		code := int(putStatus.Load())
		return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	if err := g.compareExistingMeasurement(context.Background(), "http://eval/api/measurements/a1", expectedBytes); err != nil {
		t.Fatalf("CAS recovery failed: %v", err)
	}
	putStatus.Store(http.StatusPreconditionFailed)
	if err := g.compareExistingMeasurement(context.Background(), "http://eval/api/measurements/a1", expectedBytes); err == nil {
		t.Fatal("expected CAS failure on 412")
	}
}

func TestReplayPendingUsageEdgeCases(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// 404 on pending list returns nil / 0.
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv404.Close()
	g404 := &Gateway{blobstoreURL: srv404.URL, evaluationURL: srv404.URL, log: logger, evidenceClient: srv404.Client()}
	if n, err := g404.pendingUsageCount(context.Background()); err != nil || n != 0 {
		t.Fatalf("pendingUsageCount 404 = (%d, %v)", n, err)
	}
	if err := g404.replayPendingUsage(context.Background(), true); err != nil {
		t.Fatalf("replayPendingUsage 404 = %v", err)
	}

	// Empty objects list returns nil.
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"objects":[]}`)
	}))
	defer srvEmpty.Close()
	gEmpty := &Gateway{blobstoreURL: srvEmpty.URL, evaluationURL: srvEmpty.URL, log: logger, evidenceClient: srvEmpty.Client()}
	if err := gEmpty.replayPendingUsage(context.Background(), true); err != nil {
		t.Fatalf("replayPendingUsage empty = %v", err)
	}

	// Various malformed pending/outbox items produce joined errors.
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/list/" + usagePendingPrefix:
			_, _ = io.WriteString(w, `{"objects":[{"id":"p/missing"},{"id":"p/badprefix"},{"id":"p/missingoutbox"},{"id":"p/badphase"}]}`)
		case "/blobs/p/missing":
			http.NotFound(w, r)
		case "/blobs/p/badprefix":
			_, _ = io.WriteString(w, `{"artifact_id":"other/1.json"}`)
		case "/blobs/p/missingoutbox":
			_, _ = io.WriteString(w, `{"artifact_id":"gateway-measurements/missing.json"}`)
		case "/blobs/gateway-measurements/missing.json":
			http.NotFound(w, r)
		case "/blobs/p/badphase":
			_, _ = io.WriteString(w, `{"artifact_id":"gateway-measurements/badphase.json"}`)
		case "/blobs/gateway-measurements/badphase.json":
			_, _ = io.WriteString(w, `{"phase":"bogus","measurement":{"id":"bp"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srvBad.Close()
	gBad := &Gateway{blobstoreURL: srvBad.URL, evaluationURL: srvBad.URL, log: logger, evidenceClient: srvBad.Client()}
	err := gBad.replayPendingUsage(context.Background(), true)
	if err == nil {
		t.Fatal("expected joined replay errors")
	}
	msg := err.Error()
	for _, want := range []string{"reading pending p/missing", "invalid artifact", "reading outbox", "invalid phase"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}
