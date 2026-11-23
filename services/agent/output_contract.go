package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type OutputContract struct {
	Format   string          `json:"format"`
	Mappings []OutputMapping `json:"mappings"`
}

type OutputMapping struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func validateOutput(output string, contract OutputContract) string {
	var document any
	d := json.NewDecoder(strings.NewReader(output))
	d.UseNumber()
	if err := d.Decode(&document); err != nil {
		return "invalid JSON: " + err.Error()
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return "invalid JSON: expected one value"
	}
	for _, mapping := range contract.Mappings {
		value, ok := resolveJSONPointer(document, mapping.Path)
		if !ok {
			return fmt.Sprintf("mapping %q: path %q is missing", mapping.Name, mapping.Path)
		}
		switch mapping.Type {
		case "number":
			if _, ok := value.(json.Number); !ok {
				return fmt.Sprintf("mapping %q: path %q is not a number", mapping.Name, mapping.Path)
			}
		case "string":
			if _, ok := value.(string); !ok {
				return fmt.Sprintf("mapping %q: path %q is not a string", mapping.Name, mapping.Path)
			}
		}
	}
	return "valid"
}

func resolveJSONPointer(document any, pointer string) (any, bool) {
	if pointer == "" {
		return document, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	value := document
	for _, raw := range strings.Split(pointer[1:], "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch current := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = current[part]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(current) {
				return nil, false
			}
			value = current[index]
		default:
			return nil, false
		}
	}
	return value, true
}

type extractionRequest struct {
	ProducerRunID     string          `json:"producer_run_id"`
	TargetRunID       string          `json:"target_run_id"`
	TargetExecutionID string          `json:"target_execution_id"`
	ArtifactID        string          `json:"artifact_id"`
	AgentRef          string          `json:"agent_ref"`
	MappingVersion    string          `json:"mapping_version"`
	Mappings          []OutputMapping `json:"mappings"`
}

