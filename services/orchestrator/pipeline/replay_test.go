package pipeline

import (
	"testing"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// This history was produced by the Pipeline implementation at b7a3a0d using an
// isolated Temporal server and a synthetic gate-only workflow. Replaying the
// completed history protects workflows started before the current version gates.
func TestPipelineReplaysLegacyHistory(t *testing.T) {
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(Pipeline, workflow.RegisterOptions{Name: WorkflowName})

	if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, "testdata/legacy-history.json"); err != nil {
		t.Fatalf("replay legacy Pipeline history: %v", err)
	}
}
