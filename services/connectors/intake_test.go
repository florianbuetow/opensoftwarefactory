package main

import (
	"github.com/nats-io/nats.go/jetstream"
	"testing"
)

func TestBindingValidationRequiresPinnedGroupedDestination(t *testing.T) {
	b := IntakeBinding{ID: "quality", Name: "Quality", Source: "factory", SourceWorkflows: []string{"apps/source"}, Outcomes: []string{"completed"}, Workflow: "judges/score", WorkflowVersion: 2}
	if err := validateBinding(b); err != nil {
		t.Fatal(err)
	}
	b.WorkflowVersion = 0
	if err := validateBinding(b); err == nil {
		t.Fatal("unpinned destination accepted")
	}
	b.WorkflowVersion = 2
	b.SourceWorkflows = nil
	if err := validateBinding(b); err == nil {
		t.Fatal("implicit source filter accepted")
	}
}

func TestBindingCycleDetectionUsesFullGroupedNames(t *testing.T) {
	existing := []IntakeBinding{{ID: "one", SourceWorkflows: []string{"apps/a"}, Workflow: "apps/b"}}
	if !createsCycle(existing, IntakeBinding{ID: "two", SourceWorkflows: []string{"apps/b"}, Workflow: "apps/a"}) {
		t.Fatal("two-binding cycle not detected")
	}
	if createsCycle(existing, IntakeBinding{ID: "two", SourceWorkflows: []string{"other/b"}, Workflow: "apps/a"}) {
		t.Fatal("basename collision treated as same grouped workflow")
	}
}

func TestBindingMatchIsTerminalAndExact(t *testing.T) {
	b := IntakeBinding{SourceWorkflows: []string{"apps/source"}, Outcomes: []string{"completed"}}
	e := factoryEvent{SchemaVersion: 1, Type: "workflow.finished", Workflow: "apps/source", Status: "completed"}
	if !bindingMatches(b, e) {
		t.Fatal("matching event rejected")
	}
	e.Workflow = "other/source"
	if bindingMatches(b, e) {
		t.Fatal("basename fallback matched")
	}
	e.Workflow = "apps/source"
	e.Type = "workflow.started"
	if bindingMatches(b, e) {
		t.Fatal("started event triggered intake")
	}
}

func TestDurableBindingNamesAreStableAndDistinct(t *testing.T) {
	a := durableBindingName("a")
	if a != durableBindingName("a") {
		t.Fatal("consumer name unstable")
	}
	if a == durableBindingName("b") {
		t.Fatal("consumer names collided")
	}
}

func TestInitiallyDisabledBindingStartsAfterCreationCheckpoint(t *testing.T) {
	cfg := bindingConsumerConfig("disabled", 42)
	if cfg.DeliverPolicy != jetstream.DeliverByStartSequencePolicy || cfg.OptStartSeq != 42 {
		t.Fatalf("config=%+v", cfg)
	}
}

func TestViewerStartsAtLatestThousand(t *testing.T) {
	if got := viewerStartSequence(10, 2009, 2000); got != 1010 {
		t.Fatalf("start=%d", got)
	}
	if got := viewerStartSequence(10, 19, 10); got != 10 {
		t.Fatalf("short start=%d", got)
	}
}

func TestConnectorEventStreamIsDurableWithoutEviction(t *testing.T) {
	cfg := jetstream.StreamConfig{Storage: jetstream.FileStorage, Retention: jetstream.LimitsPolicy, Discard: jetstream.DiscardNew, Replicas: 1, MaxConsumers: -1, MaxMsgs: -1, MaxBytes: -1, MaxMsgSize: -1, Subjects: []string{"factory.workflow.started", "factory.workflow.finished"}}
	if !validConnectorEventStream(cfg) {
		t.Fatalf("valid config rejected: %+v", cfg)
	}
	cfg.Subjects = []string{"factory.workflow.>"}
	if validConnectorEventStream(cfg) {
		t.Fatal("wildcard stream accepted instead of exact publisher-owned subjects")
	}
}
