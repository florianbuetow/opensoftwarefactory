package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestPricingIsExactAndMissingIsNotZero(t *testing.T) {
	if got := decimalString(new(big.Rat).SetInt64(10)); got != "10" {
		t.Fatalf("integer decimal = %q", got)
	}
	f, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)
	price := `{"id":"openai-gpt","vendor":"openai","model":"gpt-x","effort":"high","modes":{"reasoning":"on"},"from":"2026-01-01T00:00:00Z","to":null,"currency":"USD","unit_tokens":1000,"input_uncached":"2","input_cached":"1","output":"4"}`
	code, data := conditionalRequest(t, http.MethodPut, srv.URL+"/api/prices/openai-gpt", price, "If-None-Match", "*")
	if code != 200 {
		t.Fatalf("price %d %s", code, data)
	}
	usage := `{"schema_version":1,"id":"attempt-1","kind":"usage","run_id":"` + runA + `","execution_id":"exec-1","occurred_at":"2026-11-01T12:00:00Z","vendor":"openai","model":"gpt-x","effort":"high","modes":{"reasoning":"on"},"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50,"status":"reported","missing":[],"raw_usage":{"prompt_tokens":1000}}`
	code, data = conditionalRequest(t, http.MethodPut, srv.URL+"/api/measurements/attempt-1", usage, "If-None-Match", "*")
	if code != 200 {
		t.Fatalf("usage %d %s", code, data)
	}
	waitFixtureObject(t, f, "measurement-costs/attempt-1.json")
	f.mu.Lock()
	var cost CurrentCost
	_ = json.Unmarshal(f.objects["measurement-costs/attempt-1.json"], &cost)
	f.mu.Unlock()
	if cost.Amount != "2" || cost.ConvertedAmount != "2" || cost.Status != "available" {
		t.Fatalf("cost %+v", cost)
	}

	missing := strings.Replace(usage, `"id":"attempt-1"`, `"id":"attempt-2"`, 1)
	missing = strings.Replace(missing, `,"cached_input_tokens":200`, "", 1)
	code, data = conditionalRequest(t, http.MethodPut, srv.URL+"/api/measurements/attempt-2", missing, "If-None-Match", "*")
	if code != 200 {
		t.Fatalf("missing usage %d %s", code, data)
	}
	cost = CurrentCost{}
	waitFixtureObject(t, f, "measurement-costs/attempt-2.json")
	f.mu.Lock()
	_ = json.Unmarshal(f.objects["measurement-costs/attempt-2.json"], &cost)
	f.mu.Unlock()
	if cost.Status != "unavailable" || len(cost.Missing) == 0 || cost.Amount != "" {
		t.Fatalf("missing became zero: %+v", cost)
	}
}

