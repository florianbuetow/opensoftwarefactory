package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type service struct {
	db    *sql.DB
	http  *http.Client
	log   *slog.Logger
	blobs string
	mu    sync.Mutex // serializes compound read-modify-write operations and backfill
}

// RunRow is one workflow run indexed in SQLite, keyed by trace_id.
type RunRow struct {
	TraceID         string `json:"trace_id"`
	Workflow        string `json:"workflow"`
	Started         string `json:"started"`
	Finished        string `json:"finished,omitempty"`
	Status          string `json:"status"`
	StartedBy       string `json:"started_by,omitempty"`
	InputArtifact   string `json:"input_artifact,omitempty"`
	Source          string `json:"source,omitempty"`
	Error           string `json:"error,omitempty"`
	WorkflowVersion int    `json:"workflow_version"`
}

// MeasurementRow is one usage, execution, or judge measurement indexed in SQLite.
type MeasurementRow struct {
	ID                  string   `json:"id"`
	Kind                string   `json:"kind"`
	TraceID             string   `json:"trace_id"`
	RunID               string   `json:"run_id,omitempty"` // accepted on ingest, stored as trace_id
	ExecutionID         string   `json:"execution_id,omitempty"`
	Workflow            string   `json:"workflow,omitempty"`
	Agent               string   `json:"agent,omitempty"`
	Vendor              string   `json:"vendor,omitempty"`
	Model               string   `json:"model,omitempty"`
	Effort              string   `json:"effort,omitempty"`
	OccurredAt          string   `json:"occurred_at"`
	InputTokens         *int64   `json:"input_tokens,omitempty"`
	OutputTokens        *int64   `json:"output_tokens,omitempty"`
	CachedInputTokens   *int64   `json:"cached_input_tokens,omitempty"`
	UncachedInputTokens *int64   `json:"uncached_input_tokens,omitempty"`
	ReasoningTokens     *int64   `json:"reasoning_tokens,omitempty"`
	DurationMS          *int64   `json:"duration_ms,omitempty"`
	MetricName          string   `json:"metric_name,omitempty"`
	NumberValue         *float64 `json:"number_value,omitempty"`
	TextValue           *string  `json:"text_value,omitempty"`
	BoolValue           *bool    `json:"bool_value,omitempty"`
	ProducerTraceID     string   `json:"producer_trace_id,omitempty"`
	ProducerRunID       string   `json:"producer_run_id,omitempty"` // accepted on ingest, stored as producer_trace_id
	TargetTraceID       string   `json:"target_trace_id,omitempty"`
	TargetRunID         string   `json:"target_run_id,omitempty"` // accepted on ingest, stored as target_trace_id
	ArtifactID          string   `json:"artifact_id,omitempty"`
	Status              string   `json:"status,omitempty"`
	Missing             []string `json:"missing,omitempty"`
}

// CostRow is the current calculated cost for one usage measurement.
type CostRow struct {
	UsageID           string `json:"usage_id"`
	AmountNumerator   string `json:"amount_numerator,omitempty"`
	AmountDenominator string `json:"amount_denominator,omitempty"`
	Currency          string `json:"currency,omitempty"`
	Status            string `json:"status"`
	Generation        int64  `json:"generation"`
}

