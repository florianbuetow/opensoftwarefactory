package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const projectionStream = "factory_measurements"

var projectionStreamNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,95}$`)

func validProjectionStream(name string) bool { return projectionStreamNamePattern.MatchString(name) }

func (s *service) projectionStreamName() string {
	if s.projectionStream != "" {
		return s.projectionStream
	}
	return projectionStream
}

type projectionManifest struct {
	Stream         string    `json:"stream"`
	Hour           time.Time `json:"hour"`
	Generation     int64     `json:"generation"`
	Phase          string    `json:"phase"`
	DeleteJobID    string    `json:"delete_job_id,omitempty"`
	ExpectedIDs    []string  `json:"expected_ids,omitempty"`
	PendingIDs     []string  `json:"pending_ids,omitempty"`
	ReplayCursor   int       `json:"replay_cursor,omitempty"`
	VerifyCursor   int       `json:"verify_cursor,omitempty"`
	ActualIDs      []string  `json:"actual_ids,omitempty"`
	SnapshotID     string    `json:"snapshot_id,omitempty"`
	SnapshotChunks int       `json:"snapshot_chunks,omitempty"`
	SnapshotCount  int       `json:"snapshot_count,omitempty"`
	Error          string    `json:"error,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func projectionObject(hour time.Time) string {
	return "measurement-projections/" + projectionStream + "/" + hour.UTC().Format("20060102T15") + ".json"
}

func (s *service) projectionObject(hour time.Time) string {
	return "measurement-projections/" + s.projectionStreamName() + "/" + hour.UTC().Format("20060102T15") + ".json"
}

