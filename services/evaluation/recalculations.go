package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type recalculationJob struct {
	ID              string                              `json:"id"`
	Revision        string                              `json:"revision,omitempty"`
	Status          string                              `json:"status"`
	Processed       int                                 `json:"processed"`
	Pending         int                                 `json:"pending"`
	Error           string                              `json:"error,omitempty"`
	WorkflowID      string                              `json:"workflow_id,omitempty"`
	MeasurementIDs  []string                            `json:"measurement_ids,omitempty"`
	From            *time.Time                          `json:"from,omitempty"`
	To              *time.Time                          `json:"to,omitempty"`
	DisplayCurrency string                              `json:"display_currency"`
	Generation      int64                               `json:"generation"`
	Prices          []Price                             `json:"prices"`
	Conversions     []Conversion                        `json:"conversions"`
	Next            int                                 `json:"next"`
	CreatedAt       time.Time                           `json:"created_at"`
	UpdatedAt       time.Time                           `json:"updated_at"`
	BatchResults    map[string]recalculationBatchResult `json:"batch_results,omitempty"`
}

type recalculationBatchResult struct {
	Processed         int    `json:"processed"`
	Pending           int    `json:"pending"`
	NextCursor        string `json:"next_cursor,omitempty"`
	Done              bool   `json:"done"`
	ProjectionPending bool   `json:"projection_pending"`
	Status            string `json:"status"`
	Error             string `json:"error,omitempty"`
}

type recalculationInput struct {
	MeasurementIDs  []string   `json:"measurement_ids"`
	From            *time.Time `json:"from"`
	To              *time.Time `json:"to"`
	DisplayCurrency string     `json:"display_currency"`
}

func (s *service) allocateGeneration(ctx context.Context) (int64, error) {
	const id = "measurement-config/generation.json"
	for attempt := 0; attempt < 16; attempt++ {
		o, err := s.readObject(ctx, id)
		n := int64(1)
		condition := "*"
		if err == nil {
			var v struct {
				Generation int64 `json:"generation"`
			}
			if json.Unmarshal(o.Data, &v) != nil {
				return 0, errors.New("invalid generation counter")
			}
			n = v.Generation + 1
			condition = o.Revision
		} else if !errors.Is(err, errNotFound) {
			return 0, err
		}
		_, err = s.writeObject(ctx, id, map[string]int64{"generation": n}, condition)
		if errors.Is(err, errPrecondition) {
			continue
		}
		return n, err
	}
	return 0, errors.New("generation allocation contention")
}

