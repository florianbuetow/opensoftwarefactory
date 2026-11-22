package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	usagePrefix      = "measurements/usage"
	executionPrefix  = "measurements/execution"
	judgePrefix      = "measurements/judge"
	pricePrefix      = "measurement-config/prices"
	conversionPrefix = "measurement-config/conversions"
	costPrefix       = "measurement-costs"
	operationPrefix  = "measurement-operations"
	jobPrefix        = "measurement-jobs"
)

type Measurement struct {
	SchemaVersion       int                `json:"schema_version"`
	ID                  string             `json:"id"`
	Kind                string             `json:"kind"`
	Revision            string             `json:"revision,omitempty"`
	RunID               string             `json:"run_id"`
	ExecutionID         string             `json:"execution_id"`
	ParentExecutionID   string             `json:"parent_execution_id,omitempty"`
	Workflow            string             `json:"workflow,omitempty"`
	WorkflowVersion     *int               `json:"workflow_version,omitempty"`
	Agent               string             `json:"agent,omitempty"`
	AgentVersion        *int               `json:"agent_version,omitempty"`
	TraceID             string             `json:"trace_id,omitempty"`
	SpanID              string             `json:"span_id,omitempty"`
	OccurredAt          time.Time          `json:"occurred_at"`
	Vendor              string             `json:"vendor,omitempty"`
	Model               string             `json:"model,omitempty"`
	RequestedModel      string             `json:"requested_model,omitempty"`
	Effort              string             `json:"effort,omitempty"`
	Modes               *map[string]string `json:"modes,omitempty"`
	InputTokens         *int64             `json:"input_tokens,omitempty"`
	OutputTokens        *int64             `json:"output_tokens,omitempty"`
	CachedInputTokens   *int64             `json:"cached_input_tokens,omitempty"`
	UncachedInputTokens *int64             `json:"uncached_input_tokens,omitempty"`
	ReasoningTokens     *int64             `json:"reasoning_tokens,omitempty"`
	DurationMS          *int64             `json:"duration_ms,omitempty"`
	TargetRunID         string             `json:"target_run_id,omitempty"`
	TargetExecutionID   string             `json:"target_execution_id,omitempty"`
	ProducerRunID       string             `json:"producer_run_id,omitempty"`
	ArtifactID          string             `json:"artifact_id,omitempty"`
	ArtifactRevision    string             `json:"artifact_revision,omitempty"`
	MappingVersion      string             `json:"mapping_version,omitempty"`
	MetricName          string             `json:"metric_name,omitempty"`
	NumberValue         json.Number        `json:"number_value,omitempty"`
	TextValue           *string            `json:"text_value,omitempty"`
	Status              string             `json:"status"`
	Missing             []string           `json:"missing"`
	RawUsage            json.RawMessage    `json:"raw_usage,omitempty"`
}

type Price struct {
	ID            string            `json:"id"`
	Revision      string            `json:"revision,omitempty"`
	Vendor        string            `json:"vendor"`
	Model         string            `json:"model"`
	Effort        string            `json:"effort"`
	Modes         map[string]string `json:"modes"`
	From          time.Time         `json:"from"`
	To            *time.Time        `json:"to"`
	Currency      string            `json:"currency"`
	UnitTokens    int64             `json:"unit_tokens"`
	InputUncached string            `json:"input_uncached"`
	InputCached   string            `json:"input_cached"`
	Output        string            `json:"output"`
}

type Conversion struct {
	ID           string `json:"id"`
	Revision     string `json:"revision,omitempty"`
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	Rate         string `json:"rate"`
}

type CurrentCost struct {
	UsageID              string    `json:"usage_id"`
	Amount               string    `json:"amount,omitempty"`
	AmountNumerator      string    `json:"amount_numerator,omitempty"`
	AmountDenominator    string    `json:"amount_denominator,omitempty"`
	Currency             string    `json:"currency,omitempty"`
	ConvertedAmount      string    `json:"converted_amount,omitempty"`
	ConvertedNumerator   string    `json:"converted_numerator,omitempty"`
	ConvertedDenominator string    `json:"converted_denominator,omitempty"`
	DisplayCurrency      string    `json:"display_currency"`
	PriceID              string    `json:"price_id,omitempty"`
	PriceRevision        string    `json:"price_revision,omitempty"`
	ConversionID         string    `json:"conversion_id,omitempty"`
	ConversionRevision   string    `json:"conversion_revision,omitempty"`
	Generation           int64     `json:"generation"`
	Status               string    `json:"status"`
	Missing              []string  `json:"missing"`
	CalculatedAt         time.Time `json:"calculated_at"`
}

