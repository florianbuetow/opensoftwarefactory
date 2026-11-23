package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// podRun is the run ID of the dev pod tests; a work branch gets its first eight characters.
const podRun = "0b7e3c1e-1111-4a4a-9b9b-123456789abc"

// listStore is a syncStore that can also list the objects of a run, as the blobstore can.
type listStore struct{ *syncStore }

func (s listStore) TraceObjects(context.Context, string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.m))
	for id := range s.m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s listStore) text(id string) string {
	data, _ := s.Get(context.Background(), id)
	return string(data)
}

// fakeConnectors stands in for the connector service. calls lists what the orchestrator
// asked for, in order.
type fakeConnectors struct {
	mu        sync.Mutex
	calls     []string
	created   []CreatePod
	createErr error
	exec      map[string]PodExecResult
	requests  []PublishRequest
	decisions []PublishDecision
}

func (f *fakeConnectors) note(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeConnectors) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeConnectors) CreatePod(_ context.Context, req CreatePod) (CreatedPod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("create %s", req.Repository)
	if f.createErr != nil {
		return CreatedPod{}, f.createErr
	}
	f.created = append(f.created, req)
	return CreatedPod{ID: "pod-1", Reused: req.Name != "", StartCommit: "abc123"}, nil
}

func (f *fakeConnectors) Hold(_ context.Context, pod, run string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("hold %s %s", pod, run)
	return nil
}

func (f *fakeConnectors) Release(_ context.Context, pod, run string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("release %s %s", pod, run)
	return nil
}

func (f *fakeConnectors) Exec(_ context.Context, pod string, req PodExec) (PodExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("exec %s %s %d", pod, req.Command, req.TimeoutSeconds)
	return f.exec[req.Command], nil
}

func (f *fakeConnectors) DeletePod(_ context.Context, pod string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("delete %s", pod)
	return nil
}

func (f *fakeConnectors) PublishRequests(_ context.Context, pod, run string) ([]PublishRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("publish-requests %s %s", pod, run)
	return append([]PublishRequest{}, f.requests...), nil
}

func (f *fakeConnectors) DecidePublish(_ context.Context, pod string, n int, d PublishDecision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("decide %s %d %s", pod, n, d.Decision)
	f.decisions = append(f.decisions, d)
	for i := range f.requests {
		if f.requests[i].N == n {
			f.requests[i].State = d.Decision
		}
	}
	return nil
}

// agentStub stands in for the agent service. Every run of a step writes "<step> v<n>" as its
// output, with a run record, an attempt record, and prompt evidence beside it, as the agent
// service does.
type agentStub struct {
	mu      sync.Mutex
	store   Store
	calls   []StepRequest
	blocked map[string]string
}

func (a *agentStub) process(ctx context.Context, req StepRequest) (StepResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, req)
	n := 0
	for _, call := range a.calls {
		if call.StepID == req.StepID {
			n++
		}
	}
	reason := a.blocked[req.StepID]
	a.mu.Unlock()
	meta := Meta{Creator: "agent", TraceID: req.TraceID}
	text := fmt.Sprintf("%s v%d", req.StepID, n)
	for id, content := range map[string]string{
		req.OutputArtifact:                                        text,
		req.OutputArtifact + ".run.json":                          "record of " + text,
		req.OutputArtifact + ".attempt-1.run.json":                "attempt of " + text,
		evidenceBase(req.OutputArtifact) + "/attempt-1/system.md": "prompt of " + text,
	} {
		if err := a.store.Put(ctx, id, []byte(content), meta); err != nil {
			return StepResult{}, err
		}
	}
	return StepResult{Artifact: req.OutputArtifact, Blocked: reason}, nil
}

// checks returns the inputs of every run of the step "check".
func checks(a *agentStub) [][]Item {
	var out [][]Item
	for _, req := range a.runsOf("check") {
		out = append(out, req.Inputs)
	}
	return out
}

// runsOf returns the requests of a step, in the order they ran.
func (a *agentStub) runsOf(step string) []StepRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []StepRequest
	for _, call := range a.calls {
		if call.StepID == step {
			out = append(out, call)
		}
	}
	return out
}

// podEnv registers the pipeline, the team loop, the orchestrator's activities with a fake
// connector service, and an agent stub as the service of every agent step. The run has
// metadata, so its end is recorded.
func podEnv(t *testing.T, definitions ...Definition) (*testsuite.TestWorkflowEnvironment, listStore, *fakeConnectors, *agentStub) {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	store := listStore{newSyncStore()}
	connectors := &fakeConnectors{exec: map[string]PodExecResult{}}
	agents := &agentStub{store: store, blocked: map[string]string{}}
	env.RegisterActivity(&Activities{
		Store: store, Connectors: connectors, LoadDefinitions: func() ([]Definition, error) { return definitions, nil },
		LoadAgentSettings: func(context.Context, string) (AgentExecutionSettings, error) {
			return AgentExecutionSettings{Skills: map[string]string{"git": "v3"}, SkillScriptTimeoutSeconds: 300}, nil
		},
	})
	env.RegisterWorkflowWithOptions(Pipeline, workflow.RegisterOptions{Name: WorkflowName})
	env.RegisterWorkflowWithOptions(Team, workflow.RegisterOptions{Name: TeamWorkflowName})
	env.RegisterActivityWithOptions(agents.process, activity.RegisterOptions{Name: DefaultActivity})
	ctx := context.Background()
	_ = store.Put(ctx, "runs/"+podRun+"/input", []byte("task"), Meta{})
	_ = store.Put(ctx, "runs/"+podRun+"/metadata.json", []byte(`{"run_id":"`+podRun+`"}`), Meta{})
	return env, store, connectors, agents
}

func agentStep(id string, deps ...string) Step {
	return Step{ID: id, Service: AgentService, Agent: id + "@v1", DependsOn: deps}
}

func podStep() Step {
	return Step{
		ID: "pod", Service: DevPodService, Repository: "claims-build", SourceBranch: "main", WorkBranch: "agent/payment-date-{run}",
		Protected: []string{"tests/**", "verify.sh"},
		Rules:     []Rule{{Tool: "write_file", Path: "src/panel/**", Matches: "rejection ?reason", Message: "Blocked by CL-7: fix it."}},
	}
}

func runPod(env *testsuite.TestWorkflowEnvironment, def Definition) {
	env.ExecuteWorkflow(Pipeline, Input{TraceID: podRun, Definition: def, Inputs: runInput("runs/" + podRun + "/input")})
}

