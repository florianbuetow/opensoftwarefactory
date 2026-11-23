package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/types/known/timestamppb"
)

import "opensoftwarefactory/orchestrator/pipeline"

type eventMemoryStore struct {
	data map[string][]byte
	mu   sync.Mutex
}

func (s *eventMemoryStore) Put(_ context.Context, id string, data []byte, _ pipeline.Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[id] = append([]byte{}, data...)
	return nil
}

func (s *eventMemoryStore) Get(_ context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.data[id]
	if !ok {
		return nil, pipeline.ErrNotFound
	}
	return append([]byte{}, data...), nil
}

func (s *eventMemoryStore) GetWithRevision(ctx context.Context, id string) (data []byte, revision string, err error) {
	data, err = s.Get(ctx, id)
	return data, "r", err
}

func (s *eventMemoryStore) PutIfAbsent(_ context.Context, id string, data []byte, _ pipeline.Meta) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[id]; ok {
		return "", pipeline.ErrPreconditionFailed
	}
	s.data[id] = append([]byte{}, data...)
	return "r", nil
}

func (s *eventMemoryStore) PutIfMatch(_ context.Context, id string, data []byte, _ pipeline.Meta, _ string) (string, error) {
	return "", errors.New("unused")
}

func (s *eventMemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return nil
}

func (s *eventMemoryStore) List(_ context.Context, prefix string) (dirs, objects []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.data {
		if filepath.Dir(id) == prefix {
			ids = append(ids, id)
		}
	}
	return nil, ids, nil
}

type recordingPublisher struct {
	subject string
	payload []byte
	calls   uint64
}

func (p *recordingPublisher) Publish(_ context.Context, subject string, payload []byte, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	p.subject = subject
	p.payload = append([]byte{}, payload...)
	p.calls++
	return &jetstream.PubAck{Stream: "EVENTS", Sequence: p.calls}, nil
}

func TestPendingPublicationCompletesIndependentlyOfVisibilityScan(t *testing.T) {
	store := &eventMemoryStore{data: map[string][]byte{}}
	publisher := &recordingPublisher{}
	event := WorkflowEvent{SchemaVersion: 1, ID: workflowEventID("execution", "workflow.finished"), Type: "workflow.finished", RunID: "run", ExecutionID: "execution", Workflow: "group/build", WorkflowVersion: 2, OccurredAt: time.Now().UTC(), Status: "completed"}
	pending := workflowEventPrefix + "/" + event.ID + ".json"
	store.data[pending] = mustJSON(workflowEventPublication{Event: event})
	service := &eventService{store: store, js: publisher}
	if err := service.publishPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 1 || publisher.subject != workflowEventFinishedSubject {
		t.Fatalf("publisher=%+v", publisher)
	}
	if _, ok := store.data[pending]; ok {
		t.Fatal("pending obligation remained after ack")
	}
	if _, ok := store.data[workflowEventPublishedPrefix+"/"+event.ID+".json"]; !ok {
		t.Fatal("publication completion missing")
	}
	if err := service.publishPending(context.Background()); err != nil || publisher.calls != 1 {
		t.Fatalf("duplicate publish calls=%d err=%v", publisher.calls, err)
	}
}

func TestMissingRootOutcomeEvidenceBlocksTerminalPublication(t *testing.T) {
	dc := converter.GetDefaultDataConverter()
	wp, _ := dc.ToPayload("group/build")
	vp, _ := dc.ToPayload(2)
	tp, _ := dc.ToPayload("run")
	start := time.Now().UTC().Add(-time.Second)
	info := &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: "run", RunId: "execution"}, Memo: &commonpb.Memo{Fields: map[string]*commonpb.Payload{"workflow": wp, "workflow_version": vp, "trace_id": tp}}, StartTime: timestamppb.New(start), CloseTime: timestamppb.New(time.Now().UTC()), Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}
	store := &eventMemoryStore{data: map[string][]byte{}}
	service := &eventService{store: store}
	if err := service.persistExecution(context.Background(), info); err == nil {
		t.Fatal("missing root outcome evidence was accepted")
	}
	terminal := workflowEventPrefix + "/" + workflowEventID("execution", "workflow.finished") + ".json"
	if _, ok := store.data[terminal]; ok {
		t.Fatal("terminal event was permanently published without scoped outcome evidence")
	}
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

type eventTemporalFake struct {
	info *workflowpb.WorkflowExecutionInfo
}

func (f eventTemporalFake) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: f.info}, nil
}

func (f eventTemporalFake) ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	return &workflowservice.ListWorkflowExecutionsResponse{}, nil
}

