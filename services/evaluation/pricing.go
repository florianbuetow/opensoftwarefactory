package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
)

func validatePrice(p *Price, id string) error {
	if !validRecordID(id) || p.ID != id || p.Vendor == "" || p.Model == "" || p.Effort == "" || p.Modes == nil || p.From.IsZero() {
		return errors.New("id, vendor, model, effort, modes, and from are required")
	}
	if p.To != nil && !p.From.Before(*p.To) {
		return errors.New("price interval must be non-empty")
	}
	if p.Currency != "EUR" && p.Currency != "USD" {
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

func (s *service) putPrice(w http.ResponseWriter, r *http.Request) {
	var p Price
	if err := decodeStrictJSON(w, r, &p); err != nil {
		s.fail(w, 400, err)
		return
	}
	if err := validatePrice(&p, r.PathValue("id")); err != nil {
		s.fail(w, 422, err)
		return
	}
	condition, err := requestCondition(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	if condition == "" {
		s.fail(w, 400, errors.New("price writes require If-None-Match:* or If-Match"))
		return
	}
	s.measurementMu.Lock()
	defer s.measurementMu.Unlock()
	prices, err := s.loadPrices(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	for _, other := range prices {
		if other.ID != p.ID && other.Vendor == p.Vendor && other.Model == p.Model && other.Effort == p.Effort && modesKey(other.Modes) == modesKey(p.Modes) && intervalsOverlap(p.From, p.To, other.From, other.To) {
			s.fail(w, 422, errors.New("price interval overlaps another row with the same dimensions"))
			return
		}
	}
	p.Revision = ""
	revision, err := s.writeObject(r.Context(), pricePrefix+"/"+p.ID+".json", p, condition)
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

func (s *service) getPrice(w http.ResponseWriter, r *http.Request) {
	var p Price
	object, err := s.readObject(r.Context(), pricePrefix+"/"+r.PathValue("id")+".json")
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("price not found"))
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	if json.Unmarshal(object.Data, &p) != nil {
		s.dependencyFailure(w, errors.New("stored price is invalid"))
		return
	}
	p.Revision = object.Revision
	w.Header().Set("ETag", quoteETag(object.Revision))
	writeJSON(w, 200, p)
}

func (s *service) listPrices(w http.ResponseWriter, r *http.Request) {
	p, err := s.loadPrices(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"prices": p})
}

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
		if json.Unmarshal(o.Data, &p) != nil {
			return nil, errors.New("stored price configuration is malformed: " + id)
		}
		if err := validatePrice(&p, p.ID); err != nil {
			return nil, fmt.Errorf("stored price configuration %s is invalid: %w", id, err)
		}
		p.Revision = o.Revision
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From.Before(out[j].From) })
	return out, nil
}

func validateConversion(c *Conversion, id string) error {
	if !validRecordID(id) || c.ID != id {
		return errors.New("id must match path")
	}
	if c.FromCurrency == c.ToCurrency || (c.FromCurrency != "EUR" && c.FromCurrency != "USD") || (c.ToCurrency != "EUR" && c.ToCurrency != "USD") {
		return errors.New("conversion must be between different EUR/USD currencies")
	}
	if _, err := decimal(c.Rate, true); err != nil {
		return errors.New("rate must be a positive decimal string")
	}
	return nil
}

func (s *service) putConversion(w http.ResponseWriter, r *http.Request) {
	var c Conversion
	if err := decodeStrictJSON(w, r, &c); err != nil {
		s.fail(w, 400, err)
		return
	}
	if err := validateConversion(&c, r.PathValue("id")); err != nil {
		s.fail(w, 422, err)
		return
	}
	condition, err := requestCondition(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	if condition == "" {
		s.fail(w, 400, errors.New("conversion writes require If-None-Match:* or If-Match"))
		return
	}
	s.measurementMu.Lock()
	defer s.measurementMu.Unlock()
	all, err := s.loadConversions(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	for _, other := range all {
		if other.ID != c.ID && other.FromCurrency == c.FromCurrency && other.ToCurrency == c.ToCurrency {
			s.fail(w, 422, errors.New("a conversion for this currency pair already exists"))
			return
		}
	}
	c.Revision = ""
	rev, err := s.writeObject(r.Context(), conversionPrefix+"/"+c.ID+".json", c, condition)
	if errors.Is(err, errPrecondition) {
		s.fail(w, 412, err)
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	c.Revision = rev
	w.Header().Set("ETag", quoteETag(rev))
	writeJSON(w, 200, c)
}

func (s *service) getConversion(w http.ResponseWriter, r *http.Request) {
	o, err := s.readObject(r.Context(), conversionPrefix+"/"+r.PathValue("id")+".json")
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("conversion not found"))
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	var c Conversion
	if json.Unmarshal(o.Data, &c) != nil {
		s.dependencyFailure(w, errors.New("stored conversion invalid"))
		return
	}
	c.Revision = o.Revision
	w.Header().Set("ETag", quoteETag(o.Revision))
	writeJSON(w, 200, c)
}

func (s *service) listConversions(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadConversions(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"conversions": c})
}

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
		if json.Unmarshal(o.Data, &c) != nil {
			return nil, errors.New("stored conversion configuration is malformed: " + id)
		}
		if err := validateConversion(&c, c.ID); err != nil {
			return nil, fmt.Errorf("stored conversion configuration %s is invalid: %w", id, err)
		}
		c.Revision = o.Revision
		out = append(out, c)
	}
	return out, nil
}
