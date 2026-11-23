package pipeline

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
)

// Handler implements one service step. inputs holds the content of each item
// of req.Inputs, in the same order.
type Handler func(ctx context.Context, req StepRequest, inputs [][]byte) ([]byte, error)

// LoadInputs reads every input artifact of a step request, in the order of its items.
func LoadInputs(ctx context.Context, store Store, req *StepRequest) ([][]byte, error) {
	in := make([][]byte, 0, len(req.Inputs))
	for _, item := range req.Inputs {
		data, err := store.Get(ctx, item.Artifact)
		if err != nil {
			return nil, fmt.Errorf("reading artifact %q: %w", item.Artifact, err)
		}
		in = append(in, data)
	}
	return in, nil
}

// StepActivity adapts a Handler into a Temporal activity: it loads the input
// artifacts, runs the handler, and stores the output under the artifact ID
// the orchestrator assigned, recording creator as the artifact's creator.
func StepActivity(store Store, creator string, h Handler) func(context.Context, StepRequest) (StepResult, error) {
	return func(ctx context.Context, req StepRequest) (StepResult, error) {
		in, err := LoadInputs(ctx, store, &req)
		if err != nil {
			return StepResult{}, err
		}
		out, err := h(ctx, req, in)
		if err != nil {
			return StepResult{}, err
		}
		if err = store.Put(ctx, req.OutputArtifact, out, Meta{Creator: creator, TraceID: req.TraceID}); err != nil {
			return StepResult{}, fmt.Errorf("writing artifact %q: %w", req.OutputArtifact, err)
		}
		return StepResult{Artifact: req.OutputArtifact}, nil
	}
}

// Dial connects to Temporal at TEMPORAL_ADDRESS in TEMPORAL_NAMESPACE,
// retrying for up to two minutes while Temporal starts.
func Dial(interceptors ...interceptor.ClientInterceptor) (client.Client, error) {
	opts := client.Options{
		HostPort:     env("TEMPORAL_ADDRESS", "temporal:7233"),
		Namespace:    env("TEMPORAL_NAMESPACE", "default"),
		Interceptors: interceptors,
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		c, err := client.Dial(opts)
		if err == nil || time.Now().After(deadline) {
			return c, err
		}
		time.Sleep(2 * time.Second)
	}
}

// ServeStep runs a worker that executes h for steps whose service is
// taskQueue, until the process is interrupted. Artifacts it writes name
// taskQueue as their creator.
func ServeStep(c client.Client, store Store, taskQueue string, h Handler) error {
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterActivityWithOptions(StepActivity(store, taskQueue, h), activity.RegisterOptions{Name: env("STEP_ACTIVITY", DefaultActivity)})
	return w.Run(worker.InterruptCh())
}
