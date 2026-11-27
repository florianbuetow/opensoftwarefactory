package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, blobs string) *httptest.Server {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &service{
		db:    db,
		blobs: blobs,
		http:  http.DefaultClient,
		log:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, method, url string, body any) (status int, data []byte) {
	t.Helper()
	var r io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func TestOpenDBOnDiskAndHelpers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sub", "factory.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"completed":  "Completed",
		"failed":     "Failed",
		"blocked":    "Blocked",
		"cancelled":  "Cancelled",
		"terminated": "Terminated",
		"timed_out":  "TimedOut",
		"running":    "Running",
		"custom":     "custom",
	} {
		if got := titleStatus(in); got != want {
			t.Fatalf("titleStatus(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "1.2.3", "1e10", "-1", strings.Repeat("9", 65)} {
		if _, ok := parseDecimal(bad); ok {
			t.Fatalf("parseDecimal(%q) unexpectedly succeeded", bad)
		}
	}
	if r, ok := parseDecimal("0.00"); !ok || decimalString(r) != "0" {
		t.Fatalf("decimalString(0.00) = %v %v", r, ok)
	}
	if got := env("SQLDB_UNSET_TEST_VAR", "fallback"); got != "fallback" {
		t.Fatalf("env fallback = %q", got)
	}
	t.Setenv("SQLDB_SET_TEST_VAR", "val")
	if got := env("SQLDB_SET_TEST_VAR", "fallback"); got != "val" {
		t.Fatalf("env set = %q", got)
	}
}

func TestRunsLifecycleAndFilters(t *testing.T) {
	srv := newTestServer(t, "")

	if code, _ := doJSON(t, http.MethodGet, srv.URL+"/healthz", nil); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	if code, _ := doJSON(t, http.MethodPut, srv.URL+"/api/runs/t1", "{bad"); code != http.StatusBadRequest {
		t.Fatalf("bad json run = %d", code)
	}

	code, _ := doJSON(t, http.MethodPut, srv.URL+"/api/runs/t1", RunRow{
		Workflow: "demo/build", WorkflowVersion: 2, Started: "2026-11-27T01:00:00Z", Status: "Running", StartedBy: "flo", InputArtifact: "runs/t1/input",
	})
	if code != http.StatusOK {
		t.Fatalf("put start = %d", code)
	}
	// Partial finish update preserves workflow and started_at.
	code, _ = doJSON(t, http.MethodPut, srv.URL+"/api/runs/t1", RunRow{
		Status: "completed", Finished: "2026-11-27T01:05:00Z",
	})
	if code != http.StatusOK {
		t.Fatalf("put finish = %d", code)
	}
	code, data := doJSON(t, http.MethodGet, srv.URL+"/api/runs?since=2026-11-27T00:00:00Z&workflow=demo/build&status=completed&limit=10", nil)
	if code != http.StatusOK {
		t.Fatalf("list runs = %d %s", code, data)
	}
	var listed struct {
		Runs []RunRow `json:"runs"`
	}
	if err := json.Unmarshal(data, &listed); err != nil || len(listed.Runs) != 1 {
		t.Fatalf("listed = %s %v", data, err)
	}
	if listed.Runs[0].TraceID != "t1" || listed.Runs[0].Workflow != "demo/build" || listed.Runs[0].Status != "Completed" || listed.Runs[0].Finished != "2026-11-27T01:05:00Z" {
		t.Fatalf("unexpected run row: %+v", listed.Runs[0])
	}
	if badCode, _ := doJSON(t, http.MethodGet, srv.URL+"/api/runs?limit=0", nil); badCode != http.StatusBadRequest {
		t.Fatalf("bad limit = %d", badCode)
	}

	// Delete hides from GET /api/runs but preserves status for Metrics outcome joins.
	if delCode, _ := doJSON(t, http.MethodDelete, srv.URL+"/api/runs/t1", nil); delCode != http.StatusNoContent {
		t.Fatalf("delete = %d", delCode)
	}
	code, data = doJSON(t, http.MethodGet, srv.URL+"/api/runs", nil)
	if code != http.StatusOK {
		t.Fatalf("list after delete = %d", code)
	}
	_ = json.Unmarshal(data, &listed)
	if len(listed.Runs) != 0 {
		t.Fatalf("deleted run still listed: %+v", listed.Runs)
	}
}

func TestMeasurementsConversionsAndSeries(t *testing.T) {
	srv := newTestServer(t, "")
	inTok, outTok, cachedTok, uncachedTok, reasonTok, dur := int64(100), int64(50), int64(20), int64(80), int64(10), int64(2500)
	score1, score2 := 0.8, 1.0
	finding := "looks good"
	okBool := true

	doJSON(t, http.MethodPut, srv.URL+"/api/runs/t-ok", RunRow{Workflow: "w/a", Started: "2026-11-27T01:00:00Z", Status: "Completed"})
	doJSON(t, http.MethodPut, srv.URL+"/api/runs/t-fail", RunRow{Workflow: "w/a", Started: "2026-11-27T01:10:00Z", Status: "Failed"})

	// Conversion validation & CRUD.
	if c, _ := doJSON(t, http.MethodPut, srv.URL+"/api/conversions", ConversionRow{FromCurrency: "USD"}); c != http.StatusBadRequest {
		t.Fatalf("bad conv = %d", c)
	}
	if c, _ := doJSON(t, http.MethodDelete, srv.URL+"/api/conversions?from_currency=USD", nil); c != http.StatusBadRequest {
		t.Fatalf("bad delete conv = %d", c)
	}
	doJSON(t, http.MethodPut, srv.URL+"/api/conversions", ConversionRow{FromCurrency: "USD", ToCurrency: "EUR", Rate: "0.9"})

	// Measurements & costs.
	doJSON(t, http.MethodPut, srv.URL+"/api/measurements/u1", MeasurementRow{
		Kind: "usage", RunID: "t-ok", Workflow: "w/a", Agent: "a/1", Model: "m1", OccurredAt: "2026-11-27T01:01:00Z",
		InputTokens: &inTok, OutputTokens: &outTok, CachedInputTokens: &cachedTok, UncachedInputTokens: &uncachedTok, ReasoningTokens: &reasonTok, Status: "reported",
	})
	doJSON(t, http.MethodPut, srv.URL+"/api/costs/u1", CostRow{
		AmountNumerator: "10", AmountDenominator: "100", Currency: "USD", Status: "available", Generation: 2,
	})
	// Older generation must not overwrite newer cost.
	doJSON(t, http.MethodPut, srv.URL+"/api/costs/u1", CostRow{
		AmountNumerator: "999", AmountDenominator: "100", Currency: "USD", Status: "available", Generation: 1,
	})
	doJSON(t, http.MethodPut, srv.URL+"/api/measurements/u2", MeasurementRow{
		Kind: "usage", TraceID: "t-fail", Workflow: "w/a", Agent: "a/1", Model: "m1", OccurredAt: "2026-11-27T01:11:00Z",
		InputTokens: &inTok, OutputTokens: &outTok, Status: "reported",
	})
	doJSON(t, http.MethodPut, srv.URL+"/api/costs/u2", CostRow{
		AmountNumerator: "20", AmountDenominator: "100", Currency: "USD", Status: "available", Generation: 1,
	})
	doJSON(t, http.MethodPut, srv.URL+"/api/measurements/e1", MeasurementRow{
		Kind: "execution", TraceID: "t-ok", Workflow: "w/a", OccurredAt: "2026-11-27T01:05:00Z", DurationMS: &dur, Status: "reported",
	})
	doJSON(t, http.MethodPut, srv.URL+"/api/measurements/j1", MeasurementRow{
		Kind: "judge", TargetRunID: "t-ok", ProducerRunID: "t-judge", Agent: "judges/q", MetricName: "quality",
		OccurredAt: "2026-11-27T01:06:00Z", NumberValue: &score1, TextValue: &finding, BoolValue: &okBool, Status: "reported", Missing: []string{"none"},
	})
	doJSON(t, http.MethodPut, srv.URL+"/api/measurements/j2", MeasurementRow{
		Kind: "judge", TargetTraceID: "t-ok", ProducerTraceID: "t-judge", Agent: "judges/q", MetricName: "quality",
		OccurredAt: "2026-11-27T01:07:00Z", NumberValue: &score2, Status: "reported",
	})

	// List measurements with pagination and error paths.
	code, data := doJSON(t, http.MethodGet, srv.URL+"/api/measurements?kind=judge&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z&limit=1&cursor=0", nil)
	if code != http.StatusOK {
		t.Fatalf("list measurements = %d %s", code, data)
	}
	var mList struct {
		NextCursor   string           `json:"next_cursor"`
		Measurements []MeasurementRow `json:"measurements"`
	}
	if err := json.Unmarshal(data, &mList); err != nil || len(mList.Measurements) != 1 || mList.NextCursor != "1" {
		t.Fatalf("unexpected measurements list: %s", data)
	}
	if c, _ := doJSON(t, http.MethodGet, srv.URL+"/api/measurements?limit=0", nil); c != http.StatusBadRequest {
		t.Fatalf("bad limit = %d", c)
	}
	if c, _ := doJSON(t, http.MethodGet, srv.URL+"/api/measurements?cursor=-1", nil); c != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d", c)
	}

	// Series validation errors.
	for _, q := range []string{
		"",
		"metric=cost&group_by=model&from=bad&to=2026-11-27T03:00:00Z",
		"metric=cost&group_by=model&from=2026-11-27T03:00:00Z&to=2026-11-27T01:00:00Z",
		"metric=cost&group_by=model&outcome=weird&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z",
		"metric=cost&group_by=bogus&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z",
		"metric=duration_ms&group_by=model&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z",
	} {
		if c, _ := doJSON(t, http.MethodGet, srv.URL+"/api/series?"+q, nil); c != http.StatusBadRequest {
			t.Fatalf("expected 400 for %q, got %d", q, c)
		}
	}

	// Cost in EUR filtered to completed runs: 0.10 USD * 0.9 = 0.09 EUR.
	code, data = doJSON(t, http.MethodGet, srv.URL+"/api/series?metric=cost&group_by=model&currency=EUR&outcome=completed&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z", nil)
	if code != http.StatusOK {
		t.Fatalf("cost series = %d %s", code, data)
	}
	var res struct {
		Series []seriesLine `json:"series"`
	}
	if err := json.Unmarshal(data, &res); err != nil || len(res.Series) != 1 || res.Series[0].ExactTotal.Decimal != "0.09" {
		t.Fatalf("unexpected cost series: %s", data)
	}

	// Token, duration, and judge quality metrics across groupings and failed outcome.
	for _, q := range []string{
		"metric=duration_ms&group_by=workflow&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z",
		"metric=input_tokens&group_by=agent&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z",
		"metric=output_tokens&group_by=run&outcome=failed&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z",
		"metric=cached_input_tokens&group_by=model&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z",
		"metric=uncached_input_tokens&group_by=workflow&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z",
		"metric=reasoning_tokens&group_by=model&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z",
		"metric=quality&group_by=run&outcome=completed&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z",
	} {
		if c, body := doJSON(t, http.MethodGet, srv.URL+"/api/series?"+q, nil); c != http.StatusOK {
			t.Fatalf("series %q = %d %s", q, c, body)
		}
	}

	// Delete conversion -> EUR conversion now missing for USD usage.
	if c, _ := doJSON(t, http.MethodDelete, srv.URL+"/api/conversions?from_currency=USD&to_currency=EUR", nil); c != http.StatusNoContent {
		t.Fatalf("delete conv = %d", c)
	}
	code, data = doJSON(t, http.MethodGet, srv.URL+"/api/series?metric=cost&group_by=workflow&currency=EUR&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z", nil)
	if code != http.StatusOK || !strings.Contains(string(data), `"missing":2`) {
		t.Fatalf("cost after delete conv = %d %s", code, data)
	}
}

func TestBackfillFromBlobstore(t *testing.T) {
	blobs := map[string]string{
		"runs/r1/run.json":                            `{"trace_id":"r1","workflow":"demo/a","workflow_version":1,"started":"2026-11-27T01:00:00Z","started_by":"flo","input_artifact":"runs/r1/input"}`,
		"runs/r1/metadata.json":                       `{"status":"completed","finished":"2026-11-27T01:02:00Z","error":""}`,
		"workflow-executions/exec1.json":              `{"run_id":"r1","workflow":"demo/a","workflow_version":1,"started_at":"2026-11-27T01:00:00Z","closed_at":"2026-11-27T01:02:00Z","status":"completed"}`,
		"workflow-executions/exec-removed.json":       `{"run_id":"r-old","workflow":"demo/a","workflow_version":1,"started_at":"2026-11-26T01:00:00Z","closed_at":"2026-11-26T01:02:00Z","status":"failed"}`,
		"measurement-config/conversions/usd-eur.json": `{"from_currency":"USD","to_currency":"EUR","rate":"0.9"}`,
		"measurements/usage/u1.json":                  `{"id":"u1","kind":"usage","run_id":"r1","workflow":"demo/a","model":"m1","occurred_at":"2026-11-27T01:01:00Z","input_tokens":10,"output_tokens":5,"status":"reported"}`,
		"measurements/execution/e1.json":              `{"id":"e1","kind":"execution","run_id":"r1","workflow":"demo/a","occurred_at":"2026-11-27T01:02:00Z","duration_ms":1200,"status":"reported"}`,
		"measurement-costs/u1.json":                   `{"usage_id":"u1","amount_numerator":"1","amount_denominator":"10","currency":"USD","status":"available","generation":1}`,
	}
	blobSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/list/") {
			prefix := strings.TrimPrefix(r.URL.Path, "/list/")
			if prefix == "runs" {
				writeJSON(w, http.StatusOK, map[string]any{"directories": []string{"r1"}})
				return
			}
			objs := []map[string]string{}
			for k := range blobs {
				if strings.HasPrefix(k, prefix+"/") {
					objs = append(objs, map[string]string{"id": k})
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"objects": objs})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/blobs/") {
			id := strings.TrimPrefix(r.URL.Path, "/blobs/")
			if v, ok := blobs[id]; ok {
				_, _ = w.Write([]byte(v))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer blobSrv.Close()

	srv := newTestServer(t, blobSrv.URL)
	if code, body := doJSON(t, http.MethodPost, srv.URL+"/api/backfill", nil); code != http.StatusOK {
		t.Fatalf("backfill = %d %s", code, body)
	}
	code, data := doJSON(t, http.MethodGet, srv.URL+"/api/runs", nil)
	if code != http.StatusOK || !strings.Contains(string(data), `"trace_id":"r1"`) || strings.Contains(string(data), `"r-old"`) {
		t.Fatalf("runs after backfill = %d %s", code, data)
	}
	code, data = doJSON(t, http.MethodGet, srv.URL+"/api/series?metric=cost&group_by=workflow&currency=EUR&outcome=completed&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z", nil)
	if code != http.StatusOK || !strings.Contains(string(data), `"decimal":"0.09"`) {
		t.Fatalf("cost after backfill = %d %s", code, data)
	}

	// Error paths: blobstore 500 -> POST /api/backfill returns 502.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()
	srvFail := newTestServer(t, failSrv.URL)
	if c, _ := doJSON(t, http.MethodPost, srvFail.URL+"/api/backfill", nil); c != http.StatusBadGateway {
		t.Fatalf("backfill 500 = %d", c)
	}

	// Bad JSON & missing fields on write endpoints.
	if c, _ := doJSON(t, http.MethodPut, srv.URL+"/api/measurements/m-bad", "{bad"); c != http.StatusBadRequest {
		t.Fatalf("bad measurement json = %d", c)
	}
	if c, _ := doJSON(t, http.MethodPut, srv.URL+"/api/costs/c-bad", "{bad"); c != http.StatusBadRequest {
		t.Fatalf("bad cost json = %d", c)
	}
	if c, _ := doJSON(t, http.MethodPut, srv.URL+"/api/conversions", "{bad"); c != http.StatusBadRequest {
		t.Fatalf("bad conv json = %d", c)
	}
	// Missing/unavailable cost rows increment missing count cleanly.
	doJSON(t, http.MethodPut, srv.URL+"/api/measurements/u-miss", MeasurementRow{
		Kind: "usage", TraceID: "r1", Workflow: "demo/a", Model: "m1", OccurredAt: "2026-11-27T01:03:00Z", Status: "reported",
	})
	doJSON(t, http.MethodPut, srv.URL+"/api/costs/u-miss", CostRow{
		AmountNumerator: "bad", AmountDenominator: "0", Currency: "USD", Status: "available", Generation: 1,
	})
	doJSON(t, http.MethodPut, srv.URL+"/api/measurements/u-unavail", MeasurementRow{
		Kind: "usage", TraceID: "r1", Workflow: "demo/a", Model: "m1", OccurredAt: "2026-11-27T01:04:00Z", Status: "reported",
	})
	doJSON(t, http.MethodPut, srv.URL+"/api/costs/u-unavail", CostRow{
		Status: "unavailable", Generation: 1,
	})
	code, data = doJSON(t, http.MethodGet, srv.URL+"/api/series?metric=cost&group_by=agent&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z", nil)
	if code != http.StatusOK || !strings.Contains(string(data), `"missing":3`) {
		t.Fatalf("cost missing counts = %d %s", code, data)
	}
	// Missing numeric values increment missing count cleanly.
	code, data = doJSON(t, http.MethodGet, srv.URL+"/api/series?metric=reasoning_tokens&group_by=workflow&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z", nil)
	if code != http.StatusOK || !strings.Contains(string(data), `"missing":3`) {
		t.Fatalf("numeric missing counts = %d %s", code, data)
	}
}

func TestBackfillLoopAndErrorPaths(t *testing.T) {
	// Empty 404 blobstore returns nil from backfillLoop cleanly.
	emptyBlobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer emptyBlobs.Close()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s := &service{
		db:    db,
		blobs: emptyBlobs.URL,
		http:  http.DefaultClient,
		log:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	s.backfillLoop(t.Context())

	// Closed DB exercises 500 error paths across handlers cleanly.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	for _, tc := range []struct {
		body         any
		method, path string
	}{
		{nil, http.MethodGet, "/api/runs"},
		{RunRow{Workflow: "w"}, http.MethodPut, "/api/runs/t1"},
		{nil, http.MethodDelete, "/api/runs/t1"},
		{nil, http.MethodGet, "/api/measurements"},
		{MeasurementRow{Kind: "usage"}, http.MethodPut, "/api/measurements/m1"},
		{CostRow{Status: "available"}, http.MethodPut, "/api/costs/u1"},
		{ConversionRow{FromCurrency: "USD", ToCurrency: "EUR", Rate: "0.9"}, http.MethodPut, "/api/conversions"},
		{nil, http.MethodDelete, "/api/conversions?from_currency=USD&to_currency=EUR"},
		{nil, http.MethodGet, "/api/series?metric=cost&group_by=model&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z"},
		{nil, http.MethodGet, "/api/series?metric=duration_ms&group_by=workflow&from=2026-11-27T00:00:00Z&to=2026-11-27T03:00:00Z"},
	} {
		if c, _ := doJSON(t, tc.method, srv.URL+tc.path, tc.body); c != http.StatusInternalServerError {
			t.Fatalf("%s %s on closed db = %d, want 500", tc.method, tc.path, c)
		}
	}
}
