package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
	f.objects[pricePrefix+"/bad.json"] = []byte(`{"id":"bad","vendor":"v","model":"m","effort":"e","modes":{},"from":"2026-01-01T00:00:00Z","currency":"GBP","unit_tokens":1,"input_uncached":"1","input_cached":"0","output":"0"}`)
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
	f.objects[conversionPrefix+"/bad.json"] = []byte(`{"id":"bad","from_currency":"EUR","to_currency":"EUR","rate":"1"}`)
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
		{ID: "wrong", Vendor: "v", Model: "m", Effort: "e", Modes: map[string]string{}, From: time.Now().UTC(), Currency: currencyEUR, UnitTokens: 1, InputUncached: "0", InputCached: "0", Output: "0"},
		{ID: "p1", Vendor: "v", Model: "m", Effort: "e", Modes: map[string]string{}, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), To: &toBefore, Currency: currencyEUR, UnitTokens: 1, InputUncached: "0", InputCached: "0", Output: "0"},
		{ID: "p1", Vendor: "v", Model: "m", Effort: "e", Modes: map[string]string{}, From: time.Now().UTC(), Currency: "GBP", UnitTokens: 1, InputUncached: "0", InputCached: "0", Output: "0"},
		{ID: "p1", Vendor: "v", Model: "m", Effort: "e", Modes: map[string]string{}, From: time.Now().UTC(), Currency: currencyEUR, UnitTokens: 0, InputUncached: "0", InputCached: "0", Output: "0"},
		{ID: "p1", Vendor: "v", Model: "m", Effort: "e", Modes: map[string]string{}, From: time.Now().UTC(), Currency: currencyEUR, UnitTokens: 1, InputUncached: "-1", InputCached: "0", Output: "0"},
	}
	for i := range invalidPrices {
		if err := validatePrice(&invalidPrices[i], "p1"); err == nil {
			t.Fatalf("invalid price %d accepted", i)
		}
	}

	// Price HTTP error paths.
	if code, _ := conditionalPut(t, srv.URL+"/api/prices/p1", `{bad`, "If-None-Match", "*"); code != http.StatusBadRequest {
		t.Fatalf("bad JSON price = %d", code)
	}
	validPrice := `{"id":"p1","vendor":"v","model":"m","effort":"e","modes":{},"from":"2026-02-01T00:00:00Z","to":null,"currency":"EUR","unit_tokens":1000,"input_uncached":"1","input_cached":"0.5","output":"2"}`
	if code, _ := request(t, http.MethodPut, srv.URL+"/api/prices/p1", validPrice); code != http.StatusBadRequest {
		t.Fatalf("missing condition price = %d", code)
	}
	if code, _ := conditionalPut(t, srv.URL+"/api/prices/p1", validPrice, "If-None-Match", "other"); code != http.StatusBadRequest {
		t.Fatalf("bad condition price = %d", code)
	}
	if code, _ := conditionalPut(t, srv.URL+"/api/prices/p1", strings.Replace(validPrice, `"currency":"EUR"`, `"currency":"JPY"`, 1), "If-None-Match", "*"); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid price = %d", code)
	}

	// Create two prices, get, update, precondition fail, list.
	if code, body := conditionalPut(t, srv.URL+"/api/prices/p1", validPrice, "If-None-Match", "*"); code != http.StatusOK {
		t.Fatalf("create p1 = %d %s", code, body)
	}
	p0 := `{"id":"p0","vendor":"v","model":"m2","effort":"e","modes":{},"from":"2026-01-01T00:00:00Z","to":null,"currency":"USD","unit_tokens":1000,"input_uncached":"1","input_cached":"0.5","output":"2"}`
	if code, body := conditionalPut(t, srv.URL+"/api/prices/p0", p0, "If-None-Match", "*"); code != http.StatusOK {
		t.Fatalf("create p0 = %d %s", code, body)
	}
	if code, _ := conditionalPut(t, srv.URL+"/api/prices/p1", validPrice, "If-None-Match", "*"); code != http.StatusPreconditionFailed {
		t.Fatalf("duplicate create p1 = %d", code)
	}

	resp := get(t, srv.URL+"/api/prices/p1")
	etag := resp.Header.Get("ETag")
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if resp.StatusCode != http.StatusOK || etag == "" {
		t.Fatalf("get p1 = %d etag=%q", resp.StatusCode, etag)
	}
	if code, _ := conditionalPut(t, srv.URL+"/api/prices/p1", validPrice, "If-Match", etag); code != http.StatusOK {
		t.Fatalf("update p1 = %d", code)
	}
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/prices/missing", ""); code != http.StatusNotFound {
		t.Fatalf("missing price = %d", code)
	}

	code, data := request(t, http.MethodGet, srv.URL+"/api/prices", "")
	if code != http.StatusOK {
		t.Fatalf("list prices = %d %s", code, data)
	}
	var listedPrices struct {
		Prices []Price `json:"prices"`
	}
	if err := json.Unmarshal(data, &listedPrices); err != nil {
		t.Fatal(err)
	}
	if len(listedPrices.Prices) != 2 || listedPrices.Prices[0].ID != "p0" || listedPrices.Prices[1].ID != "p1" {
		t.Fatalf("prices order = %+v", listedPrices.Prices)
	}

	// Dependency failure on list/put/get price.
	f.mu.Lock()
	f.failPriceList = true
	f.objects[pricePrefix+"/corrupt.json"] = []byte(`{bad`)
	f.mu.Unlock()
	if got, _ := request(t, http.MethodGet, srv.URL+"/api/prices", ""); got != http.StatusServiceUnavailable {
		t.Fatalf("list prices failure = %d", got)
	}
	if got, _ := conditionalPut(t, srv.URL+"/api/prices/p3", strings.Replace(validPrice, `"p1"`, `"p3"`, 1), "If-None-Match", "*"); got != http.StatusServiceUnavailable {
		t.Fatalf("put price failure = %d", got)
	}
	if got, _ := request(t, http.MethodGet, srv.URL+"/api/prices/corrupt", ""); got != http.StatusServiceUnavailable {
		t.Fatalf("corrupt price = %d", got)
	}
	f.mu.Lock()
	f.failPriceList = false
	delete(f.objects, pricePrefix+"/corrupt.json")
	f.mu.Unlock()

	// Conversion validation & CRUD.
	invalidConversions := []Conversion{
		{ID: "wrong", FromCurrency: currencyUSD, ToCurrency: currencyEUR, Rate: "0.9"},
		{ID: "c1", FromCurrency: currencyEUR, ToCurrency: currencyEUR, Rate: "1"},
		{ID: "c1", FromCurrency: "GBP", ToCurrency: currencyEUR, Rate: "1"},
		{ID: "c1", FromCurrency: currencyUSD, ToCurrency: currencyEUR, Rate: "0"},
	}
	for i := range invalidConversions {
		if err := validateConversion(&invalidConversions[i], "c1"); err == nil {
			t.Fatalf("invalid conversion %d accepted", i)
		}
	}

	validConv := `{"id":"usd-eur","from_currency":"USD","to_currency":"EUR","rate":"0.92"}`
	if got, _ := conditionalPut(t, srv.URL+"/api/conversions/usd-eur", `{bad`, "If-None-Match", "*"); got != http.StatusBadRequest {
		t.Fatalf("bad JSON conv = %d", got)
	}
	if got, _ := conditionalPut(t, srv.URL+"/api/conversions/usd-eur", `{"id":"usd-eur","from_currency":"USD","to_currency":"USD","rate":"1"}`, "If-None-Match", "*"); got != http.StatusUnprocessableEntity {
		t.Fatalf("invalid conv = %d", got)
	}
	if got, _ := request(t, http.MethodPut, srv.URL+"/api/conversions/usd-eur", validConv); got != http.StatusBadRequest {
		t.Fatalf("missing condition conv = %d", got)
	}
	if got, _ := conditionalPut(t, srv.URL+"/api/conversions/usd-eur", validConv, "If-None-Match", "bad"); got != http.StatusBadRequest {
		t.Fatalf("bad condition conv = %d", got)
	}
	if got, body := conditionalPut(t, srv.URL+"/api/conversions/usd-eur", validConv, "If-None-Match", "*"); got != http.StatusOK {
		t.Fatalf("create conv = %d %s", got, body)
	}
	if got, _ := conditionalPut(t, srv.URL+"/api/conversions/usd-eur", validConv, "If-None-Match", "*"); got != http.StatusPreconditionFailed {
		t.Fatalf("duplicate create conv = %d", got)
	}
	dupPair := `{"id":"usd-eur-2","from_currency":"USD","to_currency":"EUR","rate":"0.95"}`
	if got, _ := conditionalPut(t, srv.URL+"/api/conversions/usd-eur-2", dupPair, "If-None-Match", "*"); got != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate pair conv = %d", got)
	}

	resp = get(t, srv.URL+"/api/conversions/usd-eur")
	convETag := resp.Header.Get("ETag")
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if resp.StatusCode != http.StatusOK || convETag == "" {
		t.Fatalf("get conv = %d etag=%q", resp.StatusCode, convETag)
	}
	if got, _ := conditionalPut(t, srv.URL+"/api/conversions/usd-eur", validConv, "If-Match", convETag); got != http.StatusOK {
		t.Fatalf("update conv = %d", got)
	}
	if got, _ := request(t, http.MethodGet, srv.URL+"/api/conversions/missing", ""); got != http.StatusNotFound {
		t.Fatalf("missing conv = %d", got)
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
	if len(listedConvs.Conversions) != 1 || listedConvs.Conversions[0].ID != "usd-eur" {
		t.Fatalf("conversions = %+v", listedConvs.Conversions)
	}

	f.mu.Lock()
	f.objects[conversionPrefix+"/corrupt.json"] = []byte(`{bad`)
	f.mu.Unlock()
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/conversions/corrupt", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt get conv = %d", code)
	}
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/conversions", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt list convs = %d", code)
	}
	if code, _ := conditionalPut(t, srv.URL+"/api/conversions/eur-usd", `{"id":"eur-usd","from_currency":"EUR","to_currency":"USD","rate":"1.1"}`, "If-None-Match", "*"); code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt put conv = %d", code)
	}
}
