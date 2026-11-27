package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestJudgeSeriesUsesTargetRunCountsMissingAndHidesIncompleteHours(t *testing.T) {
	f, blob, orch := newFixture(t)
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	op := mutationOperation{SchemaVersion: 1, ID: "dirty", MeasurementID: "m", AffectedHour: hour, Generation: 1, Phase: "projection_pending"}
	f.mu.Lock()
	f.objects[operationPrefix+"/dirty.json"], _ = json.Marshal(op)
	f.mu.Unlock()
	queries := []string{}
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query struct {
				SQL string `json:"sql"`
			} `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		queries = append(queries, request.Query.SQL)
		if strings.Contains(request.Query.SQL, "COUNT(*)") {
			writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"bucket": hour.Add(time.Hour).Format(time.RFC3339), "missing_count": 3}}, "total": 1, "is_partial": false})
			return
		}
		writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"bucket": hour.Format(time.RFC3339), "group_key": "target-a", "value": 1}, {"bucket": hour.Add(time.Hour).Format(time.RFC3339), "group_key": "target-a", "value": 2}, {"bucket": hour.Add(time.Hour).Format(time.RFC3339), "group_key": "", "value": 9}}, "total": 3, "is_partial": false})
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	resp := get(t, server.URL+"/api/series?metric=quality&group_by=run&from=2026-11-01T12:00:00Z&to=2026-11-01T14:00:00Z")
	defer closeBody(t, resp.Body)
	var result struct {
		Coverage map[string]int `json:"coverage"`
		Series   []seriesLine   `json:"series"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		t.Fatal("decode")
	}
	if resp.StatusCode != 200 || len(result.Series) != 1 || result.Series[0].Key != "target-a" || len(result.Series[0].Points) != 1 || result.Coverage["known"] != 1 || result.Coverage["missing"] != 3 {
		t.Fatalf("series status=%d %+v", resp.StatusCode, result)
	}
	if len(queries) < 2 || !strings.Contains(queries[0], "target_run_id") || !strings.Contains(queries[0], "metric_name = 'quality'") {
		t.Fatalf("queries %v", queries)
	}
}

