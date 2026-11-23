package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Gateway struct {
	routes         map[string]route
	providers      []*Provider
	transport      *http.Transport
	log            *slog.Logger
	blobstoreURL   string
	evaluationURL  string
	evidenceClient *http.Client
	replayMu       sync.Mutex
	replayOffset   int
}

func newGateway(c Config, l *slog.Logger) (*Gateway, error) {
	routes, err := routes(&c)
	if err != nil {
		return nil, err
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 16
	// Compression must not obscure upstream bodies or SSE framing.
	t.DisableCompression = true
	providers := make([]*Provider, len(c.Providers))
	for i := range c.Providers {
		providers[i] = &c.Providers[i]
	}
	return &Gateway{
		routes: routes, providers: providers, transport: t, log: l,
		blobstoreURL: strings.TrimRight(c.BlobstoreURL, "/"), evaluationURL: strings.TrimRight(c.EvaluationURL, "/"),
		evidenceClient: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// resolveRoute keeps configured aliases authoritative, then allows explicitly enabled providers
// to route a currently loaded model by its upstream instance ID. A duplicate ID is ambiguous and
// therefore remains unavailable.
func (g *Gateway) resolveRoute(ctx context.Context, model string) (route, bool) {
	if configured, ok := g.routes[model]; ok {
		return configured, true
	}
	var found route
	matches := 0
	for _, p := range g.providers {
		if p.ModelDiscovery != "lm-studio" {
			continue
		}
		catalog := g.providerModels(ctx, p)
		if catalog.loaded[model] && catalog.discovered[model] {
			found = route{provider: p, model: model}
			matches++
		}
	}
	return found, matches == 1
}

func apiError(w http.ResponseWriter, status int, message, kind string) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": nil}})
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	status := http.StatusOK
	provider := ""
	operation := "unknown"
	requestError := ""
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, span := otel.Tracer("llm-gateway").Start(ctx, "llm-gateway", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()
	correlation := []any{"run_id", r.Header.Get("X-Trace-Id"), "step", r.Header.Get("X-Step-Id"), "agent", r.Header.Get("X-Agent-Ref"), "trace_id", span.SpanContext().TraceID().String(), "span_id", span.SpanContext().SpanID().String()}
	requestLog := g.log.With(correlation...)
	span.SetAttributes(attribute.String("factory.run_id", r.Header.Get("X-Trace-Id")), attribute.String("factory.execution_id", r.Header.Get("X-Execution-Id")), attribute.String("factory.parent_execution_id", r.Header.Get("X-Parent-Execution-Id")), attribute.String("factory.step", r.Header.Get("X-Step-Id")), attribute.String("factory.agent", r.Header.Get("X-Agent-Ref")))
	defer func() {
		span.SetAttributes(attribute.Int("http.response.status_code", status), attribute.String("provider", provider))
		if status >= 400 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		} else if status >= 400 {
			level = slog.LevelWarn
		}
		attrs := []any{"operation", operation, "provider", provider, "status", status, "duration_ms", time.Since(started).Milliseconds()}
		if requestError != "" {
			level = slog.LevelError
			attrs = append(attrs, "outcome", "failed", "error", requestError)
		}
		requestLog.Log(ctx, level, "request completed", attrs...)
	}()
	fail := func(s int, msg, kind string) {
		status = s
		if writeErr := apiError(w, s, msg, kind); writeErr != nil {
			requestLog.ErrorContext(ctx, "error response write failed", "error", writeErr)
		}
	}
	var path string
	var body io.Reader
	var selected route
	var streaming, synthetic, includeUsage bool
	var requestedModel, effort string
	modes := map[string]string{}
	requestSettings := map[string]string{}
	var sampling []any // temperature of a chat request, as passed on to the provider
	switch {
	case r.URL.Path == "/v1/chat/completions":
		operation = "chat.completions"
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			fail(405, "Method not allowed", "invalid_request_error")
			return
		}
		var request map[string]json.RawMessage
		d := json.NewDecoder(r.Body)
		if err := d.Decode(&request); err != nil || request == nil {
			fail(400, "Invalid JSON request", "invalid_request_error")
			return
		}
		var extra any
		if err := d.Decode(&extra); err != io.EOF {
			fail(400, "Expected one JSON object", "invalid_request_error")
			return
		}
		var model string
		if json.Unmarshal(request["model"], &model) != nil || model == "" {
			fail(400, "A model is required", "invalid_request_error")
			return
		}
		requestedModel = model
		if raw, ok := request["reasoning_effort"]; ok {
			if json.Unmarshal(raw, &effort) != nil || strings.TrimSpace(effort) == "" {
				fail(400, "reasoning_effort must be a non-empty string", "invalid_request_error")
				return
			}
		}
		var ok bool
		selected, ok = g.resolveRoute(ctx, model)
		if !ok {
			fail(404, "Model not configured", "invalid_request_error")
			return
		}
		if raw, ok := request["stream"]; ok {
			if json.Unmarshal(raw, &streaming) != nil {
				fail(400, "stream must be a boolean", "invalid_request_error")
				return
			}
		}
		synthetic = streaming && !selected.provider.Streaming
		switch {
		case synthetic:
			requestSettings["stream"] = "synthetic"
		case streaming:
			requestSettings["stream"] = "native"
		default:
			requestSettings["stream"] = "none"
		}
		if raw, ok := request["temperature"]; ok && string(raw) != "null" {
			requestSettings["temperature"] = string(raw)
		}
		if raw, ok := request["service_tier"]; ok && string(raw) != "null" {
			var tier string
			if json.Unmarshal(raw, &tier) != nil || strings.TrimSpace(tier) == "" {
				fail(400, "service_tier must be a non-empty string", "invalid_request_error")
				return
			}
			modes["service_tier"] = tier
		}
		if raw, ok := request["modes"]; ok && string(raw) != "null" {
			var supplied map[string]string
			if json.Unmarshal(raw, &supplied) != nil || supplied == nil {
				fail(400, "modes must be an object of string values", "invalid_request_error")
				return
			}
			for key, value := range supplied {
				if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
					fail(400, "modes keys and values must be non-empty strings", "invalid_request_error")
					return
				}
				modes[key] = value
			}
		}
		if synthetic {
			if raw, ok := request["stream_options"]; ok && string(raw) != "null" {
				var options struct {
					IncludeUsage bool `json:"include_usage"`
				}
				if json.Unmarshal(raw, &options) != nil {
					fail(400, "Invalid stream_options", "invalid_request_error")
					return
				}
				includeUsage = options.IncludeUsage
			}
			request["stream"] = json.RawMessage("false")
			delete(request, "stream_options")
		}
		encodedModel, err := json.Marshal(selected.model)
		if err != nil {
			fail(500, "Unable to encode upstream model", "api_error")
			return
		}
		request["model"] = encodedModel
		encoded, err := json.Marshal(request)
		if err != nil {
			fail(400, "Invalid JSON request", "invalid_request_error")
			return
		}
		// The temperature in the body that goes to the provider, for the log: it decides how
		// a run can be compared with another, so the log shows that it was passed on.
		if raw, ok := request["temperature"]; ok {
			sampling = append(sampling, "temperature", string(raw))
		}
		body = bytes.NewReader(encoded)
		path = "/chat/completions"
	case r.URL.Path == "/v1/models":
		operation = "models.list"
		span.SetName(operation)
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			fail(405, "Method not allowed", "invalid_request_error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if writeErr := json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": g.listModels(ctx)}); writeErr != nil {
			requestLog.ErrorContext(ctx, "model list write failed", "error", writeErr)
		}
		return
	case strings.HasPrefix(r.URL.Path, "/v1/models/"):
		operation = "models.retrieve"
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			fail(405, "Method not allowed", "invalid_request_error")
			return
		}
		model := strings.TrimPrefix(r.URL.Path, "/v1/models/")
		var ok bool
		selected, ok = g.resolveRoute(ctx, model)
		if !ok {
			fail(404, "Model not configured", "invalid_request_error")
			return
		}
		path = "/models/" + url.PathEscape(selected.model)
	default:
		fail(404, "Not found", "invalid_request_error")
		return
	}
	span.SetName(operation)
	provider = selected.provider.Name
	requestLog.DebugContext(ctx, "request routed", append([]any{"operation", operation, "provider", provider, "stream", streaming, "synthetic_stream", synthetic}, sampling...)...)
	endpoint := selected.provider.BaseURL + path
	if r.URL.RawQuery != "" {
		endpoint += "?" + r.URL.RawQuery
	}
	upstream, err := http.NewRequestWithContext(ctx, r.Method, endpoint, body)
	if err != nil {
		fail(502, "Unable to construct upstream request", "api_error")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "application/json")
	if streaming && !synthetic {
		upstream.Header.Set("Accept", "text/event-stream")
	}
	authorize(upstream, selected.provider)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(upstream.Header))
	var attemptID string
	requestedAt := time.Now().UTC()
	if operation == "chat.completions" {
		attemptID, err = newAttemptID()
		if err != nil {
			fail(500, "Unable to allocate upstream attempt", "api_error")
			return
		}
	}
	var usageCtx usageContext
	if attemptID != "" {
		usageCtx = usageContextFromRequest(r, span, attemptID, requestedAt, selected, requestedModel, effort, modes, requestSettings)
		if beginErr := g.beginUsageAttempt(ctx, usageCtx); beginErr != nil {
			fail(503, "Usage evidence unavailable", "api_error")
			return
		}
	}
	response, err := g.transport.RoundTrip(upstream)
	if err != nil {
		if attemptID != "" {
			g.completeUsage(ctx, usageCtx, nil, false)
		}
		fail(502, "Upstream connection failed", "api_error")
		return
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			requestLog.ErrorContext(ctx, "upstream response close failed", "provider", provider, "error", closeErr)
		}
	}()
	if attemptID != "" {
		w.Header().Set("X-Usage-Measurement-Id", attemptID)
	}
	status = response.StatusCode
	if synthetic && status >= 200 && status < 300 {
		completion, readErr := readCompletion(response.Body)
		if readErr != nil {
			g.completeUsage(ctx, usageCtx, nil, false)
			fail(502, "Invalid upstream completion", "api_error")
			return
		}
		copyHeaders(w.Header(), response.Header)
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Encoding")
		w.Header().Del("ETag")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(status)
		if err = writeCompletionStream(w, completion, includeUsage); err != nil {
			g.completeUsage(ctx, usageCtx, completion.Usage, false)
			requestError = "stream interrupted"
			span.SetStatus(codes.Error, "stream interrupted")
			requestLog.WarnContext(ctx, "stream interrupted", "provider", provider)
			panic(http.ErrAbortHandler)
		}
		g.completeUsage(ctx, usageCtx, completion.Usage, true)
		return
	}
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(status)
	var dst io.Writer = w
	if streaming && status >= 200 && status < 300 {
		dst = flushWriter{w}
		if flushErr := http.NewResponseController(w).Flush(); flushErr != nil {
			requestLog.ErrorContext(ctx, "response flush failed", "provider", provider, "error", flushErr)
		}
	}
	observer := newUsageObserver(streaming)
	if _, err = io.Copy(dst, io.TeeReader(response.Body, observer)); err != nil {
		if attemptID != "" {
			g.completeUsage(ctx, usageCtx, observer.Usage(), false)
		}
		requestError = "response interrupted"
		span.SetStatus(codes.Error, "response interrupted")
		requestLog.WarnContext(ctx, "response interrupted", "provider", provider)
		panic(http.ErrAbortHandler)
	}
	if attemptID != "" {
		g.completeUsage(ctx, usageCtx, observer.Usage(), true)
	}
}