func (s *service) startRecalculation(w http.ResponseWriter, r *http.Request) {
	var in recalculationInput
	if err := decodeStrictJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	if in.DisplayCurrency != "EUR" && in.DisplayCurrency != "USD" {
		s.fail(w, 422, errors.New("display_currency must be EUR or USD"))
		return
	}
	if in.From != nil && in.To != nil && !in.From.Before(*in.To) {
		s.fail(w, 422, errors.New("from must precede to"))
		return
	}
	seen := map[string]bool{}
	for _, id := range in.MeasurementIDs {
		if !validRecordID(id) || seen[id] {
			s.fail(w, 422, errors.New("measurement_ids must be unique valid ids"))
			return
		}
		seen[id] = true
	}
	gen, err := s.allocateGeneration(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	prices, err := s.loadPrices(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	conversions, err := s.loadConversions(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	job := recalculationJob{ID: uuid.NewString(), Status: "pending", MeasurementIDs: in.MeasurementIDs, From: in.From, To: in.To, DisplayCurrency: in.DisplayCurrency, Generation: gen, Prices: prices, Conversions: conversions, BatchResults: map[string]recalculationBatchResult{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	ids, err := s.selectedUsageIDs(r.Context(), job)
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	job.Pending = len(ids)
	jobRevision, err := s.writeObject(r.Context(), jobPrefix+"/"+job.ID+".json", job, "*")
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	body, _ := json.Marshal(map[string]any{"id": job.ID, "display_currency": job.DisplayCurrency, "measurement_ids": job.MeasurementIDs, "from": job.From, "to": job.To})
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, s.orchestrator+"/api/measurement-recalculations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		job.Status = "start_failed"
		job.Error = err.Error()
	} else {
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			job.Status = "start_failed"
			job.Error = "orchestrator returned " + resp.Status
		} else {
			var accepted struct {
				WorkflowID string `json:"workflow_id"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&accepted) != nil || accepted.WorkflowID == "" {
				job.Status = "start_failed"
				job.Error = "orchestrator response omitted workflow_id"
			} else {
				job.Status = "running"
				job.WorkflowID = accepted.WorkflowID
			}
		}
	}
	job.UpdatedAt = time.Now().UTC()
	jobRevision, _ = s.writeObject(r.Context(), jobPrefix+"/"+job.ID+".json", job, jobRevision)
	job.Revision = jobRevision
	if job.Status == "start_failed" {
		writeJSON(w, 503, recalculationResponse(job))
		return
	}
	writeJSON(w, 202, recalculationResponse(job))
}

func (s *service) getRecalculation(w http.ResponseWriter, r *http.Request) {
	o, err := s.readObject(r.Context(), jobPrefix+"/"+r.PathValue("id")+".json")
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("recalculation not found"))
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	var job recalculationJob
	if json.Unmarshal(o.Data, &job) != nil {
		s.dependencyFailure(w, errors.New("stored job invalid"))
		return
	}
	job.Revision = o.Revision
	writeJSON(w, 200, recalculationResponse(job))
}

func recalculationResponse(job recalculationJob) map[string]any {
	out := map[string]any{"id": job.ID, "revision": job.Revision, "status": job.Status, "processed": job.Processed, "pending": job.Pending, "workflow_id": job.WorkflowID, "done": job.Status == "complete", "projection_pending": job.Status == "projection_pending"}
	if job.Error != "" {
		out["error"] = job.Error
	}
	return out
}

type batchInput struct {
	DisplayCurrency string     `json:"display_currency"`
	MeasurementIDs  []string   `json:"measurement_ids"`
	From            *time.Time `json:"from"`
	To              *time.Time `json:"to"`
	Cursor          string     `json:"cursor,omitempty"`
	Limit           int        `json:"limit"`
}

func (s *service) recalculationBatch(w http.ResponseWriter, r *http.Request) {
	var in batchInput
	if err := decodeStrictJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	if in.Limit < 1 || in.Limit > 500 {
		s.fail(w, 422, errors.New("limit must be 1..500"))
		return
	}
	o, err := s.readObject(r.Context(), jobPrefix+"/"+r.PathValue("id")+".json")
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("recalculation not found"))
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	var job recalculationJob
	if json.Unmarshal(o.Data, &job) != nil {
		s.dependencyFailure(w, errors.New("stored job invalid"))
		return
	}
	if in.DisplayCurrency != job.DisplayCurrency || !sameStrings(in.MeasurementIDs, job.MeasurementIDs) || !sameTime(in.From, job.From) || !sameTime(in.To, job.To) {
		s.fail(w, 412, errors.New("batch selection differs from the pinned recalculation"))
		return
	}
	if replay, ok := job.BatchResults[in.Cursor]; ok {
		writeJSON(w, 200, replay)
		return
	}
	offset := 0
	if in.Cursor != "" {
		offset, err = strconv.Atoi(in.Cursor)
		if err != nil || offset < 0 {
			s.fail(w, 400, errors.New("invalid cursor"))
			return
		}
	}
	if offset != job.Next {
		s.fail(w, 412, fmt.Errorf("expected offset %d", job.Next))
		return
	}
	ids, err := s.selectedUsageIDs(r.Context(), job)
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	end := offset + in.Limit
	if end > len(ids) {
		end = len(ids)
	}
	failures := 0
	for _, id := range ids[offset:end] {
		m, err := s.loadMeasurement(r.Context(), "usage", id)
		if err != nil {
			failures++
			continue
		}
		cost, err := calculateCostWith(m, job.DisplayCurrency, job.Generation, job.Prices, job.Conversions)
		if err != nil {
			failures++
			continue
		}
		raw, _ := json.Marshal(cost)
		condition := "*"
		if current, readErr := s.readObject(r.Context(), costPrefix+"/"+id+".json"); readErr == nil {
			var old CurrentCost
			if json.Unmarshal(current.Data, &old) == nil && old.Generation >= job.Generation {
				continue
			}
			condition = current.Revision
		} else if !errors.Is(readErr, errNotFound) {
			failures++
			continue
		}
		op := mutationOperation{SchemaVersion: 1, ID: uuid.NewString(), ObjectID: costPrefix + "/" + id + ".json", MeasurementID: id, ExpectedRevision: condition, Replacement: raw, AffectedHour: m.OccurredAt.UTC().Truncate(time.Hour), Generation: job.Generation, Phase: "intent", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		_, opErr := s.writeObject(r.Context(), operationPrefix+"/"+op.ID+".json", op, "*")
		if err = opErr; err != nil {
			failures++
			continue
		}
	}
	batchProcessed := end - offset - failures
	if failures == 0 {
		job.Next = end
		job.Processed += batchProcessed
		job.Pending = len(ids) - end
	} else {
		job.Next = offset
		job.Pending = len(ids) - offset
	}
	projectionsComplete := false
	hours := map[time.Time]bool{}
	for _, id := range ids {
		if m, e := s.loadMeasurement(r.Context(), "usage", id); e == nil {
			hours[m.OccurredAt.UTC().Truncate(time.Hour)] = true
		}
	}
	if failures == 0 && end == len(ids) {
		projectionsComplete, _ = s.recalculationProjectionComplete(r.Context(), hours, job.Generation)
	}
	if failures > 0 {
		job.Status = "running_with_errors"
		job.Error = strconv.Itoa(failures) + " records failed in last batch"
	} else if end == len(ids) && !projectionsComplete {
		job.Status = "projection_pending"
		job.Error = ""
	} else if end == len(ids) && projectionsComplete {
		job.Status = "complete"
		job.Error = ""
	} else {
		job.Status = "running"
		job.Error = ""
	}
	job.UpdatedAt = time.Now().UTC()
	next := ""
	done := failures == 0 && end == len(ids) && projectionsComplete
	if !done {
		next = strconv.Itoa(job.Next)
	}
	result := recalculationBatchResult{Processed: batchProcessed, Pending: job.Pending, NextCursor: next, Done: done, ProjectionPending: job.Status == "projection_pending", Status: job.Status, Error: job.Error}
	if job.BatchResults == nil {
		job.BatchResults = map[string]recalculationBatchResult{}
	}
	if job.Next > offset {
		job.BatchResults[in.Cursor] = result
	}
	rev, err := s.writeObject(r.Context(), jobPrefix+"/"+job.ID+".json", job, o.Revision)
	if errors.Is(err, errPrecondition) {
		s.fail(w, 412, err)
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	job.Revision = rev
	writeJSON(w, 200, result)
}

func (s *service) recalculationProjectionComplete(ctx context.Context, hours map[time.Time]bool, generation int64) (bool, error) {
	for hour := range hours {
		object, err := s.readObject(ctx, s.projectionObject(hour))
		if err != nil {
			return false, nil
		}
		var manifest projectionManifest
		if json.Unmarshal(object.Data, &manifest) != nil || manifest.Phase != "complete" || manifest.Generation < generation {
			return false, nil
		}
	}
	ids, err := s.listObjects(ctx, operationPrefix)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		object, e := s.readObject(ctx, id)
		if e != nil {
			return false, e
		}
		var op mutationOperation
		if json.Unmarshal(object.Data, &op) == nil && op.Generation == generation && op.Phase != "complete" && op.Phase != "cancelled" {
			return false, nil
		}
	}
	return true, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func (s *service) selectedUsageIDs(ctx context.Context, job recalculationJob) ([]string, error) {
	ids, err := s.listObjects(ctx, usagePrefix)
	if err != nil {
		return nil, err
	}
	selected := map[string]bool{}
	for _, id := range job.MeasurementIDs {
		selected[id] = true
	}
	out := make([]string, 0, len(ids))
	for _, objectID := range ids {
		id := strings.TrimSuffix(strings.TrimPrefix(objectID, usagePrefix+"/"), ".json")
		if len(selected) > 0 && !selected[id] {
			continue
		}
		m, e := s.loadMeasurement(ctx, "usage", id)
		if e != nil {
			return nil, e
		}
		if job.From != nil && m.OccurredAt.Before(*job.From) || job.To != nil && !m.OccurredAt.Before(*job.To) {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}
func (s *service) loadMeasurement(ctx context.Context, kind, id string) (Measurement, error) {
	o, err := s.readObject(ctx, measurementObject(kind, id))
	if err != nil {
		return Measurement{}, err
	}
	var m Measurement
	if json.Unmarshal(o.Data, &m) != nil {
		return m, errors.New("stored measurement invalid")
	}
	m.Revision = o.Revision
	return m, nil
}