// advanceProjection advances a bounded number of durable phases. An ambiguous
// remote delete or ingest is terminal until an operator resolves it: another
// local operation cannot establish remote quiescence.
func (s *service) advanceProjection(ctx context.Context, hour time.Time, generation int64) (bool, error) {
	if s.openobserve == "" || s.openobserveUser == "" || s.openobservePassword == "" {
		return false, errors.New("OpenObserve is not configured")
	}
	hour = hour.UTC().Truncate(time.Hour)
	id := s.projectionObject(hour)
	stream := s.projectionStreamName()
	m := projectionManifest{Stream: stream, Hour: hour, Generation: generation, Phase: "snapshot_needed", UpdatedAt: time.Now().UTC()}
	condition := "*"
	if object, err := s.readObject(ctx, id); err == nil {
		if json.Unmarshal(object.Data, &m) != nil {
			return false, errors.New("invalid projection manifest")
		}
		condition = object.Revision
		if m.Phase == "complete" && m.Generation >= generation {
			return true, nil
		}
		if m.Generation < generation {
			if m.Phase != "complete" {
				return false, nil
			}
			m.Generation, m.Phase, m.DeleteJobID, m.ExpectedIDs, m.Error = generation, "snapshot_needed", "", nil, ""
			m.PendingIDs = nil
			m.ActualIDs = nil
			m.ReplayCursor = 0
			m.VerifyCursor = 0
			m.SnapshotID = ""
			m.SnapshotChunks = 0
			m.SnapshotCount = 0
		}
	} else if !errors.Is(err, errNotFound) {
		return false, err
	}
	write := func() error {
		m.UpdatedAt = time.Now().UTC()
		revision, err := s.writeObject(ctx, id, m, condition)
		if err == nil {
			condition = revision
		}
		return err
	}
	if condition == "*" {
		if err := write(); err != nil {
			return false, err
		}
	}
	for step := 0; step < 4; step++ {
		switch m.Phase {
		case "snapshot_needed":
			m.SnapshotID = uuid.NewString()
			m.Phase = "snapshot_building"
			m.Error = ""
			if err := write(); err != nil {
				return false, err
			}
			chunks, count, err := s.freezeProjectionSnapshot(ctx, hour, m.Generation, m.SnapshotID)
			if err != nil {
				m.Error = err.Error()
				_ = write()
				return false, nil
			}
			m.SnapshotChunks, m.SnapshotCount, m.Phase = chunks, count, "delete_needed"
			if err = write(); err != nil {
				return false, err
			}
		case "snapshot_building":
			m.Phase, m.SnapshotID, m.Error = "snapshot_needed", "", "interrupted snapshot build discarded before remote mutation"
			m.SnapshotChunks, m.SnapshotCount = 0, 0
			if err := write(); err != nil {
				return false, err
			}
		case "delete_needed":
			quiescent, absent, qerr := s.projectionQuiescent(ctx, hour)
			if qerr != nil {
				m.Error = qerr.Error()
				_ = write()
				return false, nil
			}
			if !quiescent {
				m.Error = "waiting for OpenObserve WAL to persist the complete owned hour"
				_ = write()
				return false, nil
			}
			if absent {
				m.Phase, m.Error, m.DeleteJobID = "replay_ready", "", "stream_absent"
				if err := write(); err != nil {
					return false, err
				}
				continue
			}
			m.Phase, m.Error = "delete_submitting", ""
			if err := write(); err != nil {
				return false, err
			}
			job, definitive, err := s.requestHourDelete(ctx, hour)
			if err != nil {
				m.Error = err.Error()
				if !definitive {
					m.Phase = "delete_ambiguous"
				} else {
					m.Phase = "delete_needed"
				}
				_ = write()
				return false, nil
			}
			if job == "" {
				m.Phase = "deletion_verifying"
			} else if job == "stream_absent" {
				m.Phase = "replay_ready"
				m.DeleteJobID = ""
			} else {
				m.DeleteJobID, m.Phase = job, "delete_requested"
			}
			m.Error = ""
			if err = write(); err != nil {
				return false, err
			}
			if job != "" && job != "stream_absent" {
				return false, nil
			}
		case "delete_submitting":
			m.Phase, m.Error = "delete_ambiguous", "delete submission was interrupted before its remote identity was persisted"
			_ = write()
			return false, nil
		case "delete_requested":
			complete, ambiguous, err := s.deleteStatus(ctx, m.DeleteJobID)
			if err != nil {
				m.Error = err.Error()
				if ambiguous {
					m.Phase = "delete_ambiguous"
				}
				_ = write()
				return false, nil
			}
			if !complete {
				return false, nil
			}
			m.Phase, m.Error = "deletion_verifying", ""
			if err = write(); err != nil {
				return false, err
			}
		case "deletion_verifying":
			empty, err := s.projectionEmpty(ctx, hour)
			if err != nil {
				m.Error = err.Error()
				_ = write()
				return false, nil
			}
			if !empty {
				return false, nil
			}
			m.Phase, m.Error = "replay_ready", ""
			if err = write(); err != nil {
				return false, err
			}
		case "replay_ready":
			records, ids, more, err := s.projectionSnapshotBatch(ctx, m, m.ReplayCursor, 500)
			if err != nil {
				return false, err
			}
			m.PendingIDs, m.Phase, m.Error = ids, "ingestion_submitting", ""
			if err = write(); err != nil {
				return false, err
			}
			if len(records) == 0 {
				if m.SnapshotCount == 0 && m.DeleteJobID == "stream_absent" {
					m.Phase, m.Error = "complete", ""
					if err = write(); err != nil {
						return false, err
					}
					if err = s.completeProjectionOperations(ctx, hour, m.Generation); err != nil {
						return false, err
					}
					return true, nil
				}
				m.Phase = "ingestion_submitted"
				if err = write(); err != nil {
					return false, err
				}
				continue
			}
			definitive, err := s.ingestProjection(ctx, records)
			if err != nil {
				m.Error = err.Error()
				var partial partialIngestError
				if errors.As(err, &partial) {
					m.Phase = "delete_needed"
					m.DeleteJobID = ""
					m.ExpectedIDs = nil
					m.PendingIDs = nil
					m.ActualIDs = nil
					m.ReplayCursor = 0
					m.VerifyCursor = 0
				} else if !definitive {
					m.Phase = "ingestion_ambiguous"
				} else {
					m.Phase = "replay_ready"
				}
				_ = write()
				return false, nil
			}
			m.ExpectedIDs = append(m.ExpectedIDs, m.PendingIDs...)
			m.ReplayCursor += len(m.PendingIDs)
			m.PendingIDs = nil
			if more {
				m.Phase = "replay_ready"
			} else {
				m.Phase = "ingestion_submitted"
			}
			m.Error = ""
			if err = write(); err != nil {
				return false, err
			}
			if more {
				return false, nil
			}
		case "ingestion_submitted":
			actual, total, partial, err := s.verifyProjectionBatch(ctx, hour, m.VerifyCursor, 500)
			if err != nil {
				m.Error = err.Error()
				_ = write()
				return false, nil
			}
			if partial {
				m.Error = "OpenObserve verification query was partial"
				_ = write()
				return false, nil
			}
			for _, row := range actual {
				if row.Generation != m.Generation {
					m.Error = "projection generation mismatch"
					_ = write()
					return false, nil
				}
				m.ActualIDs = append(m.ActualIDs, row.ID)
			}
			m.VerifyCursor += len(actual)
			if m.VerifyCursor < total {
				_ = write()
				return false, nil
			}
			want, got := append([]string(nil), m.ExpectedIDs...), append([]string(nil), m.ActualIDs...)
			sort.Strings(want)
			sort.Strings(got)
			if len(want) != len(got) || strings.Join(want, "\x00") != strings.Join(got, "\x00") {
				m.Error = "projection identity verification mismatch"
				_ = write()
				return false, nil
			}
			m.Phase, m.Error = "complete", ""
			if err = write(); err != nil {
				return false, err
			}
			if err = s.completeProjectionOperations(ctx, hour, m.Generation); err != nil {
				return false, err
			}
			return true, nil
		case "complete":
			return m.Generation >= generation, nil
		case "ingestion_submitting":
			m.Phase, m.Error = "ingestion_ambiguous", "ingestion was interrupted before its acknowledgement was persisted"
			_ = write()
			return false, nil
		case "delete_ambiguous", "ingestion_ambiguous":
			return false, nil
		default:
			return false, fmt.Errorf("unknown projection phase %q", m.Phase)
		}
	}
	return false, nil
}

