package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

type conditionalMemory struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (c *conditionalMemory) Put(_ context.Context, id string, data []byte, _ Meta) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[id] = append([]byte{}, data...)
	return nil
}

func (c *conditionalMemory) Get(_ context.Context, id string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.data[id]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte{}, data...), nil
}

func (c *conditionalMemory) PutIfAbsent(_ context.Context, id string, data []byte, _ Meta) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.data[id]; ok {
		return "", ErrPreconditionFailed
	}
	c.data[id] = append([]byte{}, data...)
	return "r1", nil
}

func (c *conditionalMemory) GetWithRevision(ctx context.Context, id string) ([]byte, string, error) {
	data, err := c.Get(ctx, id)
	return data, "r1", err
}

func (c *conditionalMemory) PutIfMatch(_ context.Context, id string, data []byte, _ Meta, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.data[id]; !ok {
		return "", ErrPreconditionFailed
	}
	c.data[id] = append([]byte{}, data...)
	return "r1", nil
}

func TestInitializeIntakeRetriesSameExecutionAndStopsAnother(t *testing.T) {
	store := &conditionalMemory{data: map[string][]byte{}}
	a := Activities{Store: store, Conditional: store}
	req := InitializeIntakeRequest{TraceID: "run", ExecutionID: "exec-1", Definition: Definition{Name: "group/score", Version: 2}, Intake: IntakeInput{BindingID: "binding", BindingRevision: "rev", EventID: "event", Event: []byte(`{"run_id":"target"}`), Binding: []byte(`{"id":"binding"}`)}}
	req.Intake.AcceptanceHash, _ = IntakeAcceptanceHash(req.Definition, req.Intake)
	first, err := a.InitializeIntake(context.Background(), req)
	if err != nil || !first.Proceed {
		t.Fatalf("first = %+v, %v", first, err)
	}
	delete(store.data, "runs/run/input")
	var receipt IntakeReceipt
	_ = json.Unmarshal(store.data["intake-receipts/run.json"], &receipt)
	receipt.Phase, receipt.InitializedAt = "claimed", nil
	store.data["intake-receipts/run.json"] = dataForReceipt(receipt)
	retry, err := a.InitializeIntake(context.Background(), req)
	if err != nil || !retry.Proceed {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	if _, ok := store.data["runs/run/input"]; !ok {
		t.Fatal("retry did not restore immutable input")
	}
	if got := string(store.data["runs/run/run.json"]); !strings.Contains(got, `"execution_id": "exec-1"`) {
		t.Fatalf("run record lacks execution identity: %s", got)
	}
	if _, ok := store.data["runs/run/metadata.json"]; !ok {
		t.Fatal("intake metadata was not initialized")
	}
	req.ExecutionID = "exec-2"
	other, err := a.InitializeIntake(context.Background(), req)
	if err != nil || other.Proceed {
		t.Fatalf("other execution = %+v, %v", other, err)
	}
}

func TestInitializeIntakeRequiresConditionalStore(t *testing.T) {
	a := Activities{Store: &conditionalMemory{data: map[string][]byte{}}}
	_, err := a.InitializeIntake(context.Background(), InitializeIntakeRequest{})
	if err == nil {
		t.Fatal("missing conditional capability was accepted")
	}
}

func TestInitializeIntakeTakesOverOnlyClaimedMatchingReceipt(t *testing.T) {
	store := &conditionalMemory{data: map[string][]byte{}}
	a := Activities{Store: store, Conditional: store}
	req := InitializeIntakeRequest{TraceID: "run", ExecutionID: "old", Definition: Definition{Name: "group/score", Version: 2}, Intake: IntakeInput{BindingID: "b", BindingRevision: "r", EventID: "e", Event: []byte(`{"run_id":"target"}`), Binding: []byte(`{"id":"b"}`)}}
	req.Intake.AcceptanceHash, _ = IntakeAcceptanceHash(req.Definition, req.Intake)
	if out, err := a.InitializeIntake(context.Background(), req); err != nil || !out.Proceed {
		t.Fatalf("first=%+v err=%v", out, err)
	}
	var receipt IntakeReceipt
	_ = json.Unmarshal(store.data["intake-receipts/run.json"], &receipt)
	receipt.Phase = "claimed"
	receipt.InitializedAt = nil
	store.data["intake-receipts/run.json"] = dataForReceipt(receipt)
	req.ExecutionID = "replacement"
	out, err := a.InitializeIntake(context.Background(), req)
	if err != nil || !out.Proceed {
		t.Fatalf("takeover=%+v err=%v", out, err)
	}
	_ = json.Unmarshal(store.data["intake-receipts/run.json"], &receipt)
	if receipt.ExecutionID != "replacement" || receipt.TakenOverFrom != "old" || receipt.Phase != "initialized" {
		t.Fatalf("receipt=%+v", receipt)
	}
	req.ExecutionID = "conflict"
	req.Intake.AcceptanceHash = "different"
	if _, err = a.InitializeIntake(context.Background(), req); err == nil {
		t.Fatal("different pinned configuration took over receipt")
	}
}

func TestLegacyReplayVersionSkipsNewIntakeInitializationCommand(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.OnGetVersion("factory-measurements-v1", workflow.DefaultVersion, workflow.Version(1)).Return(workflow.DefaultVersion)
	env.OnGetVersion("factory-lifecycle-open-v1", workflow.DefaultVersion, workflow.Version(1)).Return(workflow.DefaultVersion)
	env.RegisterActivityWithOptions(func(context.Context, StepRequest) (StepResult, error) {
		return StepResult{Artifact: "runs/run/work"}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})
	env.RegisterActivityWithOptions(func(context.Context, FinishRunRequest) error { return nil }, activity.RegisterOptions{Name: FinishRunActivity})
	def := Definition{Name: "group/legacy", Version: 1, Steps: []Step{{ID: "work", Service: "test"}}}
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "run", Definition: def, Inputs: []Item{{Source: InputSource, Artifact: "runs/run/input"}}, Intake: &IntakeInput{AcceptanceHash: "new-only"}})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("legacy replay path failed: %v", err)
	}
}
