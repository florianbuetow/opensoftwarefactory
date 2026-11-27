package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	runIDPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	traceIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	spanIDPattern  = regexp.MustCompile(`^[0-9a-fA-F]{16}$`)
	stepPattern    = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)
)

// The states of the health report and of each of its components.
const (
	healthOK          = "ok"
	healthDegraded    = "degraded"
	healthUnavailable = "unavailable"
)

type observabilityClient struct {
	http                                         *http.Client
	base, user, password                         string
	collectorBase, tracingHealth, tracingMetrics string
	retentionDays                                int
	retentionKnown                               bool
}

type observabilityLog struct {
	Timestamp any    `json:"timestamp,omitempty"`
	Level     string `json:"level,omitempty"`
	Message   string `json:"message,omitempty"`
	Container string `json:"container,omitempty"`
	Pod       string `json:"pod,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Step      string `json:"step,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Attempt   any    `json:"attempt,omitempty"`
	TraceID   string `json:"trace_id,omitempty"`
	SpanID    string `json:"span_id,omitempty"`
	Status    any    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

type observabilitySpan struct {
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	Operation    string `json:"operation,omitempty"`
	Service      string `json:"service,omitempty"`
	Start        any    `json:"start,omitempty"`
	End          any    `json:"end,omitempty"`
	Duration     any    `json:"duration,omitempty"`
	Status       string `json:"status,omitempty"`
}

type observabilitySearch struct {
	Error   string           `json:"function_error"`
	Hits    []map[string]any `json:"hits"`
	Total   int              `json:"total"`
	Partial bool             `json:"is_partial"`
}

func loadObservability(base, configPath, collectorBase, tracingHealth, tracingMetrics string) *observabilityClient {
	client := &observabilityClient{
		base: strings.TrimRight(base, "/"), collectorBase: strings.TrimRight(collectorBase, "/"),
		tracingHealth: strings.TrimRight(tracingHealth, "/"), tracingMetrics: strings.TrimRight(tracingMetrics, "/"),
		http: &http.Client{Timeout: 15 * time.Second},
	}
	data, err := readInDirectory(configPath)
	if err != nil {
		return client
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		switch key {
		case "ZO_ROOT_USER_EMAIL":
			client.user = value
		case "ZO_ROOT_USER_PASSWORD":
			client.password = value
		case "ZO_COMPACT_DATA_RETENTION_DAYS":
			_, err := fmt.Sscan(value, &client.retentionDays)
			client.retentionKnown = err == nil && client.retentionDays >= 0
		}
	}
	return client
}

// readInDirectory reads the file at path without leaving the directory that holds it: a
// link placed there that points anywhere else is refused.
func readInDirectory(path string) (_ []byte, err error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { err = closeAfter(err, root) }()
	return root.ReadFile(filepath.Base(path))
}

func (o *observabilityClient) configured() bool {
	return o != nil && o.base != "" && o.user != "" && o.password != ""
}

func (o *observabilityClient) search(ctx context.Context, sql string, start, end time.Time, size int) (observabilitySearch, error) {
	return o.searchType(ctx, sql, start, end, size, "")
}

func (o *observabilityClient) searchType(ctx context.Context, sql string, start, end time.Time, size int, signal string) (observabilitySearch, error) {
	return o.searchWithin(ctx, sql, start, end, size, signal, searchSeconds)
}

// searchSeconds is how long the store may take for one search before it gives up.
const searchSeconds = 12

// searchWithin is one search that the store gives up on after the given number of seconds.
func (o *observabilityClient) searchWithin(ctx context.Context, sql string, start, end time.Time, size int, signal string, seconds int) (_ observabilitySearch, err error) {
	if !o.configured() {
		return observabilitySearch{}, errors.New("log search is not configured")
	}
	body, _ := json.Marshal(map[string]any{"query": map[string]any{
		"sql": sql, "start_time": start.UnixMicro(), "end_time": end.UnixMicro(), "from": 0, "size": size,
	}, "search_type": "ui", "timeout": seconds})
	endpoint := o.base + "/api/default/_search"
	if signal != "" {
		endpoint += "?type=" + url.QueryEscape(signal)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return observabilitySearch{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(o.user, o.password)
	resp, err := o.http.Do(req)
	if err != nil {
		return observabilitySearch{}, err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == http.StatusRequestTimeout {
			return observabilitySearch{}, errSearchTooSlow
		}
		return observabilitySearch{}, fmt.Errorf("log search returned %s", resp.Status)
	}
	var result observabilitySearch
	err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result)
	if err != nil {
		return observabilitySearch{}, fmt.Errorf("decoding log search: %w", err)
	}
	if result.Error != "" {
		return observabilitySearch{}, errors.New("log search was incomplete")
	}
	return result, nil
}

func stringField(row map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := row[key].(string); ok {
			return value
		}
	}
	return ""
}

func nestedString(row map[string]any, parent, key string) string {
	if nested, ok := row[parent].(map[string]any); ok {
		return stringField(nested, key)
	}
	return ""
}

func normalizeLog(row map[string]any) observabilityLog {
	return observabilityLog{
		Timestamp: row["_timestamp"], Level: stringField(row, "level"), Message: stringField(row, "msg", "message", "log"),
		Container: firstNonempty(nestedString(row, "kubernetes", "container_name"), stringField(row, "kubernetes_container_name")),
		Pod:       firstNonempty(nestedString(row, "kubernetes", "pod_name"), stringField(row, "kubernetes_pod_name")),
		RunID:     stringField(row, "run_id"), Step: stringField(row, "step"), Agent: stringField(row, "agent"), Attempt: row["attempt"],
		TraceID: stringField(row, "trace_id"), SpanID: stringField(row, "span_id"), Status: row["status"], Error: stringField(row, "error"),
	}
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func observabilityHandler(client *observabilityClient) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/observability/logs", func(w http.ResponseWriter, r *http.Request) {
		runID := r.URL.Query().Get("run_id")
		if !runIDPattern.MatchString(runID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id must be a UUID"})
			return
		}
		traceID, spanID, step := r.URL.Query().Get("trace_id"), r.URL.Query().Get("span_id"), r.URL.Query().Get("step")
		if traceID != "" && !traceIDPattern.MatchString(traceID) || spanID != "" && !spanIDPattern.MatchString(spanID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "trace_id or span_id is invalid"})
			return
		}
		if step != "" && !stepPattern.MatchString(step) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "step is invalid"})
			return
		}
		now := time.Now().UTC()
		where := `run_id = '` + runID + `'`
		if traceID != "" {
			where += ` AND trace_id = '` + traceID + `'`
		}
		if spanID != "" {
			where += ` AND span_id = '` + spanID + `'`
		}
		if step != "" {
			where += ` AND step = '` + step + `'`
		}
		if attempt := r.URL.Query().Get("attempt"); attempt != "" {
			var number int
			if _, err := fmt.Sscan(attempt, &number); err != nil || number < 1 || number > 1000 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "attempt is invalid"})
				return
			}
			where += fmt.Sprintf(" AND (attempt = %d OR attempt IS NULL)", number)
		}
		searchStart, searchEnd := client.searchStart(now), now
		if rawStart, rawEnd := r.URL.Query().Get("start"), r.URL.Query().Get("end"); rawStart != "" || rawEnd != "" {
			started, startErr := time.Parse(time.RFC3339Nano, rawStart)
			finished, endErr := time.Parse(time.RFC3339Nano, rawEnd)
			if startErr != nil || endErr != nil || finished.Before(started) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "start and end must be an ordered RFC3339 interval"})
				return
			}
			// Include immediate boundary logs while excluding other retry attempts in the same trace.
			lower, upper := started.Add(-time.Second), finished.Add(time.Second)
			where += fmt.Sprintf(" AND _timestamp >= %d AND _timestamp <= %d", lower.UnixMicro(), upper.UnixMicro())
			searchStart, searchEnd = lower.Add(-time.Minute), upper.Add(time.Minute)
		}
		result, err := client.search(r.Context(), `SELECT * FROM "factory" WHERE `+where+` ORDER BY _timestamp ASC`, searchStart, searchEnd, 250)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		logs := make([]observabilityLog, 0, len(result.Hits))
		for _, row := range result.Hits {
			logs = append(logs, normalizeLog(row))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"logs": logs, "complete": !result.Partial && result.Total <= len(result.Hits),
			"partial": result.Partial, "truncated": result.Total > len(result.Hits), "total": result.Total,
		})
	})
	mux.HandleFunc("GET /api/observability/spans", func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("trace_id")
		if !traceIDPattern.MatchString(traceID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "trace_id must be 32 hexadecimal characters"})
			return
		}
		now := time.Now().UTC()
		sql := `SELECT * FROM "default" WHERE trace_id = '` + traceID + `' ORDER BY start_time ASC`
		var result observabilitySearch
		var err error
		var searchedAll bool
		// The caller knows when the attempt ran: searching only that time, with a margin for
		// spans that began before it or ended after it, answers at once whatever is kept.
		if rawStart, rawEnd := r.URL.Query().Get("start"), r.URL.Query().Get("end"); rawStart != "" || rawEnd != "" {
			started, startErr := time.Parse(time.RFC3339Nano, rawStart)
			finished, endErr := time.Parse(time.RFC3339Nano, rawEnd)
			if startErr != nil || endErr != nil || finished.Before(started) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "start and end must be an ordered RFC3339 interval"})
				return
			}
			// The time of the attempt is read an hour at a time, from its beginning, and all of it.
			result, searchedAll, err = client.searchByHour(r.Context(), sql, started.Add(-traceSearchMargin), finished.Add(traceSearchMargin), 500, "traces", false)
		} else {
			// Without a time, the newest hours are read first, until the trace is found or the
			// time for an answer is used up; the answer then says that not everything was
			// searched instead of failing.
			result, searchedAll, err = client.searchByHour(r.Context(), sql, client.searchStart(now), now, 500, "traces", true)
		}
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, errSearchTooSlow) {
				status = http.StatusGatewayTimeout
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		result.Partial = result.Partial || !searchedAll
		spans := make([]observabilitySpan, 0, len(result.Hits))
		seen := map[string]bool{}
		for _, row := range result.Hits {
			// A span that two neighbouring hours both returned is shown once.
			if id := stringField(row, "span_id"); id != "" {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			spans = append(spans, observabilitySpan{
				SpanID: stringField(row, "span_id"), ParentSpanID: stringField(row, "parent_span_id", "reference_parent_span_id"),
				Operation: stringField(row, "operation_name", "name"), Service: stringField(row, "service_name"), Status: stringField(row, "span_status", "status"),
				Start: row["start_time"], End: row["end_time"], Duration: row["duration"],
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"spans": spans, "complete": !result.Partial && result.Total <= len(result.Hits),
			"partial": result.Partial, "truncated": result.Total > len(result.Hits), "total": result.Total,
		})
	})
	mux.HandleFunc("GET /api/observability/health", func(w http.ResponseWriter, r *http.Request) {
		components := map[string]map[string]any{}
		status := healthOK
		if !client.configured() {
			components["logs"] = map[string]any{"status": healthUnavailable, "reason": "Log search is not configured"}
			status = healthUnavailable
		} else {
			now := time.Now().UTC()
			_, err := client.search(r.Context(), `SELECT _timestamp FROM "factory" ORDER BY _timestamp DESC`, now.Add(-10*time.Minute), now, 1)
			if err != nil {
				components["logs"] = map[string]any{"status": healthUnavailable, "reason": "Cannot query collected logs"}
				status = healthUnavailable
			} else {
				components["logs"] = map[string]any{"status": healthOK}
			}
		}
		collector := client.componentHealth(r.Context(), client.collectorBase+"/api/v1/health")
		if collector["status"] == healthOK {
			metrics, err := client.getText(r.Context(), client.collectorBase+"/api/v1/metrics/prometheus")
			if err != nil {
				collector = map[string]any{"status": healthUnavailable, "reason": "Cannot read log delivery metrics"}
			} else if value := failedDeliveryCount(metrics); value > 0 {
				collector = map[string]any{"status": healthDegraded, "reason": "Collected logs were dropped or exhausted delivery retries", "lost_records": value}
			}
			if storage, err := client.getText(r.Context(), client.collectorBase+"/api/v1/storage"); err == nil {
				pending, over := storagePressure(storage)
				collector["pending_chunks"] = pending
				if over {
					collector["status"], collector["reason"] = healthDegraded, "The log delivery buffer is over its configured limit"
				}
			}
		}
		components["log_delivery"] = collector
		traces := client.componentHealth(r.Context(), client.tracingHealth)
		if traces["status"] == healthOK {
			metrics, err := client.getText(r.Context(), client.tracingMetrics)
			if err != nil {
				traces = map[string]any{"status": healthUnavailable, "reason": "Cannot read trace delivery metrics"}
			} else if value := failedTraceCount(metrics); value > 0 {
				traces = map[string]any{"status": healthDegraded, "reason": "Trace export failures or refused spans were reported", "affected_spans": value}
			} else if size, capacity := traceQueue(metrics); capacity > 0 {
				traces["queued_batches"], traces["queue_capacity"] = size, capacity
				if size/capacity >= .8 {
					traces["status"], traces["reason"] = healthDegraded, "The trace delivery queue is near capacity"
				}
			}
		}
		components["traces"] = traces
		for _, component := range components {
			if component["status"] == healthUnavailable {
				status = healthUnavailable
			} else if component["status"] == healthDegraded && status == healthOK {
				status = healthDegraded
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": status, "components": components, "retention_days": client.retentionDays, "retention_known": client.retentionKnown || client.retentionDays > 0})
	})
	return mux
}

func (o *observabilityClient) getText(ctx context.Context, endpoint string) (_ string, err error) {
	if endpoint == "" {
		return "", errors.New("endpoint is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("health endpoint returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(data), err
}

func (o *observabilityClient) componentHealth(ctx context.Context, endpoint string) map[string]any {
	body, err := o.getText(ctx, endpoint)
	if err != nil {
		return map[string]any{"status": healthUnavailable, "reason": "Telemetry component is unreachable"}
	}
	var result map[string]any
	if json.Unmarshal([]byte(body), &result) == nil {
		state := strings.ToLower(fmt.Sprint(result["status"]))
		if state != "" && state != "ok" && state != "healthy" && state != "ready" && state != "server available" {
			return map[string]any{"status": healthDegraded, "reason": "Telemetry component reports " + state}
		}
	}
	return map[string]any{"status": healthOK}
}

func failedDeliveryCount(metrics string) float64 {
	var total float64
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "#") || !strings.Contains(line, "fluentbit_output_") {
			continue
		}
		if !strings.Contains(line, "dropped_records_total") && !strings.Contains(line, "retries_failed_total") {
			continue
		}
		if value, ok := prometheusValue(line); ok {
			total += value
		}
	}
	return total
}

func failedTraceCount(metrics string) float64 {
	var total float64
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "otelcol_") {
			continue
		}
		if !strings.Contains(line, "send_failed_spans") && !strings.Contains(line, "refused_spans") && !strings.Contains(line, "enqueue_failed_spans") {
			continue
		}
		if value, ok := prometheusValue(line); ok {
			total += value
		}
	}
	return total
}

