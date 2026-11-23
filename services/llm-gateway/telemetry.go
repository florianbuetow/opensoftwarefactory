package main

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	telemetryQueueSize = 512
	telemetryBatchSize = 128
)

type TelemetryConfig struct {
	Endpoint string `yaml:"endpoint"`
	Insecure bool   `yaml:"insecure"`
}

func initTelemetry(ctx context.Context, config TelemetryConfig) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	endpoint := strings.TrimSpace(config.Endpoint)
	if endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}
	if err := validateTelemetryEndpoint(endpoint); err != nil {
		return nil, err
	}

	exporterOptions := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(endpoint),
	}
	if config.Insecure {
		exporterOptions = append(exporterOptions, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, exporterOptions...)
	if err != nil {
		return nil, err
	}

	serviceResource, err := resource.New(
		ctx,
		resource.WithAttributes(attribute.String("service.name", "llm-gateway")),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(serviceResource),
		sdktrace.WithBatcher(
			exporter,
			sdktrace.WithMaxQueueSize(telemetryQueueSize),
			sdktrace.WithMaxExportBatchSize(telemetryBatchSize),
		),
	)
	otel.SetTracerProvider(provider)

	return provider.Shutdown, nil
}

func validateTelemetryEndpoint(endpoint string) error {
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("telemetry endpoint must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	return nil
}