func TestSeriesNarrowsToRunsWithTheChosenOutcome(t *testing.T) {
	f, blob, orch := newFixture(t)
	f.mu.Lock()
	for run, status := range map[string]string{"run-ok": "completed", "run-bad": "failed", "run-going": "running"} {
		f.objects["workflow-executions/exec-"+run+".json"] = []byte(`{"schema_version":1,"execution_id":"exec-` + run + `","run_id":"` + run + `","artifact_prefix":"runs/` + run + `","status":"` + status + `"}`)
	}
	// A nested workflow's own execution never decides its run's outcome.
	f.objects["workflow-executions/exec-child.json"] = []byte(`{"schema_version":1,"execution_id":"exec-child","parent_execution_id":"exec-run-ok","run_id":"run-ok-child","artifact_prefix":"runs/run-ok","status":"failed"}`)
	f.mu.Unlock()
	queries := []string{}
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query struct {
				SQL string `json:"sql"`
			} `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		queries = append(queries, request.Query.SQL)
		if strings.Contains(request.Query.SQL, "DISTINCT") {
			writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"run_key": "run-ok", "execution_key": "exec-run-ok"}, {"run_key": "run-bad", "execution_key": "exec-run-bad"}, {"run_key": "run-going", "execution_key": "exec-run-going"}, {"run_key": "run-removed", "execution_key": "exec-run-removed"}, {"run_key": "run-ok-child", "execution_key": "exec-child"}}, "total": 5, "is_partial": false})
			return
		}
		writeJSON(w, 200, map[string]any{"hits": []any{}, "total": 0, "is_partial": false})
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	series := server.URL + "/api/series?metric=duration_ms&group_by=workflow&from=2026-11-01T12:00:00Z&to=2026-11-01T14:00:00Z"
	// statusOf asks for a series and returns the status of the answer, with its body closed.
	statusOf := func(url string) int {
		t.Helper()
		resp := get(t, url)
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	for outcome, want := range map[string]string{"failed": "run_id IN ('run-bad')", "completed": "run_id IN ('run-ok')"} {
		queries = queries[:0]
		if status := statusOf(series + "&outcome=" + outcome); status != 200 || len(queries) < 3 {
			t.Fatalf("outcome %s: status %d, queries %v", outcome, status, queries)
		}
		for _, sql := range queries[1:] { // the values and the missing count alike
			if !strings.Contains(sql, want) {
				t.Fatalf("outcome %s: query not narrowed to %s: %s", outcome, want, sql)
			}
		}
	}
	queries = queries[:0]
	if status := statusOf(series); status != 200 || len(queries) == 0 || strings.Contains(queries[0], "DISTINCT") || strings.Contains(queries[0], "run_id IN") {
		t.Fatalf("without an outcome every run counts: status %d, queries %v", status, queries)
	}
	if status := statusOf(series + "&outcome=running"); status != 422 {
		t.Fatalf("unknown outcome = %d", status)
	}
}

func TestPendingHoursComeFromTheProjectorsLastReadingWhileItHolds(t *testing.T) {
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	reads := 0
	f, blob, orch := newFixture(t)
	f.mu.Lock()
	f.objects[operationPrefix+"/op.json"], _ = json.Marshal(mutationOperation{SchemaVersion: 1, ID: "op", AffectedHour: hour, Phase: opProjectionPending})
	f.mu.Unlock()
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, operationPrefix) {
			reads++
		}
		http.Redirect(w, r, blob.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer counting.Close()
	s := &service{blobs: counting.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	from, to := hour.Add(-time.Hour), hour.Add(time.Hour)

	version := s.pending.begin()
	s.pending.store(version, map[time.Time]bool{hour: true}, time.Now())
	if got, err := s.pendingProjectionHours(t.Context(), from, to); err != nil || len(got) != 1 || !got[0].Equal(hour) || reads != 0 {
		t.Fatalf("a fresh reading answers without reading operations: %v %v, %d reads", got, err, reads)
	}
	s.pending.invalidate()
	s.pending.store(version, map[time.Time]bool{}, time.Now())
	if got, err := s.pendingProjectionHours(t.Context(), from, to); err != nil || len(got) != 1 || reads == 0 {
		t.Fatalf("a reading begun before an operation was written is never kept: %v %v, %d reads", got, err, reads)
	}
	reads = 0
	s.pending.store(s.pending.begin(), map[time.Time]bool{}, time.Now().Add(-2*pendingSnapshotAge))
	if got, err := s.pendingProjectionHours(t.Context(), from, to); err != nil || len(got) != 1 || reads == 0 {
		t.Fatalf("an old reading gives way to the operations themselves: %v %v, %d reads", got, err, reads)
	}
}

func TestCurrencyIsOnlyRequiredForCostSeries(t *testing.T) {
	_, blob, orch := newFixture(t)
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"hits": []any{}, "total": 0, "is_partial": false})
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	resp := get(t, server.URL+"/api/series?metric=input_tokens&group_by=model&from=2026-11-01T12:00:00Z&to=2026-11-01T14:00:00Z")
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("non-cost series without currency = %d", resp.StatusCode)
	}
	resp = get(t, server.URL+"/api/series?metric=cost&group_by=model&from=2026-11-01T12:00:00Z&to=2026-11-01T14:00:00Z")
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 422 {
		t.Fatalf("cost series without currency = %d", resp.StatusCode)
	}
}

func TestPartialSeriesSuppressesTotals(t *testing.T) {
	_, blob, orch := newFixture(t)
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query struct {
				SQL string `json:"sql"`
			} `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if strings.Contains(request.Query.SQL, "COUNT(*)") {
			writeJSON(w, 200, map[string]any{"hits": []any{}, "total": 0, "is_partial": false})
			return
		}
		writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"bucket": "2026-11-01T12:00:00Z", "group_key": "m", "value": 10}}, "total": 2, "is_partial": true})
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	resp := get(t, server.URL+"/api/series?metric=input_tokens&group_by=model&from=2026-11-01T12:00:00Z&to=2026-11-01T14:00:00Z")
	defer closeBody(t, resp.Body)
	var result struct {
		Projection struct {
			Status string `json:"status"`
		} `json:"projection"`
		Series        []seriesLine `json:"series"`
		QueryComplete bool         `json:"query_complete"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Series) != 0 || result.QueryComplete || result.Projection.Status != "refreshing" {
		t.Fatalf("partial result %+v", result)
	}
}

func TestSeriesParsesOpenObserveNaiveUTCBucket(t *testing.T) {
	_, blob, orch := newFixture(t)
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query struct {
				SQL string `json:"sql"`
			} `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if strings.Contains(request.Query.SQL, "COUNT(*)") {
			writeJSON(w, 200, map[string]any{"hits": []any{}, "total": 0, "is_partial": false})
			return
		}
		writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"bucket": "2026-11-21T20:00:00", "group_key": "workflow-a", "value": 938}}, "total": 1, "is_partial": false})
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	resp := get(t, server.URL+"/api/series?metric=duration_ms&group_by=workflow&from=2026-11-21T19:00:00Z&to=2026-11-21T21:00:00Z")
	defer closeBody(t, resp.Body)
	var result struct {
		Series        []seriesLine `json:"series"`
		QueryComplete bool         `json:"query_complete"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	want := time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)
	if !result.QueryComplete || len(result.Series) != 1 || len(result.Series[0].Points) != 1 || !result.Series[0].Points[0].Time.Equal(want) || result.Series[0].Points[0].Time.Location() != time.UTC {
		t.Fatalf("naive UTC result %+v", result)
	}
}

func TestInvalidSeriesBucketMakesQueryIncomplete(t *testing.T) {
	_, blob, orch := newFixture(t)
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query struct {
				SQL string `json:"sql"`
			} `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if strings.Contains(request.Query.SQL, "COUNT(*)") {
			writeJSON(w, 200, map[string]any{"hits": []any{}, "total": 0, "is_partial": false})
			return
		}
		writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"bucket": "not-a-time", "group_key": "workflow-a", "value": 938}}, "total": 1, "is_partial": false})
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	resp := get(t, server.URL+"/api/series?metric=duration_ms&group_by=workflow&from=2026-11-21T19:00:00Z&to=2026-11-21T21:00:00Z")
	defer closeBody(t, resp.Body)
	var result struct {
		Projection struct {
			Status string `json:"status"`
		} `json:"projection"`
		Series        []seriesLine `json:"series"`
		QueryComplete bool         `json:"query_complete"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if result.QueryComplete || len(result.Series) != 0 || result.Projection.Status != "refreshing" {
		t.Fatalf("invalid bucket result %+v", result)
	}
}

func TestCostSeriesReturnsExactRationalTotalAndApproximateChart(t *testing.T) {
	_, blob, orch := newFixture(t)
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"_timestamp": hour.Format(time.RFC3339), "group_key": "model-a", "amount_numerator": "1", "amount_denominator": "3", "currency": "EUR", "cost_status": "available"}, {"_timestamp": hour.Add(time.Minute).Format(time.RFC3339), "group_key": "model-a", "amount_numerator": "1", "amount_denominator": "3", "currency": "EUR", "cost_status": "available"}}, "total": 2, "is_partial": false})
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	resp := get(t, server.URL+"/api/series?metric=cost&group_by=model&currency=EUR&from=2026-11-01T12:00:00Z&to=2026-11-01T14:00:00Z")
	defer closeBody(t, resp.Body)
	var result struct {
		Series        []seriesLine `json:"series"`
		QueryComplete bool         `json:"query_complete"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if !result.QueryComplete || len(result.Series) != 1 || result.Series[0].ExactTotal == nil || result.Series[0].ExactTotal.Numerator != "2" || result.Series[0].ExactTotal.Denominator != "3" || !result.Series[0].Points[0].Approximate {
		t.Fatalf("exact series %+v", result)
	}
}