// metadataOf reads the status and error the run recorded in its metadata.
func metadataOf(t *testing.T, store listStore) (status, failure string, pod *DevPod) {
	t.Helper()
	var meta struct {
		Status string  `json:"status"`
		Error  string  `json:"error"`
		DevPod *DevPod `json:"dev_pod"`
	}
	if err := json.Unmarshal([]byte(store.text("runs/"+podRun+"/metadata.json")), &meta); err != nil {
		t.Fatal(err)
	}
	return meta.Status, meta.Error, meta.DevPod
}

func TestDevPodStepCreatesThePodAndHandsItToEveryLaterStep(t *testing.T) {
	env, store, connectors, agents := podEnv(t)
	connectors.exec["./verify.sh"] = PodExecResult{ExitCode: 0, Stdout: "ok\n"}
	def := Definition{Name: "build", Steps: []Step{
		agentStep("plan"),
		podStep(),
		agentStep("code", "pod", "plan"),
		{ID: "verify", Service: CommandService, Command: "./verify.sh", Timeout: "5m", DependsOn: []string{"code"}},
		{ID: "stop", Service: DevPodStopService, DependsOn: []string{"verify"}},
	}}
	runPod(env, def)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(connectors.created) != 1 {
		t.Fatalf("created %+v", connectors.created)
	}
	// The body of POST /api/devpods: the run placeholder is the first eight characters of the run ID.
	body, _ := json.Marshal(connectors.created[0])
	want := `{"repository":"claims-build","source_branch":"main","work_branch":"agent/payment-date-0b7e3c1e","run":"` + podRun + `",` +
		`"protected":["tests/**","verify.sh"],"rules":[{"tool":"write_file","path":"src/panel/**","matches":"rejection ?reason","message":"Blocked by CL-7: fix it."}]}`
	if string(body) != want {
		t.Errorf("create request\n got %s\nwant %s", body, want)
	}
	// The pod is created, held, used, deleted, and released at the end, in this order.
	calls := strings.Join(connectors.calls, "; ")
	if calls != "create claims-build; hold pod-1 "+podRun+"; publish-requests pod-1 "+podRun+"; exec pod-1 ./verify.sh 300; delete pod-1; release pod-1 "+podRun {
		t.Errorf("calls %s", calls)
	}
	var out DevPodOutput
	if err := json.Unmarshal([]byte(store.text("runs/"+podRun+"/pod")), &out); err != nil {
		t.Fatal(err)
	}
	if out != (DevPodOutput{Pod: "pod-1", Repository: "claims-build", SourceBranch: "main", WorkBranch: "agent/payment-date-0b7e3c1e", StartCommit: "abc123"}) {
		t.Errorf("pod output %+v", out)
	}
	if !strings.Contains(store.text("runs/"+podRun+"/pod"), `"start_commit": "abc123"`) || !strings.Contains(store.text("runs/"+podRun+"/pod"), `"reused": false`) {
		t.Errorf("pod document %s", store.text("runs/"+podRun+"/pod"))
	}
	// A step after the devpod step works in the pod; a step before it does not.
	if got := agents.runsOf("code")[0]; got.DevPod != "pod-1" || show(got.Inputs) != "pod=runs/"+podRun+"/pod#1 plan=runs/"+podRun+"/plan#1" {
		t.Errorf("code request %+v", got)
	}
	if got := agents.runsOf("plan")[0]; got.DevPod != "" {
		t.Errorf("plan request %+v", got)
	}
	request, _ := json.Marshal(agents.runsOf("code")[0])
	if !strings.Contains(string(request), `"dev_pod":"pod-1"`) {
		t.Errorf("Process request %s", request)
	}
	var command CommandOutput
	_ = json.Unmarshal([]byte(store.text("runs/"+podRun+"/verify")), &command)
	if command != (CommandOutput{Command: "./verify.sh", ExitCode: 0, Stdout: "ok\n"}) {
		t.Errorf("command output %+v", command)
	}
	if !strings.Contains(store.text("runs/"+podRun+"/stop"), `"stopped": true`) {
		t.Errorf("stop output %s", store.text("runs/"+podRun+"/stop"))
	}
	status, _, pod := metadataOf(t, store)
	if status != RunCompleted || pod == nil || *pod != (DevPod{ID: "pod-1", Repository: "claims-build", SourceBranch: "main", WorkBranch: "agent/payment-date-0b7e3c1e"}) {
		t.Errorf("metadata status %q pod %+v", status, pod)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if len(res.Artifacts) != 5 || res.Artifacts["verify"] != "runs/"+podRun+"/verify" {
		t.Errorf("artifacts %v", res.Artifacts)
	}
}

func TestCommandStepFailsTheRunOnANonZeroExitCodeUnlessItAllowsFailure(t *testing.T) {
	def := func(allow bool) Definition {
		return Definition{Name: "check", Steps: []Step{
			podStep(),
			{ID: "baseline", Service: CommandService, Command: "./verify.sh", AllowFailure: allow, DependsOn: []string{"pod"}},
			agentStep("code", "baseline"),
		}}
	}
	failing := PodExecResult{ExitCode: 3, Stdout: "1 test failed", Stderr: "boom"}

	env, store, connectors, agents := podEnv(t)
	connectors.exec["./verify.sh"] = failing
	runPod(env, def(false))
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `step "baseline" failed`) || !strings.Contains(err.Error(), `command "./verify.sh" ended with exit code 3`) {
		t.Fatalf("error %v", err)
	}
	if len(agents.runsOf("code")) != 0 {
		t.Error("the step after a failed command ran")
	}
	// The output document is stored although the step failed, and the default time limit is two minutes.
	var out CommandOutput
	_ = json.Unmarshal([]byte(store.text("runs/"+podRun+"/baseline")), &out)
	if out != (CommandOutput{Command: "./verify.sh", ExitCode: 3, Stdout: "1 test failed", Stderr: "boom"}) {
		t.Errorf("command output %+v", out)
	}
	if connectors.called("exec pod-1 ./verify.sh 120") != 1 || connectors.called("release pod-1 "+podRun) != 1 {
		t.Errorf("calls %v", connectors.calls)
	}
	if status, failure, _ := metadataOf(t, store); status != RunFailed || !strings.Contains(failure, "exit code 3") {
		t.Errorf("metadata %q %q", status, failure)
	}

	env, store, connectors, agents = podEnv(t)
	connectors.exec["./verify.sh"] = failing
	runPod(env, def(true))
	if err = env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if got := agents.runsOf("code"); len(got) != 1 || got[0].DevPod != "pod-1" || show(got[0].Inputs) != "baseline=runs/"+podRun+"/baseline#1" {
		t.Errorf("code requests %+v", got)
	}
	if !strings.Contains(store.text("runs/"+podRun+"/baseline"), `"exit_code": 3`) || connectors.called("release") != 1 {
		t.Errorf("output %s calls %v", store.text("runs/"+podRun+"/baseline"), connectors.calls)
	}
}

