package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// runInput is the input of a top-level run.
func runInput(artifact string) []Item {
	return []Item{{Source: InputSource, Artifact: artifact, Round: 1}}
}

// show writes items as "<source>=<artifact>#<round>", separated by spaces.
func show(items []Item) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s=%s#%d", item.Source, item.Artifact, item.Round))
	}
	return strings.Join(parts, " ")
}

func diamond() Definition {
	return Definition{Name: "diamond", Steps: []Step{
		{ID: "top", Service: "top"},
		{ID: "left", Service: "left", DependsOn: []string{"top"}},
		{ID: "right", Service: "right", DependsOn: []string{"top"}},
		{ID: "join", Service: "join", Activity: "Merge", DependsOn: []string{"left", "right"}},
	}}
}

func registerExecutionLifecycle(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(func(context.Context, ExecutionOpenRecord) error { return nil }, activity.RegisterOptions{Name: RecordExecutionOpenActivity})
}

func TestPipelineHandsOutputsToDependents(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	registerExecutionLifecycle(env)
	var mu sync.Mutex
	seen := map[string]StepRequest{}
	record := func(_ context.Context, req StepRequest) (StepResult, error) {
		mu.Lock()
		seen[req.StepID] = req
		mu.Unlock()
		return StepResult{Artifact: req.OutputArtifact}, nil
	}
	env.RegisterActivityWithOptions(record, activity.RegisterOptions{Name: DefaultActivity})
	env.RegisterActivityWithOptions(record, activity.RegisterOptions{Name: "Merge"})

	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t1", Definition: diamond(), Inputs: runInput("runs/t1/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res Result
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.TraceID != "t1" || len(res.Artifacts) != 4 || res.Artifacts["join"] != "runs/t1/join" {
		t.Fatalf("result %+v", res)
	}
	if got := show(seen["top"].Inputs); got != "input=runs/t1/input#1" {
		t.Errorf("top inputs %v", got)
	}
	if got := show(seen["left"].Inputs); got != "top=runs/t1/top#1" {
		t.Errorf("left inputs %v", got)
	}
	// The items are in depends_on order.
	if got := show(seen["join"].Inputs); got != "left=runs/t1/left#1 right=runs/t1/right#1" {
		t.Errorf("join inputs %v", got)
	}
	for id, req := range seen {
		if req.TraceID != "t1" || req.Workflow != "diamond" {
			t.Errorf("step %s request %+v", id, req)
		}
	}
}

func TestPipelineStopsOnStepFailure(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	registerExecutionLifecycle(env)
	var mu sync.Mutex
	joined := false
	env.RegisterActivityWithOptions(func(_ context.Context, req StepRequest) (StepResult, error) {
		if req.StepID == "left" {
			return StepResult{}, temporal.NewNonRetryableApplicationError("boom", "Test", nil)
		}
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})
	env.RegisterActivityWithOptions(func(context.Context, StepRequest) (StepResult, error) {
		mu.Lock()
		joined = true
		mu.Unlock()
		return StepResult{}, nil
	}, activity.RegisterOptions{Name: "Merge"})

	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t2", Definition: diamond(), Inputs: runInput("runs/t2/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `step "left" failed`) {
		t.Fatalf("error %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if joined {
		t.Error("join ran after a dependency failed")
	}
}

func TestPipelineRejectsInvalidDefinition(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: memStore{}})
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t3", Definition: Definition{Name: "empty"}})
	var appErr *temporal.ApplicationError
	if err := env.GetWorkflowError(); !errors.As(err, &appErr) || appErr.Type() != "InvalidDefinition" {
		t.Fatalf("error %v", err)
	}
}

