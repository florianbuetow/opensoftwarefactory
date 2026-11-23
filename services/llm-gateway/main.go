package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (err error) {
	configPath := flag.String("config", "/config/config.yaml", "configuration file")
	logsPath := flag.String("logs", "/logs", "log directory")
	flag.Parse()
	c, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if c.BlobstoreURL == "" {
		c.BlobstoreURL = env("BLOBSTORE_URL", "http://blobstore:8080")
	}
	if c.EvaluationURL == "" {
		c.EvaluationURL = env("EVALUATION_URL", "http://evaluation:8080")
	}
	if c.Telemetry.Endpoint == "" {
		c.Telemetry.Endpoint = os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
		c.Telemetry.Insecure = strings.HasPrefix(c.Telemetry.Endpoint, "http://")
	}
	var level slog.Level
	if err = level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return fmt.Errorf("log_level must be DEBUG, INFO, WARN or ERROR")
	}
	if level != slog.LevelDebug && level != slog.LevelInfo && level != slog.LevelWarn && level != slog.LevelError {
		return fmt.Errorf("unsupported log_level")
	}
	err = os.MkdirAll(*logsPath, 0o700)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(*logsPath, "gateway.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = closeAfter(err, f) }()
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, f), &slog.HandlerOptions{Level: level}))
	shutdown, err := initTelemetry(context.Background(), c.Telemetry)
	if err != nil {
		return fmt.Errorf("telemetry initialization failed")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := shutdown(ctx); shutdownErr != nil {
			logger.Warn("telemetry shutdown failed")
		}
	}()
	gateway, err := newGateway(&c, logger)
	if err != nil {
		return err
	}
	replayCtx, replayCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if replayErr := gateway.replayUsageOutbox(replayCtx); replayErr != nil {
		logger.Warn("usage outbox replay incomplete", "error", replayErr)
	}
	replayCancel()
	defer gateway.transport.CloseIdleConnections()
	server := &http.Server{Addr: env("GATEWAY_ADDR", ":8080"), Handler: gateway, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go gateway.replayUsageOutboxLoop(ctx)
	result := make(chan error, 1)
	go func() { logger.Info("gateway listening", "address", server.Addr); result <- server.ListenAndServe() }()
	select {
	case err = <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("gateway stopping")
		drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(drain); err != nil {
			return errors.Join(err, server.Close())
		}
	}
	return nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