func (s *service) openObserveRequest(ctx context.Context, method, endpoint string, body []byte) (int, []byte, error) {
	if s.openobserve == "" || s.openobserveUser == "" || s.openobservePassword == "" {
		return 0, nil, errors.New("OpenObserve is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, method, s.openobserve+endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(s.openobserveUser, s.openobservePassword)
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, data, readErr
}

func (s *service) requestHourDelete(ctx context.Context, hour time.Time) (string, bool, error) {
	q := url.Values{"type": {"logs"}, "start": {strconv.FormatInt(hour.UnixMicro(), 10)}, "end": {strconv.FormatInt(hour.Add(time.Hour).UnixMicro(), 10)}}
	status, data, err := s.openObserveRequest(ctx, http.MethodDelete, "/api/default/streams/"+s.projectionStreamName()+"/data_by_time_range?"+q.Encode(), nil)
	if err != nil {
		return "", false, err
	}
	if status == 404 {
		return "stream_absent", true, nil
	}
	if status != 200 {
		return "", true, fmt.Errorf("OpenObserve delete returned %d: %s", status, strings.TrimSpace(string(data)))
	}
	var result struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &result) != nil || result.ID == "" {
		return "", false, errors.New("OpenObserve delete response omitted job identity")
	}
	return result.ID, true, nil
}

func (s *service) deleteStatus(ctx context.Context, job string) (bool, bool, error) {
	status, data, err := s.openObserveRequest(ctx, http.MethodGet, "/api/default/streams/"+s.projectionStreamName()+"/data_by_time_range/status/"+url.PathEscape(job), nil)
	if err != nil {
		return false, false, err
	}
	if status == 404 {
		return false, true, errors.New("OpenObserve deletion job identity was lost")
	}
	if status != 200 {
		return false, false, fmt.Errorf("OpenObserve delete status returned %d", status)
	}
	var result struct {
		Status string `json:"status"`
		Errors []any  `json:"errors"`
	}
	if json.Unmarshal(data, &result) != nil {
		return false, true, errors.New("invalid OpenObserve deletion status")
	}
	if len(result.Errors) > 0 {
		return false, true, errors.New("OpenObserve deletion status contains cluster errors")
	}
	return result.Status == "Completed", false, nil
}

func (s *service) projectionEmpty(ctx context.Context, hour time.Time) (bool, error) {
	stream := s.projectionStreamName()
	status, _, err := s.openObserveRequest(ctx, http.MethodGet, "/api/default/streams/"+stream+"/schema?type=logs", nil)
	if err != nil {
		return false, err
	}
	if status == 404 {
		return true, nil
	}
	if status/100 != 2 {
		return false, fmt.Errorf("OpenObserve stream schema returned %d while verifying deletion", status)
	}
	result, err := s.openObserveSearch(ctx, "SELECT projection_id FROM \""+stream+"\"", hour, hour.Add(time.Hour), 0, 1, "")
	return err == nil && !result.Partial && result.Total == 0, err
}

func (s *service) projectionQuiescent(ctx context.Context, hour time.Time) (bool, bool, error) {
	stream := s.projectionStreamName()
	schemaStatus, _, schemaErr := s.openObserveRequest(ctx, http.MethodGet, "/api/default/streams/"+stream+"/schema?type=logs", nil)
	if schemaErr != nil {
		return false, false, schemaErr
	}
	if schemaStatus == 404 {
		return true, true, nil
	}
	if schemaStatus/100 != 2 {
		return false, false, fmt.Errorf("OpenObserve stream schema returned %d", schemaStatus)
	}
	sql := "SELECT COUNT(*) AS record_count FROM \"" + stream + "\""
	full, err := s.openObserveSearch(ctx, sql, hour, hour.Add(time.Hour), 0, 1, "")
	if err != nil {
		return false, false, err
	}
	if full.Partial || len(full.Hits) != 1 {
		return false, false, nil
	}
	visible, ok := requiredInt64Value(full.Hits[0]["record_count"])
	if !ok {
		return false, false, errors.New("OpenObserve visible-count response omitted numeric record_count")
	}
	body, _ := json.Marshal(map[string]any{"sql": "SELECT projection_id FROM \"" + stream + "\" ORDER BY _timestamp", "start_time": hour.UnixMicro(), "end_time": hour.Add(time.Hour).UnixMicro()})
	status, response, err := s.openObserveRequest(ctx, http.MethodPost, "/api/default/_search_partition?type=logs", body)
	if err != nil {
		return false, false, err
	}
	if status == 404 {
		if visible == 0 {
			return true, false, nil
		}
		return false, false, errors.New("OpenObserve partition metadata is absent while the hour remains query-visible")
	}
	if status/100 != 2 {
		return false, false, fmt.Errorf("OpenObserve partition query returned %d: %s", status, strings.TrimSpace(string(response)))
	}
	var partition struct {
		Records *int64 `json:"records"`
		FileNum *int64 `json:"file_num"`
	}
	if json.Unmarshal(response, &partition) != nil || partition.Records == nil {
		return false, false, errors.New("invalid OpenObserve partition response: numeric records is required")
	}
	return visible == *partition.Records, false, nil
}

func requiredInt64Value(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		if x != float64(int64(x)) {
			return 0, false
		}
		return int64(x), true
	case json.Number:
		n, e := x.Int64()
		return n, e == nil
	case int64:
		return x, true
	case int:
		return int64(x), true
	}
	return 0, false
}

