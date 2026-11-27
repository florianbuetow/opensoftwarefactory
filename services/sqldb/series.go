package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type seriesPoint struct {
	Time        time.Time   `json:"time"`
	Value       json.Number `json:"value"`
	Status      string      `json:"status"`
	Approximate bool        `json:"approximate"`
}

type exactSeriesValue struct {
	Numerator   string `json:"numerator"`
	Denominator string `json:"denominator"`
	Decimal     string `json:"decimal"`
}

type seriesLine struct {
	ExactTotal    *exactSeriesValue `json:"exact_total,omitempty"`
	Key           string            `json:"key"`
	Points        []seriesPoint     `json:"points"`
	ExactComplete bool              `json:"exact_complete,omitempty"`
}

func (s *service) getSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metric := strings.TrimSpace(q.Get("metric"))
	groupBy := strings.TrimSpace(q.Get("group_by"))
	outcome := strings.TrimSpace(q.Get("outcome"))
	currency := strings.TrimSpace(q.Get("currency"))
	if metric == "" || groupBy == "" {
		s.fail(w, http.StatusBadRequest, errors.New("metric and group_by are required"))
		return
	}
	from, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(q.Get("from")))
	if err != nil {
		s.fail(w, http.StatusBadRequest, errors.New("from must be RFC 3339"))
		return
	}
	to, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(q.Get("to")))
	if err != nil || !from.Before(to) {
		s.fail(w, http.StatusBadRequest, errors.New("to must be RFC 3339 after from"))
		return
	}
	if outcome != "" && outcome != "completed" && outcome != "failed" {
		s.fail(w, http.StatusBadRequest, errors.New("outcome must be completed or failed"))
		return
	}
	if metric == "cost" {
		if currency == "" {
			currency = "EUR"
		}
		s.getCostSeries(w, r, groupBy, from.UTC(), to.UTC(), currency, outcome)
		return
	}
	s.getNumericSeries(w, r, metric, groupBy, from.UTC(), to.UTC(), outcome)
}

func validateGroupBy(kind, groupBy string) error {
	switch groupBy {
	case "model":
		if kind != "usage" {
			return errors.New("model grouping is only supported for usage metrics")
		}
		return nil
	case "workflow", "agent", "run":
		return nil
	default:
		return fmt.Errorf("unsupported group_by %q", groupBy)
	}
}

func pickGroup(kind, groupBy, model, workflow, agent, traceID, targetTraceID string) string {
	switch groupBy {
	case "model":
		return model
	case "workflow":
		return workflow
	case "agent":
		return agent
	case "run":
		if kind == "judge" && targetTraceID != "" {
			return targetTraceID
		}
		return traceID
	default:
		return ""
	}
}