// authorize sets the provider's credentials; authentication comes exclusively from the
// provider configuration, never from the caller.
func authorize(r *http.Request, p *Provider) {
	switch p.Auth.Type {
	case "bearer":
		r.Header.Set("Authorization", "Bearer "+p.Auth.Token)
	case "basic":
		r.SetBasicAuth(p.Auth.Username, p.Auth.Password)
	}
}

// modelInfo is one configured model in GET /v1/models: the name callers use, the provider
// and upstream model it maps to, and whether that provider lists the upstream model now.
type modelInfo struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	OwnedBy   string `json:"owned_by"`
	Upstream  string `json:"upstream"`
	Available bool   `json:"available"`
}

type providerCatalog struct {
	loaded     map[string]bool
	discovered map[string]bool
}

// listModels asks each provider once which models it serves (in parallel), reports every
// configured model, and includes loaded models from providers with explicit discovery enabled.
func (g *Gateway) listModels(ctx context.Context) []modelInfo {
	served := make(map[*Provider]providerCatalog, len(g.providers))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range g.providers {
		wg.Add(1)
		go func(p *Provider) {
			defer wg.Done()
			models := g.providerModels(ctx, p)
			mu.Lock()
			served[p] = models
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	out := make([]modelInfo, 0, len(g.routes))
	mapped := make(map[*Provider]map[string]bool, len(g.providers))
	for alias, rt := range g.routes {
		if mapped[rt.provider] == nil {
			mapped[rt.provider] = make(map[string]bool)
		}
		mapped[rt.provider][rt.model] = true
		out = append(out, modelInfo{ID: alias, Object: "model", OwnedBy: rt.provider.Name, Upstream: rt.model, Available: served[rt.provider].loaded[rt.model]})
	}
	candidates := make(map[string][]*Provider)
	for _, p := range g.providers {
		for model := range served[p].discovered {
			if _, configured := g.routes[model]; configured || mapped[p][model] {
				continue
			}
			candidates[model] = append(candidates[model], p)
		}
	}
	for model, providers := range candidates {
		if len(providers) != 1 {
			continue
		}
		p := providers[0]
		out = append(out, modelInfo{ID: model, Object: "model", OwnedBy: p.Name, Upstream: model, Available: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// providerModels lists the upstream models a provider serves now, at most three seconds. LM
// Studio discovery uses its native model API because OpenAI /v1/models may include unloaded
// downloads while just-in-time loading is enabled.
func (g *Gateway) providerModels(ctx context.Context, p *Provider) providerCatalog {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	endpoint := p.BaseURL + "/models"
	if p.ModelDiscovery == "lm-studio" {
		u, err := url.Parse(p.BaseURL)
		if err != nil {
			return providerCatalog{}
		}
		u.Path, u.RawPath = "/api/v1/models", ""
		endpoint = u.String()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return providerCatalog{}
	}
	req.Header.Set("Accept", "application/json")
	authorize(req, p)
	res, err := g.transport.RoundTrip(req)
	if err != nil {
		g.log.WarnContext(ctx, "model list unavailable", "provider", p.Name)
		return providerCatalog{}
	}
	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil {
			g.log.ErrorContext(ctx, "model list close failed", "provider", p.Name, "error", closeErr)
		}
	}()
	if res.StatusCode != http.StatusOK {
		g.log.WarnContext(ctx, "model list unavailable", "provider", p.Name, "status", res.StatusCode)
		return providerCatalog{}
	}
	if p.ModelDiscovery == "lm-studio" {
		var list struct {
			Models []struct {
				Type            string `json:"type"`
				Key             string `json:"key"`
				LoadedInstances []struct {
					ID string `json:"id"`
				} `json:"loaded_instances"`
			} `json:"models"`
		}
		if json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&list) != nil {
			g.log.WarnContext(ctx, "model list unavailable", "provider", p.Name, "status", res.StatusCode)
			return providerCatalog{}
		}
		out := providerCatalog{loaded: make(map[string]bool), discovered: make(map[string]bool)}
		for _, m := range list.Models {
			if m.Type != "llm" || len(m.LoadedInstances) == 0 {
				continue
			}
			out.loaded[m.Key] = true
			for _, instance := range m.LoadedInstances {
				if instance.ID != "" {
					out.loaded[instance.ID] = true
					out.discovered[instance.ID] = true
				}
			}
		}
		return out
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&list) != nil {
		g.log.WarnContext(ctx, "model list unavailable", "provider", p.Name, "status", res.StatusCode)
		return providerCatalog{}
	}
	out := providerCatalog{loaded: make(map[string]bool)}
	for _, m := range list.Data {
		out.loaded[m.ID] = true
	}
	return out
}

func copyHeaders(dst, src http.Header) {
	excluded := map[string]bool{"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true}
	for _, v := range src.Values("Connection") {
		for _, h := range strings.Split(v, ",") {
			excluded[http.CanonicalHeaderKey(strings.TrimSpace(h))] = true
		}
	}
	for k, values := range src {
		if !excluded[http.CanonicalHeaderKey(k)] {
			dst[k] = append([]string(nil), values...)
		}
	}
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = http.NewResponseController(f.w).Flush()
	}
	return n, err
}

// Only the non-streaming fallback buffers a completed response. Native SSE is copied incrementally.
type completion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int                        `json:"index"`
		Message      map[string]json.RawMessage `json:"message"`
		FinishReason json.RawMessage            `json:"finish_reason"`
		Logprobs     json.RawMessage            `json:"logprobs"`
	} `json:"choices"`
	Usage             json.RawMessage `json:"usage"`
	SystemFingerprint json.RawMessage `json:"system_fingerprint"`
	ServiceTier       json.RawMessage `json:"service_tier"`
}

func readCompletion(r io.Reader) (completion, error) {
	var c completion
	d := json.NewDecoder(r)
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("extra response data")
	}
	if c.ID == "" || c.Model == "" || c.Object != "chat.completion" || len(c.Choices) == 0 {
		return c, fmt.Errorf("missing completion fields")
	}
	for i := range c.Choices {
		choice := &c.Choices[i]
		if choice.Message == nil {
			return c, fmt.Errorf("missing message")
		}
		if raw, ok := choice.Message["tool_calls"]; ok && string(raw) != "null" {
			var calls []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &calls); err != nil {
				return c, err
			}
			for j := range calls {
				if calls[j] == nil {
					return c, fmt.Errorf("invalid tool call")
				}
				index, err := json.Marshal(j)
				if err != nil {
					return c, err
				}
				calls[j]["index"] = index
			}
			encodedCalls, err := json.Marshal(calls)
			if err != nil {
				return c, err
			}
			choice.Message["tool_calls"] = encodedCalls
		}
	}
	return c, nil
}

