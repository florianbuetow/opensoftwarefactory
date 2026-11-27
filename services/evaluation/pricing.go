package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// PriceKey is what one price row describes. Two rows with the same key
// cannot exist, so the key addresses the row.
type PriceKey struct {
	From   time.Time         `json:"from"`
	Modes  map[string]string `json:"modes"`
	Vendor string            `json:"vendor"`
	Model  string            `json:"model"`
	Effort string            `json:"effort"`
}

// Key returns the fields of p that address it.
func (p *Price) Key() *PriceKey {
	return &PriceKey{Vendor: p.Vendor, Model: p.Model, Effort: p.Effort, Modes: p.Modes, From: p.From}
}

func (k *PriceKey) equal(o *PriceKey) bool {
	return k.Vendor == o.Vendor && k.Model == o.Model && k.Effort == o.Effort && modesKey(k.Modes) == modesKey(o.Modes) && k.From.Equal(o.From)
}

// objectID is the blob that stores the row with key k.
func (k *PriceKey) objectID() string {
	return pricePrefix + "/" + hashID(k.Vendor, k.Model, k.Effort, modesKey(k.Modes), k.From.UTC().Format(time.RFC3339Nano)) + ".json"
}

// queryPriceKey reads the key from ?vendor=&model=&effort=&modes=&from=,
// where modes is the modesKey JSON object and from is RFC 3339.
func queryPriceKey(r *http.Request) (PriceKey, error) {
	q := r.URL.Query()
	k := PriceKey{Vendor: q.Get("vendor"), Model: q.Get("model"), Effort: q.Get("effort")}
	if k.Vendor == "" || k.Model == "" || k.Effort == "" || q.Get("modes") == "" || q.Get("from") == "" {
		return PriceKey{}, errors.New("vendor, model, effort, modes, and from query parameters are required")
	}
	if err := json.Unmarshal([]byte(q.Get("modes")), &k.Modes); err != nil || k.Modes == nil {
		return PriceKey{}, errors.New("modes must be a JSON object")
	}
	from, err := time.Parse(time.RFC3339Nano, q.Get("from"))
	if err != nil {
		return PriceKey{}, errors.New("from must be an RFC 3339 time")
	}
	k.From = from
	return k, nil
}

func validatePrice(p *Price) error {
	if p.Vendor == "" || p.Model == "" || p.Effort == "" || p.Modes == nil || p.From.IsZero() {
		return errors.New("vendor, model, effort, modes, and from are required")
	}
	if p.To != nil && !p.From.Before(*p.To) {
		return errors.New("price interval must be non-empty")
	}
	if p.Currency != currencyEUR && p.Currency != currencyUSD {
		return errors.New("currency must be EUR or USD")
	}
	if p.UnitTokens <= 0 {
		return errors.New("unit_tokens must be positive")
	}
	for _, v := range []string{p.InputUncached, p.InputCached, p.Output} {
		if _, err := decimal(v, false); err != nil {
			return errors.New("prices must be non-negative decimal strings")
		}
	}
	return nil
}

// postPrice creates a row. A row with the same key is refused with 412.
func (s *service) postPrice(w http.ResponseWriter, r *http.Request) {
	var p Price
	if err := decodeStrictJSON(w, r, &p); err != nil {
		s.fail(w, 400, err)
		return
	}
	s.savePrice(w, r, &p, "*")
}