func (s *service) loadConversionRates(r *http.Request, targetCurrency string) (map[string]*big.Rat, error) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT from_currency, rate FROM conversions WHERE to_currency = ?`, targetCurrency)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	rates := map[string]*big.Rat{}
	for rows.Next() {
		var from, rateStr string
		if err := rows.Scan(&from, &rateStr); err != nil {
			return nil, err
		}
		if rat, ok := parseDecimal(rateStr); ok && rat.Sign() > 0 {
			rates[from] = rat
		}
	}
	return rates, rows.Err()
}

func (s *service) getCostSeries(w http.ResponseWriter, r *http.Request, groupBy string, from, to time.Time, currency, outcome string) {
	if err := validateGroupBy("usage", groupBy); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	rates, err := s.loadConversionRates(r, currency)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	const query = `
		SELECT m.occurred_at, m.model, m.workflow, m.agent, m.trace_id,
		       COALESCE(c.amount_numerator, ''), COALESCE(c.amount_denominator, ''), COALESCE(c.currency, ''), COALESCE(c.status, '')
		FROM measurements m
		LEFT JOIN costs c ON c.usage_id = m.id
		LEFT JOIN runs r ON r.trace_id = m.trace_id
		WHERE m.kind = 'usage'
		  AND m.occurred_at >= ? AND m.occurred_at < ?
		  AND (
		    ? = ''
		    OR (? = 'completed' AND LOWER(COALESCE(r.status, '')) = 'completed')
		    OR (? = 'failed' AND LOWER(COALESCE(r.status, '')) IN ('failed', 'cancelled', 'terminated', 'timed_out', 'timedout'))
		  )
		ORDER BY m.occurred_at ASC`
	rows, err := s.db.QueryContext(r.Context(), query, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), outcome, outcome, outcome)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	defer func() { _ = rows.Close() }()

	type bucketKey struct {
		Hour  time.Time
		Group string
	}
	buckets := map[bucketKey]*big.Rat{}
	totals := map[string]*big.Rat{}
	known, missing := 0, 0
	for rows.Next() {
		var occurred, model, workflow, agent, traceID, numStr, denStr, srcCurrency, costStatus string
		if err := rows.Scan(&occurred, &model, &workflow, &agent, &traceID, &numStr, &denStr, &srcCurrency, &costStatus); err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		group := pickGroup("usage", groupBy, model, workflow, agent, traceID, "")
		ts, err := time.Parse(time.RFC3339Nano, occurred)
		if err != nil || group == "" || costStatus != "available" {
			missing++
			continue
		}
		num, okN := new(big.Int).SetString(numStr, 10)
		den, okD := new(big.Int).SetString(denStr, 10)
		if !okN || !okD || den.Sign() <= 0 {
			missing++
			continue
		}
		amount := new(big.Rat).SetFrac(num, den)
		if srcCurrency != currency {
			rate := rates[srcCurrency]
			if rate == nil {
				missing++
				continue
			}
			amount.Mul(amount, rate)
		}
		known++
		hour := ts.UTC().Truncate(time.Hour)
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
	if err := rows.Err(); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	byGroup := map[string][]seriesPoint{}
	for key, sum := range buckets {
		byGroup[key.Group] = append(byGroup[key.Group], seriesPoint{
			Time: key.Hour, Value: json.Number(decimalString(sum)), Status: "known", Approximate: true,
		})
	}
	lines := make([]seriesLine, 0, len(byGroup))
	for group, points := range byGroup {
		sort.Slice(points, func(i, j int) bool { return points[i].Time.Before(points[j].Time) })
		total := totals[group]
		lines = append(lines, seriesLine{
			Key: group, Points: points,
			ExactTotal:    &exactSeriesValue{Numerator: total.Num().String(), Denominator: total.Denom().String(), Decimal: decimalString(total)},
			ExactComplete: true,
		})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].Key < lines[j].Key })
	writeJSON(w, http.StatusOK, map[string]any{
		"series":         lines,
		"coverage":       map[string]int{"known": known, "missing": missing},
		"query_complete": true,
		"projection":     map[string]any{"status": "complete", "pending_hours": []string{}},
	})
}

func (s *service) getNumericSeries(w http.ResponseWriter, r *http.Request, metric, groupBy string, from, to time.Time, outcome string) {
	var kind, judgeMetric string
	isAverage := false
	switch metric {
	case "duration_ms":
		kind = "execution"
	case "input_tokens", "output_tokens", "cached_input_tokens", "uncached_input_tokens", "reasoning_tokens":
		kind = "usage"
	default:
		kind = "judge"
		judgeMetric = metric
		isAverage = true
	}
	if err := validateGroupBy(kind, groupBy); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	const query = `
		SELECT m.occurred_at, m.model, m.workflow, m.agent, m.trace_id, m.target_trace_id,
		       m.duration_ms, m.input_tokens, m.output_tokens, m.cached_input_tokens, m.uncached_input_tokens, m.reasoning_tokens, m.number_value
		FROM measurements m
		LEFT JOIN runs r ON r.trace_id = COALESCE(NULLIF(m.target_trace_id, ''), m.trace_id)
		WHERE m.kind = ?
		  AND m.occurred_at >= ? AND m.occurred_at < ?
		  AND (? = '' OR m.metric_name = ?)
		  AND (
		    ? = ''
		    OR (? = 'completed' AND LOWER(COALESCE(r.status, '')) = 'completed')
		    OR (? = 'failed' AND LOWER(COALESCE(r.status, '')) IN ('failed', 'cancelled', 'terminated', 'timed_out', 'timedout'))
		  )
		ORDER BY m.occurred_at ASC`
	rows, err := s.db.QueryContext(r.Context(), query, kind, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), judgeMetric, judgeMetric, outcome, outcome, outcome)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	defer func() { _ = rows.Close() }()

	type bucketKey struct {
		Hour  time.Time
		Group string
	}
	type acc struct {
		sum   float64
		count int
	}
	buckets := map[bucketKey]*acc{}
	known, missing := 0, 0
	for rows.Next() {
		var occurred, model, workflow, agent, traceID, targetTraceID string
		var durationMS, inputTok, outputTok, cachedTok, uncachedTok, reasoningTok, numVal sql.NullFloat64
		if err := rows.Scan(&occurred, &model, &workflow, &agent, &traceID, &targetTraceID,
			&durationMS, &inputTok, &outputTok, &cachedTok, &uncachedTok, &reasoningTok, &numVal); err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		var val sql.NullFloat64
		switch metric {
		case "duration_ms":
			val = durationMS
		case "input_tokens":
			val = inputTok
		case "output_tokens":
			val = outputTok
		case "cached_input_tokens":
			val = cachedTok
		case "uncached_input_tokens":
			val = uncachedTok
		case "reasoning_tokens":
			val = reasoningTok
		default:
			val = numVal
		}
		ts, err := time.Parse(time.RFC3339Nano, occurred)
		if err != nil || !val.Valid {
			missing++
			continue
		}
		known++
		group := pickGroup(kind, groupBy, model, workflow, agent, traceID, targetTraceID)
		key := bucketKey{Hour: ts.UTC().Truncate(time.Hour), Group: group}
		a := buckets[key]
		if a == nil {
			a = &acc{}
			buckets[key] = a
		}
		a.sum += val.Float64
		a.count++
	}
	if err := rows.Err(); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	byGroup := map[string][]seriesPoint{}
	for key, a := range buckets {
		v := a.sum
		if isAverage && a.count > 0 {
			v = a.sum / float64(a.count)
		}
		byGroup[key.Group] = append(byGroup[key.Group], seriesPoint{
			Time: key.Hour, Value: json.Number(strconv.FormatFloat(v, 'f', -1, 64)), Status: "known",
		})
	}
	lines := make([]seriesLine, 0, len(byGroup))
	for group, points := range byGroup {
		sort.Slice(points, func(i, j int) bool { return points[i].Time.Before(points[j].Time) })
		lines = append(lines, seriesLine{Key: group, Points: points})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].Key < lines[j].Key })
	writeJSON(w, http.StatusOK, map[string]any{
		"series":         lines,
		"coverage":       map[string]int{"known": known, "missing": missing},
		"query_complete": true,
		"projection":     map[string]any{"status": "complete", "pending_hours": []string{}},
	})
}

// parseDecimal parses a bounded plain decimal string (no exponents) into an exact big.Rat.
func parseDecimal(raw string) (*big.Rat, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 64 {
		return nil, false
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if hasDot && strings.Contains(fracPart, ".") {
		return nil, false
	}
	digits := intPart + fracPart
	if digits == "" {
		return nil, false
	}
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return nil, false
		}
	}
	num, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, false
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(fracPart))), nil)
	return new(big.Rat).SetFrac(num, den), true
}

func decimalString(r *big.Rat) string {
	s := r.FloatString(12)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}
