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
	conversion := Conversion{ID: "usd-eur", FromCurrency: "USD", ToCurrency: "EUR", Rate: "0.9"}
	f.mu.Lock()
	f.objects[conversionPrefix+"/usd-eur.json"], _ = json.Marshal(conversion)
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
