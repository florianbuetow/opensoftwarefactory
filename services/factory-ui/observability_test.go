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
	resp := get(t, ui.URL+"/api/observability/logs?run_id=11111111-1111-1111-1111-111111111111&trace_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa&span_id=bbbbbbbbbbbbbbbb&step=review&attempt=2&start=2026-01-01T00%3A00%3A00Z&end=2026-01-01T00%3A00%3A02Z")
	var result struct {
		Logs []observabilityLog `json:"logs"`
	}
	if err := json.Unmarshal([]byte(resp.body), &result); err != nil {
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
	resp := get(t, ui.URL+"/api/observability/logs?run_id='OR%201=1")
	if resp.status != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.status)
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
	resp := get(t, ui.URL+"/api/observability/spans?trace_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	var result struct {
		Spans []observabilitySpan `json:"spans"`
	}
	if err := json.Unmarshal([]byte(resp.body), &result); err != nil {
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
	resp := get(t, ui.URL+"/api/observability/health")
	var result map[string]any
	if err := json.Unmarshal([]byte(resp.body), &result); err != nil {
		t.Fatal(err)
	}
	if result["status"] != healthDegraded || result["retention_days"] != float64(7) {
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

// The configuration directory is shared with other pods: a link placed there must not make
// the UI read a file outside it.
func TestLoadObservabilityRefusesLinkOutOfConfigDirectory(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "elsewhere.env")
	if err := os.WriteFile(outside, []byte("ZO_ROOT_USER_EMAIL=ada@example.test\nZO_ROOT_USER_PASSWORD=safe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "openobserve.env")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	client := loadObservability("http://openobserve:5080", path, "http://logs:2020", "http://traces:13133", "http://traces:8888/metrics")
	if client.configured() {
		t.Fatalf("credentials read through a link out of the configuration directory: %+v", client)
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

func TestValidateObservabilityURL(t *testing.T) {
	for _, valid := range []string{"http://openobserve:5080", "https://obs.example.test/metrics"} {
		if got := validateObservabilityURL(valid); got != valid {
			t.Errorf("validateObservabilityURL(%q) = %q", valid, got)
		}
	}
	for _, invalid := range []string{
		"",
		"ftp://openobserve:5080",
		"http:///no-host",
		"http://user:pass@openobserve:5080",
		"http://openobserve:5080?q=1",
		"http://openobserve:5080#frag",
		"://bad-url",
	} {
		if got := validateObservabilityURL(invalid); got != "" {
			t.Errorf("validateObservabilityURL(%q) = %q, want empty", invalid, got)
		}
	}
}

func TestObservabilityHelpersAndClientErrors(t *testing.T) {
	if got := nestedString(map[string]any{"kubernetes": "not-a-map"}, "kubernetes", "pod_name"); got != "" {
		t.Fatalf("nestedString non-map = %q", got)
	}
	if got := firstNonempty("", ""); got != "" {
		t.Fatalf("firstNonempty empty = %q", got)
	}
	if _, ok := prometheusValue("only_one_field"); ok {
		t.Fatal("accepted single-field prometheus line")
	}
	if _, ok := prometheusValue("metric notanumber"); ok {
		t.Fatal("accepted non-numeric prometheus line")
	}
	if pending, over := storagePressure("not-json"); pending != 0 || over {
		t.Fatalf("storagePressure invalid = %d/%v", pending, over)
	}
	if pending, over := storagePressure(`[{"fs_chunks":2},{"overlimit":true}]`); pending != 2 || !over {
		t.Fatalf("storagePressure array = %d/%v", pending, over)
	}

	client := &observabilityClient{http: http.DefaultClient}
	if _, err := client.getText(t.Context(), ""); err == nil {
		t.Fatal("getText accepted empty endpoint")
	}
	if _, err := client.search(t.Context(), "SELECT 1", time.Now(), time.Now(), 1); err == nil {
		t.Fatal("search accepted unconfigured client")
	}

	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
		case "bad-json":
			_, _ = io.WriteString(w, "{bad")
		case "func-error":
			_, _ = io.WriteString(w, `{"function_error":"timeout"}`)
		case "degraded-json":
			_, _ = io.WriteString(w, `{"status":"degraded"}`)
		case "healthy-json":
			_, _ = io.WriteString(w, `{"status":"Server Available"}`)
		}
	}))
	defer srv.Close()

	client = &observabilityClient{base: srv.URL, user: "a", password: "b", http: srv.Client()}
	for _, m := range []string{"500", "bad-json", "func-error"} {
		mode = m
		if _, err := client.search(t.Context(), "SELECT 1", time.Now(), time.Now(), 1); err == nil {
			t.Errorf("search(%s): expected error", m)
		}
	}
	mode = "500"
	if _, err := client.getText(t.Context(), srv.URL); err == nil {
		t.Fatal("getText accepted 500")
	}
	mode = "degraded-json"
	if got := client.componentHealth(t.Context(), srv.URL); got["status"] != healthDegraded {
		t.Fatalf("componentHealth degraded = %#v", got)
	}
	mode = "healthy-json"
	if got := client.componentHealth(t.Context(), srv.URL); got["status"] != healthOK {
		t.Fatalf("componentHealth healthy = %#v", got)
	}
	if got := client.componentHealth(t.Context(), ""); got["status"] != healthUnavailable {
		t.Fatalf("componentHealth empty = %#v", got)
	}
}

func TestObservabilityHandlerValidationAndHealthStates(t *testing.T) {
	const validRun = "11111111-1111-1111-1111-111111111111"
	const validTrace = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Validation errors on /logs and /spans, plus 502 when unconfigured.
	ui := httptest.NewServer(observabilityHandler(&observabilityClient{http: http.DefaultClient}))
	defer ui.Close()
	for _, path := range []string{
		"/api/observability/logs?run_id=" + validRun + "&trace_id=bad",
		"/api/observability/logs?run_id=" + validRun + "&span_id=bad",
		"/api/observability/logs?run_id=" + validRun + "&step=bad%20step",
		"/api/observability/logs?run_id=" + validRun + "&attempt=0",
		"/api/observability/logs?run_id=" + validRun + "&attempt=abc",
		"/api/observability/logs?run_id=" + validRun + "&start=bad&end=2026-01-01T00:00:00Z",
		"/api/observability/logs?run_id=" + validRun + "&start=2026-01-02T00:00:00Z&end=2026-01-01T00:00:00Z",
		"/api/observability/spans?trace_id=short",
	} {
		if resp := get(t, ui.URL+path); resp.status != http.StatusBadRequest {
			t.Errorf("GET %s: status %d, want 400", path, resp.status)
		}
	}
	for _, path := range []string{
		"/api/observability/logs?run_id=" + validRun,
		"/api/observability/spans?trace_id=" + validTrace,
	} {
		if resp := get(t, ui.URL+path); resp.status != http.StatusBadGateway {
			t.Errorf("GET %s (unconfigured): status %d, want 502", path, resp.status)
		}
	}
	if resp := get(t, ui.URL+"/api/observability/health"); resp.status != http.StatusOK || !strings.Contains(resp.body, `"status":"unavailable"`) {
		t.Fatalf("unconfigured health = %d %s", resp.status, resp.body)
	}

	// Health states: all ok, degraded queue + storage overlimit, and failing sub-endpoints.
	const failAll = "fail-all"
	var scenario string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/default/_search":
			if scenario == failAll {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, `{"hits":[]}`)
		case "/api/v1/health", "/trace-health":
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case "/api/v1/metrics/prometheus":
			if scenario == failAll {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, "fluentbit_output_dropped_records_total 0\n")
		case "/api/v1/storage":
			if scenario == "degraded" {
				_, _ = io.WriteString(w, `{"chunks":{"fs_chunks":9},"status":{"overlimit":true}}`)
				return
			}
			_, _ = io.WriteString(w, `{"chunks":{"fs_chunks":0},"status":{"overlimit":false}}`)
		case "/trace-metrics":
			if scenario == failAll {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if scenario == "degraded" {
				_, _ = io.WriteString(w, "otelcol_exporter_queue_size{data_type=\"traces\"} 9\notelcol_exporter_queue_capacity{data_type=\"traces\"} 10\n")
				return
			}
			_, _ = io.WriteString(w, "otelcol_exporter_queue_size{data_type=\"traces\"} 1\notelcol_exporter_queue_capacity{data_type=\"traces\"} 10\n")
		}
	}))
	defer srv.Close()

	client := &observabilityClient{
		base: srv.URL, user: "a", password: "b", collectorBase: srv.URL,
		tracingHealth: srv.URL + "/trace-health", tracingMetrics: srv.URL + "/trace-metrics", http: srv.Client(),
	}
	ui2 := httptest.NewServer(observabilityHandler(client))
	defer ui2.Close()

	for _, tc := range []struct {
		name, want string
	}{
		{"ok", healthOK},
		{"degraded", healthDegraded},
		{"fail-all", healthUnavailable},
	} {
		scenario = tc.name
		resp := get(t, ui2.URL+"/api/observability/health")
		var result map[string]any
		if err := json.Unmarshal([]byte(resp.body), &result); err != nil {
			t.Fatal(err)
		}
		if result["status"] != tc.want {
			t.Errorf("scenario %s: status = %v, want %s", tc.name, result["status"], tc.want)
		}
	}
}