type mutationOperation struct {
	SchemaVersion        int             `json:"schema_version"`
	ID                   string          `json:"id"`
	ObjectID             string          `json:"object_id"`
	MeasurementID        string          `json:"measurement_id"`
	ExpectedRevision     string          `json:"expected_revision,omitempty"`
	Replacement          json.RawMessage `json:"replacement"`
	CostReplacement      *CurrentCost    `json:"cost_replacement,omitempty"`
	CostExpectedRevision string          `json:"cost_expected_revision,omitempty"`
	AffectedHour         time.Time       `json:"affected_hour"`
	Generation           int64           `json:"generation"`
	Phase                string          `json:"phase"`
	Error                string          `json:"error,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

func (s *service) measurementRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /api/measurements/{id}", s.putMeasurement)
	mux.HandleFunc("GET /api/measurements/{id}", s.getMeasurement)
	mux.HandleFunc("GET /api/measurements", s.listMeasurements)
	mux.HandleFunc("PUT /api/prices/{id}", s.putPrice)
	mux.HandleFunc("GET /api/prices/{id}", s.getPrice)
	mux.HandleFunc("GET /api/prices", s.listPrices)
	mux.HandleFunc("PUT /api/conversions/{id}", s.putConversion)
	mux.HandleFunc("GET /api/conversions/{id}", s.getConversion)
	mux.HandleFunc("GET /api/conversions", s.listConversions)
	mux.HandleFunc("POST /api/recalculations", s.startRecalculation)
	mux.HandleFunc("GET /api/recalculations/{id}", s.getRecalculation)
	mux.HandleFunc("POST /api/recalculations/{id}/batch", s.recalculationBatch)
	mux.HandleFunc("POST /api/extractions", s.extractMetrics)
	mux.HandleFunc("GET /api/series", s.getSeries)
	mux.HandleFunc("GET /api/evidence", s.getEvidence)
	mux.HandleFunc("GET /api/evidence/artifact", s.getEvidenceArtifact)
	s.backfillRoutes(mux)
}

func measurementObject(kind, id string) string {
	return "measurements/" + kind + "/" + id + ".json"
}

func validRecordID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for index, r := range id {
		if index == 0 && !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

func validateMeasurement(m *Measurement, pathID string) error {
	if m.SchemaVersion != 1 || m.ID != pathID || !validRecordID(m.ID) {
		return errors.New("schema_version must be 1 and id must match the path")
	}
	if m.Kind != "usage" && m.Kind != "execution" && m.Kind != "judge" {
		return errors.New("kind must be usage, execution, or judge")
	}
	if m.OccurredAt.IsZero() {
		return errors.New("occurred_at is required")
	}
	if m.Missing == nil {
		return errors.New("missing must explicitly describe unavailable evidence")
	}
	if m.RunID == "" && !hasMissingField(m.Missing, "run_id") || m.ExecutionID == "" && !hasMissingField(m.Missing, "execution_id") {
		return errors.New("missing run/execution attribution requires explicit missing reasons")
	}
	if m.Kind != "usage" && (m.RunID == "" || m.ExecutionID == "") {
		return errors.New("execution and judge measurements require run_id and execution_id")
	}
	if m.Status != "reported" && m.Status != "derived" && m.Status != "missing" && m.Status != "invalid" && m.Status != "estimated" {
		return errors.New("invalid status")
	}
	for _, value := range []*int64{m.InputTokens, m.OutputTokens, m.CachedInputTokens, m.UncachedInputTokens, m.ReasoningTokens, m.DurationMS} {
		if value != nil && *value < 0 {
			return errors.New("counts and duration cannot be negative")
		}
	}
	if m.CachedInputTokens != nil && m.InputTokens != nil && *m.CachedInputTokens > *m.InputTokens {
		return errors.New("cached_input_tokens cannot exceed input_tokens")
	}
	if m.UncachedInputTokens != nil && m.CachedInputTokens != nil && m.InputTokens != nil && *m.UncachedInputTokens+*m.CachedInputTokens != *m.InputTokens {
		return errors.New("cached and uncached input must equal total input")
	}
	if m.Kind == "usage" {
		for field, value := range map[string]bool{"vendor": m.Vendor != "", "model": m.Model != "", "effort": m.Effort != "", "modes": m.Modes != nil, "raw_usage": len(m.RawUsage) > 0} {
			if !value && !hasMissingField(m.Missing, field) {
				return errors.New("unavailable usage dimensions require explicit missing reasons")
			}
		}
	}
	if m.Kind == "judge" && (m.ProducerRunID == "" || m.ArtifactID == "" || m.MappingVersion == "" || m.MetricName == "") {
		return errors.New("judge measurement identity is incomplete")
	}
	return nil
}

func hasMissingField(missing []string, field string) bool {
	for _, reason := range missing {
		prefix, _, _ := strings.Cut(reason, "/")
		if prefix == field {
			return true
		}
	}
	return false
}

func (s *service) putMeasurement(w http.ResponseWriter, r *http.Request) {
	var m Measurement
	if err := decodeStrictJSON(w, r, &m); err != nil {
		s.fail(w, 400, err)
		return
	}
	if err := validateMeasurement(&m, r.PathValue("id")); err != nil {
		s.fail(w, 422, err)
		return
	}
	s.measurementMu.Lock()
	defer s.measurementMu.Unlock()
	condition, err := requestCondition(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	if condition == "" {
		s.fail(w, 400, errors.New("measurement writes require If-None-Match:* or If-Match"))
		return
	}
	if condition != "*" {
		current, readErr := s.loadMeasurement(r.Context(), m.Kind, m.ID)
		if errors.Is(readErr, errNotFound) {
			s.fail(w, 412, errPrecondition)
			return
		}
		if readErr != nil {
			s.dependencyFailure(w, readErr)
			return
		}
		if !current.OccurredAt.Equal(m.OccurredAt) {
			s.fail(w, 422, errors.New("occurred_at is immutable for a measurement identity"))
			return
		}
	}
	m.Revision = ""
	data, _ := json.Marshal(m)
	generation, err := s.allocateGeneration(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	op := mutationOperation{SchemaVersion: 1, ID: uuid.NewString(), ObjectID: measurementObject(m.Kind, m.ID), MeasurementID: m.ID, ExpectedRevision: condition, Replacement: data, AffectedHour: m.OccurredAt.UTC().Truncate(time.Hour), Generation: generation, Phase: "intent", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	opRevision, err := s.writeObject(r.Context(), operationPrefix+"/"+op.ID+".json", op, "*")
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	revision, err := s.writeObject(r.Context(), op.ObjectID, m, condition)
	if errors.Is(err, errPrecondition) {
		op.Phase, op.UpdatedAt = "cancelled", time.Now().UTC()
		_, _ = s.writeObject(r.Context(), operationPrefix+"/"+op.ID+".json", op, opRevision)
		s.fail(w, 412, err)
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	if m.Kind == "usage" {
		op.Phase = "calculation_pending"
	} else {
		op.Phase = "projection_pending"
	}
	op.UpdatedAt = time.Now().UTC()
	_, _ = s.writeObject(r.Context(), operationPrefix+"/"+op.ID+".json", op, opRevision)
	m.Revision = revision
	w.Header().Set("ETag", quoteETag(revision))
	writeJSON(w, http.StatusOK, m)
}

func requestCondition(r *http.Request) (string, error) {
	match, none := strings.TrimSpace(r.Header.Get("If-Match")), strings.TrimSpace(r.Header.Get("If-None-Match"))
	if match != "" && none != "" {
		return "", errors.New("conditional headers cannot be combined")
	}
	if none != "" {
		if none != "*" {
			return "", errors.New("If-None-Match must be *")
		}
		return "*", nil
	}
	if match != "" {
		v := trimETag(match)
		if v == "" || strings.Contains(v, ",") || match[:1] != `"` {
			return "", errors.New("If-Match must contain one strong ETag")
		}
		return v, nil
	}
	return "", nil
}