func TestRepeatingCostRetainsExactRationalComponents(t *testing.T) {
	one, zero := int64(1), int64(0)
	m := Measurement{ID: "u", Vendor: "v", Model: "m", Effort: "none", Modes: modesPtr(), OccurredAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), UncachedInputTokens: &one, CachedInputTokens: &zero, OutputTokens: &zero}
	cost, err := calculateCostWith(m, "EUR", 1, []Price{{ID: "p", Revision: "r", Vendor: "v", Model: "m", Effort: "none", Modes: map[string]string{}, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Currency: "EUR", UnitTokens: 3, InputUncached: "1", InputCached: "0", Output: "0"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cost.Amount != "0.333333333333333333" || cost.AmountNumerator != "1" || cost.AmountDenominator != "3" || cost.ConvertedNumerator != "1" || cost.ConvertedDenominator != "3" {
		t.Fatalf("cost %+v", cost)
	}
}

func TestPricingRejectsFractionSyntax(t *testing.T) {
	if _, err := decimal("1/3", false); err == nil {
		t.Fatal("fraction accepted as decimal string")
	}
	price := Price{ID: "fraction", Vendor: "v", Model: "m", Effort: "none", Modes: map[string]string{}, From: time.Now().UTC(), Currency: "EUR", UnitTokens: 1, InputUncached: "1/3", InputCached: "0", Output: "0"}
	if err := validatePrice(&price, price.ID); err == nil {
		t.Fatal("fraction price accepted")
	}
}

func TestUsageAcceptsExplicitlyMissingRawAndAttribution(t *testing.T) {
	m := Measurement{SchemaVersion: 1, ID: "direct", Kind: "usage", OccurredAt: time.Now().UTC(), Status: "missing", Missing: []string{"run_id/direct request unavailable", "execution_id/direct request unavailable", "vendor/provider unavailable", "model/provider unavailable", "effort/not supplied", "modes/not supplied", "raw_usage/provider omitted usage"}}
	if err := validateMeasurement(&m, m.ID); err != nil {
		t.Fatalf("explicit missing usage rejected: %v", err)
	}
}

func TestMeasurementOccurredAtIsImmutable(t *testing.T) {
	_, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)
	original := `{"schema_version":1,"id":"immutable","kind":"execution","run_id":"run","execution_id":"exec","occurred_at":"2026-11-01T12:00:00Z","status":"missing","missing":["duration_ms/unavailable"]}`
	code, _ := conditionalRequest(t, http.MethodPut, srv.URL+"/api/measurements/immutable", original, "If-None-Match", "*")
	if code != 200 {
		t.Fatalf("create %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/measurements/immutable", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	etag := resp.Header.Get("ETag")
	_ = resp.Body.Close()
	changed := strings.Replace(original, "12:00:00Z", "13:00:00Z", 1)
	code, _ = conditionalRequest(t, http.MethodPut, srv.URL+"/api/measurements/immutable", changed, "If-Match", etag)
	if code != 422 {
		t.Fatalf("timestamp update %d", code)
	}
}

func TestPriceOverlapAndMeasurementCreateRace(t *testing.T) {
	_, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)
	base := `{"id":"p1","vendor":"v","model":"m","effort":"not_applicable","modes":{},"from":"2026-01-01T00:00:00Z","to":"2027-01-01T00:00:00Z","currency":"EUR","unit_tokens":1000,"input_uncached":"0","input_cached":"0","output":"0"}`
	if code, body := conditionalRequest(t, "PUT", srv.URL+"/api/prices/p1", base, "If-None-Match", "*"); code != 200 {
		t.Fatalf("first %d %s", code, body)
	}
	overlap := strings.Replace(strings.Replace(base, `"id":"p1"`, `"id":"p2"`, 1), `"from":"2026-01-01T00:00:00Z"`, `"from":"2026-06-01T00:00:00Z"`, 1)
	if code, _ := conditionalRequest(t, "PUT", srv.URL+"/api/prices/p2", overlap, "If-None-Match", "*"); code != 422 {
		t.Fatalf("overlap = %d", code)
	}
	usage := `{"schema_version":1,"id":"race","kind":"usage","run_id":"` + runA + `","execution_id":"e","occurred_at":"2026-11-01T12:00:00Z","vendor":"v","model":"m","effort":"not_applicable","modes":{},"input_tokens":0,"cached_input_tokens":0,"uncached_input_tokens":0,"output_tokens":0,"status":"reported","missing":[],"raw_usage":{}}`
	start := make(chan struct{})
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			c, _ := conditionalRequest(t, "PUT", srv.URL+"/api/measurements/race", usage, "If-None-Match", "*")
			codes <- c
		}()
	}
	close(start)
	got := []int{<-codes, <-codes}
	sort.Ints(got)
	if got[0] != 200 || got[1] != 412 {
		t.Fatalf("race statuses %v", got)
	}
}

