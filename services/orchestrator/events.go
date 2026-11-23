package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	"opensoftwarefactory/orchestrator/pipeline"
)

const (
	workflowEventStartedSubject         = "factory.workflow.started"
	workflowEventFinishedSubject        = "factory.workflow.finished"
	workflowEventPrefix                 = "workflow-events"
	workflowEventPublishedPrefix        = "workflow-event-publications"
	workflowOpenPrefix                  = "workflow-events-open"
	executionMeasurementPrefix          = "measurement-deliveries/execution"
	executionMeasurementDeliveredPrefix = "measurement-delivery-receipts/execution"
	workflowExecutionPrefix             = "workflow-executions"
)

var workflowEventNamespace = uuid.MustParse("17972ed2-7116-5c0a-8834-73ccf87a15ae")

type WorkflowEvent struct {
	SchemaVersion     int       `json:"schema_version"`
	ID                string    `json:"id"`
	Type              string    `json:"type"`
	RunID             string    `json:"run_id"`
	ExecutionID       string    `json:"execution_id"`
	ParentExecutionID string    `json:"parent_execution_id,omitempty"`
	Workflow          string    `json:"workflow"`
	WorkflowVersion   int       `json:"workflow_version"`
	TraceID           string    `json:"trace_id,omitempty"`
	OccurredAt        time.Time `json:"occurred_at"`
	Status            string    `json:"status"`
}

type workflowEventPublication struct {
	Event       WorkflowEvent `json:"event"`
	PublishedAt *time.Time    `json:"published_at,omitempty"`
}

type knownOpenExecution struct {
	WorkflowID      string    `json:"workflow_id"`
	ExecutionID     string    `json:"execution_id"`
	RunID           string    `json:"run_id"`
	Workflow        string    `json:"workflow"`
	WorkflowVersion int       `json:"workflow_version"`
	AcceptedAt      time.Time `json:"accepted_at"`
}

type executionMeasurementDelivery struct {
	Measurement map[string]any `json:"measurement"`
	DeliveredAt *time.Time     `json:"delivered_at,omitempty"`
}

func workflowEventID(executionID, eventType string) string {
	return uuid.NewSHA1(workflowEventNamespace, []byte(executionID+"\x00"+eventType)).String()
}

