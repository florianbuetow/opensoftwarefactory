package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()
	base := os.Getenv("BLOBSTORE_URL")
	if base == "" {
		base = "http://blobstore:8080"
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	shutdownTelemetry, err := initTelemetry(context.Background(), "mcp-gateway")
	if err != nil {
		return fmt.Errorf("initializing telemetry: %w", err)
	}
	defer func() {
		drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(drain); err != nil {
			log.Warn("telemetry shutdown failed", "error", err)
		}
	}()
	tracedHTTP := &http.Client{Timeout: 10 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}
	g := newGateway(blobStore{base, tracedHTTP}, log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Blobstore may start after this deployment. Failed startup never creates an
	// empty writable configuration over an unavailable source of truth.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := g.load(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return err
		}
		log.Warn("waiting for blobstore", "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	defer g.close()
	srv := &http.Server{Addr: *addr, Handler: otelhttp.NewHandler(g.routes(), "mcp-gateway.http"), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	result := make(chan error, 1)
	go func() { log.Info("MCP gateway listening", "address", *addr); result <- srv.ListenAndServe() }()
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(drain); err != nil {
			_ = srv.Close()
			return err
		}
	}
	return nil
}