func (s *service) getMeasurement(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, kind := range []string{"usage", "execution", "judge"} {
		object, err := s.readObject(r.Context(), measurementObject(kind, id))
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			s.dependencyFailure(w, err)
			return
		}
		var m Measurement
		if json.Unmarshal(object.Data, &m) != nil {
			s.dependencyFailure(w, errors.New("stored measurement is invalid"))
			return
		}
		m.Revision = object.Revision
		w.Header().Set("ETag", quoteETag(object.Revision))
		writeJSON(w, 200, m)
		return
	}
	s.fail(w, 404, errors.New("measurement not found"))
}

type measurementCursor struct {
	Time time.Time `json:"time"`
	ID   string    `json:"id"`
}

func (s *service) listMeasurements(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "usage" && kind != "execution" && kind != "judge" {
		s.fail(w, 400, errors.New("invalid kind"))
		return
	}
	from, err := optionalTime(r.URL.Query().Get("from"))
	if err != nil {
		s.fail(w, 400, errors.New("from must be RFC3339"))
		return
	}
	to, err := optionalTime(r.URL.Query().Get("to"))
	if err != nil {
		s.fail(w, 400, errors.New("to must be RFC3339"))
		return
	}
	if from != nil && to != nil && !from.Before(*to) {
		s.fail(w, 422, errors.New("from must precede to"))
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			s.fail(w, 400, errors.New("limit must be 1..1000"))
			return
		}
	}
	var cursor measurementCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		b, e := base64.RawURLEncoding.DecodeString(raw)
		if e != nil || json.Unmarshal(b, &cursor) != nil {
			s.fail(w, 400, errors.New("invalid cursor"))
			return
		}
	}
	kinds := []string{"usage", "execution", "judge"}
	if kind != "" {
		kinds = []string{kind}
	}
	var out []Measurement
	for _, k := range kinds {
		ids, e := s.listObjects(r.Context(), "measurements/"+k)
		if e != nil {
			s.dependencyFailure(w, e)
			return
		}
		for _, objectID := range ids {
			object, e := s.readObject(r.Context(), objectID)
			if e != nil {
				s.dependencyFailure(w, e)
				return
			}
			var m Measurement
			if json.Unmarshal(object.Data, &m) != nil {
				continue
			}
			m.Revision = object.Revision
			if run := r.URL.Query().Get("run_id"); run != "" && m.RunID != run {
				continue
			}
			if from != nil && m.OccurredAt.Before(*from) || to != nil && !m.OccurredAt.Before(*to) {
				continue
			}
			if !cursor.Time.IsZero() && (m.OccurredAt.After(cursor.Time) || m.OccurredAt.Equal(cursor.Time) && m.ID >= cursor.ID) {
				continue
			}
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OccurredAt.Equal(out[j].OccurredAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].OccurredAt.After(out[j].OccurredAt)
	})
	next := ""
	if len(out) > limit {
		last := out[limit-1]
		b, _ := json.Marshal(measurementCursor{last.OccurredAt, last.ID})
		next = base64.RawURLEncoding.EncodeToString(b)
		out = out[:limit]
	}
	writeJSON(w, 200, map[string]any{"measurements": out, "next_cursor": next})
}