type eventService struct {
	temporal      eventTemporal
	store         eventStore
	js            eventPublisher
	interval      time.Duration
	log           *slog.Logger
	http          *http.Client
	evaluationURL string
	mu            sync.RWMutex
	health        eventHealth
}
type eventTemporal interface {
	ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
	DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

type eventStore interface {
	pipeline.Store
	pipeline.ConditionalStore
	List(context.Context, string) ([]string, []string, error)
	Delete(context.Context, string) error
}
type eventPublisher interface {
	Publish(context.Context, string, []byte, ...jetstream.PublishOpt) (*jetstream.PubAck, error)
}

type eventHealth struct {
	Pending       int       `json:"pending"`
	OldestPending time.Time `json:"oldest_pending,omitempty"`
	LastProgress  time.Time `json:"last_progress,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

func natsOptionsFromEnv() ([]nats.Option, error) {
	user, password := os.Getenv("NATS_USER"), os.Getenv("NATS_PASSWORD")
	if (user == "") != (password == "") {
		return nil, errors.New("NATS_USER and NATS_PASSWORD must be configured together")
	}
	if user == "" {
		return nil, nil
	}
	return []nats.Option{nats.UserInfo(user, password)}, nil
}

func connectNATS() (*nats.Conn, jetstream.JetStream, error) {
	opts, err := natsOptionsFromEnv()
	if err != nil {
		return nil, nil, err
	}
	nc, err := nats.Connect(envString("NATS_URL", nats.DefaultURL), opts...)
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensureWorkflowEventStream(ctx, js, envString("NATS_STREAM", "FACTORY_WORKFLOW_EVENTS")); err != nil {
		nc.Close()
		return nil, nil, err
	}
	return nc, js, nil
}

func ensureWorkflowEventStream(ctx context.Context, js jetstream.JetStream, name string) error {
	_, err := js.CreateOrUpdateStream(ctx, workflowEventStreamConfig(name))
	return err
}

func workflowEventStreamConfig(name string) jetstream.StreamConfig {
	return jetstream.StreamConfig{Name: name, Subjects: []string{workflowEventStartedSubject, workflowEventFinishedSubject}, Retention: jetstream.LimitsPolicy, Discard: jetstream.DiscardNew, Storage: jetstream.FileStorage, Replicas: 1, MaxConsumers: -1, MaxMsgs: -1, MaxBytes: -1, MaxMsgSize: -1}
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func (s *eventService) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		s.maintenanceCycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *eventService) maintenanceCycle(ctx context.Context) {
	actions := []struct {
		name string
		run  func(context.Context) error
	}{{"accepted run metadata recovery", s.recoverRunStarts}, {"pending workflow event publication", s.publishPending}, {"pending execution measurement delivery", s.deliverMeasurements}, {"workflow event reconciliation", s.reconcileOnce}}
	healthy := true
	for _, action := range actions {
		if err := action.run(ctx); err != nil {
			healthy = false
			s.setError(err)
			if s.log != nil {
				s.log.Warn(action.name+" failed", "error", err)
			}
		}
	}
	if healthy {
		s.mu.Lock()
		s.health.LastError = ""
		s.mu.Unlock()
	}
}

func (s *eventService) recoverRunStarts(ctx context.Context) error {
	_, ids, err := s.store.List(ctx, runStartObligationPrefix)
	if err != nil {
		return err
	}
	if len(ids) > 200 {
		ids = ids[:200]
	}
	for _, id := range ids {
		data, err := s.store.Get(ctx, id)
		if err != nil {
			return err
		}
		var obligation runStartObligation
		if json.Unmarshal(data, &obligation) != nil {
			continue
		}
		desc, err := s.temporal.DescribeWorkflowExecution(ctx, obligation.Run.TraceID, "")
		if err != nil {
			continue
		}
		exec := desc.GetWorkflowExecutionInfo().GetExecution()
		if exec == nil || exec.GetRunId() == "" {
			continue
		}
		obligation.Run.ExecutionID = exec.GetRunId()
		obligation.Metadata.ExecutionID = exec.GetRunId()
		record, _ := json.MarshalIndent(obligation.Run, "", "  ")
		metadata, _ := json.MarshalIndent(obligation.Metadata, "", "  ")
		prefix := pipeline.RunPrefix(obligation.Run.TraceID)
		if err = s.store.Put(ctx, pipeline.ArtifactID(prefix, "run.json"), record, pipeline.Meta{Creator: obligation.Run.StartedBy, TraceID: obligation.Run.TraceID}); err != nil {
			return err
		}
		if err = s.store.Put(ctx, pipeline.ArtifactID(prefix, pipeline.MetadataFile), metadata, pipeline.Meta{Creator: obligation.Run.StartedBy, TraceID: obligation.Run.TraceID}); err != nil {
			return err
		}
		if err = s.acceptExecution(ctx, knownOpenExecution{WorkflowID: obligation.Run.TraceID, ExecutionID: exec.GetRunId(), RunID: obligation.Run.TraceID, Workflow: obligation.Run.Workflow, WorkflowVersion: obligation.Run.WorkflowVersion, AcceptedAt: obligation.Run.Started}); err != nil {
			return err
		}
		if err = s.store.Delete(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *eventService) acceptExecution(ctx context.Context, accepted knownOpenExecution) error {
	data, _ := json.MarshalIndent(accepted, "", "  ")
	if _, err := s.store.PutIfAbsent(ctx, workflowOpenPrefix+"/"+accepted.ExecutionID+".json", data, pipeline.Meta{Creator: "orchestrator"}); err != nil && !errors.Is(err, pipeline.ErrPreconditionFailed) {
		return err
	}
	desc, err := s.temporal.DescribeWorkflowExecution(ctx, accepted.WorkflowID, accepted.ExecutionID)
	if err != nil {
		return err
	}
	return s.persistExecution(ctx, desc.GetWorkflowExecutionInfo())
}

func (s *eventService) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health.LastError = err.Error()
}

func (s *eventService) reconcileOnce(ctx context.Context) error {
	if err := s.reconcileKnownOpen(ctx); err != nil {
		return err
	}
	var token []byte
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		from := time.Now().UTC().Add(-15 * time.Minute).Format(time.RFC3339)
		page, err := s.temporal.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{Query: fmt.Sprintf("WorkflowType = '%s' AND StartTime >= '%s'", pipeline.WorkflowName, from), PageSize: 200, NextPageToken: token})
		if err != nil {
			return err
		}
		for _, info := range page.GetExecutions() {
			if err := s.persistExecution(ctx, info); err != nil {
				return err
			}
		}
		token = page.GetNextPageToken()
		if len(token) == 0 {
			break
		}
	}
	return nil
}

func (s *eventService) reconcileKnownOpen(ctx context.Context) error {
	_, ids, err := s.store.List(ctx, workflowOpenPrefix)
	if err != nil {
		return err
	}
	if len(ids) > 500 {
		ids = ids[:500]
	}
	for _, id := range ids {
		data, err := s.store.Get(ctx, id)
		if err != nil {
			return err
		}
		var open knownOpenExecution
		if json.Unmarshal(data, &open) != nil {
			continue
		}
		desc, err := s.temporal.DescribeWorkflowExecution(ctx, open.WorkflowID, open.ExecutionID)
		if err != nil {
			continue
		}
		if err = s.persistExecution(ctx, desc.GetWorkflowExecutionInfo()); err != nil {
			return err
		}
		if desc.GetWorkflowExecutionInfo().GetCloseTime() != nil {
			if err = s.store.Delete(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *eventService) persistExecution(ctx context.Context, info *workflowpb.WorkflowExecutionInfo) error {
	if err := s.persistExecutionIdentity(ctx, info); err != nil {
		return err
	}
	for _, event := range eventsForExecution(info) {
		if rootBlockedEligible(event) {
			data, readErr := s.store.Get(ctx, pipeline.ArtifactID(pipeline.RunPrefix(event.RunID), pipeline.MetadataFile))
			if readErr != nil {
				return fmt.Errorf("terminal outcome evidence for %s is unavailable: %w", event.ExecutionID, readErr)
			}
			var stored struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(data, &stored); err != nil {
				return fmt.Errorf("terminal outcome evidence for %s is malformed: %w", event.ExecutionID, err)
			}
			switch stored.Status {
			case pipeline.RunBlocked:
				event.Status = pipeline.RunBlocked
			case pipeline.RunCompleted:
			default:
				return fmt.Errorf("terminal outcome evidence for %s conflicts with Temporal completion: %q", event.ExecutionID, stored.Status)
			}
		}
		if _, publishedErr := s.store.Get(ctx, workflowEventPublishedPrefix+"/"+event.ID+".json"); publishedErr == nil {
			continue
		} else if !errors.Is(publishedErr, pipeline.ErrNotFound) {
			return publishedErr
		}
		data, _ := json.MarshalIndent(workflowEventPublication{Event: event}, "", "  ")
		if _, err := s.store.PutIfAbsent(ctx, workflowEventPrefix+"/"+event.ID+".json", data, pipeline.Meta{Creator: "orchestrator"}); err != nil && !errors.Is(err, pipeline.ErrPreconditionFailed) {
			return err
		}
	}
	if info.GetCloseTime() != nil {
		return s.persistExecutionMeasurement(ctx, info)
	}
	return nil
}

func (s *eventService) persistExecutionIdentity(ctx context.Context, info *workflowpb.WorkflowExecutionInfo) error {
	events := eventsForExecution(info)
	if len(events) == 0 {
		return nil
	}
	event := events[0]
	prefix := ""
	_ = converter.GetDefaultDataConverter().FromPayload(info.GetMemo().GetFields()["artifact_prefix"], &prefix)
	if prefix == "" {
		prefix = pipeline.RunPrefix(event.RunID)
	}
	record := map[string]any{"schema_version": 1, "execution_id": event.ExecutionID, "run_id": event.RunID, "workflow": event.Workflow, "workflow_version": event.WorkflowVersion, "artifact_prefix": prefix, "started_at": event.OccurredAt.Format(time.RFC3339Nano), "status": "running"}
	if event.ParentExecutionID != "" {
		record["parent_execution_id"] = event.ParentExecutionID
	}
	if info.GetCloseTime() != nil {
		record["closed_at"] = info.GetCloseTime().AsTime().Format(time.RFC3339Nano)
		record["status"] = workflowOutcome(info.GetStatus())
	}
	data, _ := json.MarshalIndent(record, "", "  ")
	return s.store.Put(ctx, workflowExecutionPrefix+"/"+event.ExecutionID+".json", data, pipeline.Meta{Creator: "orchestrator"})
}

func rootBlockedEligible(event WorkflowEvent) bool {
	return event.Type == "workflow.finished" && event.Status == "completed" && event.ParentExecutionID == ""
}

func eventsForExecution(info *workflowpb.WorkflowExecutionInfo) []WorkflowEvent {
	exec := info.GetExecution()
	if exec == nil || exec.GetRunId() == "" {
		return nil
	}
	workflowName, traceID := "", exec.GetWorkflowId()
	version := 0
	fields := info.GetMemo().GetFields()
	_ = converter.GetDefaultDataConverter().FromPayload(fields["workflow"], &workflowName)
	_ = converter.GetDefaultDataConverter().FromPayload(fields["workflow_version"], &version)
	_ = converter.GetDefaultDataConverter().FromPayload(fields["trace_id"], &traceID)
	if workflowName == "" || version < 1 {
		return nil
	}
	parent := ""
	if p := info.GetParentExecution(); p != nil {
		parent = p.GetRunId()
	}
	base := WorkflowEvent{
		SchemaVersion: 1, RunID: traceID, ExecutionID: exec.GetRunId(), ParentExecutionID: parent,
		Workflow: workflowName, WorkflowVersion: version, TraceID: traceID,
	}
	started := base
	started.Type = "workflow.started"
	started.Status = "started"
	started.OccurredAt = info.GetStartTime().AsTime()
	started.ID = workflowEventID(started.ExecutionID, started.Type)
	out := []WorkflowEvent{started}
	if closeTime := info.GetCloseTime(); closeTime != nil {
		finished := base
		finished.Type = "workflow.finished"
		finished.OccurredAt = closeTime.AsTime()
		finished.Status = workflowOutcome(info.GetStatus())
		finished.ID = workflowEventID(finished.ExecutionID, finished.Type)
		out = append(out, finished)
	}
	return out
}

func workflowOutcome(status enumspb.WorkflowExecutionStatus) string {
	switch status {
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return "completed"
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED:
		return "failed"
	case enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return "cancelled"
	case enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		return "terminated"
	case enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return "timed_out"
	case enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
		return "continued_as_new"
	default:
		return strings.ToLower(status.String())
	}
}

func (s *eventService) persistExecutionMeasurement(ctx context.Context, info *workflowpb.WorkflowExecutionInfo) error {
	measurement, ok := executionMeasurementForInfo(info)
	if !ok {
		return nil
	}
	eventID, _ := measurement["execution_id"].(string)
	if _, receiptErr := s.store.Get(ctx, executionMeasurementDeliveredPrefix+"/"+eventID+".json"); receiptErr == nil {
		return nil
	} else if !errors.Is(receiptErr, pipeline.ErrNotFound) {
		return receiptErr
	}
	delivery := executionMeasurementDelivery{Measurement: measurement}
	data, _ := json.MarshalIndent(delivery, "", "  ")
	_, err := s.store.PutIfAbsent(ctx, executionMeasurementPrefix+"/"+eventID+".json", data, pipeline.Meta{Creator: "orchestrator"})
	if errors.Is(err, pipeline.ErrPreconditionFailed) {
		return nil
	}
	return err
}

func executionMeasurementForInfo(info *workflowpb.WorkflowExecutionInfo) (map[string]any, bool) {
	events := eventsForExecution(info)
	if len(events) < 2 {
		return nil, false
	}
	event := events[len(events)-1]
	start, close := info.GetStartTime().AsTime(), info.GetCloseTime().AsTime()
	duration := close.Sub(start).Milliseconds()
	if duration < 0 {
		duration = 0
	}
	measurement := map[string]any{"schema_version": 1, "id": event.ExecutionID, "kind": "execution", "run_id": event.RunID, "execution_id": event.ExecutionID, "workflow": event.Workflow, "workflow_version": event.WorkflowVersion, "trace_id": event.TraceID, "occurred_at": close.Format(time.RFC3339Nano), "duration_ms": duration, "status": "reported", "missing": []string{}}
	if event.ParentExecutionID != "" {
		measurement["parent_execution_id"] = event.ParentExecutionID
	}
	return measurement, true
}

func (s *eventService) deliverMeasurements(ctx context.Context) error {
	_, ids, err := s.store.List(ctx, executionMeasurementPrefix)
	if err != nil {
		return err
	}
	if len(ids) > 500 {
		ids = ids[:500]
	}
	client := s.http
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimRight(s.evaluationURL, "/")
	for _, id := range ids {
		data, err := s.store.Get(ctx, id)
		if err != nil {
			return err
		}
		var delivery executionMeasurementDelivery
		if json.Unmarshal(data, &delivery) != nil || delivery.DeliveredAt != nil {
			continue
		}
		measurementID, _ := delivery.Measurement["id"].(string)
		receiptID := executionMeasurementDeliveredPrefix + "/" + measurementID + ".json"
		if _, receiptErr := s.store.Get(ctx, receiptID); receiptErr == nil {
			if err = s.store.Delete(ctx, id); err != nil {
				return err
			}
			continue
		} else if !errors.Is(receiptErr, pipeline.ErrNotFound) {
			return receiptErr
		}
		payload, _ := json.Marshal(delivery.Measurement)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/api/measurements/"+url.PathEscape(measurementID), bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-None-Match", "*")
		resp, err := client.Do(req)
		if err != nil {
			matched, verifyErr := verifyStoredMeasurement(ctx, client, base, measurementID, delivery.Measurement)
			if verifyErr == nil && matched {
				if completeErr := s.completeMeasurementDelivery(ctx, id, measurementID); completeErr != nil {
					return completeErr
				}
				continue
			}
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusPreconditionFailed {
			matched, verifyErr := verifyStoredMeasurement(ctx, client, base, measurementID, delivery.Measurement)
			if verifyErr != nil {
				return verifyErr
			}
			if !matched {
				return fmt.Errorf("evaluation measurement %s conflicts with the durable execution identity", measurementID)
			}
		} else if resp.StatusCode/100 != 2 {
			return fmt.Errorf("evaluation measurement delivery answered %s: %s", resp.Status, bytes.TrimSpace(body))
		}
		if err = s.completeMeasurementDelivery(ctx, id, measurementID); err != nil {
			return err
		}
	}
	return nil
}

func (s *eventService) completeMeasurementDelivery(ctx context.Context, pendingID, measurementID string) error {
	receipt, _ := json.MarshalIndent(map[string]any{"schema_version": 1, "measurement_id": measurementID, "delivered_at": time.Now().UTC().Format(time.RFC3339Nano)}, "", "  ")
	if _, err := s.store.PutIfAbsent(ctx, executionMeasurementDeliveredPrefix+"/"+measurementID+".json", receipt, pipeline.Meta{Creator: "orchestrator"}); err != nil && !errors.Is(err, pipeline.ErrPreconditionFailed) {
		return err
	}
	return s.store.Delete(ctx, pendingID)
}

func verifyStoredMeasurement(ctx context.Context, client *http.Client, base, id string, expected map[string]any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/measurements/"+url.PathEscape(id), nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("verifying evaluation measurement %s: %s", id, resp.Status)
	}
	var actual, normalized map[string]any
	if err = json.Unmarshal(data, &actual); err != nil {
		return false, err
	}
	encoded, _ := json.Marshal(expected)
	_ = json.Unmarshal(encoded, &normalized)
	for key, want := range normalized {
		if key == "occurred_at" {
			equivalent, parseErr := equivalentVisibilityTimestamp(want, actual[key])
			if parseErr != nil {
				return false, parseErr
			}
			if !equivalent {
				return false, nil
			}
			continue
		}
		if !reflect.DeepEqual(actual[key], want) {
			return false, nil
		}
	}
	return true, nil
}

func equivalentVisibilityTimestamp(expected, actual any) (bool, error) {
	expectedText, expectedOK := expected.(string)
	actualText, actualOK := actual.(string)
	if !expectedOK || !actualOK {
		return false, nil
	}
	expectedTime, err := time.Parse(time.RFC3339Nano, expectedText)
	if err != nil {
		return false, fmt.Errorf("parsing pending occurrence time: %w", err)
	}
	actualTime, err := time.Parse(time.RFC3339Nano, actualText)
	if err != nil {
		return false, fmt.Errorf("parsing stored occurrence time: %w", err)
	}
	return expectedTime.UTC().Truncate(time.Microsecond).Equal(actualTime.UTC().Truncate(time.Microsecond)), nil
}

func (s *eventService) publishPending(ctx context.Context) error {
	_, ids, err := s.store.List(ctx, workflowEventPrefix)
	if err != nil {
		return err
	}
	if len(ids) > 500 {
		ids = ids[:500]
	}
	s.mu.Lock()
	s.health.Pending, s.health.OldestPending = 0, time.Time{}
	s.mu.Unlock()
	for _, id := range ids {
		data, err := s.store.Get(ctx, id)
		if err != nil {
			return err
		}
		var publication workflowEventPublication
		if err := json.Unmarshal(data, &publication); err != nil {
			return fmt.Errorf("reading pending event %s: %w", id, err)
		}
		event := publication.Event
		publishedID := workflowEventPublishedPrefix + "/" + event.ID + ".json"
		if _, publishedErr := s.store.Get(ctx, publishedID); publishedErr == nil {
			if err = s.store.Delete(ctx, id); err != nil {
				return err
			}
			continue
		} else if !errors.Is(publishedErr, pipeline.ErrNotFound) {
			return publishedErr
		}
		s.mu.Lock()
		s.health.Pending++
		if s.health.OldestPending.IsZero() || event.OccurredAt.Before(s.health.OldestPending) {
			s.health.OldestPending = event.OccurredAt
		}
		s.mu.Unlock()
		subject := workflowEventFinishedSubject
		if event.Type == "workflow.started" {
			subject = workflowEventStartedSubject
		}
		payload, _ := json.Marshal(event)
		if _, err := s.js.Publish(ctx, subject, payload, jetstream.WithMsgID(event.ID)); err != nil {
			return err
		}
		now := time.Now().UTC()
		publication.PublishedAt = &now
		completed, _ := json.MarshalIndent(publication, "", "  ")
		if _, err := s.store.PutIfAbsent(ctx, publishedID, completed, pipeline.Meta{Creator: "orchestrator"}); err != nil && !errors.Is(err, pipeline.ErrPreconditionFailed) {
			return err
		}
		if err := s.store.Delete(ctx, id); err != nil {
			return err
		}
		s.mu.Lock()
		s.health.LastProgress, s.health.LastError = time.Now().UTC(), ""
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.health.Pending, s.health.OldestPending = 0, time.Time{}
	s.mu.Unlock()
	return nil
}
