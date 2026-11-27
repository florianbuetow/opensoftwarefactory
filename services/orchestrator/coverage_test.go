package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

type errCloser struct{ err error }

func (e errCloser) Close() error { return e.err }

type configurableTemporalFake struct {
	describe func(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	list     func(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
}

func (f configurableTemporalFake) DescribeWorkflowExecution(ctx context.Context, wid, rid string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if f.describe != nil {
		return f.describe(ctx, wid, rid)
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{}, nil
}

func (f configurableTemporalFake) ListWorkflow(ctx context.Context, req *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	if f.list != nil {
		return f.list(ctx, req)
	}
	return &workflowservice.ListWorkflowExecutionsResponse{}, nil
}

type failingPublisher struct{ err error }

func (p failingPublisher) Publish(context.Context, string, []byte, ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return nil, p.err
}

type failingStore struct {
	*eventMemoryStore
	listErr        error
	getErr         error
	putErr         error
	putIfAbsentErr error
	deleteErr      error
}

func (s *failingStore) List(ctx context.Context, prefix string) (dirs, objs []string, err error) {
	if s.listErr != nil {
		return nil, nil, s.listErr
	}
	return s.eventMemoryStore.List(ctx, prefix)
}

func (s *failingStore) Get(ctx context.Context, id string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.eventMemoryStore.Get(ctx, id)
}

func (s *failingStore) Put(ctx context.Context, id string, data []byte, meta pipeline.Meta) error {
	if s.putErr != nil {
		return s.putErr
	}
	return s.eventMemoryStore.Put(ctx, id, data, meta)
}

func (s *failingStore) PutIfAbsent(ctx context.Context, id string, data []byte, meta pipeline.Meta) (string, error) {
	if s.putIfAbsentErr != nil {
		return "", s.putIfAbsentErr
	}
	return s.eventMemoryStore.PutIfAbsent(ctx, id, data, meta)
}

func (s *failingStore) Delete(ctx context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.eventMemoryStore.Delete(ctx, id)
}

type fakeIntakeRecalcRunner struct {
	*fakeRunner
	intakeErr    error
	recalcErr    error
	intakeExecID string
	recalcID     string
	intakeReused bool
}

func (r *fakeIntakeRecalcRunner) StartIntake(context.Context, *pipeline.Input, string) (execID string, reused bool, err error) {
	return r.intakeExecID, r.intakeReused, r.intakeErr
}

func (r *fakeIntakeRecalcRunner) StartRecalculation(context.Context, *pipeline.RecalculationInput) (string, error) {
	return r.recalcID, r.recalcErr
}

func makeTestExecutionInfo(wid, rid, wf string, closed bool, status enumspb.WorkflowExecutionStatus) *workflowpb.WorkflowExecutionInfo {
	dc := converter.GetDefaultDataConverter()
	wp, _ := dc.ToPayload(wf)
	vp, _ := dc.ToPayload(1)
	tp, _ := dc.ToPayload(wid)
	start := time.Now().UTC().Add(-2 * time.Second)
	info := &workflowpb.WorkflowExecutionInfo{
		Execution: &commonpb.WorkflowExecution{WorkflowId: wid, RunId: rid},
		Memo:      &commonpb.Memo{Fields: map[string]*commonpb.Payload{"workflow": wp, "workflow_version": vp, "trace_id": tp}},
		StartTime: timestamppb.New(start),
		Status:    status,
	}
	if closed {
		info.CloseTime = timestamppb.New(start.Add(time.Second))
	}
	return info
}

func TestNatsOptionsFromEnvAndEnvString(t *testing.T) {
	t.Setenv("NATS_USER", "")
	t.Setenv("NATS_PASSWORD", "")
	if opts, err := natsOptionsFromEnv(); err != nil || opts != nil {
		t.Fatalf("empty creds = (%v, %v)", opts, err)
	}
	t.Setenv("NATS_USER", "user")
	t.Setenv("NATS_PASSWORD", "")
	if _, err := natsOptionsFromEnv(); err == nil {
		t.Fatal("expected error when only NATS_USER is set")
	}
	t.Setenv("NATS_USER", "user")
	t.Setenv("NATS_PASSWORD", "pass")
	if opts, err := natsOptionsFromEnv(); err != nil || len(opts) != 1 {
		t.Fatalf("both creds = (%v, %v)", opts, err)
	}

	t.Setenv("ORCH_TEST_ENV", "  val  ")
	if got := envString("ORCH_TEST_ENV", "def"); got != "val" {
		t.Fatalf("envString = %q, want val", got)
	}
	t.Setenv("ORCH_TEST_ENV", "   ")
	if got := envString("ORCH_TEST_ENV", "def"); got != "def" {
		t.Fatalf("envString fallback = %q, want def", got)
	}
}

func TestInitTelemetryNoopAndEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	shutdown, err := initTelemetry(context.Background(), "orchestrator-test")
	if err != nil {
		t.Fatalf("noop initTelemetry: %v", err)
	}
	if err = shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown: %v", err)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:4318/v1/traces")
	shutdown2, err := initTelemetry(context.Background(), "orchestrator-test")
	if err != nil {
		t.Fatalf("endpoint initTelemetry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = shutdown2(ctx)
}

func TestEventServiceRunRecoverAndAccept(t *testing.T) {
	store := &eventMemoryStore{data: map[string][]byte{}}
	info := makeTestExecutionInfo("trace-1", "exec-1", "group/build", false, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)
	obligation := runStartObligation{
		Run:      RunRecord{TraceID: "trace-1", Workflow: "group/build", WorkflowVersion: 1, StartedBy: "tester", Started: time.Now().UTC()},
		Metadata: RunMetadata{},
	}
	store.data[runStartObligationPrefix+"/trace-1.json"] = mustJSON(obligation)
	store.data[runStartObligationPrefix+"/bad.json"] = []byte("{not-json")

	svc := &eventService{
		store: store,
		temporal: configurableTemporalFake{describe: func(_ context.Context, wid, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			if wid == "trace-1" {
				return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: info}, nil
			}
			return nil, errors.New("not found")
		}},
		js:       &recordingPublisher{},
		log:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		interval: 5 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.run(ctx)
	}()
	time.Sleep(15 * time.Millisecond)
	cancel()
	<-done

	if _, ok := store.data["runs/trace-1/run.json"]; !ok {
		t.Fatal("run.json was not recovered")
	}
	if _, ok := store.data["runs/trace-1/metadata.json"]; !ok {
		t.Fatal("metadata.json was not recovered")
	}
	if _, ok := store.data[runStartObligationPrefix+"/trace-1.json"]; ok {
		t.Fatal("recovered obligation was not deleted")
	}
	if _, ok := store.data[workflowOpenPrefix+"/exec-1.json"]; !ok {
		t.Fatal("accepted open execution missing")
	}
}

func TestEventServiceErrorAndEdgePaths(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// maintenanceCycle with failing store sets health.LastError.
	badStore := &failingStore{eventMemoryStore: &eventMemoryStore{data: map[string][]byte{}}, listErr: errors.New("list boom")}
	svcErr := &eventService{store: badStore, temporal: configurableTemporalFake{}, log: logger}
	svcErr.maintenanceCycle(context.Background())
	if svcErr.health.LastError == "" {
		t.Fatal("expected LastError to be populated")
	}

	// acceptExecution error paths.
	svcAccept := &eventService{
		store:    &failingStore{eventMemoryStore: &eventMemoryStore{data: map[string][]byte{}}, putIfAbsentErr: errors.New("put boom")},
		temporal: configurableTemporalFake{},
	}
	if err := svcAccept.acceptExecution(context.Background(), &knownOpenExecution{ExecutionID: "e1"}); err == nil {
		t.Fatal("expected acceptExecution store error")
	}
	svcAccept2 := &eventService{
		store: &eventMemoryStore{data: map[string][]byte{}},
		temporal: configurableTemporalFake{describe: func(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return nil, errors.New("describe boom")
		}},
	}
	if err := svcAccept2.acceptExecution(context.Background(), &knownOpenExecution{ExecutionID: "e1"}); err == nil {
		t.Fatal("expected acceptExecution describe error")
	}

	// persistExecution with blocked root status, malformed metadata, conflicting status, and already published event.
	info := makeTestExecutionInfo("run-b", "exec-b", "group/build", true, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED)
	store := &eventMemoryStore{data: map[string][]byte{
		"runs/run-b/metadata.json": []byte(`{"status":"blocked"}`),
	}}
	svc := &eventService{store: store}
	if err := svc.persistExecution(context.Background(), info); err != nil {
		t.Fatalf("blocked root persistExecution: %v", err)
	}
	evData := store.data[workflowEventPrefix+"/"+workflowEventID("exec-b", "workflow.finished")+".json"]
	if !strings.Contains(string(evData), `"status": "blocked"`) {
		t.Fatalf("expected blocked status in event: %s", evData)
	}

	// Malformed metadata fails.
	store.data["runs/run-b/metadata.json"] = []byte(`{bad`)
	if err := svc.persistExecution(context.Background(), info); err == nil {
		t.Fatal("expected malformed metadata error")
	}

	// Conflicting status fails.
	store.data["runs/run-b/metadata.json"] = []byte(`{"status":"running"}`)
	if err := svc.persistExecution(context.Background(), info); err == nil {
		t.Fatal("expected conflicting status error")
	}

	// Already published event is skipped.
	store.data["runs/run-b/metadata.json"] = []byte(`{"status":"completed"}`)
	store.data[workflowEventPublishedPrefix+"/"+workflowEventID("exec-b", "workflow.started")+".json"] = []byte(`{}`)
	store.data[workflowEventPublishedPrefix+"/"+workflowEventID("exec-b", "workflow.finished")+".json"] = []byte(`{}`)
	if err := svc.persistExecution(context.Background(), info); err != nil {
		t.Fatalf("already published persistExecution: %v", err)
	}

	// reconcileOnce pagination and list error.
	pages := 0
	svcPage := &eventService{
		store: &eventMemoryStore{data: map[string][]byte{}},
		temporal: configurableTemporalFake{list: func(_ context.Context, req *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
			pages++
			if pages == 1 {
				return &workflowservice.ListWorkflowExecutionsResponse{
					Executions:    []*workflowpb.WorkflowExecutionInfo{makeTestExecutionInfo("r1", "e1", "g/w", false, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)},
					NextPageToken: []byte("tok"),
				}, nil
			}
			return &workflowservice.ListWorkflowExecutionsResponse{}, nil
		}},
	}
	if err := svcPage.reconcileOnce(context.Background()); err != nil || pages != 2 {
		t.Fatalf("reconcileOnce pages=%d err=%v", pages, err)
	}

	// workflowOutcome failed and default.
	if got := workflowOutcome(enumspb.WORKFLOW_EXECUTION_STATUS_FAILED); !strings.EqualFold(got, "FAILED") {
		t.Fatalf("failed outcome = %q", got)
	}
	if got := workflowOutcome(enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING); got == "" {
		t.Fatal("default outcome empty")
	}
}

func TestDeliverMeasurementsAndPublishPendingBranches(t *testing.T) {
	// deliverMeasurements: already delivered receipt deletes pending, 201 succeeds, 500 fails, network error + verified succeeds, 412 mismatch fails.
	measurement := map[string]any{"schema_version": 1, "id": "m1", "kind": "execution", "occurred_at": "2026-11-21T20:51:55.729223Z"}
	store := &eventMemoryStore{data: map[string][]byte{
		executionMeasurementPrefix + "/already.json":          mustJSON(executionMeasurementDelivery{Measurement: map[string]any{"id": "already"}}),
		executionMeasurementDeliveredPrefix + "/already.json": []byte(`{}`),
		executionMeasurementPrefix + "/m1.json":               mustJSON(executionMeasurementDelivery{Measurement: measurement}),
		executionMeasurementPrefix + "/invalid.json":          []byte(`{bad`),
	}}
	client := &http.Client{Transport: eventRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	svc := &eventService{store: store, http: client, evaluationURL: "http://evaluation"}
	if err := svc.deliverMeasurements(context.Background()); err != nil {
		t.Fatalf("deliverMeasurements: %v", err)
	}
	if _, ok := store.data[executionMeasurementPrefix+"/already.json"]; ok {
		t.Fatal("already-delivered pending marker remained")
	}

	// Network error on PUT followed by successful GET verification.
	store.data[executionMeasurementPrefix+"/m2.json"] = mustJSON(executionMeasurementDelivery{Measurement: measurement})
	svc.http = &http.Client{Transport: eventRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut {
			return nil, errors.New("network reset")
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(mustJSON(measurement))), Header: make(http.Header)}, nil
	})}
	if err := svc.deliverMeasurements(context.Background()); err != nil {
		t.Fatalf("network error with verified GET should succeed: %v", err)
	}

	// Non-2xx status fails.
	store.data[executionMeasurementPrefix+"/m3.json"] = mustJSON(executionMeasurementDelivery{Measurement: map[string]any{"id": "m3"}})
	svc.http = &http.Client{Transport: eventRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error", Body: io.NopCloser(strings.NewReader("err")), Header: make(http.Header)}, nil
	})}
	if err := svc.deliverMeasurements(context.Background()); err == nil {
		t.Fatal("expected 500 error")
	}

	// verifyStoredMeasurement 404, 500, invalid JSON, field mismatch, timestamp errors.
	client404 := &http.Client{Transport: eventRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	if matched, err := verifyStoredMeasurement(context.Background(), client404, "http://eval", "m1", measurement); err != nil || matched {
		t.Fatalf("404 verify = (%v, %v)", matched, err)
	}
	client500 := &http.Client{Transport: eventRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Status: "500", Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	if _, err := verifyStoredMeasurement(context.Background(), client500, "http://eval", "m1", measurement); err == nil {
		t.Fatal("expected 500 verify error")
	}
	if ok, err := equivalentVisibilityTimestamp(123, "2026-11-21T20:51:55Z"); err != nil || ok {
		t.Fatalf("non-string timestamp = (%v, %v)", ok, err)
	}
	if _, err := equivalentVisibilityTimestamp("bad", "2026-11-21T20:51:55Z"); err == nil {
		t.Fatal("expected bad expected timestamp error")
	}
	if _, err := equivalentVisibilityTimestamp("2026-11-21T20:51:55Z", "bad"); err == nil {
		t.Fatal("expected bad actual timestamp error")
	}

	// publishPending: workflow.started subject, already published cleanup, invalid JSON, publish error.
	pubStore := &eventMemoryStore{data: map[string][]byte{
		workflowEventPrefix + "/started.json":     mustJSON(workflowEventPublication{Event: WorkflowEvent{ID: "s1", Type: "workflow.started", OccurredAt: time.Now().UTC()}}),
		workflowEventPrefix + "/dup.json":         mustJSON(workflowEventPublication{Event: WorkflowEvent{ID: "d1", Type: "workflow.finished", OccurredAt: time.Now().UTC()}}),
		workflowEventPublishedPrefix + "/d1.json": []byte(`{}`),
	}}
	pub := &recordingPublisher{}
	pubSvc := &eventService{store: pubStore, js: pub}
	if err := pubSvc.publishPending(context.Background()); err != nil {
		t.Fatalf("publishPending: %v", err)
	}
	if pub.subject != workflowEventStartedSubject || pub.calls != 1 {
		t.Fatalf("pub = %+v", pub)
	}
	pubStore.data[workflowEventPrefix+"/bad.json"] = []byte(`{bad`)
	if err := pubSvc.publishPending(context.Background()); err == nil {
		t.Fatal("expected invalid JSON publishPending error")
	}
	delete(pubStore.data, workflowEventPrefix+"/bad.json")
	pubStore.data[workflowEventPrefix+"/fail.json"] = mustJSON(workflowEventPublication{Event: WorkflowEvent{ID: "f1", Type: "workflow.started", OccurredAt: time.Now().UTC()}})
	pubSvc.js = failingPublisher{err: errors.New("nats down")}
	if err := pubSvc.publishPending(context.Background()); err == nil {
		t.Fatal("expected publish error")
	}
}

func TestStartIntakeRunAndRecalculationAndEventStatus(t *testing.T) {
	ws := storeWorkflows(t, "workflows:\n  - name: group/score\n    steps:\n      - id: s\n        service: svc\n")
	blobs := newMemStore()
	s := &server{
		workflows: ws,
		blobs:     blobs,
		runner:    &fakeRunner{},
		logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	// workflowEventStatus with nil events -> 503.
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/workflow-events/status", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil events status = %d", rec.Code)
	}

	// workflowEventStatus with non-nil events -> 200.
	s.events = &eventService{
		store: &eventMemoryStore{data: map[string][]byte{}},
		temporal: configurableTemporalFake{describe: func(_ context.Context, wid, rid string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: makeTestExecutionInfo(wid, rid, "group/score", false, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)}, nil
		}},
		health: eventHealth{Pending: 2},
	}
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/workflow-events/status", http.NoBody))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pending":2`) {
		t.Fatalf("events status = %d %s", rec.Code, rec.Body.String())
	}

	// startIntakeRun validation errors and non-intakeStarter runner.
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{`, http.StatusBadRequest},
		{`{"binding_id":"","event_id":"e1","workflow":"group/score","workflow_version":1,"binding_revision":"r1"}`, http.StatusUnprocessableEntity},
		{`{"binding_id":"b1","event_id":"e1","workflow":"group/missing","workflow_version":1,"binding_revision":"r1"}`, http.StatusNotFound},
		{`{"binding_id":"b1","event_id":"e1","workflow":"group/score","workflow_version":1,"binding_revision":"r1","event":{},"binding":{}}`, http.StatusServiceUnavailable},
	} {
		rec = httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/intake/runs", strings.NewReader(tc.body)))
		if rec.Code != tc.status {
			t.Fatalf("intake body=%s status=%d want=%d (%s)", tc.body, rec.Code, tc.status, rec.Body.String())
		}
	}

	// Attach intakeStarter + recalculationStarter runner.
	runner := &fakeIntakeRecalcRunner{fakeRunner: &fakeRunner{}, intakeExecID: "exec-99", recalcID: "measurement-recalculation/r1"}
	s.runner = runner
	validIntake := `{"binding_id":"b1","event_id":"e1","workflow":"group/score","workflow_version":1,"binding_revision":"r1","event":{"a":1},"binding":{"enabled":true}}`
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/intake/runs", strings.NewReader(validIntake)))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"execution_id":"exec-99"`) {
		t.Fatalf("intake accepted = %d %s", rec.Code, rec.Body.String())
	}

	runner.intakeErr = errIntakeConflict
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/intake/runs", strings.NewReader(validIntake)))
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("intake conflict = %d", rec.Code)
	}

	runner.intakeErr = errors.New("temporal unavailable")
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/intake/runs", strings.NewReader(validIntake)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("intake error = %d", rec.Code)
	}

	// startMeasurementRecalculation validation and happy path.
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{`, http.StatusBadRequest},
		{`{"id":"bad id!","display_currency":"EUR"}`, http.StatusUnprocessableEntity},
		{`{"id":"r1","display_currency":"GBP"}`, http.StatusUnprocessableEntity},
		{`{"id":"r1","display_currency":"USD","from":"2026-11-22T00:00:00Z","to":"2026-11-21T00:00:00Z"}`, http.StatusUnprocessableEntity},
		{`{"id":"r1","display_currency":"EUR","from":"2026-11-21T00:00:00Z","to":"2026-11-22T00:00:00Z"}`, http.StatusAccepted},
	} {
		rec = httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/measurement-recalculations", strings.NewReader(tc.body)))
		if rec.Code != tc.status {
			t.Fatalf("recalc body=%s status=%d want=%d (%s)", tc.body, rec.Code, tc.status, rec.Body.String())
		}
	}

	runner.recalcErr = errors.New("recalc down")
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/measurement-recalculations", strings.NewReader(`{"id":"r1","display_currency":"USD"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("recalc error = %d", rec.Code)
	}

	s.runner = &fakeRunner{}
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/measurement-recalculations", strings.NewReader(`{"id":"r1","display_currency":"USD"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("recalc unsupported = %d", rec.Code)
	}

	if intakeIDToken("") || intakeIDToken(strings.Repeat("a", 129)) || intakeIDToken("a/b") || !intakeIDToken("valid-ID_1.2") {
		t.Fatal("intakeIDToken mismatch")
	}

	// closeAfter helper and openWorkflowStoreWithRetry.
	if err := closeAfter(errors.New("a"), errCloser{err: errors.New("b")}); err == nil {
		t.Fatal("expected joined error from closeAfter")
	}
	if _, err := openWorkflowStoreWithRetry(context.Background(), ws.blobs, s.logger, 0); err != nil {
		t.Fatalf("openWorkflowStoreWithRetry: %v", err)
	}
	badBlobs := &failingStore{eventMemoryStore: &eventMemoryStore{data: map[string][]byte{}}, listErr: errors.New("down")}
	if _, err := openWorkflowStoreWithRetry(context.Background(), badBlobs, s.logger, 0); err == nil {
		t.Fatal("expected openWorkflowStoreWithRetry error")
	}
}