func TestCostSeriesConvertsRequestedCurrencyWithoutRecalculation(t *testing.T) {
	f, blob, orch := newFixture(t)
	conversion := Conversion{FromCurrency: "USD", ToCurrency: "EUR", Rate: "0.9"}
	f.mu.Lock()
	f.objects[conversion.Key().objectID()], _ = json.Marshal(conversion)
	f.mu.Unlock()
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"hits": []map[string]any{{"_timestamp": hour.Format(time.RFC3339), "group_key": "model-a", "amount_numerator": "11", "amount_denominator": "50", "currency": "USD", "cost_status": "available"}}, "total": 1, "is_partial": false})
	}))
	defer openobserve.Close()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	get := func(currency string) *exactSeriesValue {
		resp := get(t, server.URL+"/api/series?metric=cost&group_by=model&currency="+currency+"&from=2026-11-01T12:00:00Z&to=2026-11-01T14:00:00Z")
		defer closeBody(t, resp.Body)
		var result struct {
			Series []seriesLine `json:"series"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&result)
		if resp.StatusCode != 200 || len(result.Series) != 1 {
			t.Fatalf("%s status=%d result=%+v", currency, resp.StatusCode, result)
		}
		return result.Series[0].ExactTotal
	}
	usd, eur := get("USD"), get("EUR")
	if usd.Numerator != "11" || usd.Denominator != "50" || usd.Decimal != "0.22" {
		t.Fatalf("USD %+v", usd)
	}
	if eur.Numerator != "99" || eur.Denominator != "500" || eur.Decimal != "0.198" {
		t.Fatalf("EUR %+v", eur)
	}
}

func TestEvidenceArtifactIsExecutionScopedAndCapped(t *testing.T) {
	f, blob, orch := newFixture(t)
	content := strings.Repeat("x", (1<<20)+17)
	f.mu.Lock()
	f.objects["workflow-executions/exec.json"] = []byte(`{"schema_version":1,"execution_id":"exec","run_id":"run","workflow":"w","workflow_version":1,"artifact_prefix":"runs/run/child","started_at":"2026-01-01T00:00:00Z","status":"completed"}`)
	f.objects["runs/run/child/output"] = []byte(content)
	f.objects["runs/run/sibling/output"] = []byte("secret")
	f.mu.Unlock()
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	resp := get(t, server.URL+"/api/evidence/artifact?run_id=run&execution_id=exec&artifact_id=runs/run/child/output")
	var result struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
		Bytes     int    `json:"bytes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !result.Truncated || len(result.Content) != (1<<20) || result.Bytes != len(content) {
		t.Fatalf("artifact status=%d len=%d result=%+v", resp.StatusCode, len(result.Content), result)
	}
	resp = get(t, server.URL+"/api/evidence/artifact?run_id=run&execution_id=exec&artifact_id=runs/run/sibling/output")
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 422 {
		t.Fatalf("sibling artifact status %d", resp.StatusCode)
	}
}

