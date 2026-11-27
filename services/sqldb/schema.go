package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS runs (
	trace_id         TEXT PRIMARY KEY,
	workflow         TEXT NOT NULL DEFAULT '',
	workflow_version INTEGER NOT NULL DEFAULT 0,
	started_at       TEXT NOT NULL DEFAULT '',
	finished_at      TEXT NOT NULL DEFAULT '',
	status           TEXT NOT NULL DEFAULT 'Running',
	started_by       TEXT NOT NULL DEFAULT '',
	input_artifact   TEXT NOT NULL DEFAULT '',
	source           TEXT NOT NULL DEFAULT '',
	error            TEXT NOT NULL DEFAULT '',
	removed          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_runs_started_at ON runs(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);

CREATE TABLE IF NOT EXISTS measurements (
	id                    TEXT PRIMARY KEY,
	kind                  TEXT NOT NULL,
	trace_id              TEXT NOT NULL DEFAULT '',
	execution_id          TEXT NOT NULL DEFAULT '',
	workflow              TEXT NOT NULL DEFAULT '',
	agent                 TEXT NOT NULL DEFAULT '',
	vendor                TEXT NOT NULL DEFAULT '',
	model                 TEXT NOT NULL DEFAULT '',
	effort                TEXT NOT NULL DEFAULT '',
	occurred_at           TEXT NOT NULL DEFAULT '',
	input_tokens          INTEGER,
	output_tokens         INTEGER,
	cached_input_tokens   INTEGER,
	uncached_input_tokens INTEGER,
	reasoning_tokens      INTEGER,
	duration_ms           INTEGER,
	metric_name           TEXT NOT NULL DEFAULT '',
	number_value          REAL,
	text_value            TEXT,
	bool_value            INTEGER,
	producer_trace_id     TEXT NOT NULL DEFAULT '',
	target_trace_id       TEXT NOT NULL DEFAULT '',
	artifact_id           TEXT NOT NULL DEFAULT '',
	status                TEXT NOT NULL DEFAULT '',
	missing_json          TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_measurements_kind_occurred ON measurements(kind, occurred_at);
CREATE INDEX IF NOT EXISTS idx_measurements_trace_id ON measurements(trace_id);

CREATE TABLE IF NOT EXISTS costs (
	usage_id           TEXT PRIMARY KEY,
	amount_numerator   TEXT NOT NULL DEFAULT '',
	amount_denominator TEXT NOT NULL DEFAULT '',
	currency           TEXT NOT NULL DEFAULT '',
	status             TEXT NOT NULL DEFAULT '',
	generation         INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS conversions (
	from_currency TEXT NOT NULL,
	to_currency   TEXT NOT NULL,
	rate          TEXT NOT NULL,
	PRIMARY KEY (from_currency, to_currency)
);
`

func openDB(path string) (*sql.DB, error) {
	if path != ":memory:" {
		clean := filepath.Clean(path)
		if err := os.MkdirAll(filepath.Dir(clean), 0o750); err != nil {
			return nil, fmt.Errorf("creating db directory: %w", err)
		}
		path = clean
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite %s: %w", path, err)
	}
	// Single connection serializes all SQLite writes safely and prevents SQLITE_BUSY races.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("applying %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing schema: %w", err)
	}
	return db, nil
}