// ConversionRow is a configured currency exchange rate between two currencies.
type ConversionRow struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	Rate         string `json:"rate"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	dbPath := env("SQLDB_PATH", "/state/factory.db")
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	s := &service{
		db:    db,
		blobs: strings.TrimRight(env("BLOBSTORE_URL", "http://blobstore:8080"), "/"),
		http:  &http.Client{Timeout: 30 * time.Second},
		log:   log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if s.blobs != "" {
		go s.backfillLoop(ctx)
	}

	srv := &http.Server{
		Addr:              env("SQLDB_ADDR", ":8080"),
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	done := make(chan error, 1)
	go func() {
		log.Info("sqldb listening", "address", srv.Addr, "db", dbPath)
		done <- srv.ListenAndServe()
	}()
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(drain)
	}
	return nil
}

func (s *service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("PUT /api/runs/{trace_id}", s.putRun)
	mux.HandleFunc("DELETE /api/runs/{trace_id}", s.deleteRun)
	mux.HandleFunc("PUT /api/measurements/{id}", s.putMeasurement)
	mux.HandleFunc("GET /api/measurements", s.listMeasurements)
	mux.HandleFunc("PUT /api/costs/{usage_id}", s.putCost)
	mux.HandleFunc("PUT /api/conversions", s.putConversion)
	mux.HandleFunc("DELETE /api/conversions", s.deleteConversion)
	mux.HandleFunc("GET /api/series", s.getSeries)
	mux.HandleFunc("POST /api/backfill", s.handleBackfill)
	return mux
}

func (s *service) putRun(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimSpace(r.PathValue("trace_id"))
	if traceID == "" {
		s.fail(w, http.StatusBadRequest, errors.New("trace_id is required"))
		return
	}
	var row RunRow
	if err := decodeJSON(r, &row); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	row.TraceID = traceID
	if err := s.upsertRun(r.Context(), &row, false); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *service) upsertRun(ctx context.Context, row *RunRow, removed bool) error {
	if row.TraceID == "" {
		return errors.New("trace_id is required")
	}
	if row.Status == "" {
		row.Status = "Running"
	} else {
		row.Status = titleStatus(row.Status)
	}
	removedInt := 0
	if removed {
		removedInt = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (trace_id, workflow, workflow_version, started_at, finished_at, status, started_by, input_artifact, source, error, removed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trace_id) DO UPDATE SET
			workflow         = CASE WHEN excluded.workflow != '' THEN excluded.workflow ELSE runs.workflow END,
			workflow_version = CASE WHEN excluded.workflow_version > 0 THEN excluded.workflow_version ELSE runs.workflow_version END,
			started_at       = CASE WHEN excluded.started_at != '' THEN excluded.started_at ELSE runs.started_at END,
			finished_at      = CASE WHEN excluded.finished_at != '' THEN excluded.finished_at ELSE runs.finished_at END,
			status           = CASE WHEN excluded.status != '' THEN excluded.status ELSE runs.status END,
			started_by       = CASE WHEN excluded.started_by != '' THEN excluded.started_by ELSE runs.started_by END,
			input_artifact   = CASE WHEN excluded.input_artifact != '' THEN excluded.input_artifact ELSE runs.input_artifact END,
			source           = CASE WHEN excluded.source != '' THEN excluded.source ELSE runs.source END,
			error            = CASE WHEN excluded.error != '' OR excluded.status != 'Running' THEN excluded.error ELSE runs.error END,
			removed          = CASE WHEN excluded.removed = 1 THEN 1 ELSE runs.removed END
	`, row.TraceID, row.Workflow, row.WorkflowVersion, row.Started, row.Finished, row.Status, row.StartedBy, row.InputArtifact, row.Source, row.Error, removedInt)
	return err
}

