package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestHTTPSpansContinueIncomingTraceOnSuccessAndError(t *testing.T) {
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

	handler, err := newHandler(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(otelhttp.NewHandler(handler, "blobstore"))
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/healthz", want: http.StatusOK},
		{path: "/blobs/missing", want: http.StatusNotFound},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+tc.path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err = resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.want {
			t.Fatalf("GET %s: status %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}

	ended := recorder.Ended()
	if len(ended) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(ended))
	}
	wantTraceID, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	wantParentID, _ := trace.SpanIDFromHex("0123456789abcdef")
	for _, span := range ended {
		if span.SpanContext().TraceID() != wantTraceID || span.Parent().SpanID() != wantParentID {
			t.Errorf("span %q did not continue incoming trace: trace=%s parent=%s", span.Name(), span.SpanContext().TraceID(), span.Parent().SpanID())
		}
	}
}
