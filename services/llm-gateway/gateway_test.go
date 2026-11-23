package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

func TestAQueryStringIsRefusedAndNeverReachesTheProvider(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	g := testGateway(t, upstream, false, Auth{Type: "none"})
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions?api-version=1", strings.NewReader(`{"model":"public-model","messages":[]}`)),
		httptest.NewRequest(http.MethodGet, "/v1/models/public-model?expand=1", http.NoBody),
	} {
		recorder := httptest.NewRecorder()
		g.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Query parameters are not supported") {
			t.Fatalf("%s: status=%d body=%s", request.URL, recorder.Code, recorder.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("the provider was called %d times", calls.Load())
	}
}

func testGateway(t *testing.T, upstream *httptest.Server, streaming bool, auth Auth) *Gateway {
	t.Helper()
	c := Config{Providers: []Provider{{
		Name: "test", BaseURL: upstream.URL, Auth: auth, Streaming: streaming,
		Models: map[string]string{"public-model": "upstream/model"},
	}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.transport.CloseIdleConnections)
	return g
}

func TestModelDetailsRoutesAndIsolatesAuthentication(t *testing.T) {
	var receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/models/upstream%2Fmodel" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.EscapedPath())
		}
		w.Header().Set("X-Upstream", "model")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"id":"upstream/model","object":"model","owned_by":"provider"}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()

	g := testGateway(t, upstream, true, Auth{Type: "bearer", Token: "configured-secret"})
	req := httptest.NewRequest(http.MethodGet, "/v1/models/public-model", http.NoBody)
	req.Header.Set("Authorization", "Bearer caller-secret")
	recorder := httptest.NewRecorder()
	g.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK || recorder.Header().Get("X-Upstream") != "model" {
		t.Fatalf("unexpected response: status=%d headers=%v", recorder.Code, recorder.Header())
	}
	if receivedAuth != "Bearer configured-secret" {
		t.Fatalf("upstream authorization = %q", receivedAuth)
	}
}