func TestDevPodStepFailsTheRunWhenThePodCannotBeCreated(t *testing.T) {
	env, store, connectors, agents := podEnv(t)
	connectors.createErr = &ConnectorError{Status: http.StatusConflict, Message: "the work branch equals the default branch"}
	runPod(env, Definition{Name: "build", Steps: []Step{podStep(), agentStep("code", "pod")}})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `step "pod" failed`) || !strings.Contains(err.Error(), "the work branch equals the default branch") {
		t.Fatalf("error %v", err)
	}
	// A refusal is final: the pod is asked for once, and there is nothing to hold or release.
	if strings.Join(connectors.calls, "; ") != "create claims-build" || len(agents.calls) != 0 {
		t.Errorf("calls %v, agent calls %d", connectors.calls, len(agents.calls))
	}
	if status, _, pod := metadataOf(t, store); status != RunFailed || pod != nil {
		t.Errorf("metadata %q %+v", status, pod)
	}
}

func TestCancelledRunReleasesThePod(t *testing.T) {
	env, store, connectors, _ := podEnv(t)
	def := Definition{Name: "build", Steps: []Step{
		podStep(), agentStep("code", "pod"),
		{ID: "review", Service: GateService, DependsOn: []string{"code"}},
	}}
	env.RegisterDelayedCallback(func() {
		if connectors.called("release") != 0 {
			t.Error("the pod was released while the run was open")
		}
		env.CancelWorkflow()
	}, time.Second)
	runPod(env, def)
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("the cancelled run completed")
	}
	if connectors.called("hold pod-1 "+podRun) != 1 || connectors.called("release pod-1 "+podRun) != 1 {
		t.Errorf("calls %v", connectors.calls)
	}
	if status, _, _ := metadataOf(t, store); status != RunCancelled {
		t.Errorf("metadata status %q", status)
	}
}

func TestPodReachesSessionsTeamsAndNestedWorkflows(t *testing.T) {
	inner := Definition{Name: "inner", Steps: []Step{agentStep("deep")}}
	def := Definition{Name: "outer", Steps: []Step{
		podStep(),
		{ID: "talk", Service: InteractiveService, Agent: "helper@v1", DependsOn: []string{"pod"}},
		{ID: "crew", Service: TeamService, Agent: "lead@v1", DependsOn: []string{"pod"}},
		{ID: "sub", Service: WorkflowService, Workflow: "inner", DependsOn: []string{"pod"}},
	}}
	env, store, connectors, agents := podEnv(t, inner, def)
	a := &Activities{Store: store}
	members := &fakeMembers{a: a, turns: map[string]int{}}
	var turns []TeamTurnRequest
	env.RegisterActivityWithOptions(func(ctx context.Context, req TeamTurnRequest) (map[string]int, error) {
		turns = append(turns, req)
		members.take(ctx, req.Team, req.Member)
		members.finish(ctx, req.Team, "done", "code.py")
		return map[string]int{"model_calls": 1}, nil
	}, activity.RegisterOptions{Name: TeamTurnActivity})
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Sessions) != 1 || state.Sessions[0].DevPod != "pod-1" || state.Sessions[0].Skills["git"] != "v3" || state.Sessions[0].SkillScriptTimeoutSeconds != 300 {
			t.Errorf("sessions %+v", state.Sessions)
		}
		_ = store.Put(context.Background(), "user-sessions/"+podRun+"/talk/files/result.md", []byte("result"), Meta{})
		env.SignalWorkflow(CompleteSignal, Completion{Step: "talk", File: "result.md", CompletedBy: "flo"})
	}, time.Second)
	runPod(env, def)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	// The session names the pod for the agent service's chat.
	if got := store.text("user-sessions/" + podRun + "/talk/session.json"); !strings.Contains(got, `"dev_pod": "pod-1"`) || !strings.Contains(got, `"trace_id": "`+podRun+`"`) {
		t.Errorf("session.json %s", got)
	}
	if len(turns) == 0 || turns[0].DevPod != "pod-1" || turns[0].Skills["git"] != "v3" || turns[0].SkillScriptTimeoutSeconds != 300 {
		t.Errorf("team turns %+v", turns)
	}
	if deep := agents.runsOf("deep"); len(deep) != 1 || deep[0].Skills["git"] != "v3" || deep[0].SkillScriptTimeoutSeconds != 300 {
		t.Errorf("nested process requests %+v", deep)
	}
	turn, _ := json.Marshal(turns[0])
	if !strings.Contains(string(turn), `"dev_pod":"pod-1"`) {
		t.Errorf("team turn request %s", turn)
	}
	if got := agents.runsOf("deep"); len(got) != 1 || got[0].DevPod != "pod-1" || got[0].Workflow != "inner" {
		t.Errorf("nested request %+v", got)
	}
	// Each of the three steps that worked in the pod asked for publish requests; the nested
	// run asked for its own agent step and releases nothing, since it did not create the pod.
	if connectors.called("publish-requests pod-1") != 3 || connectors.called("release") != 1 {
		t.Errorf("calls %v", connectors.calls)
	}
}

func TestBlockedStepEndsTheRunAsBlocked(t *testing.T) {
	env, store, connectors, agents := podEnv(t)
	agents.blocked["code"] = "the test service is down"
	def := Definition{Name: "build", Steps: []Step{
		podStep(), agentStep("code", "pod"),
		{ID: "verify", Service: CommandService, Command: "./verify.sh", DependsOn: []string{"code"}},
		agentStep("deliver", "verify"),
	}}
	runPod(env, def)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a blocked run is not a failed run: %v", err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Blocked != "the test service is down" || res.BlockedStep != "code" || res.Artifacts["code"] != "runs/"+podRun+"/code" {
		t.Errorf("result %+v", res)
	}
	if len(agents.runsOf("deliver")) != 0 || connectors.called("exec") != 0 || connectors.called("publish-requests") != 0 {
		t.Errorf("a step ran after the blocked one: %v", connectors.calls)
	}
	if connectors.called("release pod-1 "+podRun) != 1 {
		t.Errorf("a blocked run must release the pod: %v", connectors.calls)
	}
	if status, reason, _ := metadataOf(t, store); status != RunBlocked || reason != "the test service is down" {
		t.Errorf("metadata %q %q", status, reason)
	}
}