func TestAgentStepsCarryThePinnedAgent(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	registerExecutionLifecycle(env)
	var got StepRequest
	env.RegisterActivityWithOptions(func(context.Context, ResolveAgentSettingsRequest) (AgentExecutionSettings, error) {
		return AgentExecutionSettings{Skills: map[string]string{"git": "v3"}, SkillScriptTimeoutSeconds: 300}, nil
	}, activity.RegisterOptions{Name: ResolveAgentSettingsActivity})
	env.RegisterActivityWithOptions(func(_ context.Context, req StepRequest) (StepResult, error) {
		got = req
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})
	def := Definition{Name: "w", Steps: []Step{{ID: "intake", Service: AgentService, Agent: "intake@v2"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t4", Definition: def, Inputs: runInput("runs/t4/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if got.Agent != "intake@v2" || got.OutputArtifact != "runs/t4/intake" || got.Skills["git"] != "v3" || got.SkillScriptTimeoutSeconds != 300 {
		t.Fatalf("request %+v", got)
	}
}

func TestMissingSkillVersionStopsTheStepWithoutRetry(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	registerExecutionLifecycle(env)
	attempts := 0
	env.RegisterActivityWithOptions(func(context.Context, ResolveAgentSettingsRequest) (AgentExecutionSettings, error) {
		attempts++
		return AgentExecutionSettings{}, temporal.NewNonRetryableApplicationError("the skill registry does not know git@v9", "MissingSkillVersion", nil)
	}, activity.RegisterOptions{Name: ResolveAgentSettingsActivity})
	processed := false
	env.RegisterActivityWithOptions(func(context.Context, StepRequest) (StepResult, error) {
		processed = true
		return StepResult{}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})

	def := Definition{Name: "w", Steps: []Step{{ID: "code", Service: AgentService, Agent: "coder@v1"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "missing-skill", Definition: def, Inputs: runInput("runs/missing-skill/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), "the skill registry does not know git@v9") {
		t.Fatalf("error %v", err)
	}
	if attempts != 1 || processed {
		t.Fatalf("attempts %d processed %v", attempts, processed)
	}
}

func gated() Definition {
	return Definition{Name: "gated", Steps: []Step{
		{ID: "draft", Service: "draft"},
		{ID: "approve", Service: GateService, Question: "Ship it?", DependsOn: []string{"draft"}},
		{ID: "publish", Service: "publish", DependsOn: []string{"approve"}},
	}}
}

func TestGateWaitsForADecisionAndStoresIt(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	store := memStore{}
	env.RegisterActivity(&Activities{Store: store})
	var published StepRequest
	env.RegisterActivityWithOptions(func(_ context.Context, req StepRequest) (StepResult, error) {
		if req.StepID == publishStep {
			published = req
		}
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})

	env.RegisterDelayedCallback(func() {
		value, err := env.QueryWorkflow(PendingQuery)
		if err != nil {
			t.Error(err)
			return
		}
		var state PendingState
		if err = value.Get(&state); err != nil {
			t.Error(err)
			return
		}
		if state.TraceID != "t5" || state.Workflow != "gated" || len(state.Gates) != 1 {
			t.Errorf("pending %+v", state)
			return
		}
		g := state.Gates[0]
		if g.Step != approveStep || g.Question != "Ship it?" || show(g.Inputs) != "draft=runs/t5/draft#1" || g.Artifact != "runs/t5/approve" {
			t.Errorf("gate %+v", g)
		}
		env.SignalWorkflow(DecisionSignal, Decision{Step: "nope", Decision: Approved, DecidedBy: "x"})
		env.SignalWorkflow(DecisionSignal, Decision{Step: "approve", Decision: Approved, Comment: "fine", DecidedBy: testUser})
	}, time.Second)

	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t5", Definition: gated(), Inputs: runInput("runs/t5/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Artifacts["approve"] != "runs/t5/approve" || res.Artifacts["publish"] != "runs/t5/publish" {
		t.Fatalf("artifacts %v", res.Artifacts)
	}
	if show(published.Inputs) != "approve=runs/t5/approve#1" {
		t.Fatalf("publish inputs %v", published.Inputs)
	}
	var rec DecisionRecord
	if err := json.Unmarshal(store["runs/t5/approve"], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Decision != Approved || rec.DecidedBy != testUser || rec.Comment != "fine" || rec.Question != "Ship it?" || rec.DecidedAt.IsZero() {
		t.Fatalf("record %+v", rec)
	}
	// The record stores its inputs as the item list.
	if !strings.Contains(string(store["runs/t5/approve"]), `"inputs": [`) || show(rec.Inputs) != "draft=runs/t5/draft#1" {
		t.Fatalf("record inputs %s", store["runs/t5/approve"])
	}
	if m := store.metaOf("runs/t5/approve"); m.Creator != testUser || m.TraceID != "t5" {
		t.Fatalf("meta %+v", m)
	}
}

func TestRejectedGateFailsTheRunAfterRecordingIt(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	store := memStore{}
	env.RegisterActivity(&Activities{Store: store})
	published := false
	env.RegisterActivityWithOptions(func(_ context.Context, req StepRequest) (StepResult, error) {
		if req.StepID == publishStep {
			published = true
		}
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(DecisionSignal, Decision{Step: "approve", Decision: Rejected, Comment: "redo", DecidedBy: testUser})
	}, time.Second)

	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t6", Definition: gated(), Inputs: runInput("runs/t6/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `step "approve" rejected by flo: redo`) {
		t.Fatalf("error %v", err)
	}
	if published {
		t.Error("publish ran after a rejection")
	}
	if _, ok := store["runs/t6/approve"]; !ok {
		t.Error("rejection was not recorded")
	}
}

func interactive() Definition {
	return Definition{Name: "talk", Steps: []Step{
		{ID: "draft", Service: "draft"},
		{ID: "chat", Service: InteractiveService, Agent: "triage@v1", Agents: []string{"plain@v2", "triage@v1"}, DependsOn: []string{"draft"}},
		{ID: "publish", Service: "publish", DependsOn: []string{"chat"}},
	}}
}

// pendingState queries the workflow under test for its waiting steps.
func pendingState(t *testing.T, env *testsuite.TestWorkflowEnvironment) PendingState {
	t.Helper()
	var state PendingState
	value, err := env.QueryWorkflow(PendingQuery)
	if err != nil {
		t.Error(err)
		return state
	}
	if err = value.Get(&state); err != nil {
		t.Error(err)
	}
	return state
}

func TestInteractiveStepWaitsForACompletionAndHandsOnTheChosenFile(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	const session = "user-sessions/t11/chat"
	store := memStore{
		"runs/t11/draft":             []byte(`{"title":"draft"}`),
		session + "/files/result.md": []byte("# Result"),
	}
	env.RegisterActivity(&Activities{Store: store, LoadAgentSettings: func(_ context.Context, ref string) (AgentExecutionSettings, error) {
		if ref == "triage@v1" {
			return AgentExecutionSettings{Skills: map[string]string{"triage": "v2"}, SkillScriptTimeoutSeconds: 45}, nil
		}
		return AgentExecutionSettings{}, nil
	}})
	var published StepRequest
	env.RegisterActivityWithOptions(func(_ context.Context, req StepRequest) (StepResult, error) {
		if req.StepID == publishStep {
			published = req
		}
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})

	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if state.TraceID != sessionRun || state.Workflow != "talk" || len(state.Sessions) != 1 || state.Gates == nil || len(state.Gates) != 0 {
			t.Errorf("pending %+v", state)
			return
		}
		p := state.Sessions[0]
		if p.Step != chatStep || p.Agent != "triage@v1" || strings.Join(p.Agents, ",") != "triage@v1,plain@v2" ||
			show(p.Inputs) != "draft=runs/t11/draft#1" || p.Artifact != sessionArtifact || p.Session != session {
			t.Errorf("session %+v", p)
		}
		if p.Skills["triage"] != "v2" || p.SkillScriptTimeoutSeconds != 45 || p.AgentSettings["triage@v1"].Skills["triage"] != "v2" {
			t.Errorf("session settings %+v", p)
		}
		if string(store[session+"/files/input-draft.json"]) != `{"title":"draft"}` {
			t.Errorf("input file not opened: %q", store[session+"/files/input-draft.json"])
		}
		if m := store.metaOf(session + "/files/input-draft.json"); m.TraceID != sessionRun {
			t.Errorf("input file meta %+v", m)
		}
		// None of these may end the step: an unknown step, a missing person, an
		// invalid file name, and a file that does not exist.
		env.SignalWorkflow(CompleteSignal, Completion{Step: "nope", File: "result.md", CompletedBy: testUser})
		env.SignalWorkflow(CompleteSignal, Completion{Step: "chat", File: "result.md"})
		env.SignalWorkflow(CompleteSignal, Completion{Step: "chat", File: "../result.md", CompletedBy: testUser})
		env.SignalWorkflow(CompleteSignal, Completion{Step: "chat", File: "absent.md", CompletedBy: testUser})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		if state := pendingState(t, env); len(state.Sessions) != 1 || state.Sessions[0].Step != chatStep {
			t.Errorf("a failed completion ended the step: %+v", state)
		}
		if _, ok := store[session+"/completed.json"]; ok {
			t.Error("a failed completion closed the session")
		}
		if published.StepID != "" {
			t.Error("publish ran before the session was completed")
		}
		env.SignalWorkflow(CompleteSignal, Completion{Step: "chat", File: "result.md", CompletedBy: testUser})
	}, time.Minute)

	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t11", Definition: interactive(), Inputs: runInput("runs/t11/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Artifacts["chat"] != sessionArtifact || res.Artifacts["publish"] != "runs/t11/publish" {
		t.Fatalf("artifacts %v", res.Artifacts)
	}
	if show(published.Inputs) != "chat=runs/t11/chat#1" {
		t.Fatalf("publish inputs %v", published.Inputs)
	}
	if string(store["runs/t11/chat"]) != "# Result" {
		t.Fatalf("output %q", store["runs/t11/chat"])
	}
	if m := store.metaOf("runs/t11/chat"); m.Creator != testUser || m.TraceID != sessionRun {
		t.Fatalf("meta %+v", m)
	}
	var rec CompletionRecord
	if err := json.Unmarshal(store[session+"/completed.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.File != "result.md" || rec.CompletedBy != testUser || rec.Artifact != sessionArtifact || rec.CompletedAt.IsZero() {
		t.Fatalf("record %+v", rec)
	}
}

func TestTerminalChatFailureStopsTheWaitingInteractiveStep(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	store := memStore{"runs/chat-failure/draft": []byte("draft"), "runs/chat-failure/metadata.json": []byte(`{}`)}
	env.RegisterActivity(&Activities{Store: store})
	published := false
	env.RegisterActivityWithOptions(func(_ context.Context, req StepRequest) (StepResult, error) {
		if req.StepID == publishStep {
			published = true
		}
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Sessions) != 1 || state.Sessions[0].Step != chatStep {
			t.Errorf("pending sessions %+v", state.Sessions)
		}
		env.SignalWorkflow(SessionFailureSignal, SessionFailure{Step: "chat", Type: "SkillScriptNeedsDevPod", Error: "The workflow needs a devpod step before this step to run a skill script."})
	}, time.Second)

	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "chat-failure", Definition: interactive(), Inputs: runInput("runs/chat-failure/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `step "chat" failed: The workflow needs a devpod step`) {
		t.Fatalf("error %v", err)
	}
	if published {
		t.Fatal("publish ran after the terminal chat failure")
	}
	var metadata map[string]any
	_ = json.Unmarshal(store["runs/chat-failure/metadata.json"], &metadata)
	if metadata["status"] != RunFailed || !strings.Contains(fmt.Sprint(metadata["error"]), `step "chat" failed`) {
		t.Fatalf("metadata %s", store["runs/chat-failure/metadata.json"])
	}
}

// Coverage: two completions in a row end the step once, with the first file, and the run
// neither ends early nor hangs.
func TestInteractiveStepTakesTheFirstOfTwoCompletions(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	const session = "user-sessions/t12/chat"
	store := memStore{
		"runs/t12/draft":             []byte("draft"),
		session + "/files/first.md":  []byte("first"),
		session + "/files/second.md": []byte("second"),
	}
	env.RegisterActivity(&Activities{Store: store})
	publishes := 0
	env.RegisterActivityWithOptions(func(_ context.Context, req StepRequest) (StepResult, error) {
		if req.StepID == publishStep {
			publishes++
		}
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CompleteSignal, Completion{Step: "chat", File: "first.md", CompletedBy: testUser})
		env.SignalWorkflow(CompleteSignal, Completion{Step: "chat", File: "second.md", CompletedBy: "ada"})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CompleteSignal, Completion{Step: "chat", File: "second.md", CompletedBy: "ada"})
	}, 2*time.Second)

	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t12", Definition: interactive(), Inputs: runInput("runs/t12/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if string(store["runs/t12/chat"]) != "first" || store.metaOf("runs/t12/chat").Creator != testUser {
		t.Fatalf("output %q by %+v", store["runs/t12/chat"], store.metaOf("runs/t12/chat"))
	}
	if publishes != 1 {
		t.Fatalf("publish ran %d times", publishes)
	}
}

func TestOpenSessionNamesInputFilesByContent(t *testing.T) {
	store := memStore{"runs/t/a": []byte("plain text"), "runs/t/b": []byte(" {\"k\":1}\n"), "runs/t/input": []byte("")}
	a := &Activities{Store: store}
	res, err := a.OpenSession(context.Background(), &OpenSessionRequest{
		TraceID: "t", Session: "user-sessions/t/chat",
		Inputs: []Item{
			{Source: "a", Artifact: "runs/t/a", Round: 1},
			{Source: "route/b", Artifact: "runs/t/b", Round: 1},
			{Source: InputSource, Artifact: "runs/t/input", Round: 1},
			// A source that appears twice gets the round in its file names.
			{Source: "c", Artifact: "runs/t/a", Round: 1},
			{Source: "c", Artifact: "runs/t/b", Round: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Files, ","); got != "input-a.md,input-route-b.json,input-input.md,input-c-1.md,input-c-2.json" {
		t.Fatalf("files %s", got)
	}
	for _, name := range res.Files {
		if !SessionFilePattern.MatchString(name) {
			t.Errorf("%s is not a session file name", name)
		}
		if _, ok := store["user-sessions/t/chat/files/"+name]; !ok {
			t.Errorf("%s was not written", name)
		}
	}
	if _, err = a.OpenSession(context.Background(), &OpenSessionRequest{Session: "user-sessions/t/chat", Inputs: []Item{{Source: "x", Artifact: "runs/t/missing", Round: 1}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing input error %v", err)
	}
}

func TestSessionPath(t *testing.T) {
	if got := SessionPath(RunPrefix("t"), "chat"); got != "user-sessions/t/chat" {
		t.Errorf("top-level run: %s", got)
	}
	if got := SessionPath(ArtifactID(RunPrefix("t"), "route"), "chat"); got != "user-sessions/t/route/chat" {
		t.Errorf("routed child: %s", got)
	}
}

func TestRouteStartsTheNamedWorkflowAsAChild(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	store := memStore{}
	small := Definition{Name: "small-fix", Steps: []Step{{ID: "fix", Service: "fix"}}}
	env.RegisterActivity(&Activities{Store: store, LoadDefinitions: func() ([]Definition, error) {
		return []Definition{small}, nil
	}})
	env.RegisterWorkflowWithOptions(Pipeline, workflow.RegisterOptions{Name: WorkflowName})
	var mu sync.Mutex
	seen := map[string]StepRequest{}
	env.RegisterActivityWithOptions(func(ctx context.Context, req StepRequest) (StepResult, error) {
		mu.Lock()
		seen[req.StepID] = req
		mu.Unlock()
		if req.StepID == "triage" {
			_ = store.Put(ctx, req.OutputArtifact, []byte(`{"type":"bug","route":"small-fix"}`), Meta{TraceID: req.TraceID})
		}
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})
	def := Definition{Name: "intake", Steps: []Step{
		{ID: "triage", Service: "triage"},
		{ID: "route", Service: RouteService, DependsOn: []string{"triage"}},
	}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t7", Definition: def, Inputs: runInput("runs/t7/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Artifacts["route/fix"] != "runs/t7/route/fix" || res.Artifacts["triage"] != "runs/t7/triage" {
		t.Fatalf("artifacts %v", res.Artifacts)
	}
	fix := seen["fix"]
	// The routed workflow receives the item that named the route.
	if fix.Workflow != smallFix || fix.TraceID != "t7" || show(fix.Inputs) != "triage=runs/t7/triage#1" {
		t.Fatalf("fix request %+v", fix)
	}
}

func TestRouteRejectsUnknownWorkflow(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	store := memStore{"runs/t8/triage": []byte(`{"route":"nowhere"}`)}
	env.RegisterActivity(&Activities{Store: store, LoadDefinitions: func() ([]Definition, error) { return nil, nil }})
	env.RegisterActivityWithOptions(func(_ context.Context, req StepRequest) (StepResult, error) {
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})
	def := Definition{Name: "intake", Steps: []Step{
		{ID: "triage", Service: "triage"},
		{ID: "route", Service: RouteService, DependsOn: []string{"triage"}},
	}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t8", Definition: def, Inputs: runInput("runs/t8/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), "not a configured workflow") {
		t.Fatalf("error %v", err)
	}
}

// memStore is an in-memory Store that records metadata under ".meta/<id>".
type memStore map[string][]byte

func (m memStore) Put(_ context.Context, id string, d []byte, meta Meta) error {
	m[id] = d
	m[".meta/"+id] = []byte(meta.Creator + "\n" + meta.TraceID)
	return nil
}

func (m memStore) Get(_ context.Context, id string) ([]byte, error) {
	d, ok := m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return d, nil
}

type metaView struct{ Creator, TraceID string }

// meta returns the metadata recorded for id.
func (m memStore) metaOf(id string) metaView {
	parts := strings.SplitN(string(m[".meta/"+id]), "\n", 2)
	if len(parts) != 2 {
		return metaView{}
	}
	return metaView{Creator: parts[0], TraceID: parts[1]}
}

func TestStepActivityLoadsInputsAndStoresOutput(t *testing.T) {
	store := memStore{"runs/t/a": []byte("A"), "runs/t/b": []byte("B")}
	act := StepActivity(store, "joiner", func(_ context.Context, _ StepRequest, in [][]byte) ([]byte, error) {
		return append(append([]byte{}, in[0]...), in[1]...), nil
	})
	res, err := act(context.Background(), StepRequest{
		TraceID: "t", StepID: "c", OutputArtifact: "runs/t/c",
		Inputs: []Item{{Source: "a", Artifact: "runs/t/a", Round: 1}, {Source: "b", Artifact: "runs/t/b", Round: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Artifact != "runs/t/c" || string(store["runs/t/c"]) != "AB" {
		t.Fatalf("result %+v store %q", res, store["runs/t/c"])
	}
	if m := store.metaOf("runs/t/c"); m.Creator != "joiner" || m.TraceID != "t" {
		t.Fatalf("meta %+v", m)
	}
	if _, err = act(context.Background(), StepRequest{Inputs: []Item{{Source: "x", Artifact: "runs/t/missing", Round: 1}}, OutputArtifact: "runs/t/d"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing input error %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]Definition{
		"bad name":       {Name: "Bad Name", Steps: []Step{{ID: "a", Service: "s"}}},
		"no steps":       {Name: "w"},
		"duplicate id":   {Name: "w", Steps: []Step{{ID: "a", Service: "s"}, {ID: "a", Service: "s"}}},
		"reserved id":    {Name: "w", Steps: []Step{{ID: InputSource, Service: "s"}}},
		"no service":     {Name: "w", Steps: []Step{{ID: "a"}}},
		"unknown dep":    {Name: "w", Steps: []Step{{ID: "a", Service: "s", DependsOn: []string{"b"}}}},
		"bad timeout":    {Name: "w", Steps: []Step{{ID: "a", Service: "s", Timeout: "soon"}}},
		"neg attempts":   {Name: "w", Steps: []Step{{ID: "a", Service: "s", MaxAttempts: -1}}},
		"cycle":          {Name: "w", Steps: []Step{{ID: "a", Service: "s", DependsOn: []string{"b"}}, {ID: "b", Service: "s", DependsOn: []string{"a"}}}},
		"self reliance":  {Name: "w", Steps: []Step{{ID: "a", Service: "s", DependsOn: []string{"a"}}}},
		"agent no ref":   {Name: "w", Steps: []Step{{ID: "a", Service: AgentService}}},
		"agent bad ref":  {Name: "w", Steps: []Step{{ID: "a", Service: AgentService, Agent: "intake"}}},
		"route no dep":   {Name: "w", Steps: []Step{{ID: "r", Service: RouteService}}},
		"route two deps": {Name: "w", Steps: []Step{{ID: "a", Service: "s"}, {ID: "b", Service: "s"}, {ID: "r", Service: RouteService, DependsOn: []string{"a", "b"}}}},
		"after route":    {Name: "w", Steps: []Step{{ID: "a", Service: "s"}, {ID: "r", Service: RouteService, DependsOn: []string{"a"}}, {ID: "z", Service: "s", DependsOn: []string{"r"}}}},
		"chat no agent":  {Name: "w", Steps: []Step{{ID: "c", Service: InteractiveService, Agents: []string{"plain@v1"}}}},
		"chat bad agent": {Name: "w", Steps: []Step{{ID: "c", Service: InteractiveService, Agent: "triage"}}},
		"chat bad list":  {Name: "w", Steps: []Step{{ID: "c", Service: InteractiveService, Agent: "triage@v1", Agents: []string{"plain@v1", "plain"}}}},
		"chat timeout":   {Name: "w", Steps: []Step{{ID: "c", Service: InteractiveService, Agent: "triage@v1", Timeout: "soon"}}},
	}
	for name, d := range cases {
		if Validate(&d) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, d := range map[string]Definition{
		"diamond": diamond(),
		"gated":   gated(),
		"chat":    interactive(),
		"chat-1":  {Name: "w", Steps: []Step{{ID: "c", Service: InteractiveService, Agent: "folder/triage@v1", Timeout: "30s"}}},
		"chat-rt": {Name: "w", Steps: []Step{{ID: "c", Service: InteractiveService, Agent: "triage@v1"}, {ID: "r", Service: RouteService, DependsOn: []string{"c"}}}},
		"agent":   {Name: "w", Steps: []Step{{ID: "a", Service: AgentService, Agent: "intake@v12"}}},
		"route":   {Name: "w", Steps: []Step{{ID: "a", Service: "s"}, {ID: "r", Service: RouteService, DependsOn: []string{"a"}}}},
	} {
		if err := Validate(&d); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"", "..", "a/../b", "a//b", ".x", "a/b/", "a b"} {
		if ValidID(id) {
			t.Errorf("accepted %q", id)
		}
	}
	if !ValidID(ArtifactID(RunPrefix("0b7e3c1e-1111-4a4a-9b9b-123456789abc"), "top")) {
		t.Error("rejected artifact id")
	}
}

func TestResolveRouteAcceptsFencedJSON(t *testing.T) {
	store := memStore{"runs/t9/triage": []byte("```json\n{\"type\":\"bug\",\"route\":\"small-fix\"}\n```\n")}
	a := &Activities{Store: store, LoadDefinitions: func() ([]Definition, error) {
		return []Definition{{Name: "small-fix", Steps: []Step{{ID: "fix", Service: "fix"}}}}, nil
	}}
	res, err := a.ResolveRoute(context.Background(), RouteRequest{Inputs: []Item{{Source: "triage", Artifact: "runs/t9/triage", Round: 1}}})
	if err != nil || res.Definition.Name != smallFix {
		t.Fatalf("result %+v error %v", res, err)
	}
	if string(unfence([]byte(`{"a":1}`))) != `{"a":1}` {
		t.Error("unfenced content changed")
	}
}

func TestResolveRouteFollowsGateRecordToStepOutput(t *testing.T) {
	store := memStore{
		"runs/t10/triage":  []byte(`{"type":"bug","route":"small-fix"}`),
		"runs/t10/confirm": []byte(`{"decision":"approved","inputs":[{"source":"input","artifact":"runs/t10/input","round":1},{"source":"triage","artifact":"runs/t10/triage","round":1}]}`),
	}
	a := &Activities{Store: store, LoadDefinitions: func() ([]Definition, error) {
		return []Definition{{Name: "small-fix", Steps: []Step{{ID: "fix", Service: "agent", Agent: "fixer@v1"}}}}, nil
	}}
	res, err := a.ResolveRoute(context.Background(), RouteRequest{Inputs: []Item{{Source: "confirm", Artifact: "runs/t10/confirm", Round: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Definition.Name != smallFix || show(res.Inputs) != "triage=runs/t10/triage#1" {
		t.Fatalf("got %q from %v", res.Definition.Name, res.Inputs)
	}
}

func TestValidateAcceptsAgentsInFolders(t *testing.T) {
	for ref, ok := range map[string]bool{
		"intake@v1":                  true,
		"translation/en-2-de@v2":     true,
		"a/b/c/d@v1":                 true,
		"a/b/c/d/e@v1":               false,
		"translation/@v1":            false,
		"Translation/en@v1":          false,
		"translation/en-2-de@latest": false,
	} {
		err := Validate(&Definition{Name: "w", Steps: []Step{{ID: "a", Service: AgentService, Agent: ref}}})
		if (err == nil) != ok {
			t.Errorf("%s: error %v, want ok=%v", ref, err, ok)
		}
	}
}

func TestStepTimeoutDefaults(t *testing.T) {
	for _, c := range []struct {
		step Step
		want time.Duration
	}{
		{Step{ID: "a", Service: "agent"}, 2 * time.Minute},
		{Step{ID: "b", Service: "build"}, 10 * time.Minute},
		{Step{ID: "c", Service: "agent", Timeout: "30s"}, 30 * time.Second},
	} {
		got, err := c.step.StartToClose()
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", c.step.ID, got, err, c.want)
		}
	}
}

func TestNamesMayHaveWordsInCapitals(t *testing.T) {
	ok := Definition{Name: "API-review", Steps: []Step{{ID: "QA-check", Service: GateService}}}
	if err := Validate(&ok); err != nil {
		t.Fatalf("capital words rejected: %v", err)
	}
	dup := Definition{Name: "review", Steps: []Step{{ID: "QA-check", Service: GateService}, {ID: "qa-check", Service: GateService}}}
	if err := Validate(&dup); err == nil {
		t.Fatal("step ids that differ only in case were accepted")
	}
	input := Definition{Name: "review", Steps: []Step{{ID: "INPUT", Service: GateService}}}
	if err := Validate(&input); err == nil {
		t.Fatal("a step named like the run input was accepted")
	}
}

func TestValidateReservesSessionFolderNamesForInteractiveSteps(t *testing.T) {
	for _, id := range []string{"files", "chats", "Files"} {
		d := Definition{Name: "w", Steps: []Step{{ID: id, Service: InteractiveService, Agent: "triage@v1"}}}
		if err := Validate(&d); err == nil {
			t.Errorf("interactive step id %q must be refused", id)
		}
	}
	// Other kinds of step may still use the names.
	if err := Validate(&Definition{Name: "w", Steps: []Step{{ID: "files", Service: GateService}}}); err != nil {
		t.Errorf("a gate step named files must stay valid: %v", err)
	}
}