func TestChatForwardsMessagesAndUnrecognizedFields(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		var model string
		if err := json.Unmarshal(body["model"], &model); err != nil {
			t.Error(err)
		}
		if model != "upstream/model" {
			t.Errorf("model = %q", model)
		}
		if string(body["messages"]) != `[{"role":"system","content":"be concise"},{"role":"user","content":"hi"}]` {
			t.Errorf("messages changed: %s", body["messages"])
		}
		if string(body["seed"]) != "42" || string(body["metadata"]) != `{"request":"kept"}` {
			t.Errorf("extra fields not forwarded: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"upstream/model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()

	g := testGateway(t, upstream, true, Auth{Type: "none"})
	body := `{"model":"public-model","messages":[{"role":"system","content":"be concise"},{"role":"user","content":"hi"}],"seed":42,"metadata":{"request":"kept"}}`
	recorder := httptest.NewRecorder()
	g.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestUpstreamErrorIsTransparent(t *testing.T) {
	wantBody := `{"error":{"message":"quota","type":"insufficient_quota","code":"quota"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "9")
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "must-not-pass")
		w.WriteHeader(http.StatusTooManyRequests)
		if _, err := io.WriteString(w, wantBody); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()

	g := testGateway(t, upstream, true, Auth{Type: "none"})
	recorder := httptest.NewRecorder()
	g.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[]}`)))
	if recorder.Code != http.StatusTooManyRequests || recorder.Body.String() != wantBody {
		t.Fatalf("error changed: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") != "9" || recorder.Header().Get("X-Hop") != "" {
		t.Fatalf("unexpected forwarded headers: %v", recorder.Header())
	}
}

func TestNativeStreamingFlushesAndClientCancellationReachesUpstream(t *testing.T) {
	firstSent := make(chan struct{})
	release := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(canceled)
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: first\n\n"); err != nil {
			t.Error(err)
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		close(firstSent)
		select {
		case <-release:
			if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
				t.Error(err)
			}
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()

	g := testGateway(t, upstream, true, Auth{Type: "none"})
	server := httptest.NewServer(g)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	select {
	case <-firstSent:
	case <-time.After(time.Second):
		t.Fatal("upstream did not send first event")
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first streamed line=%q err=%v", line, err)
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("client cancellation did not reach upstream")
	}
}

func TestSyntheticStreamingProducesCompletionChunks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if bytes.Contains(requestBody, []byte(`"stream_options"`)) || !bytes.Contains(requestBody, []byte(`"stream":false`)) {
			t.Errorf("synthetic request not converted: %s", requestBody)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"id":"c1","object":"chat.completion","created":7,"model":"upstream/model","system_fingerprint":"fp","choices":[{"index":0,"message":{"role":"assistant","content":"answer","tool_calls":[{"id":"call1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()

	g := testGateway(t, upstream, false, Auth{Type: "none"})
	recorder := httptest.NewRecorder()
	g.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[],"stream":true,"stream_options":{"include_usage":true}}`)))
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected stream response: status=%d headers=%v", recorder.Code, recorder.Header())
	}
	frames := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n\n")
	if len(frames) != 4 || frames[3] != "data: [DONE]" {
		t.Fatalf("unexpected SSE frames: %q", frames)
	}
	type chunk struct {
		Usage *struct {
			TotalTokens *int `json:"total_tokens"`
		} `json:"usage"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
			Delta        struct {
				Content   *string `json:"content"`
				Role      *string `json:"role"`
				ToolCalls []struct {
					Index *int `json:"index"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	var first, finish, usage chunk
	for index, target := range []*chunk{&first, &finish, &usage} {
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frames[index], "data: ")), target); err != nil {
			t.Fatal(err)
		}
	}
	if len(first.Choices) == 0 {
		t.Fatalf("first chunk has no choices: %q", frames[0])
	}
	delta := first.Choices[0].Delta
	if delta.Content == nil || *delta.Content != "answer" || delta.Role == nil || *delta.Role != "assistant" {
		t.Fatalf("completion content not reconstructed: %q", frames[0])
	}
	if len(delta.ToolCalls) == 0 || delta.ToolCalls[0].Index == nil || *delta.ToolCalls[0].Index != 0 {
		t.Fatalf("tool call index missing: %q", frames[0])
	}
	if len(finish.Choices) == 0 || finish.Choices[0].FinishReason == nil || *finish.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish reason changed: %q", frames[1])
	}
	if usage.Choices == nil || usage.Usage == nil || usage.Usage.TotalTokens == nil || *usage.Usage.TotalTokens != 5 {
		t.Fatalf("usage chunk invalid: %q", frames[2])
	}
}

func TestSyntheticStreamingPreservesUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		if _, err := io.WriteString(w, `{"error":{"message":"provider down"}}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()

	g := testGateway(t, upstream, false, Auth{Type: "none"})
	recorder := httptest.NewRecorder()
	g.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[],"stream":true}`)))
	if recorder.Code != http.StatusBadGateway || recorder.Header().Get("Content-Type") != "application/json" || strings.Contains(recorder.Body.String(), "data:") {
		t.Fatalf("upstream error incorrectly streamed: status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestGatewayHandlesConcurrentConnections(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, `{"id":"upstream/model","object":"model"}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	g := testGateway(t, upstream, true, Auth{Type: "none"})

	const count = 48
	var wg sync.WaitGroup
	errors := make(chan string, count)
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			g.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models/public-model", http.NoBody))
			if recorder.Code != http.StatusOK {
				errors <- recorder.Body.String()
			}
		}()
	}
	wg.Wait()
	close(errors)
	for message := range errors {
		t.Errorf("concurrent request failed: %s", message)
	}
}

func TestNullableStreamIsForwarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["stream"]) != jsonNull {
			t.Errorf("stream changed: %s", body["stream"])
		}
		if _, err := io.WriteString(w, `{"id":"c1","object":"chat.completion"}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	g := testGateway(t, upstream, true, Auth{Type: "none"})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[],"stream":null}`)))
	if w.Code != 200 {
		t.Fatalf("nullable stream rejected: %d %s", w.Code, w.Body.String())
	}
}

