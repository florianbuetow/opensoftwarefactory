package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

func (s *service) syncMeasurementToSQLDB(ctx context.Context, raw json.RawMessage) {
	if s.sqldb == "" || s.http == nil || len(raw) == 0 {
		return
	}
	var meta struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &meta) != nil || meta.ID == "" {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := s.newRequest(reqCtx, s.sqldb, http.MethodPut, "/api/measurements/"+url.PathEscape(meta.ID), bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		if s.log != nil {
			s.log.Warn("syncing measurement to sqldb failed", "id", meta.ID, "error", err)
		}
		return
	}
	_ = resp.Body.Close()
}

func (s *service) syncCostToSQLDB(ctx context.Context, cost *CurrentCost) {
	if s.sqldb == "" || s.http == nil || cost == nil || cost.UsageID == "" {
		return
	}
	raw, err := json.Marshal(cost)
	if err != nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := s.newRequest(reqCtx, s.sqldb, http.MethodPut, "/api/costs/"+url.PathEscape(cost.UsageID), bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		if s.log != nil {
			s.log.Warn("syncing cost to sqldb failed", "usage_id", cost.UsageID, "error", err)
		}
		return
	}
	_ = resp.Body.Close()
}

func (s *service) syncConversionToSQLDB(ctx context.Context, c *Conversion) {
	if s.sqldb == "" || s.http == nil || c == nil {
		return
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := s.newRequest(reqCtx, s.sqldb, http.MethodPut, "/api/conversions", bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		if s.log != nil {
			s.log.Warn("syncing conversion to sqldb failed", "from", c.FromCurrency, "to", c.ToCurrency, "error", err)
		}
		return
	}
	_ = resp.Body.Close()
}

func (s *service) deleteConversionFromSQLDB(ctx context.Context, key ConversionKey) {
	if s.sqldb == "" || s.http == nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := s.newRequest(reqCtx, s.sqldb, http.MethodDelete, "/api/conversions", http.NoBody)
	if err != nil {
		return
	}
	req.URL.RawQuery = url.Values{"from_currency": {key.FromCurrency}, "to_currency": {key.ToCurrency}}.Encode()
	resp, err := s.http.Do(req)
	if err != nil {
		if s.log != nil {
			s.log.Warn("deleting conversion from sqldb failed", "from", key.FromCurrency, "to", key.ToCurrency, "error", err)
		}
		return
	}
	_ = resp.Body.Close()
}
