package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var safeQueryID = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,255}$`)

// What the evidence and series endpoints report about their sources.
const (
	sourceAvailable   = "available"
	sourceUnavailable = "unavailable"
	sourcePartial     = "partial"
	executionUnknown  = "unknown"
	pointKnown        = "known"
	seriesComplete    = "complete"
	seriesRefreshing  = "refreshing"
)

// The metrics, groupings and columns a series can be asked for.
const (
	metricCost        = "cost"
	metricDuration    = "duration_ms"
	groupModel        = "model"
	groupWorkflow     = "workflow"
	groupAgent        = "agent"
	groupRun          = "run"
	columnNumberValue = "number_value"
)

type openObserveResult struct {
	Error   string           `json:"function_error"`
	Hits    []map[string]any `json:"hits"`
	Total   int              `json:"total"`
	Partial bool             `json:"is_partial"`
}
type openObserveHTTPError struct{ Status int }

func (e openObserveHTTPError) Error() string {
	return fmt.Sprintf("OpenObserve query returned HTTP %d", e.Status)
}

func (s *service) openObserveSearch(ctx context.Context, sql string, from, to time.Time, offset, limit int, signal string) (result openObserveResult, err error) {
	if s.openobserve == "" || s.openobserveUser == "" || s.openobservePassword == "" {
		return openObserveResult{}, errors.New("OpenObserve query is not configured")
	}
	body, _ := json.Marshal(map[string]any{"query": map[string]any{"sql": sql, "start_time": from.UnixMicro(), "end_time": to.UnixMicro(), "from": offset, "size": limit}, "search_type": "ui", "timeout": 12})
	endpoint := s.openobserve + "/api/default/_search?use_cache=false"
	if signal != "" {
		endpoint += "&type=" + signal
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return openObserveResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(s.openobserveUser, s.openobservePassword)
	resp, err := s.http.Do(req)
	if err != nil {
		return openObserveResult{}, err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode != 200 {
		return openObserveResult{}, openObserveHTTPError{resp.StatusCode}
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&result)
	if err != nil {
		return result, err
	}
	if result.Error != "" {
		return result, errors.New("OpenObserve reported an incomplete query")
	}
	return result, nil
}
func sqlLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (s *service) getEvidenceArtifact(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runID, executionID, artifactID := q.Get("run_id"), q.Get("execution_id"), q.Get("artifact_id")
	if runID == "" || executionID == "" || artifactID == "" || !safeQueryID.MatchString(runID) || !safeQueryID.MatchString(executionID) {
		s.fail(w, 400, errors.New("run_id, execution_id, and artifact_id are required"))
		return
	}
	scope, err := s.executionScope(r.Context(), executionID)
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("execution evidence scope not found"))
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	if scope.RunID != runID || !artifactInScope(artifactID, scope.ArtifactPrefix) {
		s.fail(w, 422, errors.New("artifact does not belong to the requested execution scope"))
		return
	}
	const limit int64 = 1 << 20
	object, total, err := s.readObjectLimited(r.Context(), artifactID, limit)
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("artifact not found"))
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	content := object.Data
	truncated := int64(len(content)) > limit || total > limit
	if int64(len(content)) > limit {
		content = content[:limit]
	}
	writeJSON(w, 200, map[string]any{"artifact_id": artifactID, "content": string(content), "truncated": truncated, "bytes": total})
}

func (s *service) getEvidence(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runID, executionID, traceID := q.Get("run_id"), q.Get("execution_id"), q.Get("trace_id")
	if runID == "" || executionID == "" || !safeQueryID.MatchString(runID) || !safeQueryID.MatchString(executionID) || traceID != "" && !regexp.MustCompile(`^[a-fA-F0-9]{32}$`).MatchString(traceID) {
		s.fail(w, 400, errors.New("run_id and execution_id are required; trace_id must be 32 hex characters when provided"))
		return
	}
	scope, scopeErr := s.executionScope(r.Context(), executionID)
	if scopeErr != nil {
		writeJSON(w, 200, map[string]any{"spans": []any{}, "artifacts": []any{}, "next_cursor": "", "query_complete": false, "source_status": sourceUnavailable, "execution_status": executionUnknown})
		return
	}
	if scope.RunID != runID {
		s.fail(w, 422, errors.New("run_id does not identify the requested execution"))
		return
	}
	var retainedRun struct {
		DistributedTraceID string `json:"distributed_trace_id"`
	}
	_ = s.getJSON(r.Context(), s.blobs+"/blobs/runs/"+runID+"/run.json", &retainedRun)
	if traceID == "" {
		traceID = retainedRun.DistributedTraceID
	} else if retainedRun.DistributedTraceID != "" && traceID != retainedRun.DistributedTraceID {
		s.fail(w, 422, errors.New("trace_id does not belong to the requested run"))
		return
	}
	limit := 100
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			s.fail(w, 400, errors.New("limit must be 1..1000"))
			return
		}
		limit = n
	}
	offset := 0
	if raw := q.Get("cursor"); raw != "" {
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			s.fail(w, 400, errors.New("invalid cursor"))
			return
		}
		offset, err = strconv.Atoi(string(b))
		if err != nil || offset < 0 {
			s.fail(w, 400, errors.New("invalid cursor"))
			return
		}
	}
	to := time.Now().UTC()
	if scope.ClosedAt != nil {
		to = scope.ClosedAt.Add(5 * time.Minute)
	}
	from := scope.StartedAt.Add(-5 * time.Minute)
	if scope.StartedAt.IsZero() {
		from = to.Add(-30 * 24 * time.Hour)
	}
	var err error
	if raw := q.Get("from"); raw != "" {
		from, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			s.fail(w, 400, errors.New("from must be RFC3339"))
			return
		}
	}
	if raw := q.Get("to"); raw != "" {
		to, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			s.fail(w, 400, errors.New("to must be RFC3339"))
			return
		}
	}
	if !from.Before(to) || to.Sub(from) > 366*24*time.Hour {
		s.fail(w, 422, errors.New("evidence range must be positive and at most 366 days"))
		return
	}
	var result openObserveResult
	var queryErr error
	if traceID == "" {
		queryErr = errors.New("trace identity is unavailable")
	} else {
		sql := "SELECT * FROM \"default\" WHERE trace_id = " + sqlLiteral(traceID) + " ORDER BY start_time ASC"
		result, queryErr = s.openObserveSearch(r.Context(), sql, from, to, 0, 5000, "traces")
	}
	spans := []map[string]any{}
	sourceStatus := sourceAvailable
	if queryErr != nil {
		sourceStatus = sourceUnavailable
	} else {
		var scoped bool
		spans, scoped = executionSubtree(result.Hits, executionID)
		if !scoped {
			sourceStatus = sourceUnavailable
			spans = []map[string]any{}
		}
		if result.Partial && sourceStatus != sourceUnavailable {
			sourceStatus = sourcePartial
		}
	}
	artifacts := []map[string]any{}
	var listing struct {
		Objects []map[string]any `json:"objects"`
	}
	artifactErr := s.getJSON(r.Context(), s.blobs+"/trace/"+runID, &listing)
	if artifactErr == nil {
		for _, artifact := range listing.Objects {
			id, _ := artifact["id"].(string)
			if artifactInScope(id, scope.ArtifactPrefix) {
				artifacts = append(artifacts, artifact)
			}
		}
	} else if sourceStatus == sourceAvailable {
		sourceStatus = sourcePartial
	}
	totalScoped := len(spans)
	if offset > totalScoped {
		offset = totalScoped
	}
	end := offset + limit
	if end > totalScoped {
		end = totalScoped
	}
	if queryErr == nil {
		spans = spans[offset:end]
	}
	next := ""
	complete := queryErr == nil && sourceStatus != sourceUnavailable && !result.Partial && result.Total <= 5000 && end >= totalScoped
	if queryErr == nil && end < totalScoped {
		next = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	executionStatus := scope.Status
	if executionStatus == "" {
		executionStatus = executionUnknown
	}
	writeJSON(w, 200, map[string]any{"spans": spans, "artifacts": artifacts, "next_cursor": next, "query_complete": complete, "source_status": sourceStatus, "execution_status": executionStatus})
}

func (s *service) getCostSeries(w http.ResponseWriter, r *http.Request, groupField string, from, to time.Time, currency string) {
	pending, err := s.pendingProjectionHours(r.Context(), from, to)
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	pendingSet := map[time.Time]bool{}
	for _, hour := range pending {
		pendingSet[hour.UTC().Truncate(time.Hour)] = true
	}
	stream := s.projectionStreamName()
	sql := fmt.Sprintf("SELECT _timestamp, %s AS group_key, amount_numerator, amount_denominator, currency, cost_status FROM %q WHERE kind = 'usage' ORDER BY _timestamp ASC", groupField, stream)
	result, err := s.openObserveSearch(r.Context(), sql, from, to, 0, 10000, "")
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	complete := !result.Partial && result.Total <= len(result.Hits)
	if !complete {
		writeJSON(w, 200, map[string]any{"series": []seriesLine{}, "coverage": map[string]int{"known": 0, "missing": 0}, "query_complete": false, "projection": map[string]any{"status": seriesRefreshing, "pending_hours": pending}})
		return
	}
	conversions, err := s.loadConversions(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	rates := map[string]*big.Rat{}
	for _, conversion := range conversions {
		if conversion.ToCurrency == currency {
			rate, _ := decimal(conversion.Rate, true)
			rates[conversion.FromCurrency] = rate
		}
	}
	type bucketKey struct {
		Hour  time.Time
		Group string
	}
	buckets := map[bucketKey]*big.Rat{}
	totals := map[string]*big.Rat{}
	missing := 0
	invalidTimestamp := false
	for _, row := range result.Hits {
		timestamp, ok := timeValue(row["_timestamp"])
		if !ok {
			invalidTimestamp = true
			continue
		}
		hour := timestamp.UTC().Truncate(time.Hour)
		if pendingSet[hour] {
			continue
		}
		group := stringValue(row["group_key"])
		if group == "" {
			missing++
			continue
		}
		if stringValue(row["cost_status"]) != costAvailable {
			missing++
			continue
		}
		numerator, okN := new(big.Int).SetString(stringValue(row["amount_numerator"]), 10)
		denominator, okD := new(big.Int).SetString(stringValue(row["amount_denominator"]), 10)
		if !okN || !okD || denominator.Sign() <= 0 {
			missing++
			continue
		}
		amount := new(big.Rat).SetFrac(numerator, denominator)
		source := stringValue(row["currency"])
		if source != currency {
			rate := rates[source]
			if rate == nil {
				missing++
				continue
			}
			amount.Mul(amount, rate)
		}
		key := bucketKey{Hour: hour, Group: group}
		if buckets[key] == nil {
			buckets[key] = new(big.Rat)
		}
		buckets[key].Add(buckets[key], amount)
		if totals[group] == nil {
			totals[group] = new(big.Rat)
		}
		totals[group].Add(totals[group], amount)
	}
	if invalidTimestamp {
		writeJSON(w, 200, map[string]any{"series": []seriesLine{}, "coverage": map[string]int{"known": 0, "missing": 0}, "query_complete": false, "projection": map[string]any{"status": seriesRefreshing, "pending_hours": pending}})
		return
	}
	byGroup := map[string][]seriesPoint{}
	for key, total := range buckets {
		byGroup[key.Group] = append(byGroup[key.Group], seriesPoint{Time: key.Hour, Value: json.Number(decimalString(total)), Status: pointKnown, Approximate: true})
	}
	lines := make([]seriesLine, 0, len(byGroup))
	for group, points := range byGroup {
		sort.Slice(points, func(i, j int) bool { return points[i].Time.Before(points[j].Time) })
		total := totals[group]
		lines = append(lines, seriesLine{Key: group, Points: points, ExactTotal: &exactSeriesValue{Numerator: total.Num().String(), Denominator: total.Denom().String(), Decimal: decimalString(total)}, ExactComplete: true})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].Key < lines[j].Key })
	known := 0
	for _, line := range lines {
		known += len(line.Points)
	}
	status := seriesComplete
	if len(pending) > 0 {
		status = seriesRefreshing
	}
	writeJSON(w, 200, map[string]any{"series": lines, "coverage": map[string]int{"known": known, "missing": missing}, "query_complete": true, "projection": map[string]any{"status": status, "pending_hours": pending}})
}

func executionSubtree(rows []map[string]any, executionID string) ([]map[string]any, bool) {
	children := map[string][]int{}
	explicit := make([]string, len(rows))
	seeds := []int{}
	for i, row := range rows {
		parent := firstRowString(row, "parent_span_id", "reference_parent_span_id")
		children[parent] = append(children[parent], i)
		explicit[i] = spanExecutionID(row)
		if explicit[i] == executionID {
			seeds = append(seeds, i)
		}
	}
	if len(seeds) == 0 {
		return nil, false
	}
	included := map[int]bool{}
	queue := append([]int(nil), seeds...)
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		if included[i] {
			continue
		}
		if explicit[i] != "" && explicit[i] != executionID {
			continue
		}
		included[i] = true
		span := firstRowString(rows[i], "span_id")
		queue = append(queue, children[span]...)
	}
	out := make([]map[string]any, 0, len(included))
	for i, row := range rows {
		if included[i] {
			out = append(out, row)
		}
	}
	return out, true
}

func firstRowString(row map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := row[key].(string); ok {
			return value
		}
	}
	return ""
}

func spanExecutionID(row map[string]any) string {
	for _, key := range []string{"factory.execution_id", "factory_execution_id"} {
		if value := firstRowString(row, key); value != "" {
			return value
		}
	}
	attrs := row["span_attributes"]
	if text, ok := attrs.(string); ok {
		var decoded map[string]any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			attrs = decoded
		}
	}
	if values, ok := attrs.(map[string]any); ok {
		for _, key := range []string{"factory.execution_id", "factory_execution_id"} {
			if value, ok := values[key].(string); ok {
				return value
			}
		}
	}
	return ""
}

type seriesPoint struct {
	Time        time.Time `json:"time"`
	Value       any       `json:"value"`
	Status      string    `json:"status"`
	Approximate bool      `json:"approximate,omitempty"`
}
type seriesLine struct {
	Key           string            `json:"key"`
	ExactTotal    *exactSeriesValue `json:"exact_total,omitempty"`
	Points        []seriesPoint     `json:"points"`
	ExactComplete bool              `json:"exact_complete,omitempty"`
}
type exactSeriesValue struct {
	Numerator   string `json:"numerator"`
	Denominator string `json:"denominator"`
	Decimal     string `json:"decimal"`
}

func (s *service) getSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metric, group, currency := q.Get("metric"), q.Get("group_by"), q.Get("currency")
	allowedMetric := map[string]string{metricCost: "converted_amount", "input_tokens": "input_tokens", "output_tokens": "output_tokens", "cached_input_tokens": "cached_input_tokens", "uncached_input_tokens": "uncached_input_tokens", "reasoning_tokens": "reasoning_tokens", metricDuration: metricDuration}
	field, ok := allowedMetric[metric]
	if !ok {
		if strings.TrimSpace(metric) == "" || len(metric) > 255 {
			s.fail(w, 422, errors.New("metric is required"))
			return
		}
		field = columnNumberValue
	}
	groups := map[string]string{groupModel: "model", groupWorkflow: "workflow", groupAgent: "agent", groupRun: "run_id"}
	groupField, ok := groups[group]
	if !ok {
		s.fail(w, 422, errors.New("group_by must be model, workflow, agent, or run"))
		return
	}
	if metric == metricDuration && (group == groupModel || group == groupAgent) {
		s.fail(w, 422, errors.New("execution duration has no exact agent or model attribution"))
		return
	}
	if field == columnNumberValue && (group == groupModel || group == groupWorkflow) {
		s.fail(w, 422, errors.New("judge metrics have no exact target model or workflow attribution"))
		return
	}
	if field == columnNumberValue && group == groupRun {
		groupField = "target_run_id"
	}
	if group == groupModel && (metric == metricDuration || field == columnNumberValue) {
		s.fail(w, 422, errors.New("this whole-execution metric has no exact model attribution"))
		return
	}
	if metric == metricCost && currency != currencyEUR && currency != currencyUSD {
		s.fail(w, 422, errors.New("currency must be EUR or USD"))
		return
	}
	from, err := time.Parse(time.RFC3339, q.Get("from"))
	if err != nil {
		s.fail(w, 400, errors.New("from must be RFC3339"))
		return
	}
	to, err := time.Parse(time.RFC3339, q.Get("to"))
	if err != nil {
		s.fail(w, 400, errors.New("to must be RFC3339"))
		return
	}
	if !from.Before(to) || to.Sub(from) > 366*24*time.Hour {
		s.fail(w, 422, errors.New("series range must be positive and at most 366 days"))
		return
	}
	if metric == metricCost {
		s.getCostSeries(w, r, groupField, from, to, currency)
		return
	}
	baseWhere := "kind = 'usage'"
	if metric == metricDuration {
		baseWhere = "kind = 'execution'"
	}
	if field == columnNumberValue {
		baseWhere = "kind = 'judge' AND metric_name = " + sqlLiteral(metric)
	}
	where := field + " IS NOT NULL AND status IN ('reported','derived','estimated') AND " + baseWhere
	if metric == metricCost {
		baseWhere = "kind = 'usage' AND display_currency = " + sqlLiteral(currency)
		where = "converted_amount IS NOT NULL AND cost_status = 'available' AND " + baseWhere
	}
	stream := s.projectionStreamName()
	sql := fmt.Sprintf("SELECT histogram(_timestamp, '1 hour') AS bucket, %s AS group_key, SUM(CAST(%s AS DOUBLE)) AS value, 'known' AS status FROM %q WHERE %s GROUP BY bucket, group_key ORDER BY bucket ASC", groupField, field, stream, where)
	result, queryErr := s.openObserveSearch(r.Context(), sql, from, to, 0, 10000, "")
	if queryErr != nil {
		s.dependencyFailure(w, queryErr)
		return
	}
	lines := map[string][]seriesPoint{}
	known := 0
	invalidBucket := false
	for _, row := range result.Hits {
		key := stringValue(row["group_key"])
		if key == "" {
			continue
		}
		timestamp, ok := timeValue(row["bucket"])
		if !ok {
			invalidBucket = true
			continue
		}
		value := row["value"]
		lines[key] = append(lines[key], seriesPoint{Time: timestamp, Value: value, Status: pointKnown, Approximate: metric == metricCost})
		known++
	}
	out := make([]seriesLine, 0, len(lines))
	for key, points := range lines {
		out = append(out, seriesLine{Key: key, Points: points})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	pending, err := s.pendingProjectionHours(r.Context(), from, to)
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	pendingSet := map[time.Time]bool{}
	for _, hour := range pending {
		pendingSet[hour.UTC().Truncate(time.Hour)] = true
	}
	known = 0
	for i := range out {
		filtered := out[i].Points[:0]
		for _, point := range out[i].Points {
			if pendingSet[point.Time.UTC().Truncate(time.Hour)] {
				continue
			}
			filtered = append(filtered, point)
			known++
		}
		out[i].Points = filtered
	}
	nonempty := out[:0]
	for _, line := range out {
		if len(line.Points) > 0 {
			nonempty = append(nonempty, line)
		}
	}
	out = nonempty
	queryIncomplete := result.Partial || result.Total > len(result.Hits) || invalidBucket
	if queryIncomplete {
		out = []seriesLine{}
		known = 0
	}
	exactComplete := true
	if metric == metricCost && !queryIncomplete {
		exactSQL := fmt.Sprintf("SELECT _timestamp, %s AS group_key, converted_numerator, converted_denominator FROM %q WHERE %s ORDER BY _timestamp ASC", groupField, stream, where)
		exactResult, e := s.openObserveSearch(r.Context(), exactSQL, from, to, 0, 10000, "")
		if e != nil {
			s.dependencyFailure(w, e)
			return
		}
		exactComplete = !exactResult.Partial && exactResult.Total <= len(exactResult.Hits)
		totals := map[string]*big.Rat{}
		if exactComplete {
			for _, row := range exactResult.Hits {
				key := stringValue(row["group_key"])
				timestamp, ok := timeValue(row["_timestamp"])
				if key == "" || !ok || pendingSet[timestamp.UTC().Truncate(time.Hour)] {
					continue
				}
				numerator, okN := new(big.Int).SetString(stringValue(row["converted_numerator"]), 10)
				denominator, okD := new(big.Int).SetString(stringValue(row["converted_denominator"]), 10)
				if !okN || !okD || denominator.Sign() <= 0 {
					exactComplete = false
					break
				}
				if totals[key] == nil {
					totals[key] = new(big.Rat)
				}
				totals[key].Add(totals[key], new(big.Rat).SetFrac(numerator, denominator))
			}
		}
		if exactComplete {
			for i := range out {
				if total := totals[out[i].Key]; total != nil {
					out[i].ExactTotal = &exactSeriesValue{Numerator: total.Num().String(), Denominator: total.Denom().String(), Decimal: decimalString(total)}
					out[i].ExactComplete = true
				}
			}
		}
	}
	missing := 0
	missingSQL := fmt.Sprintf("SELECT histogram(_timestamp, '1 hour') AS bucket, COUNT(*) AS missing_count FROM %q WHERE (%s) AND (%s IS NULL OR status IN ('missing','invalid')) GROUP BY bucket", stream, baseWhere, field)
	hourLimit := int(to.Sub(from)/time.Hour) + 2
	if hourLimit > 10000 {
		hourLimit = 10000
	}
	missingResult, e := s.openObserveSearch(r.Context(), missingSQL, from, to, 0, hourLimit, "")
	if e != nil {
		s.dependencyFailure(w, e)
		return
	}
	missingComplete := !missingResult.Partial && missingResult.Total <= len(missingResult.Hits)
	if missingComplete {
		for _, row := range missingResult.Hits {
			bucket, ok := timeValue(row["bucket"])
			if !ok {
				missingComplete = false
				break
			}
			if !pendingSet[bucket.UTC().Truncate(time.Hour)] {
				missing += int(int64Value(row["missing_count"]))
			}
		}
	}
	status := seriesComplete
	if len(pending) > 0 || queryIncomplete || !missingComplete || !exactComplete {
		status = seriesRefreshing
	}
	writeJSON(w, 200, map[string]any{"series": out, "coverage": map[string]int{"known": known, "missing": missing}, "query_complete": !queryIncomplete && missingComplete && exactComplete, "projection": map[string]any{"status": status, "pending_hours": pending}})
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func timeValue(v any) (time.Time, bool) {
	switch x := v.(type) {
	case string:
		if t, e := time.Parse(time.RFC3339, x); e == nil {
			return t, true
		}
		t, e := time.ParseInLocation("2006-01-02T15:04:05", x, time.UTC)
		return t, e == nil
	case float64:
		return time.UnixMicro(int64(x)).UTC(), true
	case json.Number:
		n, e := x.Int64()
		return time.UnixMicro(n).UTC(), e == nil
	}
	return time.Time{}, false
}

func (s *service) pendingProjectionHours(ctx context.Context, from, to time.Time) ([]time.Time, error) {
	ids, err := s.listObjects(ctx, operationPrefix)
	if err != nil {
		return nil, err
	}
	seen := map[time.Time]bool{}
	for _, id := range ids {
		o, e := s.readObject(ctx, id)
		if e != nil {
			return nil, e
		}
		var op mutationOperation
		if json.Unmarshal(o.Data, &op) != nil || op.Phase == opComplete || op.Phase == opCancelled {
			continue
		}
		if !op.AffectedHour.Before(from.Truncate(time.Hour)) && !op.AffectedHour.After(to.Truncate(time.Hour)) {
			seen[op.AffectedHour] = true
		}
	}
	out := make([]time.Time, 0, len(seen))
	for hour := range seen {
		out = append(out, hour)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, nil
}
