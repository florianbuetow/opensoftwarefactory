package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestHTTPSpansContinueIncomingTraceAndPropagateToBlobstore(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		_ = provider.Shutdown(context.Background())
	})

	blobs := newFakeBlobstore()
	var propagated string
	blobServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		propagated = r.Header.Get("traceparent")
		blobs.ServeHTTP(w, r)
	}))
	t.Cleanup(blobServer.Close)
	reg := &registry{
		prefix: "agents",
		files:  []string{"systemprompt.md", "agent.json"},
		blobs:  blobClient{base: blobServer.URL, client: tracedHTTPClient()},
		logger: slog.New(slog.DiscardHandler),
	}
	server := httptest.NewServer(otelhttp.NewHandler(reg.routes(), "registry"))
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/agents", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	const incomingTrace = "0123456789abcdef0123456789abcdef"
	const incomingParent = "0123456789abcdef"
	req.Header.Set("traceparent", "00-"+incomingTrace+"-"+incomingParent+"-01")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /agents: status %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if propagated == "" {
		t.Fatal("blobstore request did not propagate traceparent")
	}
	parts := strings.Split(propagated, "-")
	if len(parts) != 4 || parts[1] != incomingTrace || parts[2] == incomingParent {
		t.Fatalf("blobstore traceparent %q does not continue incoming trace with a client span", propagated)
	}

	ended := recorder.Ended()
	var serverSpan, clientSpan sdktrace.ReadOnlySpan
	for _, span := range ended {
		switch span.SpanKind() {
		case trace.SpanKindServer:
			serverSpan = span
		case trace.SpanKindClient:
			clientSpan = span
		case trace.SpanKindUnspecified, trace.SpanKindInternal, trace.SpanKindProducer, trace.SpanKindConsumer:
			// Only the server span and the client span are asserted on.
		}
	}
	if serverSpan == nil || clientSpan == nil {
		t.Fatalf("expected server and client spans; got %d ended spans", len(ended))
	}
	wantTraceID, _ := trace.TraceIDFromHex(incomingTrace)
	wantParentID, _ := trace.SpanIDFromHex(incomingParent)
	if serverSpan.SpanContext().TraceID() != wantTraceID || serverSpan.Parent().SpanID() != wantParentID {
		t.Errorf("server span %q did not continue incoming trace", serverSpan.Name())
	}
	if clientSpan.SpanContext().TraceID() != wantTraceID || clientSpan.Parent().SpanID() != serverSpan.SpanContext().SpanID() {
		t.Errorf("client span %q is not a child of registry server span", clientSpan.Name())
	}
}