func (s *service) ingestProjection(ctx context.Context, records []map[string]any) (bool, error) {
	data, _ := json.Marshal(records)
	status, response, err := s.openObserveRequest(ctx, http.MethodPost, "/api/default/"+s.projectionStreamName()+"/_json", data)
	if err != nil {
		return false, err
	}
	if status/100 != 2 {
		return true, fmt.Errorf("OpenObserve ingestion returned %d: %s", status, strings.TrimSpace(string(response)))
	}
	var result struct {
		Status []struct {
			Successful int `json:"successful"`
			Failed     int `json:"failed"`
		} `json:"status"`
	}
	if json.Unmarshal(response, &result) != nil || len(result.Status) == 0 {
		return false, errors.New("OpenObserve ingestion response omitted per-stream status")
	}
	successful, failed := 0, 0
	for _, entry := range result.Status {
		successful += entry.Successful
		failed += entry.Failed
	}
	if failed > 0 || successful != len(records) {
		err := fmt.Errorf("OpenObserve ingestion acknowledged %d successful and %d failed records; expected %d", successful, failed, len(records))
		if successful > 0 {
			return true, partialIngestError{err}
		}
		return true, err
	}
	return true, nil
}

type (
	partialIngestError struct{ error }
	projectedIdentity  struct {
		ID         string
		Generation int64
	}
)