func TestBlockedNestedRunBlocksTheRunThatStartedIt(t *testing.T) {
	inner := Definition{Name: "inner", Steps: []Step{agentStep("deep")}}
	def := Definition{Name: "outer", Steps: []Step{
		{ID: "sub", Service: WorkflowService, Workflow: "inner"},
		agentStep("after", "sub"),
	}}
	env, store, _, agents := podEnv(t, inner, def)
	agents.blocked["deep"] = "no access"
	runPod(env, def)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Blocked != "no access" || res.BlockedStep != "sub" || len(agents.runsOf("after")) != 0 {
		t.Errorf("result %+v", res)
	}
	if status, reason, _ := metadataOf(t, store); status != RunBlocked || reason != "no access" {
		t.Errorf("metadata %q %q", status, reason)
	}
}

func TestBlockedTeamMemberBlocksTheRun(t *testing.T) {
	def := Definition{Name: "outer", Steps: []Step{
		{ID: "crew", Service: TeamService, Agent: "lead@v1"},
		agentStep("after", "crew"),
	}}
	env, store, _, agents := podEnv(t)
	env.RegisterActivityWithOptions(func(context.Context, TeamTurnRequest) (map[string]any, error) {
		return map[string]any{"model_calls": 1, "blocked": "no credentials"}, nil
	}, activity.RegisterOptions{Name: TeamTurnActivity})
	runPod(env, def)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Blocked != "no credentials" || res.BlockedStep != "crew" || len(agents.runsOf("after")) != 0 {
		t.Errorf("result %+v", res)
	}
	if status, reason, _ := metadataOf(t, store); status != RunBlocked || reason != "no credentials" {
		t.Errorf("metadata %q %q", status, reason)
	}
}

func TestPublishRequestsWaitForAPersonBeforeTheRunGoesOn(t *testing.T) {
	env, store, connectors, agents := podEnv(t)
	connectors.requests = []PublishRequest{
		{N: 1, Kind: PublishPush, State: PublishPending},
		{N: 2, Kind: PublishPullRequest, Title: "Add payment date", Body: "Shows the date.", State: PublishPending},
		{N: 3, Kind: PublishPush, State: "done"},
	}
	def := Definition{Name: "build", Steps: []Step{podStep(), agentStep("deliver", "pod"), agentStep("after", "deliver")}}
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 {
			t.Errorf("gates %+v", state.Gates)
			return
		}
		g := state.Gates[0]
		if g.Step != "deliver.publish-1" || g.Question != "Publish branch agent/payment-date-0b7e3c1e of claims-build?" ||
			g.Artifact != "runs/"+podRun+"/deliver.publish-1" || show(g.Inputs) != "deliver=runs/"+podRun+"/deliver#1" || g.OnReject != "" {
			t.Errorf("first decision %+v", g)
		}
		if g.Publish == nil || *g.Publish != (PublishInfo{Step: "deliver", Pod: "pod-1", N: 1, Kind: PublishPush, Repository: "claims-build", Branch: "agent/payment-date-0b7e3c1e"}) {
			t.Errorf("first request %+v", g.Publish)
		}
		if len(agents.runsOf("after")) != 0 {
			t.Error("the run went on before the publish request was decided")
		}
		env.SignalWorkflow(DecisionSignal, Decision{Step: "deliver.publish-1", Decision: Approved, Comment: "go", DecidedBy: "flo"})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 || state.Gates[0].Step != "deliver.publish-2" ||
			state.Gates[0].Question != "Publish branch agent/payment-date-0b7e3c1e of claims-build and open the pull request 'Add payment date'?" ||
			state.Gates[0].Publish.Body != "Shows the date." {
			t.Errorf("second decision %+v", state.Gates)
		}
		// A rejected request is not published, and the run goes on.
		env.SignalWorkflow(DecisionSignal, Decision{Step: "deliver.publish-2", Decision: Rejected, DecidedBy: "flo"})
	}, 2*time.Second)
	runPod(env, def)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(connectors.decisions) != 2 || connectors.decisions[0] != (PublishDecision{Decision: Approved, Comment: "go", DecidedBy: "flo"}) ||
		connectors.decisions[1] != (PublishDecision{Decision: Rejected, DecidedBy: "flo"}) {
		t.Errorf("decisions %+v", connectors.decisions)
	}
	if connectors.called("decide pod-1 1 approved") != 1 || connectors.called("decide pod-1 2 rejected") != 1 || connectors.called("decide") != 2 {
		t.Errorf("calls %v", connectors.calls)
	}
	if got := agents.runsOf("after"); len(got) != 1 || show(got[0].Inputs) != "deliver=runs/"+podRun+"/deliver#1" {
		t.Errorf("after %+v", got)
	}
	var rec DecisionRecord
	if err := json.Unmarshal([]byte(store.text("runs/"+podRun+"/deliver.publish-1")), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Decision != Approved || rec.DecidedBy != "flo" || rec.Step != "deliver.publish-1" || rec.Publish == nil || rec.Publish.N != 1 || !strings.HasPrefix(rec.Question, "Publish branch") {
		t.Errorf("record %+v", rec)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Artifacts["deliver.publish-2"] != "runs/"+podRun+"/deliver.publish-2" {
		t.Errorf("artifacts %v", res.Artifacts)
	}
}

// backEdge is build -> check -> review, with the gate's rejections going back to build. The
// step beside is not on a path from build to the gate, and after runs once the gate approves.
func backEdge() Definition {
	return Definition{Name: "iterate", Steps: []Step{
		agentStep("build"),
		agentStep("check", "build"),
		agentStep("beside", "build"),
		{ID: "review", Service: GateService, Question: "Good enough?", DependsOn: []string{"check"}, OnReject: "build", MaxRejections: 2},
		agentStep("after", "review"),
	}}
}

func TestRejectionGoesBackToAnAgentStepAndTheStepsBetweenRunAgain(t *testing.T) {
	env, store, _, agents := podEnv(t)
	run := "runs/" + podRun
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 || state.Gates[0].Round != 1 || state.Gates[0].OnReject != "build" || state.Gates[0].MaxRejections != 2 ||
			show(state.Gates[0].Inputs) != "check="+run+"/check#1" {
			t.Errorf("first round %+v", state.Gates)
		}
		// A rejection without a reason is ignored; the gate keeps waiting.
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, DecidedBy: "flo"})
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: "the date is missing", DecidedBy: "flo"})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 || state.Gates[0].Round != 2 || show(state.Gates[0].Inputs) != "check="+run+"/check#2" {
			t.Errorf("second round %+v", state.Gates)
		}
		value, err := env.QueryWorkflow(RoundsQuery)
		var rounds map[string]int
		if err != nil || value.Get(&rounds) != nil || len(rounds) != 2 || rounds["build"] != 2 || rounds["check"] != 2 {
			t.Errorf("rounds %v %v", rounds, err)
		}
		if len(agents.runsOf("after")) != 0 {
			t.Error("the step after the gate ran before the approval")
		}
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Approved, DecidedBy: "flo"})
	}, 2*time.Second)
	runPod(env, backEdge())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	// Round 2 of the target: its normal inputs, then the decision record, its own previous
	// output, and the document the person was shown, all as they were in round 1.
	builds := agents.runsOf("build")
	if len(builds) != 2 || show(builds[0].Inputs) != "input="+run+"/input#1" {
		t.Fatalf("build runs %+v", builds)
	}
	want := "input=" + run + "/input#1 review=" + run + "/review.1#1 build=" + run + "/build.round-1#1 check=" + run + "/check.round-1#1"
	if got := show(builds[1].Inputs); got != want {
		t.Errorf("round 2 inputs\n got %s\nwant %s", got, want)
	}
	// The added items say what they are; the normal items carry no kind.
	added, _ := json.Marshal(builds[1].Inputs)
	if string(added) != `[{"source":"input","artifact":"`+run+`/input","round":1},`+
		`{"source":"review","artifact":"`+run+`/review.1","round":1,"kind":"rejection"},`+
		`{"source":"build","artifact":"`+run+`/build.round-1","round":1,"kind":"previous"},`+
		`{"source":"check","artifact":"`+run+`/check.round-1","round":1,"kind":"reviewed"}]` {
		t.Errorf("round 2 items %s", added)
	}
	if kinds, _ := json.Marshal(checks(agents)); strings.Contains(string(kinds), `"kind"`) {
		t.Errorf("a step between got an item with a kind: %s", kinds)
	}
	if builds[1].OutputArtifact != run+"/build" {
		t.Errorf("round 2 writes %s", builds[1].OutputArtifact)
	}
	var first DecisionRecord
	if err := json.Unmarshal([]byte(store.text(run+"/review.1")), &first); err != nil {
		t.Fatal(err)
	}
	if first.Decision != Rejected || first.Comment != "the date is missing" || first.Round != 1 || first.OnReject != "build" {
		t.Errorf("first record %+v", first)
	}
	// The step between runs again on the new result; the step beside the path and the step
	// after the gate run once.
	checks := agents.runsOf("check")
	if len(checks) != 2 || show(checks[1].Inputs) != "build="+run+"/build#2" {
		t.Errorf("check runs %+v", checks)
	}
	if len(agents.runsOf("beside")) != 1 || len(agents.runsOf("after")) != 1 {
		t.Errorf("beside ran %d times, after %d times", len(agents.runsOf("beside")), len(agents.runsOf("after")))
	}
	// Nothing is overwritten: round 1 keeps its output, records, and evidence; the plain
	// names hold round 2 only.
	for id, content := range map[string]string{
		run + "/build.round-1":                               "build v1",
		run + "/build.round-1.run.json":                      "record of build v1",
		run + "/build.round-1.attempt-1.run.json":            "attempt of build v1",
		run + "/_evidence/build.round-1/attempt-1/system.md": "prompt of build v1",
		run + "/check.round-1":                               "check v1",
		run + "/check.round-1.run.json":                      "record of check v1",
		run + "/build":                                       "build v2",
		run + "/build.run.json":                              "record of build v2",
		run + "/_evidence/build/attempt-1/system.md":         "prompt of build v2",
		run + "/check":                                       "check v2",
		run + "/beside":                                      "beside v1",
	} {
		if got := store.text(id); got != content {
			t.Errorf("%s holds %q, want %q", id, got, content)
		}
	}
	if store.has(run+"/beside.round-1") || store.has(run+"/build.round-2") {
		t.Error("a step that did not run again was kept as a round")
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Rounds["build"] != 2 || res.Rounds["check"] != 2 || len(res.Rounds) != 2 || res.Artifacts["review"] != run+"/review" || res.Artifacts["build"] != run+"/build" {
		t.Errorf("result %+v", res)
	}
	var last DecisionRecord
	_ = json.Unmarshal([]byte(store.text(run+"/review")), &last)
	if last.Decision != Approved || last.Round != 2 {
		t.Errorf("last record %+v", last)
	}
}

