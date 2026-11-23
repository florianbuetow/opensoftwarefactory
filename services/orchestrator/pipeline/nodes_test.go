package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// recorder stands in for the services of plain steps. It keeps every request in the order
// the steps finished. A step with a delay finishes later than the steps without one.
type recorder struct {
	mu     sync.Mutex
	calls  []StepRequest
	delays map[string]time.Duration
}

func (r *recorder) process(_ context.Context, req StepRequest) (StepResult, error) {
	time.Sleep(r.delays[req.StepID])
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req)
	return StepResult{Artifact: req.OutputArtifact}, nil
}

// inputsOf shows the inputs of the run that wrote the artifact, or "" when no run did.
func (r *recorder) inputsOf(artifact string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, req := range r.calls {
		if req.OutputArtifact == artifact {
			return show(req.Inputs)
		}
	}
	return ""
}

// runs counts the runs of a step.
func (r *recorder) runs(step string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, req := range r.calls {
		if req.StepID == step {
			n++
		}
	}
	return n
}

// nodeEnv registers the pipeline, the orchestrator's activities, and a recorder as the
// service of every plain step.
func nodeEnv(delays map[string]time.Duration) (*testsuite.TestWorkflowEnvironment, *recorder, memStore) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	store := memStore{}
	rec := &recorder{delays: delays}
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterWorkflowWithOptions(Pipeline, workflow.RegisterOptions{Name: WorkflowName})
	env.RegisterActivityWithOptions(rec.process, activity.RegisterOptions{Name: DefaultActivity})
	return env, rec, store
}

func work(id string, deps ...string) Step {
	return Step{ID: id, Service: "work", DependsOn: deps}
}

// nodeStates queries the state of the merge and batch steps as JSON, by step ID.
func nodeStates(t *testing.T, env *testsuite.TestWorkflowEnvironment) map[string]json.RawMessage {
	t.Helper()
	value, err := env.QueryWorkflow(NodesQuery)
	if err != nil {
		t.Fatal(err)
	}
	var states map[string]json.RawMessage
	if err = value.Get(&states); err != nil {
		t.Fatal(err)
	}
	return states
}