func (s *service) verifyProjectionBatch(ctx context.Context, hour time.Time, offset, limit int) ([]projectedIdentity, int, bool, error) {
	result, err := s.openObserveSearch(ctx, "SELECT projection_id, generation FROM \""+s.projectionStreamName()+"\" ORDER BY projection_id", hour, hour.Add(time.Hour), offset, limit, "")
	if err != nil {
		return nil, 0, false, err
	}
	out := make([]projectedIdentity, 0, len(result.Hits))
	for _, row := range result.Hits {
		out = append(out, projectedIdentity{stringValue(row["projection_id"]), int64Value(row["generation"])})
	}
	return out, result.Total, result.Partial, nil
}

func int64Value(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

type projectionSnapshotRecord struct {
	ProjectionID      string         `json:"projection_id"`
	CanonicalRevision string         `json:"canonical_revision"`
	Replacement       map[string]any `json:"replacement"`
}
type projectionSnapshotChunk struct {
	SchemaVersion int                        `json:"schema_version"`
	SnapshotID    string                     `json:"snapshot_id"`
	Generation    int64                      `json:"generation"`
	Index         int                        `json:"index"`
	Records       []projectionSnapshotRecord `json:"records"`
}

func projectionSnapshotObject(hour time.Time, snapshotID string, index int) string {
	return fmt.Sprintf("measurement-projection-snapshots/%s/%s/%s/%06d.json", projectionStream, hour.UTC().Format("20060102T15"), snapshotID, index)
}

func (s *service) projectionSnapshotObject(hour time.Time, snapshotID string, index int) string {
	return fmt.Sprintf("measurement-projection-snapshots/%s/%s/%s/%06d.json", s.projectionStreamName(), hour.UTC().Format("20060102T15"), snapshotID, index)
}

func (s *service) freezeProjectionSnapshot(ctx context.Context, hour time.Time, generation int64, snapshotID string) (int, int, error) {
	type candidate struct{ kind, id string }
	var candidates []candidate
	for _, kind := range []string{"usage", "execution", "judge"} {
		objects, err := s.listObjects(ctx, "measurements/"+kind)
		if err != nil {
			return 0, 0, err
		}
		sort.Strings(objects)
		for _, id := range objects {
			candidates = append(candidates, candidate{kind, id})
		}
	}
	chunk := projectionSnapshotChunk{SchemaVersion: 1, SnapshotID: snapshotID, Generation: generation, Index: 0, Records: []projectionSnapshotRecord{}}
	chunks, count := 0, 0
	flush := func() error {
		if len(chunk.Records) == 0 {
			return nil
		}
		if _, err := s.writeObject(ctx, s.projectionSnapshotObject(hour, snapshotID, chunk.Index), chunk, "*"); err != nil {
			return err
		}
		chunks++
		chunk.Index++
		chunk.Records = []projectionSnapshotRecord{}
		return nil
	}
	for _, candidate := range candidates {
		snapshot, ok, err := s.snapshotRecord(ctx, candidate.kind, candidate.id, hour, generation)
		if err != nil {
			return 0, 0, err
		}
		if !ok {
			continue
		}
		chunk.Records = append(chunk.Records, snapshot)
		count++
		if len(chunk.Records) == 500 {
			if err = flush(); err != nil {
				return 0, 0, err
			}
		}
	}
	if err := flush(); err != nil {
		return 0, 0, err
	}
	return chunks, count, nil
}

func (s *service) snapshotRecord(ctx context.Context, kind, objectID string, hour time.Time, generation int64) (projectionSnapshotRecord, bool, error) {
	object, err := s.readObject(ctx, objectID)
	if err != nil {
		return projectionSnapshotRecord{}, false, err
	}
	var measurement Measurement
	if json.Unmarshal(object.Data, &measurement) != nil {
		return projectionSnapshotRecord{}, false, fmt.Errorf("canonical measurement %s is malformed", objectID)
	}
	if measurement.OccurredAt.Before(hour) || !measurement.OccurredAt.Before(hour.Add(time.Hour)) {
		return projectionSnapshotRecord{}, false, nil
	}
	raw, _ := json.Marshal(measurement)
	var record map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&record)
	projectionID := kind + "/" + measurement.ID
	record["projection_id"], record["generation"], record["_timestamp"] = projectionID, generation, measurement.OccurredAt.UnixMicro()
	revision := object.Revision
	if kind == "usage" {
		if costObject, costErr := s.readObject(ctx, costPrefix+"/"+measurement.ID+".json"); costErr == nil {
			var cost CurrentCost
			if json.Unmarshal(costObject.Data, &cost) != nil {
				return projectionSnapshotRecord{}, false, fmt.Errorf("canonical cost %s is malformed", measurement.ID)
			}
			record["amount"], record["currency"], record["converted_amount"], record["display_currency"], record["cost_status"] = cost.Amount, cost.Currency, cost.ConvertedAmount, cost.DisplayCurrency, cost.Status
			record["amount_numerator"], record["amount_denominator"], record["converted_numerator"], record["converted_denominator"] = cost.AmountNumerator, cost.AmountDenominator, cost.ConvertedNumerator, cost.ConvertedDenominator
			revision += "/" + costObject.Revision
		} else if !errors.Is(costErr, errNotFound) {
			return projectionSnapshotRecord{}, false, costErr
		}
	}
	return projectionSnapshotRecord{projectionID, revision, record}, true, nil
}

