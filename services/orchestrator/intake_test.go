package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/types/known/timestamppb"
)

import "opensoftwarefactory/orchestrator/pipeline"

func TestDeterministicIntakeIdentityUsesBindingAndEvent(t *testing.T) {
	a := deterministicIntakeID("quality/main", "event-7")
	if a != deterministicIntakeID("quality/main", "event-7") {
		t.Fatal("identity is not stable")
	}
	if a == deterministicIntakeID("quality/other", "event-7") || a == deterministicIntakeID("quality/main", "event-8") {
		t.Fatal("identity does not include the full pair")
	}
}

func TestCanonicalIntakeIdentityGolden(t *testing.T) {
	if got := deterministicIntakeID("quality/main", "event-7"); got != "0da3ef3f-b40d-5ef3-ac72-2e3fd7446385" {
		t.Fatalf("golden=%s", got)
	}
}

func TestCanonicalWorkflowEventIdentityGolden(t *testing.T) {
	if got := workflowEventID("550e8400-e29b-41d4-a716-446655440000", "workflow.finished"); got != "94d4b292-bc24-5c64-83b0-fd41a3f6c979" {
		t.Fatalf("golden=%s", got)
	}
}

func TestWorkflowEventStreamIsDurableWithoutEviction(t *testing.T) {
	cfg := workflowEventStreamConfig("EVENTS")
	if cfg.Storage != jetstream.FileStorage || cfg.Retention != jetstream.LimitsPolicy || cfg.Discard != jetstream.DiscardNew || len(cfg.Subjects) != 2 {
		t.Fatalf("config=%+v", cfg)
	}
}

func TestCanonicalIntakeHashRejectsChangedPinnedPayload(t *testing.T) {
	a := intakeRunRequest{BindingID: "b", EventID: "e", Workflow: "group/score", WorkflowVersion: 3, BindingRevision: "r1", Event: json.RawMessage(`{"b":2,"a":1}`), Binding: json.RawMessage(`{"id":"b","enabled":true}`)}
	def := pipeline.Definition{Name: "group/score", Version: 3}
	b := a
	b.Event = json.RawMessage(`{ "a": 1, "b": 2 }`)
	ha, err := canonicalIntakeHash(&a, &def)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := canonicalIntakeHash(&b, &def)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("semantically identical JSON hashes differ: %s %s", ha, hb)
	}
	b.WorkflowVersion = 4
	hc, _ := canonicalIntakeHash(&b, &pipeline.Definition{Name: "group/score", Version: 4})
	if hc == ha {
		t.Fatal("changed pinned version reused accepted payload hash")
	}
}

func TestWorkflowOutcomeVocabulary(t *testing.T) {
	if got := workflowOutcome(4); got != "cancelled" {
		t.Fatalf("cancelled outcome = %q", got)
	}
	if got := workflowOutcome(5); got != "terminated" {
		t.Fatalf("terminated outcome = %q", got)
	}
	if got := workflowOutcome(6); got != "continued_as_new" {
		t.Fatalf("continued outcome = %q", got)
	}
	if got := workflowOutcome(7); got != "timed_out" {
		t.Fatalf("timeout outcome = %q", got)
	}
}

func TestRootBlockedMetadataNeverChangesChildOutcome(t *testing.T) {
	event := WorkflowEvent{Type: "workflow.finished", Status: "completed", ParentExecutionID: "parent-run"}
	if rootBlockedEligible(&event) {
		t.Fatal("child completion was eligible for root blocked override")
	}
	event.ParentExecutionID = ""
	if !rootBlockedEligible(&event) {
		t.Fatal("root completion was not eligible")
	}
}

func TestExecutionMeasurementUsesTemporalTimestampsAndIdentity(t *testing.T) {
	dc := converter.GetDefaultDataConverter()
	workflowPayload, _ := dc.ToPayload("group/build")
	versionPayload, _ := dc.ToPayload(4)
	tracePayload, _ := dc.ToPayload("business-run")
	start := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	closeTime := start.Add(3250 * time.Millisecond)
	info := &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: "business-run", RunId: "temporal-run"}, ParentExecution: &commonpb.WorkflowExecution{RunId: "parent-run"}, Memo: &commonpb.Memo{Fields: map[string]*commonpb.Payload{"workflow": workflowPayload, "workflow_version": versionPayload, "trace_id": tracePayload}}, StartTime: timestamppb.New(start), CloseTime: timestamppb.New(closeTime), Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}
	measurement, ok := executionMeasurementForInfo(info)
	if !ok {
		t.Fatal("measurement missing")
	}
	if measurement["duration_ms"] != int64(3250) || measurement["execution_id"] != "temporal-run" || measurement["parent_execution_id"] != "parent-run" {
		t.Fatalf("measurement=%v", measurement)
	}
}