func TestMergeForwardsItsSourcesInDependsOnOrder(t *testing.T) {
	// The first source finishes last.
	env, rec, _ := nodeEnv(map[string]time.Duration{"slow": 100 * time.Millisecond})
	def := Definition{Name: "merge", Steps: []Step{
		work("slow"), work("fast"),
		{ID: "collect", Service: MergeService, DependsOn: []string{"slow", "fast"}},
		work("compare", "collect"),
	}}
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "m1", Definition: def, Inputs: runInput("runs/m1/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if got := rec.inputsOf("runs/m1/compare.pass-1"); got != "slow=runs/m1/slow#1 fast=runs/m1/fast#1" {
		t.Fatalf("compare inputs %q", got)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	// The merge writes no artifact; the step after it stores its first pass as <step>.pass-1.
	if len(res.Artifacts) != 3 || res.Artifacts["compare"] != "runs/m1/compare.pass-1" || show(res.Final) != "compare=runs/m1/compare.pass-1#1" {
		t.Fatalf("result %+v", res)
	}
	var state MergeState
	if err := json.Unmarshal(nodeStates(t, env)["collect"], &state); err != nil {
		t.Fatal(err)
	}
	if state.Kind != MergeService || strings.Join(state.Sources, ",") != "slow,fast" || state.Capacity != DefaultCapacity || state.InputTimeout != DefaultInputTimeout ||
		state.Forwarded != 1 || len(state.Waiting) != 0 || state.Expires != nil || len(state.Releases) != 1 ||
		show(state.Releases[0].Items) != "slow=runs/m1/slow#1 fast=runs/m1/fast#1" {
		t.Fatalf("state %+v", state)
	}
}

func TestMergeKeepsARepeatedSourceInTheNextEntryAndTimesOut(t *testing.T) {
	// "part" delivers twice before "whole" delivers once. Its second result waits in the
	// second entry, which stays incomplete.
	env, rec, _ := nodeEnv(map[string]time.Duration{"whole": 200 * time.Millisecond})
	def := Definition{Name: "repeat", Steps: []Step{
		work("one"), work("two"),
		{ID: "each", Service: BatchService, DependsOn: []string{"one", "two"}, Size: 1},
		work("part", "each"),
		work("whole"),
		{ID: "collect", Service: MergeService, DependsOn: []string{"part", "whole"}, InputTimeout: 5},
		work("compare", "collect"),
	}}
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "m2", Definition: def, Inputs: runInput("runs/m2/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `merge "collect" timed out: no result from "whole"`) {
		t.Fatalf("error %v", err)
	}
	if got := rec.inputsOf("runs/m2/compare.pass-1"); got != "part=runs/m2/part.pass-1#1 whole=runs/m2/whole#1" {
		t.Fatalf("compare inputs %q", got)
	}
	if rec.runs("part") != 2 || rec.runs("compare") != 1 {
		t.Fatalf("part ran %d times, compare %d times", rec.runs("part"), rec.runs("compare"))
	}
	var state MergeState
	if err := json.Unmarshal(nodeStates(t, env)["collect"], &state); err != nil {
		t.Fatal(err)
	}
	if state.Forwarded != 1 || len(state.Waiting) != 1 || state.Expires == nil {
		t.Fatalf("state %+v", state)
	}
	if got := strings.Join(state.Waiting[0].Filled, ","); got != "part" {
		t.Fatalf("filled %q", got)
	}
}

func TestMergeTimesOutWhenASourceNeverDelivers(t *testing.T) {
	env, _, _ := nodeEnv(nil)
	def := Definition{Name: "late", Steps: []Step{
		work("draft"),
		{ID: "approve", Service: GateService},
		{ID: "collect", Service: MergeService, DependsOn: []string{"draft", "approve"}, InputTimeout: 30},
		work("publish", "collect"),
	}}
	env.RegisterDelayedCallback(func() {
		var state MergeState
		if err := json.Unmarshal(nodeStates(t, env)["collect"], &state); err != nil {
			t.Error(err)
			return
		}
		if state.Forwarded != 0 || len(state.Waiting) != 1 || state.Expires == nil || state.InputTimeout != 30 ||
			strings.Join(state.Waiting[0].Filled, ",") != "draft" {
			t.Errorf("state %+v", state)
		}
	}, 10*time.Second)
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "m3", Definition: def, Inputs: runInput("runs/m3/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `merge "collect" timed out: no result from "approve"`) {
		t.Fatalf("error %v", err)
	}
}

func TestMergeFailsTheRunWhenItsQueueIsFull(t *testing.T) {
	env, rec, _ := nodeEnv(nil)
	def := Definition{Name: "full", Steps: []Step{
		work("one"), work("two"), work("three"),
		{ID: "each", Service: BatchService, DependsOn: []string{"one", "two", "three"}, Size: 1},
		work("part", "each"),
		{ID: "hold", Service: GateService},
		{ID: "collect", Service: MergeService, DependsOn: []string{"part", "hold"}, Capacity: 2},
	}}
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "m4", Definition: def, Inputs: runInput("runs/m4/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `merge "collect" is full: capacity 2`) {
		t.Fatalf("error %v", err)
	}
	if rec.runs("part") != 3 {
		t.Fatalf("part ran %d times", rec.runs("part"))
	}
}

func TestMergeWithOneSourceForwardsEveryResultAtOnce(t *testing.T) {
	env, rec, _ := nodeEnv(map[string]time.Duration{"two": 100 * time.Millisecond})
	def := Definition{Name: "single", Steps: []Step{
		work("one"), work("two"),
		{ID: "each", Service: BatchService, DependsOn: []string{"one", "two"}, Size: 1},
		work("part", "each"),
		{ID: "collect", Service: MergeService, DependsOn: []string{"part"}},
		work("after", "collect"),
	}}
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "m5", Definition: def, Inputs: runInput("runs/m5/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if first, second := rec.inputsOf("runs/m5/after.pass-1"), rec.inputsOf("runs/m5/after.pass-2"); first != "part=runs/m5/part.pass-1#1" || second != "part=runs/m5/part.pass-2#2" {
		t.Fatalf("after inputs %q and %q", first, second)
	}
	if got := rec.inputsOf("runs/m5/part.pass-1"); got != "one=runs/m5/one#1" {
		t.Fatalf("part inputs %q", got)
	}
}

func TestBatchReleasesAFullBatchAndASmallerOneWhenItsTimeoutPasses(t *testing.T) {
	// The sources finish in the order b, a, c.
	env, rec, _ := nodeEnv(map[string]time.Duration{"a": 100 * time.Millisecond, "c": 200 * time.Millisecond})
	def := Definition{Name: "batch", Steps: []Step{
		work("a"), work("b"), work("c"),
		{ID: "gather", Service: BatchService, DependsOn: []string{"a", "b", "c"}, Size: 2, InputTimeout: 20},
		work("summarise", "gather"),
	}}
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "b1", Definition: def, Inputs: runInput("runs/b1/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	// The items are in arrival order, and the last one is released by the timeout.
	if first, second := rec.inputsOf("runs/b1/summarise.pass-1"), rec.inputsOf("runs/b1/summarise.pass-2"); first != "b=runs/b1/b#1 a=runs/b1/a#1" || second != "c=runs/b1/c#1" {
		t.Fatalf("summarise inputs %q and %q", first, second)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Artifacts["summarise"] != "runs/b1/summarise.pass-2" || len(res.Final) != 2 || res.Final[1].Round != 2 {
		t.Fatalf("result %+v", res)
	}
	var state BatchState
	if err := json.Unmarshal(nodeStates(t, env)["gather"], &state); err != nil {
		t.Fatal(err)
	}
	if state.Kind != BatchService || state.Size != 2 || state.InputTimeout != 20 || state.Released != 2 ||
		len(state.Collected) != 0 || state.Expires != nil || state.Releases[0].Timed || !state.Releases[1].Timed || state.Releases[1].Pass != 2 {
		t.Fatalf("state %+v", state)
	}
	if wait := state.Releases[1].At.Sub(state.Releases[0].At); wait < 20*time.Second {
		t.Fatalf("the smaller batch was released after %v", wait)
	}
}

func TestBatchShowsItsCollectedItemsWhileItWaits(t *testing.T) {
	env, _, _ := nodeEnv(nil)
	def := Definition{Name: "waiting", Steps: []Step{
		work("a"),
		{ID: "gather", Service: BatchService, DependsOn: []string{"a"}, Size: 2, InputTimeout: 60},
		work("summarise", "gather"),
	}}
	env.RegisterDelayedCallback(func() {
		var state BatchState
		if err := json.Unmarshal(nodeStates(t, env)["gather"], &state); err != nil {
			t.Error(err)
			return
		}
		if show(state.Collected) != "a=runs/b2/a#1" || state.Released != 0 || state.Expires == nil || len(state.Releases) != 0 {
			t.Errorf("state %+v", state)
		}
	}, 30*time.Second)
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "b2", Definition: def, Inputs: runInput("runs/b2/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
}

func TestBatchTakesARepeatedSource(t *testing.T) {
	env, rec, _ := nodeEnv(nil)
	def := Definition{Name: "again", Steps: []Step{
		work("one"), work("two"), work("three"),
		{ID: "each", Service: BatchService, DependsOn: []string{"one", "two", "three"}, Size: 1},
		work("part", "each"),
		{ID: "gather", Service: BatchService, DependsOn: []string{"part"}, Size: 2, InputTimeout: 10},
		work("summarise", "gather"),
	}}
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "b3", Definition: def, Inputs: runInput("runs/b3/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if first, second := rec.inputsOf("runs/b3/summarise.pass-1"), rec.inputsOf("runs/b3/summarise.pass-2"); first != "part=runs/b3/part.pass-1#1 part=runs/b3/part.pass-2#2" || second != "part=runs/b3/part.pass-3#3" {
		t.Fatalf("summarise inputs %q and %q", first, second)
	}
}

func TestItemsStayFlatThroughChainedNodes(t *testing.T) {
	env, rec, _ := nodeEnv(map[string]time.Duration{"c": 100 * time.Millisecond})
	def := Definition{Name: "chain", Steps: []Step{
		work("a"), work("b"), work("c"),
		{ID: "pair", Service: MergeService, DependsOn: []string{"a", "b"}},
		{ID: "gather", Service: BatchService, DependsOn: []string{"pair", "c"}, Size: 3},
		{ID: "again", Service: MergeService, DependsOn: []string{"gather"}},
		work("report", "again"),
	}}
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "c1", Definition: def, Inputs: runInput("runs/c1/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if got := rec.inputsOf("runs/c1/report.pass-1"); got != "a=runs/c1/a#1 b=runs/c1/b#1 c=runs/c1/c#1" {
		t.Fatalf("report inputs %q", got)
	}
	if rec.runs("report") != 1 {
		t.Fatalf("report ran %d times", rec.runs("report"))
	}
}

func TestGateAndNestedWorkflowAfterANodeReceiveTheWholeList(t *testing.T) {
	inner := Definition{Name: "inner", Steps: []Step{
		work("x"), work("y"),
		{ID: "each", Service: BatchService, DependsOn: []string{"x", "y"}, Size: 1},
		work("last", "each"),
	}}
	def := Definition{Name: "outer", Steps: []Step{
		work("a"), work("b"),
		{ID: "collect", Service: MergeService, DependsOn: []string{"a", "b"}},
		{ID: "approve", Service: GateService, DependsOn: []string{"collect"}},
		{ID: "nested", Service: WorkflowService, Workflow: "inner", DependsOn: []string{"collect"}},
		work("after", "nested"),
	}}
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	store := memStore{}
	rec := &recorder{}
	env.RegisterActivity(&Activities{Store: store, LoadDefinitions: func() ([]Definition, error) { return []Definition{inner, def}, nil }})
	env.RegisterWorkflowWithOptions(Pipeline, workflow.RegisterOptions{Name: WorkflowName})
	env.RegisterActivityWithOptions(rec.process, activity.RegisterOptions{Name: DefaultActivity})
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 {
			t.Errorf("pending %+v", state)
			return
		}
		g := state.Gates[0]
		if g.Step != "approve" || g.Pass != 1 || g.Artifact != "runs/n1/approve.pass-1" || show(g.Inputs) != "a=runs/n1/a#1 b=runs/n1/b#1" {
			t.Errorf("gate %+v", g)
		}
		env.SignalWorkflow(DecisionSignal, Decision{Step: "approve", Decision: Approved, DecidedBy: "flo"})
	}, time.Second)
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "n1", Definition: def, Inputs: runInput("runs/n1/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	// The nested run's first steps receive the whole list of the step that started it.
	if got := rec.inputsOf("runs/n1/nested.pass-1/x"); got != "a=runs/n1/a#1 b=runs/n1/b#1" {
		t.Fatalf("nested inputs %q", got)
	}
	// The final step of the nested run ran twice, so the nesting step hands on two items.
	// It runs in pass 1 of the outer run, which is the round of both.
	if got := rec.inputsOf("runs/n1/after.pass-1"); got != "nested=runs/n1/nested.pass-1/last.pass-1#1 nested=runs/n1/nested.pass-1/last.pass-2#1" {
		t.Fatalf("after inputs %q", got)
	}
	var rec1 DecisionRecord
	if err := json.Unmarshal(store["runs/n1/approve.pass-1"], &rec1); err != nil || rec1.Step != "approve" || len(rec1.Inputs) != 2 {
		t.Fatalf("decision record %+v %v", rec1, err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	for _, key := range []string{"approve", "nested", "nested.2", "nested/last", "after"} {
		if res.Artifacts[key] == "" {
			t.Errorf("artifact %s is missing: %v", key, res.Artifacts)
		}
	}
}

func TestTeamAndItsGateAfterANodeWorkWithinOnePass(t *testing.T) {
	env, store, f := teamEnv(t, []Definition{echoWorkflow}, cooperative)
	ctx := context.Background()
	_ = store.Put(ctx, "runs/n2/input", []byte("Build a parser."), Meta{})
	build := teamStep()
	build.DependsOn = []string{"collect"}
	def := Definition{Name: "passes", Steps: []Step{
		{ID: "a", Service: "echo"},
		{ID: "b", Service: "echo"},
		{ID: "collect", Service: MergeService, DependsOn: []string{"a", "b"}},
		build,
		{ID: "review", Service: GateService, DependsOn: []string{"build"}, OnReject: "build"},
	}}
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 || state.Gates[0].Pass != 1 || state.Gates[0].Round != 1 || show(state.Gates[0].Inputs) != "build=runs/n2/build.pass-1#1" {
			t.Errorf("first round %+v", state)
		}
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: "handle empty input", DecidedBy: "flo"})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 || state.Gates[0].Pass != 1 || state.Gates[0].Round != 2 {
			t.Errorf("second round %+v", state)
		}
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Approved, DecidedBy: "flo"})
	}, 2*time.Second)
	env.ExecuteWorkflow(Pipeline, Input{TraceID: "n2", Definition: def, Inputs: runInput("runs/n2/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	// The rejection runs the team again in the same pass, so its artifact keeps its name.
	if got := summaryOf(t, store, "runs/n2/build.pass-1"); got != "revised" {
		t.Errorf("summary %q", got)
	}
	for _, id := range []string{
		"runs/n2/review.pass-1.1", "runs/n2/review.pass-1", "teams/n2/build.pass-1/team.json",
		"teams/n2/build.pass-1/files/input-a.md", "teams/n2/build.pass-1/files/input-b.md", "runs/n2/build.pass-1.echo.r1.1/say",
	} {
		if !store.has(id) {
			t.Errorf("%s is missing", id)
		}
	}
	var manifest TeamManifest
	_, _ = f.a.getJSON(ctx, "teams/n2/build.pass-1/team.json", &manifest)
	if manifest.Round != 2 || manifest.StepID != "build" {
		t.Errorf("manifest %+v", manifest)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Artifacts["build"] != "runs/n2/build.pass-1" || res.Artifacts["review"] != "runs/n2/review.pass-1" || len(res.Artifacts) != 4 {
		t.Errorf("artifacts %v", res.Artifacts)
	}
}

func TestValidateMergeAndBatchSteps(t *testing.T) {
	merge := Step{ID: "collect", Service: MergeService, DependsOn: []string{"a", "b"}, Capacity: 4, InputTimeout: 30}
	batch := Step{ID: "gather", Service: BatchService, DependsOn: []string{"a"}, Size: 2}
	base := func(steps ...Step) Definition {
		return Definition{Name: "w", Steps: append([]Step{work("a"), work("b")}, steps...)}
	}
	for name, d := range map[string]Definition{
		"merge":          base(merge),
		"batch":          base(batch),
		"defaults":       base(Step{ID: "collect", Service: MergeService, DependsOn: []string{"a"}}),
		"chained":        base(merge, Step{ID: "gather", Service: BatchService, DependsOn: []string{"collect"}, Size: 1}, work("z", "gather")),
		"gate and team":  base(merge, Step{ID: "team", Service: TeamService, Agent: "lead@v1", DependsOn: []string{"collect"}}, Step{ID: "review", Service: GateService, DependsOn: []string{"team"}, OnReject: "team"}),
		"nested after":   base(merge, Step{ID: "n", Service: WorkflowService, Workflow: "inner", DependsOn: []string{"collect", "a"}}),
		"route after it": base(merge, work("z", "collect"), Step{ID: "r", Service: RouteService, DependsOn: []string{"z"}}),
	} {
		if err := Validate(d); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	change := func(s Step, edit func(*Step)) Step {
		edit(&s)
		return s
	}
	for name, tc := range map[string]struct {
		def  Definition
		want string
	}{
		"merge no source":     {base(change(merge, func(s *Step) { s.DependsOn = nil })), "needs at least one source"},
		"batch no source":     {base(change(batch, func(s *Step) { s.DependsOn = nil })), "needs at least one source"},
		"batch no size":       {base(change(batch, func(s *Step) { s.Size = 0 })), "needs a size of 1 or more"},
		"batch negative size": {base(change(batch, func(s *Step) { s.Size = -1 })), "needs a size of 1 or more"},
		"batch capacity":      {base(change(batch, func(s *Step) { s.Capacity = 4 })), "only a merge step"},
		"merge size":          {base(change(merge, func(s *Step) { s.Size = 2 })), "only a batch step"},
		"negative capacity":   {base(change(merge, func(s *Step) { s.Capacity = -1 })), "capacity must be a positive whole number"},
		"negative timeout":    {base(change(batch, func(s *Step) { s.InputTimeout = -5 })), "input_timeout must be a positive whole number"},
		"merge agent":         {base(change(merge, func(s *Step) { s.Agent = "intake@v1" })), "takes only depends_on"},
		"merge question":      {base(change(merge, func(s *Step) { s.Question = "ok?" })), "takes only depends_on"},
		"batch timeout":       {base(change(batch, func(s *Step) { s.Timeout = "30s" })), "takes only depends_on"},
		"batch attempts":      {base(change(batch, func(s *Step) { s.MaxAttempts = 2 })), "takes only depends_on"},
		"merge workflow":      {base(change(merge, func(s *Step) { s.Workflow = "inner" })), "takes only depends_on"},
		"agent size":          {base(Step{ID: "z", Service: AgentService, Agent: "intake@v1", Size: 2}), "only a merge or batch step"},
		"gate input timeout":  {base(Step{ID: "z", Service: GateService, InputTimeout: 30}), "only a merge or batch step"},
		"plain capacity":      {base(change(work("z"), func(s *Step) { s.Capacity = 1 })), "only a merge or batch step"},
		"route after merge":   {base(merge, Step{ID: "r", Service: RouteService, DependsOn: []string{"collect"}}), `route step "r" cannot depend on merge step "collect"`},
		"route after batch":   {base(batch, Step{ID: "r", Service: RouteService, DependsOn: []string{"gather"}}), `route step "r" cannot depend on batch step "gather"`},
		"node after the team": {base(
			Step{ID: "team", Service: TeamService, Agent: "lead@v1"},
			Step{ID: "gather", Service: BatchService, DependsOn: []string{"team"}, Size: 1},
			Step{ID: "review", Service: GateService, DependsOn: []string{"gather"}, OnReject: "team"},
		), `has the batch step "gather" after it`},
	} {
		if err := Validate(tc.def); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v", name, err)
		}
	}
}
