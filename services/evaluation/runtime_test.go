package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriterLockExcludesSecondProcessOwner(t *testing.T) {
	if _, err := acquireWriterLock(""); err == nil {
		t.Fatal("empty lock path accepted")
	}
	var nilLock *writerLock
	if err := nilLock.Close(); err != nil {
		t.Fatalf("nil lock close: %v", err)
	}
	path := filepath.Join(t.TempDir(), "writer.lock")
	first, err := acquireWriterLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, secondErr := acquireWriterLock(path); secondErr == nil {
		_ = second.Close()
		t.Fatal("second writer acquired lock")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := acquireWriterLock(path)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	_ = third.Close()
}

func TestProjectionStreamValidation(t *testing.T) {
	for _, name := range []string{"factory_measurements", "smoke_123_abc"} {
		if !validProjectionStream(name) {
			t.Fatalf("valid stream rejected %q", name)
		}
	}
	for _, name := range []string{"", "has/slash", "has space", ".hidden", "Uppercase", "1leading"} {
		if validProjectionStream(name) {
			t.Fatalf("invalid stream accepted %q", name)
		}
	}
}

func TestOpenObserveCredentialsParsing(t *testing.T) {
	if _, _, err := openObserveCredentials(filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Fatal("missing file succeeded")
	}
	incomplete := filepath.Join(t.TempDir(), "incomplete.env")
	if err := os.WriteFile(incomplete, []byte("# comment\nZO_ROOT_USER_EMAIL=root@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openObserveCredentials(incomplete); err == nil {
		t.Fatal("missing password succeeded")
	}
	valid := filepath.Join(t.TempDir(), "valid.env")
	content := "# OpenObserve\n\nNO_EQUALS_LINE\nZO_ROOT_USER_EMAIL=admin@example.com\nZO_ROOT_USER_PASSWORD=secret\n"
	if err := os.WriteFile(valid, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	user, pass, err := openObserveCredentials(valid)
	if err != nil || user != "admin@example.com" || pass != "secret" {
		t.Fatalf("got (%q, %q, %v)", user, pass, err)
	}
}

func TestInitTelemetryAndEnv(t *testing.T) {
	t.Setenv("EVALUATION_TEST_ENV", "")
	if got := env("EVALUATION_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("env default = %q", got)
	}
	t.Setenv("EVALUATION_TEST_ENV", "custom")
	if got := env("EVALUATION_TEST_ENV", "fallback"); got != "custom" {
		t.Fatalf("env set = %q", got)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	shutdown, err := initTelemetry(t.Context())
	if err != nil {
		t.Fatalf("noop telemetry: %v", err)
	}
	if err = shutdown(t.Context()); err != nil {
		t.Fatalf("noop shutdown: %v", err)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:4318/v1/traces")
	shutdown, err = initTelemetry(t.Context())
	if err != nil {
		t.Fatalf("http telemetry: %v", err)
	}
	if err = shutdown(t.Context()); err != nil {
		t.Fatalf("http shutdown: %v", err)
	}
}

type failingCloser struct{ err error }

func (f failingCloser) Close() error { return f.err }

func TestCloseAfterJoinsCloseError(t *testing.T) {
	base := errors.New("base")
	closeErr := errors.New("close")
	joined := closeAfter(base, failingCloser{err: closeErr})
	if !errors.Is(joined, base) || !errors.Is(joined, closeErr) {
		t.Fatalf("closeAfter = %v", joined)
	}
}

func TestProjectPendingBoundsDirtyHoursAndRespectsCancel(t *testing.T) {
	f, blob, orch := newFixture(t)
	now := time.Now().UTC().Truncate(time.Hour)
	hours := []time.Time{now.Add(-3 * time.Hour), now.Add(-2 * time.Hour), now}
	f.mu.Lock()
	for i, h := range hours {
		op := mutationOperation{
			SchemaVersion: 1,
			ID:            "op-" + string(rune('a'+i)),
			MeasurementID: "m",
			AffectedHour:  h,
			Generation:    int64(i + 1),
			Phase:         opProjectionPending,
		}
		f.objects[operationPrefix+"/"+op.ID+".json"], _ = json.Marshal(op)
	}
	f.mu.Unlock()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	if err := s.projectPending(t.Context()); err != nil {
		t.Fatalf("projectPending with current hour: %v", err)
	}

	// Without current hour, selects earliest and latest.
	f.mu.Lock()
	delete(f.objects, operationPrefix+"/op-c.json")
	op := mutationOperation{SchemaVersion: 1, ID: "op-d", MeasurementID: "m", AffectedHour: now.Add(-time.Hour), Generation: 4, Phase: opProjectionPending}
	f.objects[operationPrefix+"/op-d.json"], _ = json.Marshal(op)
	f.mu.Unlock()
	if err := s.projectPending(t.Context()); err != nil {
		t.Fatalf("projectPending without current hour: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.projectPending(ctx); err == nil {
		t.Fatal("cancelled context succeeded")
	}
}
