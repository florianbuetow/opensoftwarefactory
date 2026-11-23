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
	code, data := conditionalPut(t, srv.URL+"/api/prices/openai-gpt", price, "If-None-Match", "*")
	if code != 200 {
		t.Fatalf("price %d %s", code, data)
	}
	usage := `{"schema_version":1,"id":"attempt-1","kind":"usage","run_id":"` + runA + `","execution_id":"exec-1","occurred_at":"2026-11-01T12:00:00Z","vendor":"openai","model":"gpt-x","effort":"high","modes":{"reasoning":"on"},"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50,"status":"reported","missing":[],"raw_usage":{"prompt_tokens":1000}}`
	code, data = conditionalPut(t, srv.URL+"/api/measurements/attempt-1", usage, "If-None-Match", "*")
	if code != 200 {
		t.Fatalf("usage %d %s", code, data)
	}
	waitFixtureObject(t, f, "measurement-costs/attempt-1.json")
	f.mu.Lock()
	var cost CurrentCost
	_ = json.Unmarshal(f.objects["measurement-costs/attempt-1.json"], &cost)
	f.mu.Unlock()
	if cost.Amount != "2" || cost.ConvertedAmount != "2" || cost.Status != costAvailable {
		t.Fatalf("cost %+v", cost)
	}

	missing := strings.Replace(usage, `"id":"attempt-1"`, `"id":"attempt-2"`, 1)
	missing = strings.Replace(missing, `,"cached_input_tokens":200`, "", 1)
	code, data = conditionalPut(t, srv.URL+"/api/measurements/attempt-2", missing, "If-None-Match", "*")
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
	m := Measurement{ID: "u", Vendor: "v", Model: "m", Effort: "none", Modes: &map[string]string{}, OccurredAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), UncachedInputTokens: &one, CachedInputTokens: &zero, OutputTokens: &zero}
	cost, err := calculateCostWith(&m, "EUR", 1, []Price{{ID: "p", Revision: "r", Vendor: "v", Model: "m", Effort: "none", Modes: map[string]string{}, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Currency: "EUR", UnitTokens: 3, InputUncached: "1", InputCached: "0", Output: "0"}}, nil)
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

func TestDecimalIsExactAndBounded(t *testing.T) {
	for input, want := range map[string]string{"0": "0", "-0.0": "0", "1.50": "3/2", "007.5": "15/2", "0.125": "1/8", "12": "12"} {
		got, err := decimal(input, false)
		if err != nil || got.RatString() != want {
			t.Errorf("decimal(%q) = %v, %v; want %s", input, got, err, want)
		}
	}
	for _, input := range []string{"", "1e5", ".5", "5.", "+1", "-1", "1." + strings.Repeat("0", maxDecimalExponent+1)} {
		if got, err := decimal(input, false); err == nil {
			t.Errorf("decimal(%q) = %v; want an error", input, got)
		}
	}
	if _, err := decimal("0", true); err == nil {
		t.Error("zero accepted where a positive decimal is required")
	}
	if _, err := decimal("0."+strings.Repeat("0", maxDecimalExponent+1), false); err != nil {
		t.Errorf("zero with a long fraction rejected: %v", err)
	}
}