func TestExtractionUsesJSONPointersAndIsIdempotent(t *testing.T) {
	f, blob, orch := newFixture(t)
	f.mu.Lock()
	f.objects["runs/producer/result.json"] = []byte(`{"scores":{"quality":0.125},"finding":"clear"}`)
	f.objects["runs/producer/run.json"] = []byte(`{"execution_id":"producer-execution","input_artifact":"runs/producer/input"}`)
	f.objects["runs/producer/input"] = []byte(`{"event":{"run_id":"target","execution_id":"exec-target"}}`)
	f.objects["runs/target/run.json"] = []byte(`{"execution_id":"exec-target"}`)
	f.objects["workflow-executions/producer-execution.json"] = []byte(`{"schema_version":1,"execution_id":"producer-execution","run_id":"producer","workflow":"judge","workflow_version":1,"artifact_prefix":"runs/producer","started_at":"2026-11-01T12:00:00Z","status":"completed"}`)
	f.objects["workflow-executions/exec-target.json"] = []byte(`{"schema_version":1,"execution_id":"exec-target","run_id":"target","workflow":"target","workflow_version":1,"artifact_prefix":"runs/target","started_at":"2026-11-01T11:00:00Z","status":"completed"}`)
	f.mu.Unlock()
	srv := testEvaluation(t, blob.URL, orch.URL)
	body := `{"producer_run_id":"producer","target_run_id":"target","target_execution_id":"exec-target","artifact_id":"runs/producer/result.json","agent_ref":"judges/quality@v1","mapping_version":"v1","mappings":[{"path":"/scores/quality","name":"quality","type":"number"},{"path":"/finding","name":"finding","type":"string"},{"path":"/absent","name":"missing","type":"number"}]}`
	code, data := request(t, "POST", srv.URL+"/api/extractions", body)
	if code != 201 {
		t.Fatalf("extract %d %s", code, data)
	}
	var first struct {
		Measurements []Measurement `json:"measurements"`
		Created      int           `json:"created"`
	}
	_ = json.Unmarshal(data, &first)
	if first.Created != 3 || first.Measurements[0].NumberValue.String() != "0.125" || first.Measurements[1].TextValue == nil || first.Measurements[2].Status != "missing" {
		t.Fatalf("extraction %+v", first)
	}
	code, data = request(t, "POST", srv.URL+"/api/extractions", body)
	if code != 200 {
		t.Fatalf("repeat %d %s", code, data)
	}
	var repeat struct {
		Created int `json:"created"`
	}
	_ = json.Unmarshal(data, &repeat)
	if repeat.Created != 0 {
		t.Fatalf("repeat created %d", repeat.Created)
	}
	f.mu.Lock()
	f.nextRevision++
	f.revisions["runs/producer/result.json"] = fmt.Sprintf("r%d", f.nextRevision)
	f.mu.Unlock()
	code, _ = request(t, "POST", srv.URL+"/api/extractions", body)
	if code != 412 {
		t.Fatalf("overwritten artifact repeat = %d", code)
	}
}

func TestExtractionLedgerDependencyFailureIs503(t *testing.T) {
	f, blob, orch := newFixture(t)
	f.mu.Lock()
	f.failExecutionList = true
	f.mu.Unlock()
	srv := testEvaluation(t, blob.URL, orch.URL)
	body := `{"producer_run_id":"producer","target_run_id":"target","target_execution_id":"exec","artifact_id":"runs/producer/result.json","agent_ref":"judge@v1","mapping_version":"v1","mappings":[{"path":"/score","name":"score","type":"number"}]}`
	code, _ := request(t, http.MethodPost, srv.URL+"/api/extractions", body)
	if code != 503 {
		t.Fatalf("dependency failure = %d", code)
	}
}

func conditionalRequest(t *testing.T, method, url, body, header, value string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	return resp.StatusCode, raw
}

var _ = fmt.Sprintf

func waitFixtureObject(t *testing.T, f *fixture, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		_, ok := f.objects[id]
		f.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture object %s was not written", id)
}
func modesPtr(values ...map[string]string) *map[string]string {
	m := map[string]string{}
	if len(values) > 0 {
		m = values[0]
	}
	return &m
}
