// Command agent is the step service on Temporal task queue "agent". It runs
// one pinned agent definition from the agent registry against the LLM
// gateway and executes tool calls through the MCP gateway.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	temporalotel "go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
)

const taskQueue = "agent"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	// Load the token encoder now, so the first model call does not wait for it.
	tokenEncoder()
	shutdownTelemetry, err := initTelemetry(context.Background(), "agent")
	if err != nil {
		return fmt.Errorf("initializing telemetry: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(ctx); err != nil {
			logger.Warn("telemetry shutdown failed", "error", err)
		}
	}()
	tracedHTTP := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	svc := &Service{
		Store:             HTTPStore{BaseURL: env("BLOB_URL", "http://blobstore:8080/blobs"), Client: tracedHTTP},
		RegistryURL:       env("AGENT_REGISTRY_URL", "http://agent-registry:8080"),
		PolicyRegistryURL: env("POLICY_REGISTRY_URL", "http://policy-registry:8080"),
		LLMURL:            env("LLM_URL", "http://llm-gateway:8080/v1"),
		EvaluationURL:     env("EVALUATION_URL", "http://evaluation:8080"),
		MCPGatewayURL:     env("MCP_GATEWAY_URL", "http://mcp-gateway:8080"),
		HTTP:              tracedHTTP,
		Logger:            logger,
	}
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	defer serviceCancel()
	replayCtx, replayCancel := context.WithTimeout(serviceCtx, 20*time.Second)
	if err := svc.reconcileExtractionIntents(replayCtx, 100); err != nil {
		logger.Warn("judge extraction reconciliation incomplete", "error", err)
	}
	if err := svc.reconcileCompletedRunObligations(replayCtx, 100); err != nil {
		logger.Warn("completed judge extraction reconciliation incomplete", "error", err)
	}
	if err := svc.replayExtractionIntents(replayCtx); err != nil {
		logger.Warn("judge extraction replay incomplete", "error", err)
	}
	replayCancel()
	go svc.replayExtractionLoop(serviceCtx)
	// The chat API serves interactive sessions next to the worker and stops with it.
	chatServer := &http.Server{Addr: env("CHAT_ADDR", ":8080"), Handler: otelhttp.NewHandler(svc.routes(), "agent.http"), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	stop := make(chan interface{}, 2)
	chatFailed := make(chan error, 1)
	go func() {
		logger.Info("agent chat listening", "address", chatServer.Addr)
		if err := chatServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			chatFailed <- err
			stop <- nil
		}
	}()
	defer func() {
		drain, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := chatServer.Shutdown(drain); err != nil {
			logger.Warn("chat server shutdown failed", "error", err)
		}
	}()
	c, err := dial()
	if err != nil {
		return fmt.Errorf("connecting to temporal: %w", err)
	}
	defer c.Close()
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterActivityWithOptions(svc.Process, activity.RegisterOptions{Name: "Process"})
	w.RegisterActivityWithOptions(svc.TeamTurn, activity.RegisterOptions{Name: "TeamTurn"})
	logger.Info("agent worker started", "task_queue", taskQueue)
	go func() { stop <- <-worker.InterruptCh() }()
	if err := w.Run(stop); err != nil {
		return err
	}
	select {
	case err := <-chatFailed:
		return fmt.Errorf("chat server: %w", err)
	default:
		return nil
	}
}

// dial connects to Temporal, retrying for up to two minutes while it starts.
func dial() (client.Client, error) {
	tracing, err := temporalotel.NewTracingInterceptor(temporalotel.TracerOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating tracing interceptor: %w", err)
	}
	opts := client.Options{
		HostPort:     env("TEMPORAL_ADDRESS", "temporal:7233"),
		Namespace:    env("TEMPORAL_NAMESPACE", "default"),
		Interceptors: []interceptor.ClientInterceptor{tracing},
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		c, err := client.Dial(opts)
		if err == nil || time.Now().After(deadline) {
			return c, err
		}
		time.Sleep(2 * time.Second)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