func (s *service) deleteRun(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimSpace(r.PathValue("trace_id"))
	if traceID == "" {
		s.fail(w, http.StatusBadRequest, errors.New("trace_id is required"))
		return
	}
	// Mark as removed instead of hard-deleting so Metrics outcome filters still know how
	// historical runs ended even after their run folders were cleaned up.
	_, err := s.db.ExecContext(r.Context(), `UPDATE runs SET removed = 1 WHERE trace_id = ?`, traceID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *service) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since := strings.TrimSpace(q.Get("since"))
	workflow := strings.TrimSpace(q.Get("workflow"))
	status := strings.TrimSpace(q.Get("status"))
	limit := 10000
	if rawLimit := strings.TrimSpace(q.Get("limit")); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n <= 0 || n > 10000 {
			s.fail(w, http.StatusBadRequest, errors.New("limit must be between 1 and 10000"))
			return
		}
		limit = n
	}
	const query = `
		SELECT trace_id, workflow, workflow_version, started_at, finished_at, status, started_by, input_artifact, source, error
		FROM runs
		WHERE removed = 0
		  AND (? = '' OR started_at >= ?)
		  AND (? = '' OR workflow = ?)
		  AND (? = '' OR LOWER(status) = LOWER(?))
		ORDER BY started_at DESC, trace_id DESC
		LIMIT ?`
	rows, err := s.db.QueryContext(r.Context(), query, since, since, workflow, workflow, status, status, limit)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	defer func() { _ = rows.Close() }()
	out := []RunRow{}
	for rows.Next() {
		var r RunRow
		if err := rows.Scan(&r.TraceID, &r.Workflow, &r.WorkflowVersion, &r.Started, &r.Finished, &r.Status, &r.StartedBy, &r.InputArtifact, &r.Source, &r.Error); err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func (s *service) putMeasurement(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.fail(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}
	var m MeasurementRow
	if err := decodeJSON(r, &m); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	m.ID = id
	if err := s.upsertMeasurement(r.Context(), &m); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *service) upsertMeasurement(ctx context.Context, m *MeasurementRow) error {
	if m.ID == "" || m.Kind == "" {
		return errors.New("id and kind are required")
	}
	// trace_id is the run's trace ID. A usage measurement's own trace_id is the model
	// call's OpenTelemetry trace, so the run ID wins wherever one is given.
	if m.RunID != "" {
		m.TraceID = m.RunID
	}
	if m.ProducerTraceID == "" {
		m.ProducerTraceID = m.ProducerRunID
	}
	if m.TargetTraceID == "" {
		m.TargetTraceID = m.TargetRunID
	}
	if m.TraceID == "" && m.TargetTraceID != "" {
		m.TraceID = m.TargetTraceID
	}
	missingJSON := "[]"
	if len(m.Missing) > 0 {
		if b, err := json.Marshal(m.Missing); err == nil {
			missingJSON = string(b)
		}
	}
	var boolVal *int
	if m.BoolValue != nil {
		v := 0
		if *m.BoolValue {
			v = 1
		}
		boolVal = &v
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO measurements (
			id, kind, trace_id, execution_id, workflow, agent, vendor, model, effort, occurred_at,
			input_tokens, output_tokens, cached_input_tokens, uncached_input_tokens, reasoning_tokens,
			duration_ms, metric_name, number_value, text_value, bool_value,
			producer_trace_id, target_trace_id, artifact_id, status, missing_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind = excluded.kind,
			trace_id = excluded.trace_id,
			execution_id = excluded.execution_id,
			workflow = excluded.workflow,
			agent = excluded.agent,
			vendor = excluded.vendor,
			model = excluded.model,
			effort = excluded.effort,
			occurred_at = excluded.occurred_at,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			cached_input_tokens = excluded.cached_input_tokens,
			uncached_input_tokens = excluded.uncached_input_tokens,
			reasoning_tokens = excluded.reasoning_tokens,
			duration_ms = excluded.duration_ms,
			metric_name = excluded.metric_name,
			number_value = excluded.number_value,
			text_value = excluded.text_value,
			bool_value = excluded.bool_value,
			producer_trace_id = excluded.producer_trace_id,
			target_trace_id = excluded.target_trace_id,
			artifact_id = excluded.artifact_id,
			status = excluded.status,
			missing_json = excluded.missing_json
	`,
		m.ID, m.Kind, m.TraceID, m.ExecutionID, m.Workflow, m.Agent, m.Vendor, m.Model, m.Effort, normalizeRFC3339(m.OccurredAt),
		m.InputTokens, m.OutputTokens, m.CachedInputTokens, m.UncachedInputTokens, m.ReasoningTokens,
		m.DurationMS, m.MetricName, m.NumberValue, m.TextValue, boolVal,
		m.ProducerTraceID, m.TargetTraceID, m.ArtifactID, m.Status, missingJSON,
	)
	return err
}

func (s *service) listMeasurements(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := strings.TrimSpace(q.Get("kind"))
	from := normalizeRFC3339(q.Get("from"))
	to := normalizeRFC3339(q.Get("to"))
	limit := 100
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 1000 {
			s.fail(w, http.StatusBadRequest, errors.New("limit must be between 1 and 1000"))
			return
		}
		limit = n
	}
	offset := 0
	if cursor := strings.TrimSpace(q.Get("cursor")); cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			s.fail(w, http.StatusBadRequest, errors.New("invalid cursor"))
			return
		}
		offset = n
	}
	const query = `
		SELECT id, kind, trace_id, execution_id, workflow, agent, vendor, model, effort, occurred_at,
		       input_tokens, output_tokens, cached_input_tokens, uncached_input_tokens, reasoning_tokens,
		       duration_ms, metric_name, number_value, text_value, bool_value,
		       producer_trace_id, target_trace_id, artifact_id, status, missing_json
		FROM measurements
		WHERE (? = '' OR kind = ?)
		  AND (? = '' OR occurred_at >= ?)
		  AND (? = '' OR occurred_at <= ?)
		ORDER BY occurred_at DESC, id DESC
		LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(r.Context(), query, kind, kind, from, from, to, to, limit+1, offset)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	defer func() { _ = rows.Close() }()
	items := []MeasurementRow{}
	for rows.Next() {
		var m MeasurementRow
		var boolVal sql.NullInt64
		var missingJSON string
		if err := rows.Scan(
			&m.ID, &m.Kind, &m.TraceID, &m.ExecutionID, &m.Workflow, &m.Agent, &m.Vendor, &m.Model, &m.Effort, &m.OccurredAt,
			&m.InputTokens, &m.OutputTokens, &m.CachedInputTokens, &m.UncachedInputTokens, &m.ReasoningTokens,
			&m.DurationMS, &m.MetricName, &m.NumberValue, &m.TextValue, &boolVal,
			&m.ProducerTraceID, &m.TargetTraceID, &m.ArtifactID, &m.Status, &missingJSON,
		); err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		if boolVal.Valid {
			v := boolVal.Int64 != 0
			m.BoolValue = &v
		}
		_ = json.Unmarshal([]byte(missingJSON), &m.Missing)
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	nextCursor := ""
	if len(items) > limit {
		items = items[:limit]
		nextCursor = strconv.Itoa(offset + limit)
	}
	writeJSON(w, http.StatusOK, map[string]any{"measurements": items, "next_cursor": nextCursor})
}

func (s *service) putCost(w http.ResponseWriter, r *http.Request) {
	usageID := strings.TrimSpace(r.PathValue("usage_id"))
	if usageID == "" {
		s.fail(w, http.StatusBadRequest, errors.New("usage_id is required"))
		return
	}
	var c CostRow
	if err := decodeJSON(r, &c); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	c.UsageID = usageID
	if err := s.upsertCost(r.Context(), &c); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *service) upsertCost(ctx context.Context, c *CostRow) error {
	if c.UsageID == "" {
		return errors.New("usage_id is required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO costs (usage_id, amount_numerator, amount_denominator, currency, status, generation)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(usage_id) DO UPDATE SET
			amount_numerator   = CASE WHEN excluded.generation >= costs.generation THEN excluded.amount_numerator ELSE costs.amount_numerator END,
			amount_denominator = CASE WHEN excluded.generation >= costs.generation THEN excluded.amount_denominator ELSE costs.amount_denominator END,
			currency           = CASE WHEN excluded.generation >= costs.generation THEN excluded.currency ELSE costs.currency END,
			status             = CASE WHEN excluded.generation >= costs.generation THEN excluded.status ELSE costs.status END,
			generation         = CASE WHEN excluded.generation >= costs.generation THEN excluded.generation ELSE costs.generation END
	`, c.UsageID, c.AmountNumerator, c.AmountDenominator, c.Currency, c.Status, c.Generation)
	return err
}

func (s *service) putConversion(w http.ResponseWriter, r *http.Request) {
	var c ConversionRow
	if err := decodeJSON(r, &c); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if c.FromCurrency == "" || c.ToCurrency == "" || c.Rate == "" {
		s.fail(w, http.StatusBadRequest, errors.New("from_currency, to_currency, and rate are required"))
		return
	}
	if err := s.upsertConversion(r.Context(), &c); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *service) upsertConversion(ctx context.Context, c *ConversionRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conversions (from_currency, to_currency, rate) VALUES (?, ?, ?)
		ON CONFLICT(from_currency, to_currency) DO UPDATE SET rate = excluded.rate
	`, c.FromCurrency, c.ToCurrency, c.Rate)
	return err
}

func (s *service) deleteConversion(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := strings.TrimSpace(q.Get("from_currency")), strings.TrimSpace(q.Get("to_currency"))
	if from == "" || to == "" {
		s.fail(w, http.StatusBadRequest, errors.New("from_currency and to_currency are required"))
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM conversions WHERE from_currency = ? AND to_currency = ?`, from, to); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func normalizeRFC3339(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return raw
}

func decodeJSON(r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *service) fail(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