func TestModelListReportsConfiguredModelsWithAvailability(t *testing.T) {
	var receivedAuth, receivedPath string
	served := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth, receivedPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"object":"list","data":[{"id":"upstream/model"},{"id":"unmapped"}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer served.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	down.Close() // a provider that cannot be reached
	c := Config{Providers: []Provider{
		{Name: "local", BaseURL: served.URL, Auth: Auth{Type: "bearer", Token: "secret"}, Models: map[string]string{"public-model": "upstream/model", "missing-model": "upstream/missing"}},
		{Name: "offline", BaseURL: down.URL, Auth: Auth{Type: "none"}, Models: map[string]string{"far-model": "upstream/far"}},
	}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.transport.CloseIdleConnections)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if receivedAuth != "Bearer secret" || receivedPath != "/models" {
		t.Fatalf("provider asked with %q at %q", receivedAuth, receivedPath)
	}
	var got struct {
		Object string      `json:"object"`
		Data   []modelInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []modelInfo{
		{ID: "far-model", Object: "model", OwnedBy: "offline", Upstream: "upstream/far", Available: false},
		{ID: "missing-model", Object: "model", OwnedBy: "local", Upstream: "upstream/missing", Available: false},
		{ID: "public-model", Object: "model", OwnedBy: "local", Upstream: "upstream/model", Available: true},
	}
	if got.Object != "list" || len(got.Data) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got.Data[i] != want[i] {
			t.Errorf("model %d: got %+v, want %+v", i, got.Data[i], want[i])
		}
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Error("the model list exposes a credential")
	}
}

func TestLMStudioModelListUsesLoadedInstancesAndRoutesDiscoveredModels(t *testing.T) {
	var chatModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"models":[{"type":"llm","key":"qwen3-0.6b-mlx","loaded_instances":[]},{"type":"llm","key":"qwen/qwen3.6-35b-a3b","loaded_instances":[{"id":"qwen/qwen3.6-35b-a3b"}]},{"type":"embedding","key":"embed","loaded_instances":[{"id":"embed"}]}]}`); err != nil {
				t.Error(err)
			}
		case "/v1/chat/completions":
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			chatModel = body.Model
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"choices":[]}`); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	c := Config{Providers: []Provider{{
		Name: "lm-studio", BaseURL: upstream.URL + "/v1", Auth: Auth{Type: "none"}, Streaming: true,
		ModelDiscovery: "lm-studio", Models: map[string]string{"small": "qwen3-0.6b-mlx"},
	}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.transport.CloseIdleConnections)

	models := httptest.NewRecorder()
	g.ServeHTTP(models, httptest.NewRequest(http.MethodGet, "/v1/models", http.NoBody))
	if models.Code != http.StatusOK {
		t.Fatalf("model status %d: %s", models.Code, models.Body)
	}
	var list struct {
		Data []modelInfo `json:"data"`
	}
	if err := json.Unmarshal(models.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	want := []modelInfo{
		{ID: "qwen/qwen3.6-35b-a3b", Object: "model", OwnedBy: "lm-studio", Upstream: "qwen/qwen3.6-35b-a3b", Available: true},
		{ID: "small", Object: "model", OwnedBy: "lm-studio", Upstream: "qwen3-0.6b-mlx", Available: false},
	}
	if len(list.Data) != len(want) {
		t.Fatalf("models = %+v, want %+v", list.Data, want)
	}
	for i := range want {
		if list.Data[i] != want[i] {
			t.Errorf("model %d: got %+v, want %+v", i, list.Data[i], want[i])
		}
	}

	chat := httptest.NewRecorder()
	g.ServeHTTP(chat, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.6-35b-a3b","messages":[]}`)))
	if chat.Code != http.StatusOK || chatModel != "qwen/qwen3.6-35b-a3b" {
		t.Fatalf("chat status=%d upstream model=%q body=%s", chat.Code, chatModel, chat.Body)
	}
}

func TestModelListAcceptsOnlyGet(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
	}))
	defer upstream.Close()
	g := testGateway(t, upstream, true, Auth{Type: "none"})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/models", strings.NewReader("{}")))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("status %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestFailureLogIncludesRunCorrelation(t *testing.T) {
	var logs bytes.Buffer
	g, err := newGateway(&Config{Providers: []Provider{{Name: "test", BaseURL: "http://unused.invalid", Auth: Auth{Type: "none"}, Models: map[string]string{"public": "upstream"}}}}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"missing"}`))
	req.Header.Set("X-Trace-Id", "run-123")
	req.Header.Set("X-Step-Id", "review")
	req.Header.Set("X-Agent-Ref", "review/code@v1")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status %d", rec.Code)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["run_id"] != "run-123" || entry["step"] != "review" || entry["agent"] != "review/code@v1" || entry["level"] != "WARN" {
		t.Fatalf("missing correlation: %v", entry)
	}
}

func TestInterruptedUpstreamResponseLogsFailedCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"choices":[]}`); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	g, err := newGateway(&Config{Providers: []Provider{{
		Name: "test", BaseURL: upstream.URL, Auth: Auth{Type: "none"}, Streaming: true,
		Models: map[string]string{"public": "upstream"},
	}}}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public"}`))
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Fatal("interrupted response did not abort the handler")
			}
		}()
		g.ServeHTTP(rec, req)
	}()
	var completion map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var entry map[string]any
		if json.Unmarshal(line, &entry) == nil && entry["msg"] == "request completed" {
			completion = entry
		}
	}
	if completion["level"] != "ERROR" || completion["outcome"] != "failed" || completion["error"] != "response interrupted" {
		t.Fatalf("completion log = %v", completion)
	}
}

func TestChatLogsTheSamplingSettingsItPassesOn(t *testing.T) {
	var received map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"choices":[]}`)); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	var logged bytes.Buffer
	c := Config{Providers: []Provider{{Name: "test", BaseURL: upstream.URL, Auth: Auth{Type: "none"}, Models: map[string]string{"public-model": "upstream/model"}}}}
	g, err := newGateway(&c, slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.transport.CloseIdleConnections)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[],"temperature":0.2,"seed":7}`))
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if string(received["temperature"]) != "0.2" || string(received["seed"]) != "7" {
		t.Errorf("the provider received %s", received)
	}
	var routed map[string]any
	for _, line := range strings.Split(logged.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "request routed" {
			routed = entry
		}
	}
	if _, logged := routed["seed"]; routed["temperature"] != "0.2" || logged {
		t.Errorf("the routed line shows %v", routed)
	}
}
