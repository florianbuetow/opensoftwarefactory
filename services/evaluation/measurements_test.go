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

func TestListGetAndValidateMeasurements(t *testing.T) {
	f, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)

	// validRecordID edge cases.
	if validRecordID("") || validRecordID(strings.Repeat("a", 201)) || validRecordID(".leading") || validRecordID("bad/id") {
		t.Fatal("invalid record ID accepted")
	}

	// validateMeasurement edge cases.
	neg := int64(-1)
	ten, five, four := int64(10), int64(5), int64(4)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	invalidMeasurements := []Measurement{
		{SchemaVersion: 2, ID: "m1", Kind: kindExecution, RunID: "r", ExecutionID: "e", OccurredAt: now, Status: statusDerived, Missing: []string{}},
		{SchemaVersion: 1, ID: "m1", Kind: "other", RunID: "r", ExecutionID: "e", OccurredAt: now, Status: statusDerived, Missing: []string{}},
		{SchemaVersion: 1, ID: "m1", Kind: kindExecution, RunID: "r", ExecutionID: "e", Status: statusDerived, Missing: []string{}},
		{SchemaVersion: 1, ID: "m1", Kind: kindExecution, RunID: "r", ExecutionID: "e", OccurredAt: now, Status: statusDerived},
		{SchemaVersion: 1, ID: "m1", Kind: kindUsage, OccurredAt: now, Status: statusMissing, Missing: []string{}},
		{SchemaVersion: 1, ID: "m1", Kind: kindExecution, OccurredAt: now, Status: statusMissing, Missing: []string{"run_id/x", "execution_id/x"}},
		{SchemaVersion: 1, ID: "m1", Kind: kindExecution, RunID: "r", ExecutionID: "e", OccurredAt: now, Status: "bogus", Missing: []string{}},
		{SchemaVersion: 1, ID: "m1", Kind: kindExecution, RunID: "r", ExecutionID: "e", OccurredAt: now, DurationMS: &neg, Status: statusDerived, Missing: []string{}},
		{SchemaVersion: 1, ID: "m1", Kind: kindUsage, RunID: "r", ExecutionID: "e", OccurredAt: now, Vendor: "v", Model: "m", Effort: "e", Modes: &map[string]string{}, InputTokens: &five, CachedInputTokens: &ten, Status: statusReported, Missing: []string{}, RawUsage: json.RawMessage(`{}`)},
		{SchemaVersion: 1, ID: "m1", Kind: kindUsage, RunID: "r", ExecutionID: "e", OccurredAt: now, Vendor: "v", Model: "m", Effort: "e", Modes: &map[string]string{}, InputTokens: &ten, CachedInputTokens: &five, UncachedInputTokens: &four, Status: statusReported, Missing: []string{}, RawUsage: json.RawMessage(`{}`)},
		{SchemaVersion: 1, ID: "m1", Kind: kindUsage, RunID: "r", ExecutionID: "e", OccurredAt: now, Status: statusReported, Missing: []string{}},
		{SchemaVersion: 1, ID: "m1", Kind: kindJudge, RunID: "r", ExecutionID: "e", OccurredAt: now, Status: statusDerived, Missing: []string{}},
	}
	for i := range invalidMeasurements {
		if err := validateMeasurement(&invalidMeasurements[i], "m1"); err == nil {
			t.Fatalf("invalid measurement %d accepted", i)
		}
	}

	// putMeasurement error paths and requestCondition edge cases.
	if code, _ := conditionalPut(t, srv.URL+"/api/measurements/m1", `{bad`, "If-None-Match", "*"); code != http.StatusBadRequest {
		t.Fatalf("bad JSON put = %d", code)
	}
	if code, _ := conditionalPut(t, srv.URL+"/api/measurements/m1", `{"schema_version":1,"id":"wrong"}`, "If-None-Match", "*"); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid measurement put = %d", code)
	}
	validExec := `{"schema_version":1,"id":"exec-1","kind":"execution","run_id":"` + runA + `","execution_id":"e1","occurred_at":"2026-11-01T12:00:00Z","status":"derived","missing":[]}`
	if code, _ := request(t, http.MethodPut, srv.URL+"/api/measurements/exec-1", validExec); code != http.StatusBadRequest {
		t.Fatalf("missing condition = %d", code)
	}
	if code, _ := conditionalPut(t, srv.URL+"/api/measurements/exec-1", validExec, "If-Match", "unquoted"); code != http.StatusBadRequest {
		t.Fatalf("bad If-Match = %d", code)
	}
	if code, _ := conditionalPut(t, srv.URL+"/api/measurements/exec-1", validExec, "If-Match", `"missing-rev"`); code != http.StatusPreconditionFailed {
		t.Fatalf("If-Match missing = %d", code)
	}
	// Combined headers error in requestCondition.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/api/measurements/exec-1", strings.NewReader(validExec))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-Match", `"r1"`)
	req.Header.Set("If-None-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("combined headers = %d", resp.StatusCode)
	}

	// Create measurements and test getMeasurement + listMeasurements.
	if code, body := conditionalPut(t, srv.URL+"/api/measurements/exec-1", validExec, "If-None-Match", "*"); code != http.StatusOK {
		t.Fatalf("create exec-1 = %d %s", code, body)
	}
	exec2 := `{"schema_version":1,"id":"exec-2","kind":"execution","run_id":"` + runB + `","execution_id":"e2","occurred_at":"2026-11-01T13:00:00Z","status":"derived","missing":[]}`
	if code, body := conditionalPut(t, srv.URL+"/api/measurements/exec-2", exec2, "If-None-Match", "*"); code != http.StatusOK {
		t.Fatalf("create exec-2 = %d %s", code, body)
	}
	exec3 := `{"schema_version":1,"id":"exec-3","kind":"execution","run_id":"` + runB + `","execution_id":"e3","occurred_at":"2026-11-01T13:00:00Z","status":"derived","missing":[]}`
	if code, body := conditionalPut(t, srv.URL+"/api/measurements/exec-3", exec3, "If-None-Match", "*"); code != http.StatusOK {
		t.Fatalf("create exec-3 = %d %s", code, body)
	}

	if code, _ := request(t, http.MethodGet, srv.URL+"/api/measurements/nonexistent", ""); code != http.StatusNotFound {
		t.Fatalf("missing measurement = %d", code)
	}
	f.mu.Lock()
	f.objects[measurementObject(kindExecution, "corrupt")] = []byte(`{bad`)
	f.mu.Unlock()
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/measurements/corrupt", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt measurement = %d", code)
	}

	// listMeasurements validation errors.
	for _, query := range []string{
		"kind=invalid",
		"from=not-a-time",
		"to=not-a-time",
		"limit=0",
		"cursor=!!!",
	} {
		if code, _ := request(t, http.MethodGet, srv.URL+"/api/measurements?"+query, ""); code != http.StatusBadRequest {
			t.Fatalf("listMeasurements(%s) = %d, want 400", query, code)
		}
	}
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/measurements?from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z", ""); code != http.StatusUnprocessableEntity {
		t.Fatalf("from>=to = %d", code)
	}

	// listMeasurements filtering and cursor pagination.
	code, data := request(t, http.MethodGet, srv.URL+"/api/measurements?kind=execution&run_id="+runB+"&from=2026-11-01T12:30:00Z&to=2026-11-01T14:00:00Z&limit=1", "")
	if code != http.StatusOK {
		t.Fatalf("list page 1 = %d %s", code, data)
	}
	var page struct {
		NextCursor   string        `json:"next_cursor"`
		Measurements []Measurement `json:"measurements"`
	}
	if err = json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Measurements) != 1 || page.NextCursor == "" {
		t.Fatalf("page 1 = %+v", page)
	}
	code, data = request(t, http.MethodGet, srv.URL+"/api/measurements?kind=execution&run_id="+runB+"&limit=10&cursor="+page.NextCursor, "")
	if code != http.StatusOK {
		t.Fatalf("list page 2 = %d %s", code, data)
	}
	page = struct {
		NextCursor   string        `json:"next_cursor"`
		Measurements []Measurement `json:"measurements"`
	}{}
	_ = json.Unmarshal(data, &page)
	if len(page.Measurements) != 1 || page.NextCursor != "" {
		t.Fatalf("page 2 = %+v", page)
	}

	// calculateCostWith currency conversion and missing conversion.
	one, zero := int64(1), int64(0)
	usage := Measurement{ID: "u", Vendor: "v", Model: "m", Effort: "none", Modes: &map[string]string{}, OccurredAt: now, UncachedInputTokens: &one, CachedInputTokens: &zero, OutputTokens: &zero}
	prices := []Price{{ID: "p", Revision: "r1", Vendor: "v", Model: "m", Effort: "none", Modes: map[string]string{}, From: now.Add(-time.Hour), Currency: currencyUSD, UnitTokens: 1, InputUncached: "10", InputCached: "0", Output: "0"}}
	costMissingConv, err := calculateCostWith(&usage, currencyEUR, 1, prices, nil)
	if err != nil || costMissingConv.Status != costUnavailable || !hasMissingField(costMissingConv.Missing, "conversion") {
		t.Fatalf("missing conversion cost = %+v %v", costMissingConv, err)
	}
	costConverted, err := calculateCostWith(&usage, currencyEUR, 1, prices, []Conversion{{ID: "c1", Revision: "r2", FromCurrency: currencyUSD, ToCurrency: currencyEUR, Rate: "0.9"}})
	if err != nil || costConverted.Status != costAvailable || costConverted.ConvertedAmount != "9" {
		t.Fatalf("converted cost = %+v %v", costConverted, err)
	}
}