func TestJudgeNumberMustFitAnExactFraction(t *testing.T) {
	for number, want := range map[string]bool{
		"0.125":                  true,
		"-1.25E3":                true,
		"1e1000000":              true,
		"1e1000001":              false,
		"1e-1000001":             false,
		"1.5e1000001":            true,
		"1.5e1000002":            false,
		"0e999999999":            true,
		"1e99999999999999999999": false,
	} {
		if got := exactNumber(json.Number(number)); got != want {
			t.Errorf("exactNumber(%s) = %v, want %v", number, got, want)
		}
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
	code, _ := conditionalPut(t, srv.URL+"/api/measurements/immutable", original, "If-None-Match", "*")
	if code != 200 {
		t.Fatalf("create %d", code)
	}
	resp := get(t, srv.URL+"/api/measurements/immutable")
	etag := resp.Header.Get("ETag")
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(original, "12:00:00Z", "13:00:00Z", 1)
	code, _ = conditionalPut(t, srv.URL+"/api/measurements/immutable", changed, "If-Match", etag)
	if code != 422 {
		t.Fatalf("timestamp update %d", code)
	}
}

func TestPriceOverlapAndMeasurementCreateRace(t *testing.T) {
	_, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)
	base := `{"id":"p1","vendor":"v","model":"m","effort":"not_applicable","modes":{},"from":"2026-01-01T00:00:00Z","to":"2027-01-01T00:00:00Z","currency":"EUR","unit_tokens":1000,"input_uncached":"0","input_cached":"0","output":"0"}`
	if code, body := conditionalPut(t, srv.URL+"/api/prices/p1", base, "If-None-Match", "*"); code != 200 {
		t.Fatalf("first %d %s", code, body)
	}
	overlap := strings.Replace(strings.Replace(base, `"id":"p1"`, `"id":"p2"`, 1), `"from":"2026-01-01T00:00:00Z"`, `"from":"2026-06-01T00:00:00Z"`, 1)
	if code, _ := conditionalPut(t, srv.URL+"/api/prices/p2", overlap, "If-None-Match", "*"); code != 422 {
		t.Fatalf("overlap = %d", code)
	}
	usage := `{"schema_version":1,"id":"race","kind":"usage","run_id":"` + runA + `","execution_id":"e","occurred_at":"2026-11-01T12:00:00Z","vendor":"v","model":"m","effort":"not_applicable","modes":{},"input_tokens":0,"cached_input_tokens":0,"uncached_input_tokens":0,"output_tokens":0,"status":"reported","missing":[],"raw_usage":{}}`
	start := make(chan struct{})
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			c, _ := conditionalPut(t, srv.URL+"/api/measurements/race", usage, "If-None-Match", "*")
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

// A judge measurement stored before schema_version moved to the end of the
// record has its keys in another order. A repeated extraction must still
// recognise it as the same content.
func TestExtractionRepeatAcceptsRecordStoredInOldKeyOrder(t *testing.T) {
	f, blob, orch := newFixture(t)
	f.mu.Lock()
	f.objects["runs/producer/result.json"] = []byte(`{"scores":{"quality":0.125},"finding":"clear"}`)
	f.objects["runs/producer/run.json"] = []byte(`{"execution_id":"producer-execution","input_artifact":"runs/producer/input"}`)
	f.objects["runs/producer/input"] = []byte(`{"event":{"run_id":"target","execution_id":"exec-target"}}`)
	f.objects["workflow-executions/producer-execution.json"] = []byte(`{"schema_version":1,"execution_id":"producer-execution","run_id":"producer","workflow":"judge","workflow_version":1,"artifact_prefix":"runs/producer","started_at":"2026-11-01T12:00:00Z","status":"completed"}`)
	f.objects["workflow-executions/exec-target.json"] = []byte(`{"schema_version":1,"execution_id":"exec-target","run_id":"target","workflow":"target","workflow_version":1,"artifact_prefix":"runs/target","started_at":"2026-11-01T11:00:00Z","status":"completed"}`)
	f.mu.Unlock()
	srv := testEvaluation(t, blob.URL, orch.URL)
	body := `{"producer_run_id":"producer","target_run_id":"target","target_execution_id":"exec-target","artifact_id":"runs/producer/result.json","agent_ref":"judges/quality@v1","mapping_version":"v1","mappings":[{"path":"/scores/quality","name":"quality","type":"number"},{"path":"/finding","name":"finding","type":"string"}]}`
	code, data := request(t, http.MethodPost, srv.URL+"/api/extractions", body)
	if code != 201 {
		t.Fatalf("extract %d %s", code, data)
	}
	const member = `"schema_version":1`
	rewritten := 0
	f.mu.Lock()
	for id, record := range f.objects {
		if !strings.HasPrefix(id, judgePrefix+"/") {
			continue
		}
		rest := strings.Replace(string(record), ","+member, "", 1)
		if rest == string(record) {
			f.mu.Unlock()
			t.Fatalf("stored record does not carry schema_version after its first key: %s", record)
		}
		f.objects[id] = []byte("{" + member + "," + rest[1:])
		rewritten++
	}
	f.mu.Unlock()
	if rewritten != 2 {
		t.Fatalf("rewrote %d stored judge measurements, want 2", rewritten)
	}
	code, data = request(t, http.MethodPost, srv.URL+"/api/extractions", body)
	if code != 200 {
		t.Fatalf("repeat over the old key order %d %s", code, data)
	}
	var repeat struct {
		Created int `json:"created"`
	}
	if err := json.Unmarshal(data, &repeat); err != nil {
		t.Fatal(err)
	}
	if repeat.Created != 0 {
		t.Fatalf("repeat over the old key order created %d", repeat.Created)
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

// conditionalPut sends a PUT with one conditional header and returns the
// status and the JSON body of the answer.
func conditionalPut(t *testing.T, url, body, header, value string) (status int, payload []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp.Body)
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