func TestRejectionBeyondMaxRejectionsEndsTheRun(t *testing.T) {
	env, store, _, agents := podEnv(t)
	for round := 1; round <= 3; round++ {
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: fmt.Sprintf("no %d", round), DecidedBy: "flo"})
		}, time.Duration(round)*time.Second)
	}
	runPod(env, backEdge())
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `step "review" rejected 3 times, last by flo: no 3`) {
		t.Fatalf("error %v", err)
	}
	// Two rejections went back; the third ended the run and is the gate's own artifact.
	run := "runs/" + podRun
	if len(agents.runsOf("build")) != 3 || len(agents.runsOf("after")) != 0 {
		t.Errorf("build ran %d times", len(agents.runsOf("build")))
	}
	for _, id := range []string{run + "/review.1", run + "/review.2", run + "/review", run + "/build.round-1", run + "/build.round-2"} {
		if !store.has(id) {
			t.Errorf("%s is missing", id)
		}
	}
	if store.has(run+"/review.3") || store.has(run+"/build.round-3") {
		t.Error("the last rejection must not go back")
	}
	if got := show(agents.runsOf("build")[2].Inputs); got != "input="+run+"/input#1 review="+run+"/review.2#2 build="+run+"/build.round-2#2 check="+run+"/check.round-2#2" {
		t.Errorf("round 3 inputs %s", got)
	}
	if status, _, _ := metadataOf(t, store); status != RunFailed {
		t.Errorf("metadata status %q", status)
	}
}