type extractionIntent struct {
	ID         string            `json:"id"`
	Request    extractionRequest `json:"request"`
	Validation string            `json:"validation"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	Status     string            `json:"status"`
	LastError  string            `json:"last_error,omitempty"`
}

type extractionPending struct {
	IntentID string `json:"intent_id"`
}

const (
	extractionIntentPrefix  = "judge-extractions"
	extractionPendingPrefix = "judge-extractions-pending"
)

func makeExtractionIntent(req StepRequest, agentRef string, contract OutputContract, validation string) *extractionIntent {
	if req.TargetRunID == "" || req.TargetExecutionID == "" {
		return nil
	}
	producer := req.ProducerRunID
	if producer == "" {
		producer = req.TraceID
	}
	payload := extractionRequest{
		ProducerRunID: producer, TargetRunID: req.TargetRunID, TargetExecutionID: req.TargetExecutionID,
		ArtifactID: req.OutputArtifact, AgentRef: agentRef, MappingVersion: agentRef, Mappings: contract.Mappings,
	}
	payloadData, _ := json.Marshal(payload)
	sum := sha256.Sum256(payloadData)
	id := fmt.Sprintf("%x", sum[:])
	now := time.Now().UTC()
	return &extractionIntent{ID: id, Request: payload, Validation: validation, CreatedAt: now, UpdatedAt: now, Status: "pending"}
}

func (s *Service) deliverExtraction(ctx context.Context, intent *extractionIntent, traceID string, log *slog.Logger) {
	if intent == nil {
		return
	}
	durableCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.persistExtractionIntent(durableCtx, *intent, traceID, true); err != nil {
		log.Error("judge extraction intent persistence failed", "error", err)
		return
	}
	if err := s.submitExtraction(durableCtx, intent.Request); err != nil {
		intent.Status = "failed"
		intent.LastError = err.Error()
		intent.UpdatedAt = time.Now().UTC()
		_ = s.persistExtractionIntent(durableCtx, *intent, traceID, false)
		log.Error("judge extraction delivery failed", "artifact", intent.Request.ArtifactID, "error", err)
		return
	}
	intent.Status = "delivered"
	intent.LastError = ""
	intent.UpdatedAt = time.Now().UTC()
	_ = s.persistExtractionIntent(durableCtx, *intent, traceID, false)
	if store, ok := s.Store.(PrefixStore); ok {
		_ = store.Delete(durableCtx, extractionPendingPrefix+"/"+intent.ID+".json")
	}
	log.Info("judge extraction submitted", "artifact", intent.Request.ArtifactID, "target_run_id", intent.Request.TargetRunID, "validation", intent.Validation)
}

func (s *Service) persistExtractionIntent(ctx context.Context, intent extractionIntent, traceID string, createPending bool) error {
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	if err = s.Store.Put(ctx, extractionIntentPrefix+"/"+intent.ID+".json", data, Meta{Creator: "agent:" + intent.Request.AgentRef, TraceID: traceID}); err != nil {
		return err
	}
	if createPending {
		marker, _ := json.Marshal(extractionPending{IntentID: extractionIntentPrefix + "/" + intent.ID + ".json"})
		return s.Store.Put(ctx, extractionPendingPrefix+"/"+intent.ID+".json", marker, Meta{Creator: "agent:" + intent.Request.AgentRef, TraceID: traceID})
	}
	return nil
}

func (s *Service) submitExtraction(ctx context.Context, payload extractionRequest) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(s.EvaluationURL, "/") + "/api/extractions"
	httpReq, err := http.NewRequestWithContext(deliveryCtx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("evaluation extraction: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *Service) replayExtractionIntents(ctx context.Context) error {
	store, ok := s.Store.(PrefixStore)
	if !ok {
		return nil
	}
	ids, err := store.List(ctx, extractionPendingPrefix)
	if err != nil {
		return err
	}
	var failures []string
	for _, id := range ids {
		data, err := store.Get(ctx, id)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		var pending extractionPending
		if json.Unmarshal(data, &pending) != nil {
			failures = append(failures, "invalid intent "+id)
			continue
		}
		intentData, err := store.Get(ctx, pending.IntentID)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		var intent extractionIntent
		if json.Unmarshal(intentData, &intent) != nil {
			failures = append(failures, "invalid extraction record "+pending.IntentID)
			continue
		}
		if intent.Status == "delivered" {
			_ = store.Delete(ctx, id)
			continue
		}
		if intent.Status == "delivered" {
			continue
		}
		if err := s.submitExtraction(ctx, intent.Request); err != nil {
			intent.Status = "failed"
			intent.LastError = err.Error()
			intent.UpdatedAt = time.Now().UTC()
			_ = s.persistExtractionIntent(ctx, intent, "", false)
			failures = append(failures, err.Error())
			continue
		}
		intent.Status = "delivered"
		intent.LastError = ""
		intent.UpdatedAt = time.Now().UTC()
		_ = s.persistExtractionIntent(ctx, intent, "", false)
		if err := store.Delete(ctx, extractionPendingPrefix+"/"+intent.ID+".json"); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("extraction replay: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (s *Service) reconcileExtractionIntents(ctx context.Context, limit int) error {
	store, ok := s.Store.(PrefixStore)
	if !ok {
		return nil
	}
	ids, err := store.List(ctx, extractionIntentPrefix)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	s.reconcileMu.Lock()
	start := s.intentReconcileOffset % len(ids)
	count := min(limit, len(ids))
	s.intentReconcileOffset = (start + count) % len(ids)
	s.reconcileMu.Unlock()
	for i := 0; i < count; i++ {
		id := ids[(start+i)%len(ids)]
		data, err := store.Get(ctx, id)
		if err != nil {
			return err
		}
		var intent extractionIntent
		if json.Unmarshal(data, &intent) != nil {
			return fmt.Errorf("invalid extraction record %s", id)
		}
		if intent.Status == "delivered" {
			continue
		}
		marker, _ := json.Marshal(extractionPending{IntentID: id})
		if err := s.Store.Put(ctx, extractionPendingPrefix+"/"+intent.ID+".json", marker, Meta{Creator: "agent:" + intent.Request.AgentRef}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) reconcileCompletedRunObligations(ctx context.Context, limit int) error {
	store, ok := s.Store.(TreeStore)
	if !ok {
		return nil
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if len(s.reconcileDirs) == 0 {
		s.reconcileDirs = []string{"D:runs"}
	}
	var failures []string
	for processed := 0; processed < limit && len(s.reconcileDirs) > 0; processed++ {
		item := s.reconcileDirs[0]
		s.reconcileDirs = s.reconcileDirs[1:]
		if strings.HasPrefix(item, "D:") {
			dir := strings.TrimPrefix(item, "D:")
			dirs, objects, err := store.ListChildren(ctx, dir)
			if err != nil {
				failures = append(failures, err.Error())
				continue
			}
			for _, child := range dirs {
				s.reconcileDirs = append(s.reconcileDirs, "D:"+child)
			}
			for _, object := range objects {
				s.reconcileDirs = append(s.reconcileDirs, "O:"+object)
			}
			continue
		}
		id := strings.TrimPrefix(item, "O:")
		if !strings.HasSuffix(id, ".run.json") || strings.Contains(id, ".attempt-") {
			continue
		}
		data, err := store.Get(ctx, id)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		var record RunRecord
		if json.Unmarshal(data, &record) != nil || record.Status != "completed" || record.ExtractionObligation == nil || record.ExtractionObligation.Status == "delivered" {
			continue
		}
		s.deliverExtraction(ctx, record.ExtractionObligation, record.RunID, s.Logger)
		output := strings.TrimSuffix(id, ".run.json")
		if err := s.persistRunRecord(record, output, Meta{Creator: "agent:" + record.Agent, TraceID: record.RunID}); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("completed extraction reconciliation: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (s *Service) replayExtractionLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			replayCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if err := s.reconcileExtractionIntents(replayCtx, 100); err != nil {
				s.Logger.Warn("judge extraction intent reconciliation incomplete", "error", err)
			}
			if err := s.reconcileCompletedRunObligations(replayCtx, 100); err != nil {
				s.Logger.Warn("completed judge extraction reconciliation incomplete", "error", err)
			}
			if err := s.replayExtractionIntents(replayCtx); err != nil {
				s.Logger.Warn("judge extraction replay incomplete", "error", err)
			}
			cancel()
		}
	}
}