func traceQueue(metrics string) (size, capacity float64) {
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "#") || !strings.Contains(line, `data_type="traces"`) {
			continue
		}
		value, ok := prometheusValue(line)
		if !ok {
			continue
		}
		if strings.HasPrefix(line, "otelcol_exporter_queue_size") {
			size += value
		} else if strings.HasPrefix(line, "otelcol_exporter_queue_capacity") {
			capacity += value
		}
	}
	return size, capacity
}

func prometheusValue(line string) (float64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, false
	}
	var value float64
	if _, err := fmt.Sscan(fields[len(fields)-2], &value); err == nil && len(fields) > 2 {
		// Fluent Bit appends a millisecond timestamp; the sample value precedes it.
		return value, true
	}
	if _, err := fmt.Sscan(fields[len(fields)-1], &value); err != nil {
		return 0, false
	}
	return value, true
}

func storagePressure(raw string) (pending int, over bool) {
	var root any
	if json.Unmarshal([]byte(raw), &root) != nil {
		return 0, false
	}
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			for key, child := range item {
				if key == "fs_chunks" {
					if number, ok := child.(float64); ok {
						pending += int(number)
					}
				}
				if key == "overlimit" {
					over = over || child == true
				}
				walk(child)
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(root)
	return pending, over
}

func validateObservabilityURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return raw
}