func (s *service) projectionSnapshotBatch(ctx context.Context, manifest projectionManifest, cursor, limit int) ([]map[string]any, []string, bool, error) {
	if cursor >= manifest.SnapshotCount {
		return []map[string]any{}, []string{}, false, nil
	}
	index := cursor / 500
	object, err := s.readObject(ctx, s.projectionSnapshotObject(manifest.Hour, manifest.SnapshotID, index))
	if err != nil {
		return nil, nil, false, err
	}
	var chunk projectionSnapshotChunk
	if json.Unmarshal(object.Data, &chunk) != nil || chunk.SnapshotID != manifest.SnapshotID || chunk.Generation != manifest.Generation {
		return nil, nil, false, errors.New("projection snapshot chunk is invalid")
	}
	start := cursor % 500
	if start > len(chunk.Records) {
		return nil, nil, false, errors.New("projection snapshot cursor is invalid")
	}
	end := start + limit
	if end > len(chunk.Records) {
		end = len(chunk.Records)
	}
	records := make([]map[string]any, 0, end-start)
	ids := make([]string, 0, end-start)
	for _, record := range chunk.Records[start:end] {
		records = append(records, record.Replacement)
		ids = append(ids, record.ProjectionID)
	}
	return records, ids, cursor+len(records) < manifest.SnapshotCount, nil
}

func (s *service) completeProjectionOperations(ctx context.Context, hour time.Time, generation int64) error {
	ids, err := s.listObjects(ctx, operationPrefix)
	if err != nil {
		return err
	}
	for _, id := range ids {
		object, err := s.readObject(ctx, id)
		if err != nil {
			return err
		}
		var operation mutationOperation
		if json.Unmarshal(object.Data, &operation) != nil || !operation.AffectedHour.Equal(hour) || operation.Generation > generation || operation.Phase != "projection_pending" {
			continue
		}
		operation.Phase, operation.Error, operation.UpdatedAt = "complete", "", time.Now().UTC()
		if _, err = s.writeObject(ctx, id, operation, object.Revision); err != nil && !errors.Is(err, errPrecondition) {
			return err
		}
	}
	return nil
}
