package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObservabilityLogsAreFilteredAndNormalized(t *testing.T) {
	var auth, query string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query struct {
				SQL string `json:"sql"`
			} `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		query = request.Query.SQL
		user, pass, _ := r.BasicAuth()
		auth = user + ":" + pass
		_, _ = io.WriteString(w, `{"total":1,"hits":[
			{"_timestamp":10,"run_id":"11111111-1111-1111-1111-111111111111","trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","span_id":"bbbbbbbbbbbbbbbb","msg":"matched","password":"secret","kubernetes":{"container_name":"agent","pod_name":"agent-1"}}
		]}`)
	}))
	defer upstream.Close()
	client := &observabilityClient{base: upstream.URL, user: "ada", password: "secret", http: upstream.Client()}
	ui := httptest.NewServer(observabilityHandler(client))
	defer ui.Close()
	resp, err := http.Get(ui.URL + "/api/observability/logs?run_id=11111111-1111-1111-1111-111111111111&trace_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa&span_id=bbbbbbbbbbbbbbbb&step=review&attempt=2&start=2026-01-01T00%3A00%3A00Z&end=2026-01-01T00%3A00%3A02Z")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Logs []observabilityLog `json:"logs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if auth != "ada:secret" || !strings.Contains(query, "run_id = '11111111-1111-1111-1111-111111111111'") || !strings.Contains(query, "span_id = 'bbbbbbbbbbbbbbbb'") || !strings.Contains(query, "attempt = 2 OR attempt IS NULL") || !strings.Contains(query, "_timestamp >=") {
		t.Fatalf("auth/query = %q / %q", auth, query)
	}
	if len(result.Logs) != 1 || result.Logs[0].Message != "matched" || result.Logs[0].Container != "agent" {
		t.Fatalf("logs = %+v", result.Logs)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "password") || strings.Contains(string(encoded), "secret") {
		t.Fatalf("raw or secret fields escaped normalization: %s", encoded)
	}
}

func TestObservabilityRejectsInvalidRunID(t *testing.T) {
	ui := httptest.NewServer(observabilityHandler(&observabilityClient{}))
	defer ui.Close()
	resp, err := http.Get(ui.URL + "/api/observability/logs?run_id='OR%201=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestObservabilitySpansAreRestrictedAndNormalized(t *testing.T) {
	var path, query string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.String()
		var request struct {
			Query struct {
				SQL string `json:"sql"`
			} `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		query = request.Query.SQL
		_, _ = io.WriteString(w, `{"total":1,"hits":[{"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","span_id":"bbbbbbbbbbbbbbbb","reference_parent_span_id":"cccccccccccccccc","operation_name":"agent.process","service_name":"agent","span_status":"ERROR","duration":42,"secret":"hidden"}]}`)
	}))
	defer upstream.Close()
	client := &observabilityClient{base: upstream.URL, user: "a", password: "b", http: upstream.Client(), retentionDays: 7}
	ui := httptest.NewServer(observabilityHandler(client))
	defer ui.Close()
	resp, err := http.Get(ui.URL + "/api/observability/spans?trace_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Spans []observabilitySpan `json:"spans"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if path != "/api/default/_search?type=traces" || !strings.Contains(query, "trace_id = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'") || len(result.Spans) != 1 || result.Spans[0].Operation != "agent.process" || result.Spans[0].ParentSpanID != "cccccccccccccccc" {
		t.Fatalf("path/query/result = %q / %q / %+v", path, query, result)
	}
}

func TestObservabilityHealthReportsDeliveryLoss(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/default/_search":
			_, _ = io.WriteString(w, `{"hits":[]}`)
		case "/api/v1/health", "/trace-health":
			_, _ = io.WriteString(w, `ok`)
		case "/api/v1/metrics/prometheus":
			_, _ = io.WriteString(w, "fluentbit_output_dropped_records_total{name=\"openobserve\"} 2\n")
		case "/api/v1/storage":
			_, _ = io.WriteString(w, `{"chunks":{"fs_chunks":4},"status":{"overlimit":false}}`)
		case "/trace-metrics":
			_, _ = io.WriteString(w, "otelcol_exporter_send_failed_spans_total 3\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &observabilityClient{
		base: server.URL, user: "a", password: "b", collectorBase: server.URL,
		tracingHealth: server.URL + "/trace-health", tracingMetrics: server.URL + "/trace-metrics", http: server.Client(), retentionDays: 7,
	}
	ui := httptest.NewServer(observabilityHandler(client))
	defer ui.Close()
	resp, err := http.Get(ui.URL + "/api/observability/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "degraded" || result["retention_days"] != float64(7) {
		t.Fatalf("health = %#v", result)
	}
}

func TestTelemetryPressureParsers(t *testing.T) {
	size, capacity := traceQueue("otelcol_exporter_queue_size{data_type=\"traces\"} 8\notelcol_exporter_queue_capacity{data_type=\"traces\"} 10\n")
	if size != 8 || capacity != 10 {
		t.Fatalf("queue = %v/%v", size, capacity)
	}
	pending, over := storagePressure(`{"chunks":{"fs_chunks":3},"status":{"overlimit":true}}`)
	if pending != 3 || !over {
		t.Fatalf("storage = %d/%v", pending, over)
	}
}

func TestPrometheusValueIgnoresFluentBitTimestamp(t *testing.T) {
	value, ok := prometheusValue(`fluentbit_output_errors_total{name="openobserve"} 0 1794071007934`)
	if !ok || value != 0 {
		t.Fatalf("value = %v/%v", value, ok)
	}
}

func TestLoadObservabilityReadsLocalCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openobserve.env")
	if err := os.WriteFile(path, []byte("ZO_ROOT_USER_EMAIL=ada@example.test\nZO_ROOT_USER_PASSWORD=safe\nZO_COMPACT_DATA_RETENTION_DAYS=7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := loadObservability("http://openobserve:5080", path, "http://logs:2020", "http://traces:13133", "http://traces:8888/metrics")
	if client.user != "ada@example.test" || client.password != "safe" || client.retentionDays != 7 {
		t.Fatalf("client = %+v", client)
	}
}

func TestUnlimitedRetentionDoesNotUseSevenDayWindow(t *testing.T) {
	client := &observabilityClient{retentionKnown: true}
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	if got := client.searchStart(now); !got.Equal(time.Unix(0, 0)) {
		t.Fatalf("unlimited search starts at %v", got)
	}
	client.retentionDays = 7
	if got := client.searchStart(now); !got.Equal(now.AddDate(0, 0, -7)) {
		t.Fatalf("explicit retention ignored: %v", got)
	}
}