func TestQueriesHelpersEvidenceAndSeriesEdgeCases(t *testing.T) {
	// openObserveHTTPError.Error, stringValue, timeValue, spanExecutionID.
	if got := (openObserveHTTPError{Status: 502}).Error(); !strings.Contains(got, "502") {
		t.Fatalf("error string = %q", got)
	}
	if stringValue(123) != "123" || stringValue("abc") != "abc" {
		t.Fatal("stringValue mismatch")
	}
	if _, ok := timeValue(float64(1700000000000000)); !ok {
		t.Fatal("float64 timeValue failed")
	}
	if _, ok := timeValue(json.Number("1700000000000000")); !ok {
		t.Fatal("json.Number timeValue failed")
	}
	if _, ok := timeValue(struct{}{}); ok {
		t.Fatal("struct timeValue succeeded")
	}
	if spanExecutionID(map[string]any{"span_attributes": `{"factory.execution_id":"e1"}`}) != "e1" {
		t.Fatal("string span_attributes failed")
	}
	if spanExecutionID(map[string]any{"span_attributes": map[string]any{"factory_execution_id": "e2"}}) != "e2" {
		t.Fatal("map span_attributes failed")
	}
	if spanExecutionID(map[string]any{}) != "" {
		t.Fatal("empty spanExecutionID non-empty")
	}

	// openObserveSearch error cases: unconfigured, non-200, function_error.
	f, blob, orch := newFixture(t)
	sUnconf := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	if _, err := sUnconf.openObserveSearch(t.Context(), "SELECT 1", time.Now().Add(-time.Hour), time.Now(), 0, 1, ""); err == nil {
		t.Fatal("unconfigured openObserveSearch succeeded")
	}

	ooStatus := http.StatusOK
	ooResp := map[string]any{"hits": []any{}, "total": 0, "is_partial": false}
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, ooStatus, ooResp)
	}))
	defer openobserve.Close()

	s := &service{blobs: blob.URL, orchestrator: orch.URL, openobserve: openobserve.URL, openobserveUser: "u", openobservePassword: "p", http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	ooStatus = http.StatusBadGateway
	if _, err := s.openObserveSearch(t.Context(), "SELECT 1", time.Now().Add(-time.Hour), time.Now(), 0, 1, ""); err == nil {
		t.Fatal("502 openObserveSearch succeeded")
	}
	ooStatus = http.StatusOK
	ooResp = map[string]any{"function_error": "boom"}
	if _, err := s.openObserveSearch(t.Context(), "SELECT 1", time.Now().Add(-time.Hour), time.Now(), 0, 1, ""); err == nil {
		t.Fatal("function_error openObserveSearch succeeded")
	}

	// getEvidenceArtifact validation & 404s.
	for _, query := range []string{"", "run_id=r&execution_id=e"} {
		if code, _ := request(t, http.MethodGet, srv.URL+"/api/evidence/artifact?"+query, ""); code != http.StatusBadRequest {
			t.Fatalf("getEvidenceArtifact(%s) = %d", query, code)
		}
	}
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/evidence/artifact?run_id=r&execution_id=missing&artifact_id=runs/r/a", ""); code != http.StatusNotFound {
		t.Fatalf("missing scope artifact = %d", code)
	}
	f.mu.Lock()
	closedAt := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	f.objects["workflow-executions/exec-a.json"], _ = json.Marshal(executionScope{
		SchemaVersion:  1,
		ExecutionID:    "exec-a",
		RunID:          runA,
		Workflow:       "feature",
		ArtifactPrefix: "runs/" + runA,
		StartedAt:      time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
		ClosedAt:       &closedAt,
		Status:         "completed",
	})
	f.mu.Unlock()
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/evidence/artifact?run_id="+runA+"&execution_id=exec-a&artifact_id=runs/"+runA+"/nope", ""); code != http.StatusNotFound {
		t.Fatalf("missing artifact = %d", code)
	}

	// getEvidence validation & pagination.
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/evidence?run_id="+runA, ""); code != http.StatusBadRequest {
		t.Fatalf("missing execution_id = %d", code)
	}
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/evidence?run_id="+runA+"&execution_id=missing", ""); code != http.StatusOK {
		t.Fatalf("missing scope evidence = %d", code)
	}
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/evidence?run_id=other&execution_id=exec-a", ""); code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched run_id = %d", code)
	}
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/evidence?run_id="+runA+"&execution_id=exec-a&trace_id="+strings.Repeat("b", 32), ""); code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched trace_id = %d", code)
	}
	for _, query := range []string{
		"limit=0",
		"cursor=!!!",
		"cursor=YWJj",
		"from=bad",
		"to=bad",
	} {
		if code, _ := request(t, http.MethodGet, srv.URL+"/api/evidence?run_id="+runA+"&execution_id=exec-a&"+query, ""); code != http.StatusBadRequest {
			t.Fatalf("getEvidence(%s) = %d", query, code)
		}
	}
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/evidence?run_id="+runA+"&execution_id=exec-a&from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z", ""); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid evidence range = %d", code)
	}

	ooResp = map[string]any{
		"hits": []map[string]any{
			{"span_id": "s1", "factory.execution_id": "exec-a"},
			{"span_id": "s2", "parent_span_id": "s1"},
		},
		"total":      2,
		"is_partial": false,
	}
	code, data := request(t, http.MethodGet, srv.URL+"/api/evidence?run_id="+runA+"&execution_id=exec-a&limit=1", "")
	if code != http.StatusOK {
		t.Fatalf("evidence page 1 = %d %s", code, data)
	}
	var ev struct {
		NextCursor   string           `json:"next_cursor"`
		SourceStatus string           `json:"source_status"`
		Spans        []map[string]any `json:"spans"`
		Artifacts    []map[string]any `json:"artifacts"`
	}
	_ = json.Unmarshal(data, &ev)
	if len(ev.Spans) != 1 || ev.NextCursor == "" || ev.SourceStatus != sourceAvailable || len(ev.Artifacts) == 0 {
		t.Fatalf("evidence page 1 = %+v", ev)
	}
	code, _ = request(t, http.MethodGet, srv.URL+"/api/evidence?run_id="+runA+"&execution_id=exec-a&limit=1&cursor="+ev.NextCursor, "")
	if code != http.StatusOK {
		t.Fatalf("evidence page 2 = %d", code)
	}

	// getSeries validation errors.
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"group_by=model&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", http.StatusUnprocessableEntity},
		{"metric=input_tokens&group_by=bad&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", http.StatusUnprocessableEntity},
		{"metric=duration_ms&group_by=model&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", http.StatusUnprocessableEntity},
		{"metric=quality&group_by=workflow&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", http.StatusUnprocessableEntity},
		{"metric=input_tokens&group_by=model&from=bad&to=2026-11-02T00:00:00Z", http.StatusBadRequest},
		{"metric=input_tokens&group_by=model&from=2026-11-01T00:00:00Z&to=bad", http.StatusBadRequest},
		{"metric=input_tokens&group_by=model&from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z", http.StatusUnprocessableEntity},
	} {
		if got, _ := request(t, http.MethodGet, srv.URL+"/api/series?"+tc.query, ""); got != tc.want {
			t.Fatalf("getSeries(%s) = %d, want %d", tc.query, got, tc.want)
		}
	}

	// getCostSeries edge cases: missing group, unavailable cost, bad fraction, missing conversion, invalid timestamp.
	hour := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ooResp = map[string]any{
		"hits": []map[string]any{
			{"_timestamp": hour.Format(time.RFC3339), "group_key": "", "amount_numerator": "1", "amount_denominator": "1", "currency": "EUR", "cost_status": "available"},
			{"_timestamp": hour.Format(time.RFC3339), "group_key": "m1", "amount_numerator": "1", "amount_denominator": "1", "currency": "EUR", "cost_status": "unavailable"},
			{"_timestamp": hour.Format(time.RFC3339), "group_key": "m1", "amount_numerator": "bad", "amount_denominator": "0", "currency": "EUR", "cost_status": "available"},
			{"_timestamp": hour.Format(time.RFC3339), "group_key": "m1", "amount_numerator": "1", "amount_denominator": "1", "currency": "USD", "cost_status": "available"},
		},
		"total":      4,
		"is_partial": false,
	}
	code, data = request(t, http.MethodGet, srv.URL+"/api/series?metric=cost&group_by=model&currency=EUR&from=2026-11-01T12:00:00Z&to=2026-11-01T14:00:00Z", "")
	if code != http.StatusOK {
		t.Fatalf("cost series edge cases = %d %s", code, data)
	}
	var costRes struct {
		Coverage map[string]int `json:"coverage"`
	}
	_ = json.Unmarshal(data, &costRes)
	if costRes.Coverage["missing"] != 4 {
		t.Fatalf("expected 4 missing, got %+v", costRes.Coverage)
	}

	ooResp = map[string]any{
		"hits":       []map[string]any{{"_timestamp": "bad-time", "group_key": "m1"}},
		"total":      1,
		"is_partial": false,
	}
	code, data = request(t, http.MethodGet, srv.URL+"/api/series?metric=cost&group_by=model&currency=EUR&from=2026-11-01T12:00:00Z&to=2026-11-01T14:00:00Z", "")
	if code != http.StatusOK || !strings.Contains(string(data), `"query_complete":false`) {
		t.Fatalf("invalid timestamp cost series = %d %s", code, data)
	}
}