// putPrice replaces the row addressed by the query key. The body is the whole
// row; its key fields must equal the query. If-Match carries the revision.
func (s *service) putPrice(w http.ResponseWriter, r *http.Request) {
	key, err := queryPriceKey(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	var p Price
	if decodeErr := decodeStrictJSON(w, r, &p); decodeErr != nil {
		s.fail(w, 400, decodeErr)
		return
	}
	if !p.Key().equal(&key) {
		s.fail(w, 422, errors.New("the body's vendor, model, effort, modes, and from must match the query; a new start date is a delete and a create"))
		return
	}
	condition, err := requireMatch(r, "price updates require If-Match")
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	s.savePrice(w, r, &p, condition)
}

// requireMatch returns the revision of the If-Match header and refuses any
// other condition.
func requireMatch(r *http.Request, missing string) (string, error) {
	condition, err := requestCondition(r)
	if err != nil {
		return "", err
	}
	if condition == "" || condition == "*" {
		return "", errors.New(missing)
	}
	return condition, nil
}

func (s *service) savePrice(w http.ResponseWriter, r *http.Request, p *Price, condition string) {
	p.Revision = ""
	if err := validatePrice(p); err != nil {
		s.fail(w, 422, err)
		return
	}
	s.measurementMu.Lock()
	defer s.measurementMu.Unlock()
	prices, err := s.loadPrices(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	key := p.Key()
	for i := range prices {
		other := &prices[i]
		if !other.Key().equal(key) && other.Vendor == p.Vendor && other.Model == p.Model && other.Effort == p.Effort && modesKey(other.Modes) == modesKey(p.Modes) && intervalsOverlap(p.From, p.To, other.From, other.To) {
			s.fail(w, 422, errors.New("price interval overlaps another row with the same dimensions"))
			return
		}
	}
	revision, err := s.writeObject(r.Context(), key.objectID(), p, condition)
	if errors.Is(err, errPrecondition) {
		s.fail(w, 412, err)
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	p.Revision = revision
	w.Header().Set("ETag", quoteETag(revision))
	writeJSON(w, 200, p)
}

// deletePrice removes the row addressed by the query key. If-Match carries
// the revision. Evaluation is the only writer of prices, so the check and the
// delete run under measurementMu.
func (s *service) deletePrice(w http.ResponseWriter, r *http.Request) {
	key, err := queryPriceKey(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	condition, err := requireMatch(r, "price deletes require If-Match")
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	s.measurementMu.Lock()
	defer s.measurementMu.Unlock()
	object, err := s.readObject(r.Context(), key.objectID())
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("price not found"))
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	if object.Revision != condition {
		s.fail(w, 412, errPrecondition)
		return
	}
	if err := s.deleteObject(r.Context(), key.objectID()); err != nil {
		s.dependencyFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *service) listPrices(w http.ResponseWriter, r *http.Request) {
	p, err := s.loadPrices(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"prices": p})
}

// loadPrices reads every row. A row that is malformed, carries an unknown
// field such as a legacy id, or is not stored under its key's name is an error.
func (s *service) loadPrices(ctx context.Context) ([]Price, error) {
	ids, err := s.listObjects(ctx, pricePrefix)
	if err != nil {
		return nil, err
	}
	out := make([]Price, 0, len(ids))
	for _, id := range ids {
		o, e := s.readObject(ctx, id)
		if e != nil {
			return nil, e
		}
		var p Price
		decoder := json.NewDecoder(bytes.NewReader(o.Data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&p) != nil {
			return nil, errors.New("stored price configuration is malformed: " + id)
		}
		if err := validatePrice(&p); err != nil {
			return nil, fmt.Errorf("stored price configuration %s is invalid: %w", id, err)
		}
		if p.Key().objectID() != id {
			return nil, fmt.Errorf("stored price configuration %s is not stored under its key", id)
		}
		p.Revision = o.Revision
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From.Before(out[j].From) })
	return out, nil
}

// migrateLegacyPrices moves each row stored under an invented id to the name
// derived from its key and drops the id field. It runs at startup and fails
// on any row it cannot move.
func (s *service) migrateLegacyPrices(ctx context.Context) error {
	ids, err := s.listObjects(ctx, pricePrefix)
	if err != nil {
		return err
	}
	for _, id := range ids {
		o, err := s.readObject(ctx, id)
		if err != nil {
			return err
		}
		var p Price
		if json.Unmarshal(o.Data, &p) != nil {
			return errors.New("stored price configuration is malformed: " + id)
		}
		target := p.Key().objectID()
		if target == id {
			continue
		}
		if err := validatePrice(&p); err != nil {
			return fmt.Errorf("legacy price %s is invalid: %w", id, err)
		}
		p.Revision = ""
		if _, err := s.writeObject(ctx, target, p, "*"); err != nil {
			return fmt.Errorf("migrating legacy price %s: %w", id, err)
		}
		if err := s.deleteObject(ctx, id); err != nil {
			return fmt.Errorf("removing legacy price %s: %w", id, err)
		}
		s.log.Info("migrated legacy price", "from", id, "to", target)
	}
	return nil
}

// ConversionKey is the currency pair one conversion rate describes. Only one
// rate can exist per pair, so the pair addresses the row.
type ConversionKey struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
}

// Key returns the fields of c that address it.
func (c *Conversion) Key() ConversionKey {
	return ConversionKey{FromCurrency: c.FromCurrency, ToCurrency: c.ToCurrency}
}

func (k ConversionKey) equal(o ConversionKey) bool {
	return k.FromCurrency == o.FromCurrency && k.ToCurrency == o.ToCurrency
}

// objectID is the blob that stores the conversion with key k.
func (k ConversionKey) objectID() string {
	return conversionPrefix + "/" + hashID(k.FromCurrency, k.ToCurrency) + ".json"
}

// queryConversionKey reads the key from ?from_currency=&to_currency=.
func queryConversionKey(r *http.Request) (ConversionKey, error) {
	q := r.URL.Query()
	k := ConversionKey{FromCurrency: q.Get("from_currency"), ToCurrency: q.Get("to_currency")}
	if k.FromCurrency == "" || k.ToCurrency == "" {
		return ConversionKey{}, errors.New("from_currency and to_currency query parameters are required")
	}
	return k, nil
}

func validateConversion(c *Conversion) error {
	if c.FromCurrency == c.ToCurrency || (c.FromCurrency != currencyEUR && c.FromCurrency != currencyUSD) || (c.ToCurrency != currencyEUR && c.ToCurrency != currencyUSD) {
		return errors.New("conversion must be between different EUR/USD currencies")
	}
	if _, err := decimal(c.Rate, true); err != nil {
		return errors.New("rate must be a positive decimal string")
	}
	return nil
}

// postConversion creates a row. A row for the same pair is refused with 412.
func (s *service) postConversion(w http.ResponseWriter, r *http.Request) {
	var c Conversion
	if err := decodeStrictJSON(w, r, &c); err != nil {
		s.fail(w, 400, err)
		return
	}
	s.saveConversion(w, r, &c, "*")
}

// putConversion replaces the row addressed by the query key. The body is the
// whole row; its currency pair must equal the query. If-Match carries the revision.
func (s *service) putConversion(w http.ResponseWriter, r *http.Request) {
	key, err := queryConversionKey(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	var c Conversion
	if decodeErr := decodeStrictJSON(w, r, &c); decodeErr != nil {
		s.fail(w, 400, decodeErr)
		return
	}
	if !c.Key().equal(key) {
		s.fail(w, 422, errors.New("the body's from_currency and to_currency must match the query; a new currency pair is a delete and a create"))
		return
	}
	condition, err := requireMatch(r, "conversion updates require If-Match")
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	s.saveConversion(w, r, &c, condition)
}

func (s *service) saveConversion(w http.ResponseWriter, r *http.Request, c *Conversion, condition string) {
	c.Revision = ""
	if err := validateConversion(c); err != nil {
		s.fail(w, 422, err)
		return
	}
	s.measurementMu.Lock()
	defer s.measurementMu.Unlock()
	if _, err := s.loadConversions(r.Context()); err != nil {
		s.dependencyFailure(w, err)
		return
	}
	rev, err := s.writeObject(r.Context(), c.Key().objectID(), c, condition)
	if errors.Is(err, errPrecondition) {
		s.fail(w, 412, err)
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	c.Revision = rev
	s.syncConversionToSQLDB(r.Context(), c)
	w.Header().Set("ETag", quoteETag(rev))
	writeJSON(w, 200, c)
}

// deleteConversion removes the row addressed by the query key. If-Match carries
// the revision.
func (s *service) deleteConversion(w http.ResponseWriter, r *http.Request) {
	key, err := queryConversionKey(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	condition, err := requireMatch(r, "conversion deletes require If-Match")
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	s.measurementMu.Lock()
	defer s.measurementMu.Unlock()
	object, err := s.readObject(r.Context(), key.objectID())
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("conversion not found"))
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	if object.Revision != condition {
		s.fail(w, 412, errPrecondition)
		return
	}
	if err := s.deleteObject(r.Context(), key.objectID()); err != nil {
		s.dependencyFailure(w, err)
		return
	}
	s.deleteConversionFromSQLDB(r.Context(), key)
	w.WriteHeader(http.StatusNoContent)
}

func (s *service) listConversions(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadConversions(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"conversions": c})
}

// loadConversions reads every row. A row that is malformed, carries an unknown
// field such as a legacy id, or is not stored under its key's name is an error.
func (s *service) loadConversions(ctx context.Context) ([]Conversion, error) {
	ids, err := s.listObjects(ctx, conversionPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]Conversion, 0, len(ids))
	for _, id := range ids {
		o, e := s.readObject(ctx, id)
		if e != nil {
			return nil, e
		}
		var c Conversion
		decoder := json.NewDecoder(bytes.NewReader(o.Data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&c) != nil {
			return nil, errors.New("stored conversion configuration is malformed: " + id)
		}
		if err := validateConversion(&c); err != nil {
			return nil, fmt.Errorf("stored conversion configuration %s is invalid: %w", id, err)
		}
		if c.Key().objectID() != id {
			return nil, fmt.Errorf("stored conversion configuration %s is not stored under its key", id)
		}
		c.Revision = o.Revision
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromCurrency != out[j].FromCurrency {
			return out[i].FromCurrency < out[j].FromCurrency
		}
		return out[i].ToCurrency < out[j].ToCurrency
	})
	return out, nil
}

// migrateLegacyConversions moves each row stored under an invented id to the
// name derived from its key and drops the id field. It runs at startup and
// fails on any row it cannot move.
func (s *service) migrateLegacyConversions(ctx context.Context) error {
	ids, err := s.listObjects(ctx, conversionPrefix)
	if err != nil {
		return err
	}
	for _, id := range ids {
		o, err := s.readObject(ctx, id)
		if err != nil {
			return err
		}
		var c Conversion
		if json.Unmarshal(o.Data, &c) != nil {
			return errors.New("stored conversion configuration is malformed: " + id)
		}
		target := c.Key().objectID()
		if target == id {
			continue
		}
		if err := validateConversion(&c); err != nil {
			return fmt.Errorf("legacy conversion %s is invalid: %w", id, err)
		}
		c.Revision = ""
		if _, err := s.writeObject(ctx, target, c, "*"); err != nil {
			return fmt.Errorf("migrating legacy conversion %s: %w", id, err)
		}
		if err := s.deleteObject(ctx, id); err != nil {
			return fmt.Errorf("removing legacy conversion %s: %w", id, err)
		}
		s.log.Info("migrated legacy conversion", "from", id, "to", target)
	}
	return nil
}
