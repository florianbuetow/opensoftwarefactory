package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

func TestInitTelemetryDisabled(t *testing.T) {
	shutdown, err := initTelemetry(context.Background(), TelemetryConfig{})
	if err != nil {
		t.Fatalf("initTelemetry() error = %v", err)
	}

	_, span := otel.Tracer("test").Start(context.Background(), "disabled")
	if span.IsRecording() {
		t.Fatal("disabled telemetry created a recording span")
	}
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestInitTelemetryFlushesSpanOnShutdown(t *testing.T) {
	const sensitiveValue = "must-not-be-exported"

	requests := make(chan []byte, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		requests <- body
		response.Header().Set("Content-Type", "application/x-protobuf")
		response.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	shutdown, err := initTelemetry(context.Background(), TelemetryConfig{
		Endpoint: collector.URL + "/" + sensitiveValue,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("initTelemetry() error = %v", err)
	}

	_, span := otel.Tracer("test").Start(context.Background(), "gateway.request")
	span.End()

	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownContext); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}

	select {
	case body := <-requests:
		if len(body) == 0 {
			t.Fatal("collector received an empty OTLP request")
		}
		if !bytes.Contains(body, []byte("llm-gateway")) {
			t.Fatal("OTLP request does not contain the configured service name")
		}
		if bytes.Contains(body, []byte(sensitiveValue)) {
			t.Fatal("OTLP request contains a sensitive value")
		}
	default:
		t.Fatal("collector did not receive an OTLP request")
	}
}

func TestInitTelemetryRejectsEndpointCredentialsWithoutEchoingThem(t *testing.T) {
	const (
		username = "collector-user"
		password = "collector-password"
	)

	shutdown, err := initTelemetry(context.Background(), TelemetryConfig{
		Endpoint: "https://" + username + ":" + password + "@collector.example/v1/traces",
	})
	if err == nil {
		if shutdown != nil {
			if handlerErr := shutdown(context.Background()); handlerErr != nil {
				t.Error(handlerErr)
			}
		}
		t.Fatal("initTelemetry() accepted credentials in the endpoint URL")
	}
	if strings.Contains(err.Error(), username) || strings.Contains(err.Error(), password) {
		t.Fatalf("initTelemetry() error exposed endpoint credentials: %v", err)
	}
}