func writeCompletionStream(w http.ResponseWriter, c completion, includeUsage bool) error {
	chunk := map[string]any{"id": c.ID, "object": "chat.completion.chunk", "created": c.Created, "model": c.Model}
	if c.SystemFingerprint != nil {
		chunk["system_fingerprint"] = c.SystemFingerprint
	}
	if c.ServiceTier != nil {
		chunk["service_tier"] = c.ServiceTier
	}
	if includeUsage {
		chunk["usage"] = nil
	}
	deltas := make([]map[string]any, 0, len(c.Choices))
	ends := make([]map[string]any, 0, len(c.Choices))
	for _, choice := range c.Choices {
		deltas = append(deltas, map[string]any{"index": choice.Index, "delta": choice.Message, "finish_reason": nil, "logprobs": choice.Logprobs})
		ends = append(ends, map[string]any{"index": choice.Index, "delta": map[string]any{}, "finish_reason": choice.FinishReason, "logprobs": nil})
	}
	emit := func() error {
		b, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(flushWriter{w}, "data: %s\n\n", b)
		return err
	}
	chunk["choices"] = deltas
	if err := emit(); err != nil {
		return err
	}
	chunk["choices"] = ends
	if err := emit(); err != nil {
		return err
	}
	if includeUsage {
		chunk["choices"] = []any{}
		chunk["usage"] = c.Usage
		if err := emit(); err != nil {
			return err
		}
	}
	_, err := io.WriteString(flushWriter{w}, "data: [DONE]\n\n")
	return err
}