func TestRejectionGoesBackToAnInteractiveStepAndReopensItsSession(t *testing.T) {
	env, store, _, agents := podEnv(t)
	run, session := "runs/"+podRun, "user-sessions/"+podRun+"/talk"
	ctx := context.Background()
	def := Definition{Name: "talks", Steps: []Step{
		{ID: "talk", Service: InteractiveService, Agent: "helper@v1"},
		agentStep("check", "talk"),
		{ID: "review", Service: GateService, DependsOn: []string{"talk", "check"}, OnReject: "talk"},
	}}
	env.RegisterDelayedCallback(func() {
		_ = store.Put(ctx, session+"/files/result.md", []byte("first draft"), Meta{})
		env.SignalWorkflow(CompleteSignal, Completion{Step: "talk", File: "result.md", CompletedBy: "flo"})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: "too short", DecidedBy: "ada"})
	}, 2*time.Second)
	env.RegisterDelayedCallback(func() {
		// The same session is open again, with the rejection and what was rejected in its files.
		state := pendingState(t, env)
		if len(state.Sessions) != 1 || state.Sessions[0].Session != session || len(state.Gates) != 0 {
			t.Errorf("after the rejection %+v", state)
			return
		}
		want := "input=" + run + "/input#1 review=" + run + "/review.1#1 talk=" + run + "/talk.round-1#1 check=" + run + "/check.round-1#1"
		if got := show(state.Sessions[0].Inputs); got != want {
			t.Errorf("session inputs\n got %s\nwant %s", got, want)
		}
		if got := store.text(session + "/files/rejection-1.md"); got != "# Rejection from \"review\" (round 1)\n\nRejected by: ada\n\ntoo short\n" {
			t.Errorf("rejection file %q", got)
		}
		if store.text(session+"/files/input-talk.md") != "first draft" || store.text(session+"/files/input-check.md") != "check v1" {
			t.Errorf("input files %q %q", store.text(session+"/files/input-talk.md"), store.text(session+"/files/input-check.md"))
		}
		if store.has(session+"/completed.json") || store.has(session+"/files/input-review.json") {
			t.Error("the session must be open, and the decision record is no input file")
		}
		if store.text(run+"/talk.round-1") != "first draft" {
			t.Errorf("kept output %q", store.text(run+"/talk.round-1"))
		}
		_ = store.Put(ctx, session+"/files/result.md", []byte("second draft"), Meta{})
		env.SignalWorkflow(CompleteSignal, Completion{Step: "talk", File: "result.md", CompletedBy: "flo"})
	}, 3*time.Second)
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 || state.Gates[0].Round != 2 || show(state.Gates[0].Inputs) != "talk="+run+"/talk#2 check="+run+"/check#2" {
			t.Errorf("second review %+v", state.Gates)
		}
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Approved, DecidedBy: "ada"})
	}, 4*time.Second)
	runPod(env, def)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if store.text(run+"/talk") != "second draft" || store.text(run+"/talk.round-1") != "first draft" || len(agents.runsOf("check")) != 2 {
		t.Errorf("outputs %q %q, check ran %d times", store.text(run+"/talk"), store.text(run+"/talk.round-1"), len(agents.runsOf("check")))
	}
}

func TestRejectionGoesBackToAWorkflowStep(t *testing.T) {
	inner := Definition{Name: "inner", Steps: []Step{agentStep("deep")}}
	def := Definition{Name: "outer", Steps: []Step{
		{ID: "sub", Service: WorkflowService, Workflow: "inner"},
		{ID: "review", Service: GateService, DependsOn: []string{"sub"}, OnReject: "sub"},
	}}
	env, store, _, agents := podEnv(t, inner, def)
	run := "runs/" + podRun
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: "again", DecidedBy: "flo"})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Approved, DecidedBy: "flo"})
	}, 2*time.Second)
	runPod(env, def)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	// The nested run starts again with the rejection and its previous output as input; the
	// objects of its first run are kept under <step>.round-1.
	deep := agents.runsOf("deep")
	if len(deep) != 2 || show(deep[1].Inputs) != "input="+run+"/input#1 review="+run+"/review.1#1 sub="+run+"/sub.round-1/deep#1" {
		t.Fatalf("nested runs %+v", deep)
	}
	if store.text(run+"/sub.round-1/deep") != "deep v1" || store.text(run+"/sub.round-1/deep.run.json") != "record of deep v1" ||
		store.text(run+"/sub/deep") != "deep v2" || store.text(run+"/sub/deep.run.json") != "record of deep v2" {
		t.Errorf("kept %q %q, current %q", store.text(run+"/sub.round-1/deep"), store.text(run+"/sub.round-1/deep.run.json"), store.text(run+"/sub/deep"))
	}
}