// unlimitedSearchDays bounds a search that has no time of its own when nothing expires.
// OpenObserve refuses a search from Unix time 0 ("invalid time range") and gives up on one
// over decades (408 Request Timeout), since it looks at every hour of the range. A caller
// that knows when its run happened passes start and end instead and is not bound by this.
const unlimitedSearchDays = 90

// errSearchTooSlow is the telemetry store giving up on a search: the period was too long for it.
var errSearchTooSlow = errors.New("the telemetry store took too long to answer")

// The store answers a search over about an hour of traces in a few seconds and gives up on
// much more. So a period is read an hour at a time: each hour may take the store
// hourSearchSeconds, and all of them together hourSearchBudget; what could not be read in that
// time is left out, and the answer says so.
const (
	searchSlice       = time.Hour
	hourSearchSeconds = 6
	hourSearchBudget  = 10 * time.Second
)

// searchByHour searches the period from start to end one hour at a time and returns the hits
// of all hours, at most size. newestFirst reads the latest hour first and stops at the first
// hour that has hits, taking the hour before it along (what is looked for, one trace, lies
// close together in time); otherwise every hour is read, from the first. complete is false
// when the time for an answer ran out, or the store gave up on an hour, before that was done.
func (o *observabilityClient) searchByHour(ctx context.Context, sql string, start, end time.Time, size int, signal string, newestFirst bool) (result observabilitySearch, complete bool, err error) {
	type hour struct{ from, to time.Time }
	var hours []hour
	for from := start; from.Before(end); from = from.Add(searchSlice) {
		to := from.Add(searchSlice)
		if to.After(end) {
			to = end
		}
		hours = append(hours, hour{from, to})
	}
	if newestFirst {
		// Counted back from the end, so the newest hour is a full one.
		hours = hours[:0]
		for to := end; to.After(start); to = to.Add(-searchSlice) {
			from := to.Add(-searchSlice)
			if from.Before(start) {
				from = start
			}
			hours = append(hours, hour{from, to})
		}
	}
	deadline := time.Now().Add(hourSearchBudget)
	takeAlong := false
	for _, h := range hours {
		if time.Now().After(deadline) {
			return result, false, nil
		}
		found, searchErr := o.searchWithin(ctx, sql, h.from, h.to, size, signal, hourSearchSeconds)
		if errors.Is(searchErr, errSearchTooSlow) {
			return result, false, nil
		}
		if searchErr != nil {
			return observabilitySearch{}, false, searchErr
		}
		if newestFirst {
			// An earlier hour holds the earlier spans: they come first.
			result.Hits = append(found.Hits, result.Hits...)
		} else {
			result.Hits = append(result.Hits, found.Hits...)
		}
		result.Total += found.Total
		result.Partial = result.Partial || found.Partial
		if len(result.Hits) > size {
			result.Hits = result.Hits[:size]
		}
		if takeAlong {
			return result, true, nil
		}
		takeAlong = newestFirst && len(found.Hits) > 0
	}
	return result, true, nil
}

// traceSearchMargin widens the time of an attempt when its trace is searched.
const traceSearchMargin = 10 * time.Minute

// No configured expiry must not silently narrow a query to seven days.
func (o *observabilityClient) searchStart(now time.Time) time.Time {
	if o.retentionDays > 0 {
		return now.AddDate(0, 0, -o.retentionDays)
	}
	return now.AddDate(0, 0, -unlimitedSearchDays)
}