func TestExtractionPointersAndValidationPaths(t *testing.T) {
	// parsePointer & pointerValue edge cases.
	if parts, err := parsePointer(""); err != nil || parts != nil {
		t.Fatalf("empty pointer = %v %v", parts, err)
	}
	if parts, err := parsePointer("/a~1b/c~0d"); err != nil || len(parts) != 2 || parts[0] != "a/b" || parts[1] != "c~d" {
		t.Fatalf("escaped pointer = %v %v", parts, err)
	}
	for _, bad := range []string{"no-slash", "/bad~2escape", "/trailing~"} {
		if _, err := parsePointer(bad); err == nil {
			t.Fatalf("invalid pointer %q accepted", bad)
		}
	}
	doc := map[string]any{"arr": []any{"first", "second"}, "num": 42}
	if v, ok := pointerValue(doc, "/arr/1"); !ok || v != "second" {
		t.Fatalf("array pointer = %v %v", v, ok)
	}
	for _, bad := range []string{"bad", "/arr/9", "/arr/abc", "/num/sub"} {
		if _, ok := pointerValue(doc, bad); ok {
			t.Fatalf("pointer %q unexpectedly matched", bad)
		}
	}
	if !containsExecutionIdentity([]any{map[string]any{"run_id": "r1", "execution_id": "e1"}}, "r1", "e1") {
		t.Fatal("array execution identity not found")
	}

	// extractMetrics validation and invalid artifact cases.
	f, blob, orch := newFixture(t)
	f.mu.Lock()
	f.objects["runs/producer/result.json"] = []byte(`{"num":"not-a-number","huge":1e1000001,"str":123}`)
	f.objects["runs/producer/bad.json"] = []byte(`{"a":1}{"b":2}`)
	f.objects["runs/producer/run.json"] = []byte(`{"execution_id":"prod-exec","input_artifact":"runs/producer/input"}`)
	f.objects["runs/producer/input"] = []byte(`{"items":[{"run_id":"target","execution_id":"target-exec"}]}`)
	f.objects["workflow-executions/prod-exec.json"] = []byte(`{"schema_version":1,"execution_id":"prod-exec","run_id":"producer","workflow":"judge","workflow_version":1,"artifact_prefix":"runs/producer","started_at":"2026-11-01T12:00:00Z","status":"completed"}`)
	f.objects["workflow-executions/target-exec.json"] = []byte(`{"schema_version":1,"execution_id":"target-exec","run_id":"target","workflow":"target","workflow_version":1,"artifact_prefix":"runs/target","started_at":"2026-11-01T11:00:00Z","status":"completed"}`)
	f.mu.Unlock()
	srv := testEvaluation(t, blob.URL, orch.URL)

	if code, _ := request(t, http.MethodPost, srv.URL+"/api/extractions", `{bad`); code != http.StatusBadRequest {
		t.Fatalf("bad JSON = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/extractions", `{}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty request = %d", code)
	}
	// Artifact not in producer scope.
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/extractions", `{"producer_run_id":"unknown","target_run_id":"target","target_execution_id":"target-exec","artifact_id":"runs/unknown/a","agent_ref":"a@v1","mapping_version":"v1","mappings":[{"path":"/a","name":"a","type":"number"}]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("unscoped producer = %d", code)
	}
	// Target scope mismatch.
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/extractions", `{"producer_run_id":"producer","target_run_id":"wrong","target_execution_id":"target-exec","artifact_id":"runs/producer/result.json","agent_ref":"a@v1","mapping_version":"v1","mappings":[{"path":"/a","name":"a","type":"number"}]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched target = %d", code)
	}
	// Invalid mapping type and invalid pointer.
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/extractions", `{"producer_run_id":"producer","target_run_id":"target","target_execution_id":"target-exec","artifact_id":"runs/producer/result.json","agent_ref":"a@v1","mapping_version":"v1","mappings":[{"path":"/a","name":"a","type":"bool"}]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid mapping type = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/extractions", `{"producer_run_id":"producer","target_run_id":"target","target_execution_id":"target-exec","artifact_id":"runs/producer/result.json","agent_ref":"a@v1","mapping_version":"v1","mappings":[{"path":"bad","name":"a","type":"number"}]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid pointer = %d", code)
	}
	// Missing artifact -> 404.
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/extractions", `{"producer_run_id":"producer","target_run_id":"target","target_execution_id":"target-exec","artifact_id":"runs/producer/missing.json","agent_ref":"a@v1","mapping_version":"v1","mappings":[{"path":"/a","name":"a","type":"number"}]}`); code != http.StatusNotFound {
		t.Fatalf("missing artifact = %d", code)
	}
	// Invalid value types and non-exact number produce statusInvalid measurements.
	code, data := request(t, http.MethodPost, srv.URL+"/api/extractions", `{"producer_run_id":"producer","target_run_id":"target","target_execution_id":"target-exec","artifact_id":"runs/producer/result.json","agent_ref":"a@v1","mapping_version":"v1","mappings":[{"path":"/num","name":"n1","type":"number"},{"path":"/huge","name":"n2","type":"number"},{"path":"/str","name":"s1","type":"string"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("invalid value extraction = %d %s", code, data)
	}
	var out struct {
		Measurements []Measurement `json:"measurements"`
	}
	_ = json.Unmarshal(data, &out)
	for _, m := range out.Measurements {
		if m.Status != statusInvalid {
			t.Fatalf("expected invalid status, got %+v", m)
		}
	}
	// Artifact with multiple JSON values produces statusInvalid.
	code, data = request(t, http.MethodPost, srv.URL+"/api/extractions", `{"producer_run_id":"producer","target_run_id":"target","target_execution_id":"target-exec","artifact_id":"runs/producer/bad.json","agent_ref":"a@v1","mapping_version":"v1","mappings":[{"path":"/a","name":"a","type":"number"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("multi-json extraction = %d %s", code, data)
	}
	_ = json.Unmarshal(data, &out)
	if len(out.Measurements) != 1 || out.Measurements[0].Status != statusInvalid {
		t.Fatalf("multi-json status = %+v", out.Measurements)
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