func (s *service) dependencyFailure(w http.ResponseWriter, err error) {
	s.fail(w, http.StatusServiceUnavailable, err)
}

func decimal(s string, positive bool) (*big.Rat, error) {
	if !regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`).MatchString(s) {
		return nil, errors.New("invalid decimal string")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || positive && r.Sign() <= 0 || !positive && r.Sign() < 0 {
		return nil, errors.New("invalid decimal string")
	}
	return r, nil
}
func decimalString(r *big.Rat) string {
	if r.Sign() == 0 {
		return "0"
	}
	twos, fives := 0, 0
	d := new(big.Int).Set(r.Denom())
	for new(big.Int).Mod(d, big.NewInt(2)).Sign() == 0 {
		d.Quo(d, big.NewInt(2))
		twos++
	}
	for new(big.Int).Mod(d, big.NewInt(5)).Sign() == 0 {
		d.Quo(d, big.NewInt(5))
		fives++
	}
	if d.Cmp(big.NewInt(1)) != 0 {
		return r.FloatString(18)
	}
	scale := twos
	if fives > scale {
		scale = fives
	}
	value := r.FloatString(scale)
	if scale == 0 {
		return value
	}
	return strings.TrimRight(strings.TrimRight(value, "0"), ".")
}
func modesKey(m map[string]string) string { b, _ := json.Marshal(m); return string(b) }
func intervalsOverlap(aFrom time.Time, aTo *time.Time, bFrom time.Time, bTo *time.Time) bool {
	return (bTo == nil || aFrom.Before(*bTo)) && (aTo == nil || bFrom.Before(*aTo))
}

func hashID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *service) replaceCost(ctx context.Context, cost CurrentCost, parent mutationOperation) (string, error) {
	objectID := costPrefix + "/" + cost.UsageID + ".json"
	condition := "*"
	if old, err := s.readObject(ctx, objectID); err == nil {
		var current CurrentCost
		if json.Unmarshal(old.Data, &current) == nil && current.Generation > cost.Generation {
			return "", nil
		}
		condition = old.Revision
	} else if !errors.Is(err, errNotFound) {
		return "", err
	}
	return s.writeObject(ctx, objectID, cost, condition)
}

func (s *service) calculateCost(ctx context.Context, m Measurement, display string, generation int64) (CurrentCost, error) {
	prices, err := s.loadPrices(ctx)
	if err != nil {
		return CurrentCost{}, err
	}
	conversions, err := s.loadConversions(ctx)
	if err != nil {
		return CurrentCost{}, err
	}
	return calculateCostWith(m, display, generation, prices, conversions)
}
func calculateCostWith(m Measurement, display string, generation int64, prices []Price, convs []Conversion) (CurrentCost, error) {
	cost := CurrentCost{UsageID: m.ID, DisplayCurrency: display, Generation: generation, Status: "unavailable", CalculatedAt: time.Now().UTC()}
	var matches []Price
	for _, p := range prices {
		if p.Vendor == m.Vendor && p.Model == m.Model && p.Effort == m.Effort && m.Modes != nil && modesKey(p.Modes) == modesKey(*m.Modes) && !m.OccurredAt.Before(p.From) && (p.To == nil || m.OccurredAt.Before(*p.To)) {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		cost.Missing = []string{"price/no unique effective price"}
		return cost, nil
	}
	p := matches[0]
	if display == "" {
		display = p.Currency
		cost.DisplayCurrency = display
	}
	uncached := m.UncachedInputTokens
	if uncached == nil && m.InputTokens != nil && m.CachedInputTokens != nil {
		n := *m.InputTokens - *m.CachedInputTokens
		uncached = &n
	}
	if uncached == nil {
		cost.Missing = append(cost.Missing, "uncached_input_tokens/missing")
	}
	if m.CachedInputTokens == nil {
		cost.Missing = append(cost.Missing, "cached_input_tokens/missing")
	}
	if m.OutputTokens == nil {
		cost.Missing = append(cost.Missing, "output_tokens/missing")
	}
	if len(cost.Missing) > 0 {
		return cost, nil
	}
	u, _ := decimal(p.InputUncached, false)
	c, _ := decimal(p.InputCached, false)
	o, _ := decimal(p.Output, false)
	amount := new(big.Rat).Add(new(big.Rat).Mul(new(big.Rat).SetInt64(*uncached), u), new(big.Rat).Mul(new(big.Rat).SetInt64(*m.CachedInputTokens), c))
	amount.Add(amount, new(big.Rat).Mul(new(big.Rat).SetInt64(*m.OutputTokens), o))
	amount.Quo(amount, new(big.Rat).SetInt64(p.UnitTokens))
	cost.Amount, cost.AmountNumerator, cost.AmountDenominator, cost.Currency, cost.PriceID, cost.PriceRevision = decimalString(amount), amount.Num().String(), amount.Denom().String(), p.Currency, p.ID, p.Revision
	converted := new(big.Rat).Set(amount)
	if display != p.Currency {
		var found []Conversion
		for _, v := range convs {
			if v.FromCurrency == p.Currency && v.ToCurrency == display {
				found = append(found, v)
			}
		}
		if len(found) != 1 {
			cost.Missing = []string{"conversion/no unique configured rate"}
			return cost, nil
		}
		rate, _ := decimal(found[0].Rate, true)
		converted.Mul(converted, rate)
		cost.ConversionID, cost.ConversionRevision = found[0].ID, found[0].Revision
	}
	cost.ConvertedAmount = decimalString(converted)
	cost.ConvertedNumerator, cost.ConvertedDenominator = converted.Num().String(), converted.Denom().String()
	cost.Status = "available"
	return cost, nil
}