func TestKnownOpenCompletionCreatesTerminalEventAndDurationObligation(t *testing.T) {
	dc := converter.GetDefaultDataConverter()
	wp, _ := dc.ToPayload("group/build")
	vp, _ := dc.ToPayload(2)
	tp, _ := dc.ToPayload("business-run")
	start := time.Now().UTC().Add(-2 * time.Second)
	info := &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: "business-run", RunId: "execution"}, Memo: &commonpb.Memo{Fields: map[string]*commonpb.Payload{"workflow": wp, "workflow_version": vp, "trace_id": tp}}, StartTime: timestamppb.New(start), CloseTime: timestamppb.New(start.Add(1500 * time.Millisecond)), Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}
	store := &eventMemoryStore{data: map[string][]byte{}}
	store.data["runs/business-run/metadata.json"] = []byte(`{"status":"completed"}`)
	openID := workflowOpenPrefix + "/execution.json"
	store.data[openID] = mustJSON(knownOpenExecution{WorkflowID: "business-run", ExecutionID: "execution", RunID: "business-run", Workflow: "group/build", WorkflowVersion: 2})
	service := &eventService{store: store, temporal: eventTemporalFake{info: info}}
	if err := service.reconcileKnownOpen(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.data[openID]; ok {
		t.Fatal("closed execution remained known-open")
	}
	if _, ok := store.data[executionMeasurementPrefix+"/execution.json"]; !ok {
		t.Fatal("duration delivery obligation missing")
	}
	if data, ok := store.data[workflowExecutionPrefix+"/execution.json"]; !ok || !strings.Contains(string(data), `"artifact_prefix": "runs/business-run"`) {
		t.Fatalf("execution identity record=%s", data)
	}
	if _, ok := store.data[workflowEventPrefix+"/"+workflowEventID("execution", "workflow.finished")+".json"]; !ok {
		t.Fatal("terminal event obligation missing")
	}
}

type eventRoundTrip func(*http.Request) (*http.Response, error)

func (f eventRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExecutionMeasurementCreateVerifiesMatchingConflict(t *testing.T) {
	measurement := map[string]any{"schema_version": 1, "id": "execution", "kind": "execution", "run_id": "run", "execution_id": "execution", "occurred_at": "2026-11-21T20:51:55.729223Z", "duration_ms": int64(10), "status": "reported", "missing": []string{}}
	canonical := map[string]any{}
	for key, value := range measurement {
		canonical[key] = value
	}
	canonical["occurred_at"] = "2026-11-21T20:51:55.729223189Z"
	pending := executionMeasurementPrefix + "/execution.json"
	store := &eventMemoryStore{data: map[string][]byte{pending: mustJSON(executionMeasurementDelivery{Measurement: measurement})}}
	calls := 0
	client := &http.Client{Transport: eventRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == http.MethodPut {
			if r.Header.Get("If-None-Match") != "*" {
				t.Fatalf("If-None-Match=%q", r.Header.Get("If-None-Match"))
			}
			return &http.Response{StatusCode: http.StatusPreconditionFailed, Status: "412 Precondition Failed", Body: io.NopCloser(strings.NewReader("conflict")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(string(mustJSON(canonical)))), Header: make(http.Header)}, nil
	})}
	service := &eventService{store: store, http: client, evaluationURL: "http://evaluation"}
	if err := service.deliverMeasurements(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	if _, ok := store.data[pending]; ok {
		t.Fatal("verified delivery obligation was not cleared")
	}
	if _, ok := store.data[executionMeasurementDeliveredPrefix+"/execution.json"]; !ok {
		t.Fatal("durable delivered receipt missing")
	}
}

func TestExecutionMeasurementVerificationRejectsDifferentTimestamp(t *testing.T) {
	expected := map[string]any{"id": "execution", "occurred_at": "2026-11-21T20:51:55.729223Z"}
	actual := map[string]any{"id": "execution", "occurred_at": "2026-11-21T20:51:55.729224Z"}
	client := &http.Client{Transport: eventRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "OK", Body: io.NopCloser(strings.NewReader(string(mustJSON(actual)))), Header: make(http.Header)}, nil
	})}
	matched, err := verifyStoredMeasurement(context.Background(), client, "http://evaluation", "execution", expected)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("different microsecond occurrence was accepted")
	}
}

func TestDeliveredExecutionMeasurementIsNotRequeued(t *testing.T) {
	dc := converter.GetDefaultDataConverter()
	wp, _ := dc.ToPayload("group/build")
	vp, _ := dc.ToPayload(1)
	tp, _ := dc.ToPayload("run")
	start := time.Now().UTC().Add(-time.Second)
	info := &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: "run", RunId: "execution"}, Memo: &commonpb.Memo{Fields: map[string]*commonpb.Payload{"workflow": wp, "workflow_version": vp, "trace_id": tp}}, StartTime: timestamppb.New(start), CloseTime: timestamppb.New(time.Now().UTC()), Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}
	store := &eventMemoryStore{data: map[string][]byte{executionMeasurementDeliveredPrefix + "/execution.json": []byte(`{"measurement_id":"execution"}`)}}
	service := &eventService{store: store}
	if err := service.persistExecutionMeasurement(context.Background(), info); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.data[executionMeasurementPrefix+"/execution.json"]; ok {
		t.Fatal("delivered measurement was requeued")
	}
}

func TestHealthyEmptyMaintenanceClearsTransientErrorWithoutFakeProgress(t *testing.T) {
	progress := time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)
	service := &eventService{store: &eventMemoryStore{data: map[string][]byte{}}, temporal: eventTemporalFake{}, health: eventHealth{LastError: "startup unavailable", LastProgress: progress}}
	service.maintenanceCycle(context.Background())
	if service.health.LastError != "" {
		t.Fatalf("last_error=%q", service.health.LastError)
	}
	if !service.health.LastProgress.Equal(progress) {
		t.Fatalf("last_progress changed without work: %s", service.health.LastProgress)
	}
}