func TestKeepRoundsKeepsOnlyTheObjectsOfTheStep(t *testing.T) {
	store := listStore{newSyncStore()}
	a := &Activities{Store: store}
	ctx := context.Background()
	for _, id := range []string{
		"runs/t/build", "runs/t/build.run.json", "runs/t/build.attempt-2.run.json", "runs/t/_evidence/build/attempt-2/policy.md",
		"runs/t/build.round-1", "runs/t/_evidence/build.round-1/attempt-1/system.md", "runs/t/builder", "runs/t/_evidence/builder/attempt-1/system.md",
	} {
		_ = store.Put(ctx, id, []byte(id), Meta{})
	}
	req := KeepRoundsRequest{TraceID: "t", Steps: []KeptStep{{Base: "runs/t/build", Round: 2, Keep: []string{"runs/t/build"}}}}
	// A retry finds the records already moved and changes nothing more.
	for i := 0; i < 2; i++ {
		if err := a.KeepRounds(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	for id, source := range map[string]string{
		"runs/t/build.round-2":                               "runs/t/build",
		"runs/t/build.round-2.run.json":                      "runs/t/build.run.json",
		"runs/t/build.round-2.attempt-2.run.json":            "runs/t/build.attempt-2.run.json",
		"runs/t/_evidence/build.round-2/attempt-2/policy.md": "runs/t/_evidence/build/attempt-2/policy.md",
		"runs/t/build":                                       "runs/t/build",
		"runs/t/build.round-1":                               "runs/t/build.round-1",
		"runs/t/builder":                                     "runs/t/builder",
		"runs/t/_evidence/builder/attempt-1/system.md":       "runs/t/_evidence/builder/attempt-1/system.md",
	} {
		if got := store.text(id); got != source {
			t.Errorf("%s holds %q", id, got)
		}
	}
	// The records leave the plain name, so the next run starts clean; the output stays.
	for _, id := range []string{"runs/t/build.run.json", "runs/t/build.attempt-2.run.json", "runs/t/_evidence/build/attempt-2/policy.md", "runs/t/build.round-2.round-1", "runs/t/builder.round-2"} {
		if store.has(id) {
			t.Errorf("%s must not exist", id)
		}
	}
}

func TestValidateDevPodStepsAndBackEdges(t *testing.T) {
	pod := podStep()
	command := Step{ID: "verify", Service: CommandService, Command: "./verify.sh", DependsOn: []string{"code"}}
	stop := Step{ID: "stop", Service: DevPodStopService, DependsOn: []string{"verify"}}
	gate := func(target string, deps ...string) Step {
		return Step{ID: "review", Service: GateService, DependsOn: deps, OnReject: target}
	}
	change := func(s Step, edit func(*Step)) Step {
		edit(&s)
		return s
	}
	def := func(steps ...Step) Definition { return Definition{Name: "w", Steps: steps} }
	inner := Step{ID: "sub", Service: WorkflowService, Workflow: "inner"}
	talk := Step{ID: "talk", Service: InteractiveService, Agent: "helper@v1"}
	for name, d := range map[string]Definition{
		"pod":              def(pod, agentStep("code", "pod"), command, stop),
		"named pod":        def(change(pod, func(s *Step) { s.Name = "payment-date"; s.WorkBranch = "agent/payment-date" }), agentStep("code", "pod")),
		"command options":  def(pod, agentStep("code", "pod"), change(command, func(s *Step) { s.Timeout = "10m"; s.AllowFailure = true })),
		"back to agent":    def(pod, agentStep("code", "pod"), command, gate("code", "verify"), change(stop, func(s *Step) { s.DependsOn = []string{"review"} })),
		"back to workflow": def(inner, gate("sub", "sub")),
		"back to session":  def(talk, agentStep("code", "talk"), gate("talk", "code")),
		"max rejections":   def(agentStep("code"), change(gate("code", "code"), func(s *Step) { s.MaxRejections = 3 })),
		"node before both": def(work("a"), Step{ID: "collect", Service: MergeService, DependsOn: []string{"a"}}, agentStep("code", "collect"), gate("code", "code")),
	} {
		if err := Validate(d); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		def  Definition
		want string
	}{
		"two pods":            {def(pod, change(pod, func(s *Step) { s.ID = "second" })), "a workflow has only one"},
		"no repository":       {def(change(pod, func(s *Step) { s.Repository = "" })), "needs the repository"},
		"no source branch":    {def(change(pod, func(s *Step) { s.SourceBranch = "" })), "needs a source_branch"},
		"no work branch":      {def(change(pod, func(s *Step) { s.WorkBranch = "" })), "needs a work_branch"},
		"bad placeholder":     {def(change(pod, func(s *Step) { s.WorkBranch = "agent/{id}" })), "needs a work_branch"},
		"work equals source":  {def(change(pod, func(s *Step) { s.WorkBranch = "main" })), "must differ from the source_branch"},
		"bad pod name":        {def(change(pod, func(s *Step) { s.Name = "my pod" })), "name"},
		"pod with agent":      {def(change(pod, func(s *Step) { s.Agent = "coder@v1" })), "takes only repository"},
		"empty protected":     {def(change(pod, func(s *Step) { s.Protected = []string{" "} })), "empty protected path"},
		"rule tool":           {def(change(pod, func(s *Step) { s.Rules = []Rule{{Tool: "read_file", Message: "no"}} })), "tool must be write_file, edit_file, or run_command"},
		"rule message":        {def(change(pod, func(s *Step) { s.Rules = []Rule{{Tool: "edit_file"}} })), "needs a message"},
		"rule expression":     {def(change(pod, func(s *Step) { s.Rules = []Rule{{Tool: "run_command", Matches: "(", Message: "no"}} })), "not a regular expression"},
		"pod after a merge":   {def(work("a"), Step{ID: "collect", Service: MergeService, DependsOn: []string{"a"}}, change(pod, func(s *Step) { s.DependsOn = []string{"collect"} })), `cannot come after the merge step "collect"`},
		"command no pod":      {def(agentStep("code"), command), "must depend on the devpod step"},
		"command beside pod":  {def(pod, agentStep("code"), command), "must depend on the devpod step"},
		"stop beside pod":     {def(pod, agentStep("code"), change(stop, func(s *Step) { s.DependsOn = []string{"code"} })), "must depend on the devpod step"},
		"command no command":  {def(pod, agentStep("code", "pod"), change(command, func(s *Step) { s.Command = " " })), "needs the command it runs"},
		"command too long":    {def(pod, agentStep("code", "pod"), change(command, func(s *Step) { s.Timeout = "11m" })), "must not be longer than"},
		"repository on agent": {def(change(agentStep("code"), func(s *Step) { s.Repository = "claims-build" })), "only a devpod step"},
		"rules on agent":      {def(change(agentStep("code"), func(s *Step) { s.Rules = []Rule{{Tool: "edit_file", Message: "no"}} })), "only a devpod step"},
		"command on agent":    {def(change(agentStep("code"), func(s *Step) { s.Command = "ls" })), "only a command step"},
		"allow on gate":       {def(Step{ID: "review", Service: GateService, AllowFailure: true}), "only a command step"},
		"limit without edge":  {def(agentStep("code"), Step{ID: "review", Service: GateService, DependsOn: []string{"code"}, MaxRejections: 2}), "only a gate step with on_reject"},
		"limit on agent":      {def(change(agentStep("code"), func(s *Step) { s.MaxRejections = 2 })), "only a gate step with on_reject"},
		"negative limit":      {def(agentStep("code"), change(gate("code", "code"), func(s *Step) { s.MaxRejections = -1 })), "max_rejections must be a positive whole number"},
		"back to unknown":     {def(agentStep("code"), gate("gone", "code")), "on_reject must name an agent, interactive, team, or workflow step"},
		"back to itself":      {def(agentStep("code"), gate("review", "code")), "on_reject must name an agent, interactive, team, or workflow step"},
		"back to no ancestor": {def(agentStep("code"), agentStep("other"), gate("other", "code")), "on_reject must name an agent, interactive, team, or workflow step"},
		"back to plain step":  {def(work("code"), gate("code", "code")), "on_reject must name an agent, interactive, team, or workflow step"},
		"back to gate":        {def(Step{ID: "first", Service: GateService}, gate("first", "first")), "on_reject must name an agent, interactive, team, or workflow step"},
		"back to command":     {def(pod, agentStep("code", "pod"), command, gate("verify", "verify")), "on_reject must name an agent, interactive, team, or workflow step"},
		"back to pod":         {def(pod, agentStep("code", "pod"), gate("pod", "code")), "on_reject must name an agent, interactive, team, or workflow step"},
		"back to stop":        {def(pod, agentStep("code", "pod"), command, stop, gate("stop", "stop")), "on_reject must name an agent, interactive, team, or workflow step"},
		"back to merge": {
			def(work("a"), Step{ID: "collect", Service: MergeService, DependsOn: []string{"a"}}, agentStep("code", "collect"), gate("collect", "code")),
			"on_reject must name an agent, interactive, team, or workflow step",
		},
		"back to batch": {
			def(work("a"), Step{ID: "gather", Service: BatchService, Size: 1, DependsOn: []string{"a"}}, agentStep("code", "gather"), gate("gather", "code")),
			"on_reject must name an agent, interactive, team, or workflow step",
		},
		"pod between":  {def(agentStep("plan"), change(pod, func(s *Step) { s.DependsOn = []string{"plan"} }), agentStep("code", "pod"), gate("plan", "code")), `the devpod step "pod" lies between them`},
		"stop between": {def(pod, agentStep("code", "pod"), change(stop, func(s *Step) { s.DependsOn = []string{"code"} }), gate("code", "stop")), `the devpod-stop step "stop" lies between them`},
		"merge between": {
			def(agentStep("code"), Step{ID: "collect", Service: MergeService, DependsOn: []string{"code"}}, agentStep("sum", "collect"), gate("code", "sum")),
			`the merge step "collect" lies between them`,
		},
		"batch between": {
			def(agentStep("code"), Step{ID: "gather", Service: BatchService, Size: 1, DependsOn: []string{"code"}}, agentStep("sum", "gather"), gate("code", "sum")),
			`the batch step "gather" lies between them`,
		},
		"merge collects a step that runs again": {
			def(agentStep("code"), agentStep("check", "code"), gate("code", "check"), Step{ID: "collect", Service: MergeService, DependsOn: []string{"check"}}),
			`the merge step "collect" lies between them or collects a step that runs again`,
		},
	} {
		if err := Validate(tc.def); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v", name, err)
		}
	}
}

func TestWorkBranchName(t *testing.T) {
	if got := WorkBranchName("agent/payment-date-{run}", podRun); got != "agent/payment-date-0b7e3c1e" {
		t.Errorf("long run id: %s", got)
	}
	if got := WorkBranchName("agent/{run}/{run}", "t1"); got != "agent/t1/t1" {
		t.Errorf("short run id: %s", got)
	}
}

func TestHTTPConnectorsSendTheRequestsOfTheContract(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+string(body)))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/devpods":
			_, _ = w.Write([]byte(`{"id":"pod-1","reused":true,"start_commit":"abc"}`))
		case strings.HasSuffix(r.URL.Path, "/exec"):
			_, _ = w.Write([]byte(`{"exit_code":2,"stdout":"out","stderr":"err"}`))
		case strings.HasSuffix(r.URL.Path, "/publish-requests"):
			_, _ = w.Write([]byte(`[{"n":1,"kind":"push","state":"pending"},{"n":2,"kind":"pull_request","title":"T","body":"B","state":"done","result":{"url":"x"}}]`))
		case r.URL.Path == "/api/devpods/gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such dev pod"}`))
		}
	}))
	defer srv.Close()
	c := HTTPConnectors{BaseURL: srv.URL, Client: srv.Client()}
	ctx := context.Background()
	created, err := c.CreatePod(ctx, CreatePod{Repository: "claims-build", SourceBranch: "main", WorkBranch: "agent/x", Name: "x", Run: "r1"})
	if err != nil || created != (CreatedPod{ID: "pod-1", Reused: true, StartCommit: "abc"}) {
		t.Fatalf("create %+v %v", created, err)
	}
	if err = c.Hold(ctx, "pod-1", "r1"); err != nil {
		t.Fatal(err)
	}
	res, err := c.Exec(ctx, "pod-1", PodExec{Command: "./verify.sh", TimeoutSeconds: 300})
	if err != nil || res != (PodExecResult{ExitCode: 2, Stdout: "out", Stderr: "err"}) {
		t.Fatalf("exec %+v %v", res, err)
	}
	requests, err := c.PublishRequests(ctx, "pod-1", "r1")
	if err != nil || len(requests) != 2 || requests[1] != (PublishRequest{N: 2, Kind: PublishPullRequest, Title: "T", Body: "B", State: "done"}) {
		t.Fatalf("publish requests %+v %v", requests, err)
	}
	if err = c.DecidePublish(ctx, "pod-1", 2, PublishDecision{Decision: Approved, Comment: "go", DecidedBy: "flo"}); err != nil {
		t.Fatal(err)
	}
	if err = c.Release(ctx, "pod-1", "r1"); err != nil {
		t.Fatal(err)
	}
	if err = c.DeletePod(ctx, "pod-1"); err != nil {
		t.Fatal(err)
	}
	var refused *ConnectorError
	if err = c.DeletePod(ctx, "gone"); err == nil || !strings.Contains(err.Error(), "no such dev pod") {
		t.Fatalf("missing pod: %v", err)
	} else if refused, _ = err.(*ConnectorError); refused == nil || refused.Status != http.StatusNotFound {
		t.Fatalf("missing pod error %#v", err)
	}
	want := []string{
		`POST /api/devpods {"repository":"claims-build","source_branch":"main","work_branch":"agent/x","name":"x","run":"r1"}`,
		`POST /api/devpods/pod-1/hold {"run":"r1"}`,
		`POST /api/devpods/pod-1/exec {"command":"./verify.sh","timeout_seconds":300}`,
		`GET /api/devpods/pod-1/publish-requests?run=r1`,
		`POST /api/devpods/pod-1/publish-requests/2/decision {"decision":"approved","comment":"go","decided_by":"flo"}`,
		`POST /api/devpods/pod-1/release {"run":"r1"}`,
		`DELETE /api/devpods/pod-1`,
		`DELETE /api/devpods/gone`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n got %s\nwant %s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestStopAndReleaseTreatAMissingPodAsDone(t *testing.T) {
	store := listStore{newSyncStore()}
	a := &Activities{Store: store, Connectors: missingPod{&fakeConnectors{}}}
	ctx := context.Background()
	if _, err := a.StopDevPod(ctx, StopDevPodRequest{TraceID: "t", Pod: "pod-1", Artifact: "runs/t/stop"}); err != nil || !store.has("runs/t/stop") {
		t.Fatalf("stop %v", err)
	}
	if err := a.ReleaseDevPod(ctx, ReleaseDevPodRequest{Pod: "pod-1", Run: "t"}); err != nil {
		t.Fatalf("release %v", err)
	}
	if _, err := (&Activities{Store: store}).RunCommand(ctx, CommandRequest{Pod: "pod-1", Command: "ls"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("no connector service: %v", err)
	}
}

// missingPod answers every call about a pod with 404.
type missingPod struct{ *fakeConnectors }

func (missingPod) DeletePod(context.Context, string) error {
	return &ConnectorError{Status: http.StatusNotFound, Message: "no such dev pod"}
}

func (missingPod) Release(context.Context, string, string) error {
	return &ConnectorError{Status: http.StatusNotFound, Message: "no such dev pod"}
}
