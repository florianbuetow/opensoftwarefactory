package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// priceQuery is the query string that addresses one price row.
func priceQuery(vendor, model, effort, modes, from string) string {
	return url.Values{"vendor": {vendor}, "model": {model}, "effort": {effort}, "modes": {modes}, "from": {from}}.Encode()
}

// conversionQuery is the query string that addresses one currency conversion row.
func conversionQuery(fromCurrency, toCurrency string) string {
	return url.Values{"from_currency": {fromCurrency}, "to_currency": {toCurrency}}.Encode()
}

// objectCopy returns the stored bytes of id, or nil when it is absent.
func (f *fixture) objectCopy(id string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[id]
}

func TestLegacyPriceIsRefusedUntilMigrated(t *testing.T) {
	f, blob, orch := newFixture(t)
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	legacy := pricePrefix + "/0b6f6c1e-2d4f-4a57-9a43-5c1d2a8e7f10.json"
	f.mu.Lock()
	f.objects[legacy] = []byte(`{"id":"0b6f6c1e-2d4f-4a57-9a43-5c1d2a8e7f10","vendor":"v","model":"m","effort":"e","modes":{"tier":"batch"},"from":"2026-01-01T00:00:00Z","to":null,"currency":"EUR","unit_tokens":1,"input_uncached":"1","input_cached":"0","output":"0"}`)
	f.mu.Unlock()
	if _, err := s.loadPrices(t.Context()); err == nil {
		t.Fatal("legacy price with an id was read")
	}
	if err := s.migrateLegacyPrices(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.objectCopy(legacy) != nil {
		t.Fatal("legacy price object is still stored")
	}
	prices, err := s.loadPrices(t.Context())
	if err != nil || len(prices) != 1 || prices[0].Modes["tier"] != "batch" {
		t.Fatalf("migrated prices = %+v %v", prices, err)
	}
	if stored := f.objectCopy(prices[0].Key().objectID()); strings.Contains(string(stored), `"id"`) {
		t.Fatalf("migrated price keeps its id: %s", stored)
	}
	if err := s.migrateLegacyPrices(t.Context()); err != nil {
		t.Fatalf("second migration = %v", err)
	}
}

func TestLegacyConversionIsRefusedUntilMigrated(t *testing.T) {
	f, blob, orch := newFixture(t)
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	legacy := conversionPrefix + "/usd-eur.json"
	f.mu.Lock()
	f.objects[legacy] = []byte(`{"id":"usd-eur","from_currency":"USD","to_currency":"EUR","rate":"0.92"}`)
	f.mu.Unlock()
	if _, err := s.loadConversions(t.Context()); err == nil {
		t.Fatal("legacy conversion with an id was read")
	}
	if err := s.migrateLegacyConversions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.objectCopy(legacy) != nil {
		t.Fatal("legacy conversion object is still stored")
	}
	convs, err := s.loadConversions(t.Context())
	if err != nil || len(convs) != 1 || convs[0].FromCurrency != currencyUSD || convs[0].ToCurrency != currencyEUR || convs[0].Rate != "0.92" {
		t.Fatalf("migrated conversions = %+v %v", convs, err)
	}
	if stored := f.objectCopy(convs[0].Key().objectID()); strings.Contains(string(stored), `"id"`) {
		t.Fatalf("migrated conversion keeps its id: %s", stored)
	}
	if err := s.migrateLegacyConversions(t.Context()); err != nil {
		t.Fatalf("second migration = %v", err)
	}
}

func TestMalformedStoredPricingIsDependencyError(t *testing.T) {
	f, blob, orch := newFixture(t)
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	f.mu.Lock()
	f.objects[pricePrefix+"/bad.json"] = []byte(`{bad`)
	f.mu.Unlock()
	if _, err := s.loadPrices(t.Context()); err == nil {
		t.Fatal("malformed stored price was skipped")
	}
	f.mu.Lock()
	f.objects[pricePrefix+"/bad.json"] = []byte(`{"vendor":"v","model":"m","effort":"e","modes":{},"from":"2026-01-01T00:00:00Z","currency":"GBP","unit_tokens":1,"input_uncached":"1","input_cached":"0","output":"0"}`)
	f.mu.Unlock()
	if _, err := s.loadPrices(t.Context()); err == nil {
		t.Fatal("invalid stored price was skipped")
	}
	f.mu.Lock()
	delete(f.objects, pricePrefix+"/bad.json")
	f.objects[conversionPrefix+"/bad.json"] = []byte(`{bad`)
	f.mu.Unlock()
	if _, err := s.loadConversions(t.Context()); err == nil {
		t.Fatal("malformed stored conversion was skipped")
	}
	f.mu.Lock()
	f.objects[conversionPrefix+"/bad.json"] = []byte(`{"from_currency":"EUR","to_currency":"EUR","rate":"1"}`)
	f.mu.Unlock()
	if _, err := s.loadConversions(t.Context()); err == nil {
		t.Fatal("invalid stored conversion was skipped")
	}
}

func TestPriceAndConversionCRUDAndValidation(t *testing.T) {
	f, blob, orch := newFixture(t)
	s := &service{blobs: blob.URL, orchestrator: orch.URL, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Price validation failures.
	toBefore := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	invalidPrices := []Price{
		{Model: "m", Effort: "e", Modes: map[string]string{}, From: time.Now().UTC(), Currency: currencyEUR, UnitTokens: 1, InputUncached: "0", InputCached: "0", Output: "0"},
		{Vendor: "v", Model: "m", Effort: "e", Modes: map[string]string{}, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), To: &toBefore, Currency: currencyEUR, UnitTokens: 1, InputUncached: "0", InputCached: "0", Output: "0"},
		{Vendor: "v", Model: "m", Effort: "e", Modes: map[string]string{}, From: time.Now().UTC(), Currency: "GBP", UnitTokens: 1, InputUncached: "0", InputCached: "0", Output: "0"},
		{Vendor: "v", Model: "m", Effort: "e", Modes: map[string]string{}, From: time.Now().UTC(), Currency: currencyEUR, UnitTokens: 0, InputUncached: "0", InputCached: "0", Output: "0"},
		{Vendor: "v", Model: "m", Effort: "e", Modes: map[string]string{}, From: time.Now().UTC(), Currency: currencyEUR, UnitTokens: 1, InputUncached: "-1", InputCached: "0", Output: "0"},
	}
	for i := range invalidPrices {
		if err := validatePrice(&invalidPrices[i]); err == nil {
			t.Fatalf("invalid price %d accepted", i)
		}
	}

	// Price HTTP error paths.
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/prices", `{bad`); code != http.StatusBadRequest {
		t.Fatalf("bad JSON price = %d", code)
	}
	validPrice := `{"vendor":"v","model":"m","effort":"e","modes":{},"from":"2026-02-01T00:00:00Z","to":null,"currency":"EUR","unit_tokens":1000,"input_uncached":"1","input_cached":"0.5","output":"2"}`
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/prices", `{"id":"p1",`+validPrice[1:]); code != http.StatusBadRequest {
		t.Fatalf("price with an id = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/prices", strings.Replace(validPrice, `"currency":"EUR"`, `"currency":"JPY"`, 1)); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid price = %d", code)
	}
	p1 := srv.URL + "/api/prices?" + priceQuery("v", "m", "e", `{}`, "2026-02-01T00:00:00Z")

	// Create two prices; a second create of the same key is refused.
	status, created := request(t, http.MethodPost, srv.URL+"/api/prices", validPrice)
	if status != http.StatusOK || strings.Contains(string(created), `"id"`) {
		t.Fatalf("create p1 = %d %s", status, created)
	}
	var saved Price
	if err := json.Unmarshal(created, &saved); err != nil || saved.Revision == "" {
		t.Fatalf("created price %s %v", created, err)
	}
	stored := f.objectCopy(saved.Key().objectID())
	if stored == nil || strings.Contains(string(stored), `"id"`) {
		t.Fatalf("stored price %s", stored)
	}
	p0 := `{"vendor":"v","model":"m2","effort":"e","modes":{},"from":"2026-01-01T00:00:00Z","to":null,"currency":"USD","unit_tokens":1000,"input_uncached":"1","input_cached":"0.5","output":"2"}`
	if code, body := request(t, http.MethodPost, srv.URL+"/api/prices", p0); code != http.StatusOK {
		t.Fatalf("create p0 = %d %s", code, body)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/prices", validPrice); code != http.StatusPreconditionFailed {
		t.Fatalf("duplicate create p1 = %d", code)
	}

	// Update by key with If-Match.
	updated := strings.Replace(validPrice, `"output":"2"`, `"output":"3"`, 1)
	if code, _ := request(t, http.MethodPut, p1, updated); code != http.StatusBadRequest {
		t.Fatalf("update without If-Match = %d", code)
	}
	if code, _ := conditionalRequest(t, http.MethodPut, p1, updated, "If-None-Match", "*"); code != http.StatusBadRequest {
		t.Fatalf("update with If-None-Match = %d", code)
	}
	if code, _ := conditionalRequest(t, http.MethodPut, srv.URL+"/api/prices?vendor=v", updated, "If-Match", quoteETag(saved.Revision)); code != http.StatusBadRequest {
		t.Fatalf("update with partial key = %d", code)
	}
	moved := strings.Replace(updated, `"from":"2026-02-01T00:00:00Z"`, `"from":"2026-03-01T00:00:00Z"`, 1)
	if code, _ := conditionalRequest(t, http.MethodPut, p1, moved, "If-Match", quoteETag(saved.Revision)); code != http.StatusUnprocessableEntity {
		t.Fatalf("update that changes the key = %d", code)
	}
	status, updatedBody := conditionalRequest(t, http.MethodPut, p1, updated, "If-Match", quoteETag(saved.Revision))
	if status != http.StatusOK {
		t.Fatalf("update p1 = %d %s", status, updatedBody)
	}
	if code, _ := conditionalRequest(t, http.MethodPut, p1, updated, "If-Match", quoteETag(saved.Revision)); code != http.StatusPreconditionFailed {
		t.Fatalf("stale update p1 = %d", code)
	}
	if err := json.Unmarshal(updatedBody, &saved); err != nil || saved.Output != "3" {
		t.Fatalf("updated price %s %v", updatedBody, err)
	}
	missing := srv.URL + "/api/prices?" + priceQuery("v", "m", "e", `{}`, "2020-01-01T00:00:00Z")
	if code, _ := conditionalRequest(t, http.MethodPut, missing, strings.Replace(strings.Replace(updated, "2026-02-01", "2020-01-01", 1), `"to":null`, `"to":"2021-01-01T00:00:00Z"`, 1), "If-Match", `"r1"`); code != http.StatusPreconditionFailed {
		t.Fatalf("update of a missing price = %d", code)
	}

	status, data := request(t, http.MethodGet, srv.URL+"/api/prices", "")
	if status != http.StatusOK {
		t.Fatalf("list prices = %d %s", status, data)
	}
	var listedPrices struct {
		Prices []Price `json:"prices"`
	}
	if err := json.Unmarshal(data, &listedPrices); err != nil {
		t.Fatal(err)
	}
	if len(listedPrices.Prices) != 2 || listedPrices.Prices[0].Model != "m2" || listedPrices.Prices[1].Model != "m" {
		t.Fatalf("prices order = %+v", listedPrices.Prices)
	}

	// Delete by key with If-Match.
	if code, _ := request(t, http.MethodDelete, p1, ""); code != http.StatusBadRequest {
		t.Fatalf("delete without If-Match = %d", code)
	}
	if code, _ := conditionalRequest(t, http.MethodDelete, p1, "", "If-Match", `"stale"`); code != http.StatusPreconditionFailed {
		t.Fatalf("stale delete = %d", code)
	}
	if code, _ := conditionalRequest(t, http.MethodDelete, missing, "", "If-Match", `"r1"`); code != http.StatusNotFound {
		t.Fatalf("delete of a missing price = %d", code)
	}
	if code, body := conditionalRequest(t, http.MethodDelete, p1, "", "If-Match", quoteETag(saved.Revision)); code != http.StatusNoContent {
		t.Fatalf("delete p1 = %d %s", code, body)
	}
	if f.objectCopy(saved.Key().objectID()) != nil {
		t.Fatal("deleted price is still stored")
	}
	if code, body := request(t, http.MethodPost, srv.URL+"/api/prices", validPrice); code != http.StatusOK {
		t.Fatalf("create after delete = %d %s", code, body)
	}

	// Dependency failure on list and create.
	f.mu.Lock()
	f.failPriceList = true
	f.mu.Unlock()
	if got, _ := request(t, http.MethodGet, srv.URL+"/api/prices", ""); got != http.StatusServiceUnavailable {
		t.Fatalf("list prices failure = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/prices", strings.Replace(validPrice, `"model":"m"`, `"model":"m3"`, 1)); got != http.StatusServiceUnavailable {
		t.Fatalf("create price failure = %d", got)
	}
	f.mu.Lock()
	f.failPriceList = false
	f.mu.Unlock()

	// Conversion validation & CRUD.
	invalidConversions := []Conversion{
		{FromCurrency: currencyEUR, ToCurrency: currencyEUR, Rate: "1"},
		{FromCurrency: "GBP", ToCurrency: currencyEUR, Rate: "1"},
		{FromCurrency: currencyUSD, ToCurrency: currencyEUR, Rate: "0"},
		{FromCurrency: currencyUSD, ToCurrency: currencyEUR, Rate: "-0.9"},
	}
	for i := range invalidConversions {
		if err := validateConversion(&invalidConversions[i]); err == nil {
			t.Fatalf("invalid conversion %d accepted", i)
		}
	}

	validConv := `{"from_currency":"USD","to_currency":"EUR","rate":"0.92"}`
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/conversions", `{bad`); got != http.StatusBadRequest {
		t.Fatalf("bad JSON conv = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/conversions", `{"id":"usd-eur",`+validConv[1:]); got != http.StatusBadRequest {
		t.Fatalf("conversion with an id = %d", got)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/conversions", `{"from_currency":"USD","to_currency":"USD","rate":"1"}`); got != http.StatusUnprocessableEntity {
		t.Fatalf("invalid conv = %d", got)
	}
	c1 := srv.URL + "/api/conversions?" + conversionQuery(currencyUSD, currencyEUR)

	// Create; a second create of the same pair is refused.
	code, createdConv := request(t, http.MethodPost, srv.URL+"/api/conversions", validConv)
	if code != http.StatusOK || strings.Contains(string(createdConv), `"id"`) {
		t.Fatalf("create conv = %d %s", code, createdConv)
	}
	var savedConv Conversion
	if err := json.Unmarshal(createdConv, &savedConv); err != nil || savedConv.Revision == "" {
		t.Fatalf("created conv %s %v", createdConv, err)
	}
	if stored := f.objectCopy(savedConv.Key().objectID()); stored == nil || strings.Contains(string(stored), `"id"`) {
		t.Fatalf("stored conv %s", stored)
	}
	if got, _ := request(t, http.MethodPost, srv.URL+"/api/conversions", validConv); got != http.StatusPreconditionFailed {
		t.Fatalf("duplicate create conv = %d", got)
	}

	// Update by key with If-Match.
	updatedConv := `{"from_currency":"USD","to_currency":"EUR","rate":"0.95"}`
	if got, _ := request(t, http.MethodPut, c1, updatedConv); got != http.StatusBadRequest {
		t.Fatalf("update conv without If-Match = %d", got)
	}
	if got, _ := conditionalRequest(t, http.MethodPut, c1, updatedConv, "If-None-Match", "*"); got != http.StatusBadRequest {
		t.Fatalf("update conv with If-None-Match = %d", got)
	}
	if got, _ := conditionalRequest(t, http.MethodPut, srv.URL+"/api/conversions?from_currency=USD", updatedConv, "If-Match", quoteETag(savedConv.Revision)); got != http.StatusBadRequest {
		t.Fatalf("update conv with partial key = %d", got)
	}
	swappedConv := `{"from_currency":"EUR","to_currency":"USD","rate":"1.1"}`
	if got, _ := conditionalRequest(t, http.MethodPut, c1, swappedConv, "If-Match", quoteETag(savedConv.Revision)); got != http.StatusUnprocessableEntity {
		t.Fatalf("update conv that changes the key = %d", got)
	}
	code, updatedBody = conditionalRequest(t, http.MethodPut, c1, updatedConv, "If-Match", quoteETag(savedConv.Revision))
	if code != http.StatusOK {
		t.Fatalf("update conv = %d %s", code, updatedBody)
	}
	if got, _ := conditionalRequest(t, http.MethodPut, c1, updatedConv, "If-Match", quoteETag(savedConv.Revision)); got != http.StatusPreconditionFailed {
		t.Fatalf("stale update conv = %d", got)
	}
	if err := json.Unmarshal(updatedBody, &savedConv); err != nil || savedConv.Rate != "0.95" {
		t.Fatalf("updated conv %s %v", updatedBody, err)
	}
	missingConv := srv.URL + "/api/conversions?" + conversionQuery(currencyEUR, currencyUSD)
	if got, _ := conditionalRequest(t, http.MethodPut, missingConv, swappedConv, "If-Match", `"r1"`); got != http.StatusPreconditionFailed {
		t.Fatalf("update of a missing conv = %d", got)
	}

	code, data = request(t, http.MethodGet, srv.URL+"/api/conversions", "")
	if code != http.StatusOK {
		t.Fatalf("list convs = %d %s", code, data)
	}
	var listedConvs struct {
		Conversions []Conversion `json:"conversions"`
	}
	if err := json.Unmarshal(data, &listedConvs); err != nil {
		t.Fatal(err)
	}
	if len(listedConvs.Conversions) != 1 || listedConvs.Conversions[0].FromCurrency != currencyUSD || listedConvs.Conversions[0].ToCurrency != currencyEUR || listedConvs.Conversions[0].Rate != "0.95" {
		t.Fatalf("conversions = %+v", listedConvs.Conversions)
	}

	// Delete by key with If-Match.
	if got, _ := request(t, http.MethodDelete, c1, ""); got != http.StatusBadRequest {
		t.Fatalf("delete conv without If-Match = %d", got)
	}
	if got, _ := conditionalRequest(t, http.MethodDelete, c1, "", "If-Match", `"stale"`); got != http.StatusPreconditionFailed {
		t.Fatalf("stale delete conv = %d", got)
	}
	if got, _ := conditionalRequest(t, http.MethodDelete, missingConv, "", "If-Match", `"r1"`); got != http.StatusNotFound {
		t.Fatalf("delete of a missing conv = %d", got)
	}
	if got, body := conditionalRequest(t, http.MethodDelete, c1, "", "If-Match", quoteETag(savedConv.Revision)); got != http.StatusNoContent {
		t.Fatalf("delete conv = %d %s", got, body)
	}
	if f.objectCopy(savedConv.Key().objectID()) != nil {
		t.Fatal("deleted conversion is still stored")
	}
	if got, body := request(t, http.MethodPost, srv.URL+"/api/conversions", validConv); got != http.StatusOK {
		t.Fatalf("create conv after delete = %d %s", got, body)
	}

	f.mu.Lock()
	f.objects[conversionPrefix+"/corrupt.json"] = []byte(`{bad`)
	f.mu.Unlock()
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/conversions", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt list convs = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/conversions", swappedConv); code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt post conv = %d", code)
	}
}
