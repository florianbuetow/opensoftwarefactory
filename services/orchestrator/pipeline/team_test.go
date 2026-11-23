package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// syncStore is an in-memory Store that parallel team turns can share and that can delete.
type syncStore struct {
	m  map[string][]byte
	mu sync.Mutex
}

func newSyncStore() *syncStore { return &syncStore{m: map[string][]byte{}} }

func (s *syncStore) Put(_ context.Context, id string, d []byte, _ Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = d
	return nil
}

func (s *syncStore) Get(_ context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return d, nil
}

func (s *syncStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
}

func (s *syncStore) has(id string) bool {
	_, err := s.Get(context.Background(), id)
	return err == nil
}

// fakeMembers stands in for the agent service and the tool-gateway: take delivers a member's
// unread messages and advances its read markers, send posts into a channel, and finish
// writes finished.json.
type fakeMembers struct {
	a     *Activities
	turns map[string]int
	mu    sync.Mutex
}

// delivered is a message as a member receives it: with the sender, which the queue names.
type delivered struct {
	From string
	TeamMessage
}

func (f *fakeMembers) take(ctx context.Context, team, member string) []delivered {
	var manifest TeamManifest
	_, _ = f.a.getJSON(ctx, team+"/team.json", &manifest)
	file := teamMemberFile{Member: member, Read: map[string]int{}}
	_, _ = f.a.getJSON(ctx, team+"/members/"+member+".json", &file)
	var out []delivered
	for _, c := range manifest.Channels {
		if !contains(c.Members, member) {
			continue
		}
		for _, m := range unread(f.queue(ctx, team, c.ID, c.other(member)), file.Read[c.ID]) {
			out = append(out, delivered{TeamMessage: m, From: c.other(member)})
			file.Read[c.ID] = m.Seq
		}
	}
	_ = f.a.putJSON(ctx, team+"/members/"+member+".json", file, Meta{})
	return out
}

func (f *fakeMembers) queue(ctx context.Context, team, channel, sender string) TeamQueue {
	var queue TeamQueue
	_, _ = f.a.getJSON(ctx, queueID(team, channel, sender), &queue)
	return queue
}

func (f *fakeMembers) send(ctx context.Context, team, from, to, text string) {
	_ = f.a.postMessage(ctx, team, TeamChannel{ID: ChannelID(from, to), Members: []string{from, to}}, from, text, Meta{}, false, "")
}

func (f *fakeMembers) finish(ctx context.Context, team, summary string, files ...string) {
	for _, name := range files {
		_ = f.a.Store.Put(ctx, team+"/files/"+name, []byte("content of "+name), Meta{})
	}
	_ = f.a.putJSON(ctx, team+"/finished.json", TeamFinished{Summary: summary, Files: files}, Meta{})
}

func (f *fakeMembers) count(member string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns[member]++
	return f.turns[member]
}

// teamEnv registers the pipeline, the team loop, the orchestrator's activities, an "echo"
// step service, and turn as the agent service's TeamTurn activity.
func teamEnv(t *testing.T, definitions []Definition, turn func(ctx context.Context, f *fakeMembers, req *TeamTurnRequest) error) (*testsuite.TestWorkflowEnvironment, *syncStore, *fakeMembers) {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	store := newSyncStore()
	a := &Activities{
		Store: store, LoadDefinitions: func() ([]Definition, error) { return definitions, nil },
		DescribeAgent: func(_ context.Context, ref string) string { return "about " + ref },
	}
	f := &fakeMembers{a: a, turns: map[string]int{}}
	env.RegisterActivity(a)
	env.RegisterWorkflowWithOptions(Pipeline, workflow.RegisterOptions{Name: WorkflowName})
	env.RegisterWorkflowWithOptions(Team, workflow.RegisterOptions{Name: TeamWorkflowName})
	env.RegisterActivityWithOptions(StepActivity(store, "echo", func(_ context.Context, _ StepRequest, in [][]byte) ([]byte, error) {
		return []byte("echoed: " + string(in[0])), nil
	}), activity.RegisterOptions{Name: DefaultActivity})
	env.RegisterActivityWithOptions(func(ctx context.Context, req TeamTurnRequest) (map[string]int, error) {
		return map[string]int{"model_calls": 1}, turn(ctx, f, &req)
	}, activity.RegisterOptions{Name: TeamTurnActivity})
	return env, store, f
}

// cooperative is a team whose lead instructs the coder and the echo workflow, finishes once
// both have replied, and finishes again after a rejection.
func cooperative(ctx context.Context, f *fakeMembers, req *TeamTurnRequest) error {
	f.count(req.Member)
	messages := f.take(ctx, req.Team, req.Member)
	switch req.Member {
	case leadMember:
		for _, m := range messages {
			switch {
			case m.From == WorkflowSender && strings.HasPrefix(m.Text, "Review round"):
				f.finish(ctx, req.Team, "revised", "code.py", "test_code.py")
			case m.From == WorkflowSender:
				f.send(ctx, req.Team, "lead", "coder", "write the code")
				f.send(ctx, req.Team, "lead", "echo", "ping")
			}
		}
		store, ok := f.a.Store.(*syncStore)
		if !ok {
			return fmt.Errorf("the team keeps its blobs in a %T, not in a syncStore", f.a.Store)
		}
		coder, echo := f.queue(ctx, req.Team, coderLeadChannel, coderMember), f.queue(ctx, req.Team, "echo--lead", echoMember)
		if len(coder.Messages) > 0 && len(echo.Messages) > 0 && !store.has(req.Team+"/finished.json") {
			f.finish(ctx, req.Team, "done", "code.py", "test_code.py")
		}
	case coderMember:
		for range messages {
			f.send(ctx, req.Team, "coder", "lead", "code is ready")
		}
	}
	return nil
}

var echoWorkflow = Definition{Name: "echo", Description: "Echoes its input.", Steps: []Step{{ID: "say", Service: "echo"}}}

func teamStep() Step {
	return Step{ID: "build", Service: TeamService, Agent: "demo/lead@v1", Agents: []string{"demo/coder@v1"}, Workflows: []string{"echo"}}
}

func TestTeamRunsAgentAndWorkflowMembersUntilTheLeadFinishes(t *testing.T) {
	env, store, f := teamEnv(t, []Definition{echoWorkflow}, cooperative)
	ctx := context.Background()
	_ = store.Put(ctx, "runs/t1/input", []byte("Build a parser."), Meta{})
	env.RegisterDelayedCallback(func() {
		// The lead's first turn is over, so the team is listed as running until it finishes.
		if state := pendingState(t, env); len(state.Teams) > 1 {
			t.Errorf("teams %+v", state.Teams)
		}
	}, time.Second)
	def := Definition{Name: "solo", Steps: []Step{teamStep()}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t1", Definition: def, Inputs: runInput("runs/t1/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Artifacts["build"] != "runs/t1/build" {
		t.Fatalf("artifacts %v", res.Artifacts)
	}
	var out TeamOutput
	if err := json.Unmarshal(store.m["runs/t1/build"], &out); err != nil {
		t.Fatal(err)
	}
	if out.Summary != "done" || out.Team != "teams/t1/build" || len(out.Files) != 2 || out.Files[0] != (TeamOutputFile{Name: "code.py", Artifact: "teams/t1/build/files/code.py"}) {
		t.Fatalf("output %+v", out)
	}
	var manifest TeamManifest
	_, _ = f.a.getJSON(ctx, "teams/t1/build/team.json", &manifest)
	if manifest.Lead != leadMember || manifest.TraceID != "t1" || manifest.StepID != buildStep || len(manifest.Members) != 3 || len(manifest.Channels) != 3 {
		t.Fatalf("manifest %+v", manifest)
	}
	if m := manifest.Members[0]; m != (TeamMember{Name: "lead", Kind: AgentMember, Ref: "demo/lead@v1", Description: "about demo/lead@v1"}) {
		t.Errorf("lead %+v", m)
	}
	if m := manifest.Members[2]; m != (TeamMember{Name: "echo", Kind: WorkflowMember, Ref: "echo", Description: "Echoes its input."}) {
		t.Errorf("workflow member %+v", m)
	}
	task := f.queue(ctx, "teams/t1/build", "lead--workflow", WorkflowSender)
	if len(task.Messages) != 1 || task.Messages[0].Seq != 1 || task.Channel != "lead--workflow" || task.From != WorkflowSender || task.To != leadMember || !strings.Contains(task.Messages[0].Text, "Build a parser.") {
		t.Fatalf("task channel %+v", task)
	}
	if string(store.m["teams/t1/build/files/input-input.md"]) != "Build a parser." {
		t.Error("the input was not copied into the workspace")
	}
	// The workflow member ran as a child run on the lead's message and replied with its output.
	// Each side of the channel has its own queue, counted from 1.
	echo := f.queue(ctx, "teams/t1/build", "echo--lead", "echo")
	if len(echo.Messages) != 1 || echo.From != echoMember || echo.To != leadMember || echo.Messages[0].Text != "echoed: ping" || echo.Messages[0].Seq != 1 {
		t.Fatalf("echo queue %+v", echo)
	}
	if asked := f.queue(ctx, "teams/t1/build", "echo--lead", "lead"); len(asked.Messages) != 1 || asked.Messages[0].Text != "ping" || asked.Messages[0].Seq != 1 {
		t.Fatalf("lead queue %+v", asked)
	}
	if store.has("teams/t1/build/channels/echo--lead.json") || store.has("teams/t1/build/channels/lead--workflow.json") {
		t.Error("a channel is stored as one queue per sender, not as one file")
	}
	if string(store.m["runs/t1/build.echo.r1.1/input"]) != "ping" || !store.has("runs/t1/build.echo.r1.1/say") {
		t.Error("the workflow member's run did not store its artifacts beside the team step")
	}
	var member teamMemberFile
	_, _ = f.a.getJSON(ctx, "teams/t1/build/members/echo.json", &member)
	if member.Member != echoMember || member.Read["echo--lead"] != 1 {
		t.Fatalf("workflow member file %+v", member)
	}
	if f.turns["coder"] != 1 {
		t.Errorf("turns %v", f.turns)
	}
}

func TestTeamFailsWhenItStalls(t *testing.T) {
	env, store, _ := teamEnv(t, nil, func(ctx context.Context, f *fakeMembers, req *TeamTurnRequest) error {
		f.take(ctx, req.Team, req.Member) // reads the task, sends nothing, never finishes
		return nil
	})
	_ = store.Put(context.Background(), "runs/t2/input", []byte("task"), Meta{})
	def := Definition{Name: "solo", Steps: []Step{{ID: "build", Service: TeamService, Agent: "lead@v1", Agents: []string{"coder@v1"}}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t2", Definition: def, Inputs: runInput("runs/t2/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `team "build" stalled`) || !strings.Contains(err.Error(), `lead agent "lead" has not finished`) {
		t.Fatalf("error %v", err)
	}
}

func TestTeamFailsWhenItExceedsItsTurnBudget(t *testing.T) {
	env, store, f := teamEnv(t, nil, func(_ context.Context, f *fakeMembers, req *TeamTurnRequest) error {
		f.count(req.Member) // never reads its messages
		return nil
	})
	_ = store.Put(context.Background(), "runs/t3/input", []byte("task"), Meta{})
	def := Definition{Name: "solo", Steps: []Step{{ID: "build", Service: TeamService, Agent: "lead@v1", MaxTurns: 3}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t3", Definition: def, Inputs: runInput("runs/t3/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `team "build" used its budget of 3 turns`) {
		t.Fatalf("error %v", err)
	}
	if f.turns["lead"] != 3 {
		t.Errorf("turns %v", f.turns)
	}
}

func TestTeamFailsWithTheErrorOfAFailedTurn(t *testing.T) {
	env, store, _ := teamEnv(t, nil, func(context.Context, *fakeMembers, *TeamTurnRequest) error {
		return fmt.Errorf("model unavailable")
	})
	_ = store.Put(context.Background(), "runs/t4/input", []byte("task"), Meta{})
	def := Definition{Name: "solo", Steps: []Step{{ID: "build", Service: TeamService, Agent: "lead@v1"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t4", Definition: def, Inputs: runInput("runs/t4/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `turn of team member "lead" failed`) || !strings.Contains(err.Error(), "model unavailable") {
		t.Fatalf("error %v", err)
	}
}

func reviewed() Definition {
	return Definition{Name: "reviewed", Steps: []Step{
		teamStep(),
		{ID: "review", Service: GateService, Question: "Good?", DependsOn: []string{"build"}, OnReject: "build"},
	}}
}

func TestGateOnRejectSendsTheReasonBackToTheTeamAndReopens(t *testing.T) {
	env, store, f := teamEnv(t, []Definition{echoWorkflow}, cooperative)
	ctx := context.Background()
	_ = store.Put(ctx, "runs/t5/input", []byte("Build a parser."), Meta{})
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 || state.Gates[0].OnReject != buildStep || state.Gates[0].Round != 1 || len(state.Teams) != 0 {
			t.Errorf("first round %+v", state)
		}
		// A rejection without a reason is ignored; the gate keeps waiting.
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: " ", DecidedBy: testUser})
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: "handle empty input", DecidedBy: testUser})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 || state.Gates[0].Round != 2 || show(state.Gates[0].Inputs) != "build=runs/t5/build#1" {
			t.Errorf("second round %+v", state)
		}
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Approved, DecidedBy: testUser})
	}, 2*time.Second)
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t5", Definition: reviewed(), Inputs: runInput("runs/t5/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Artifacts["review"] != "runs/t5/review" || res.Artifacts["build"] != "runs/t5/build" {
		t.Fatalf("artifacts %v", res.Artifacts)
	}
	var first, last DecisionRecord
	if err := json.Unmarshal(store.m["runs/t5/review.1"], &first); err != nil {
		t.Fatal(err)
	}
	if first.Decision != Rejected || first.Comment != "handle empty input" || first.Round != 1 || first.OnReject != buildStep {
		t.Fatalf("first record %+v", first)
	}
	_ = json.Unmarshal(store.m["runs/t5/review"], &last)
	if last.Decision != Approved || last.Round != 2 {
		t.Fatalf("last record %+v", last)
	}
	task := f.queue(ctx, "teams/t5/build", "lead--workflow", WorkflowSender)
	if len(task.Messages) != 2 || task.Messages[1].Seq != 2 ||
		task.Messages[1].Text != "Review round 1: the review gate \"Review\" rejected the team's result.\n\n**Rejected by:** flo\n\n**Reason:**\n\nhandle empty input\n\nIncorporate this with your team, then finish the team again." {
		t.Fatalf("task channel %+v", task)
	}
	var out TeamOutput
	_ = json.Unmarshal(store.m["runs/t5/build"], &out)
	if out.Summary != "revised" {
		t.Fatalf("output after the rejection %+v", out)
	}
}

// A gate sends five rejections back by default; the sixth ends the run.
func TestGateOnRejectFailsTheRunWithTheRejectionBeyondItsLimit(t *testing.T) {
	env, store, _ := teamEnv(t, []Definition{echoWorkflow}, cooperative)
	_ = store.Put(context.Background(), "runs/t6/input", []byte("task"), Meta{})
	for round := 1; round <= DefaultMaxRejections+1; round++ {
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: fmt.Sprintf("no %d", round), DecidedBy: testUser})
		}, time.Duration(round)*time.Second)
	}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t6", Definition: reviewed(), Inputs: runInput("runs/t6/input")})
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), `step "review" rejected 6 times, last by flo: no 6`) {
		t.Fatalf("error %v", err)
	}
	for _, id := range []string{"runs/t6/review.1", "runs/t6/review.5", "runs/t6/review"} {
		if !store.has(id) {
			t.Errorf("record %s is missing", id)
		}
	}
	if store.has("runs/t6/review.6") {
		t.Error("the last rejection belongs in the gate's own artifact")
	}
}

func TestWorkflowStepRunsTheNamedWorkflowAndHandsOnItsFinalOutput(t *testing.T) {
	inner := Definition{Name: "inner", Steps: []Step{
		{ID: "first", Service: "echo"},
		{ID: "ask", Service: GateService, DependsOn: []string{"first"}},
		{ID: "last", Service: "echo", DependsOn: []string{"ask"}},
	}}
	outer := Definition{Name: "outer", Steps: []Step{
		{ID: "nested", Service: WorkflowService, Workflow: "inner"},
		{ID: "after", Service: "echo", DependsOn: []string{"nested"}},
	}}
	env, store, _ := teamEnv(t, []Definition{inner, outer}, cooperative)
	_ = store.Put(context.Background(), "runs/t7/input", []byte("hello"), Meta{})
	var seen sync.Map
	env.SetOnChildWorkflowStartedListener(func(info *workflow.Info, _ workflow.Context, _ converter.EncodedValues) {
		seen.Store(info.WorkflowExecution.ID, true)
	})
	env.RegisterDelayedCallback(func() {
		// The gate inside the nested run is decided through the child workflow's own ID.
		if err := env.SignalWorkflowByID("default-test-workflow-id/nested", DecisionSignal, Decision{Step: "ask", Decision: Approved, DecidedBy: testUser}); err != nil {
			t.Error(err)
		}
	}, time.Second)
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t7", Definition: outer, Inputs: runInput("runs/t7/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if res.Artifacts["nested"] != "runs/t7/nested/last" || res.Artifacts["nested/first"] != "runs/t7/nested/first" || res.Artifacts["after"] != "runs/t7/after" {
		t.Fatalf("artifacts %v", res.Artifacts)
	}
	if got := string(store.m["runs/t7/nested/first"]); got != "echoed: hello" {
		t.Fatalf("the nested run did not receive the step's input: %q", got)
	}
	if _, ok := seen.Load("default-test-workflow-id/nested"); !ok {
		t.Error("the nested run did not start under <parent>/<step>")
	}
}

func TestNestingDeeperThanEightLevelsFailsTheRun(t *testing.T) {
	env, _, _ := teamEnv(t, nil, cooperative)
	def := Definition{Name: "deep", Steps: []Step{{ID: "say", Service: "echo"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t8", Definition: def, Inputs: runInput("runs/t8/input"), Depth: MaxDepth + 1})
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), "nested deeper than 8 levels") {
		t.Fatalf("error %v", err)
	}
}

func TestWorkflowStepFailsForAnUnknownWorkflow(t *testing.T) {
	env, store, _ := teamEnv(t, nil, cooperative)
	_ = store.Put(context.Background(), "runs/t9/input", []byte("hello"), Meta{})
	def := Definition{Name: "outer", Steps: []Step{{ID: "nested", Service: WorkflowService, Workflow: "gone"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t9", Definition: def, Inputs: runInput("runs/t9/input")})
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), `workflow "gone" is not a configured workflow`) {
		t.Fatalf("error %v", err)
	}
}

func TestMemberNamesAndChannels(t *testing.T) {
	if got := MemberName("demo/lead@v1"); got != leadMember {
		t.Errorf("member name %q", got)
	}
	if got := ChannelID("lead", "coder"); got != coderLeadChannel {
		t.Errorf("channel id %q", got)
	}
	if a, b := titleCase("final-review"), titleCase("API-review"); a != "Final Review" || b != "API Review" {
		t.Errorf("title case %q %q", a, b)
	}
	if got := TeamPath("runs/t/build", "team"); got != "teams/t/build/team" {
		t.Errorf("team path %q", got)
	}
	s := Step{Agent: "demo/lead@v1", Agents: []string{"demo/coder@v1", "demo/reviewer@v1"}}
	if got := s.TeamChannels(); len(got) != 2 || got[0].ID != coderLeadChannel || got[1].ID != "lead--reviewer" {
		t.Errorf("default channels %+v", got)
	}
	s.Channels = [][]string{{"reviewer", "coder"}}
	if got := s.TeamChannels(); len(got) != 1 || got[0].ID != "coder--reviewer" || got[0].Members[0] != coderMember {
		t.Errorf("configured channels %+v", got)
	}
}

func TestValidateTeamAndWorkflowSteps(t *testing.T) {
	team := Step{
		ID: "team", Service: TeamService, Agent: "demo/lead@v1", Agents: []string{"demo/coder@v1"}, Workflows: []string{"docs"},
		Channels: [][]string{{"lead", "coder"}, {"coder", "docs"}},
	}
	gate := Step{ID: "review", Service: GateService, DependsOn: []string{"team"}, OnReject: "team"}
	node := Step{ID: "nested", Service: WorkflowService, Workflow: "inner", DependsOn: []string{"review"}}
	if err := Validate(&Definition{Name: "ok", Steps: []Step{team, gate, node}}); err != nil {
		t.Fatal(err)
	}
	change := func(edit func(team, gate, node *Step)) *Definition {
		a, b, c := team, gate, node
		edit(&a, &b, &c)
		return &Definition{Name: "bad", Steps: []Step{a, b, c, {ID: "other", Service: "x"}}}
	}
	for name, tc := range map[string]struct {
		def  *Definition
		want string
	}{
		"no lead":             {change(func(a, _, _ *Step) { a.Agent = "" }), "needs a lead agent"},
		"bad agent":           {change(func(a, _, _ *Step) { a.Agents = []string{"coder"} }), "not an agent"},
		"duplicate member":    {change(func(a, _, _ *Step) { a.Agents = []string{"demo/coder@v1", "other/coder@v2"} }), `two members named "coder"`},
		"lead twice":          {change(func(a, _, _ *Step) { a.Agents = []string{"demo/lead@v1"} }), `two members named "lead"`},
		"reserved member":     {change(func(a, _, _ *Step) { a.Workflows = []string{"workflow"} }), "reserved"},
		"unknown in channel":  {change(func(a, _, _ *Step) { a.Channels = [][]string{{"lead", "tester"}} }), `"tester", which is not a member`},
		"self channel":        {change(func(a, _, _ *Step) { a.Channels = [][]string{{"lead", "lead"}} }), "two different members"},
		"channel twice":       {change(func(a, _, _ *Step) { a.Channels = [][]string{{"lead", "coder"}, {"coder", "lead"}} }), "twice"},
		"channel case":        {change(func(a, _, _ *Step) { a.Workflows = []string{"Docs"}; a.Channels = [][]string{{"lead", "docs"}} }), `has a channel to "docs", but the member is named "Docs"`},
		"member in two cases": {change(func(a, _, _ *Step) { a.Channels = [][]string{{"lead", "Lead"}} }), `has a channel to "Lead", but the member is named "lead"`},
		"negative turns":      {change(func(a, _, _ *Step) { a.MaxTurns = -1 }), "max_turns"},
		"on_reject unknown":   {change(func(_, b, _ *Step) { b.OnReject = "gone" }), "on_reject must name an agent, interactive, team, or workflow step"},
		"on_reject no team":   {change(func(_, b, _ *Step) { b.OnReject = "other"; b.DependsOn = []string{"team", "other"} }), "on_reject must name an agent, interactive, team, or workflow step"},
		"on_reject no dep":    {change(func(_, b, _ *Step) { b.DependsOn = []string{"other"} }), "on_reject must name an agent, interactive, team, or workflow step"},
		"on_reject on agent":  {change(func(_, _, c *Step) { c.OnReject = "team" }), "only a gate step"},
		"no workflow name":    {change(func(_, _, c *Step) { c.Workflow = "" }), "needs the workflow it runs"},
	} {
		if err := Validate(tc.def); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v", name, err)
		}
	}
}

func TestValidateSetRefusesCyclesUnknownAndAmbiguousNestedWorkflows(t *testing.T) {
	node := func(id, name string) Step { return Step{ID: id, Service: WorkflowService, Workflow: name} }
	leaf := Definition{Name: "leaf", Steps: []Step{{ID: "a", Service: "x"}}}
	ok := []Definition{{Name: "root", Steps: []Step{node("n", "mid")}}, {Name: "mid", Steps: []Step{node("n", "leaf"), node("m", "leaf")}}, leaf}
	if err := ValidateSet(ok[:1]); err == nil || !strings.Contains(err.Error(), `step "n" names unknown workflow "mid"`) {
		t.Errorf("unknown: %v", err)
	}
	if err := ValidateSet(ok); err == nil || !strings.Contains(err.Error(), `workflow "mid" runs inside workflow "root"`) || !strings.Contains(err.Error(), "has 2 (n, m)") {
		t.Errorf("two final steps: %v", err)
	}
	ok[1].Steps[1].DependsOn = []string{"n"}
	if err := ValidateSet(ok); err != nil {
		t.Errorf("valid set: %v", err)
	}
	self := []Definition{{Name: "loop", Steps: []Step{node("n", "loop")}}}
	if err := ValidateSet(self); err == nil || !strings.Contains(err.Error(), "loop@v1 -> loop@v1") {
		t.Errorf("self: %v", err)
	}
	through := []Definition{
		{Name: "a", Steps: []Step{node("n", "b")}},
		{Name: "b", Steps: []Step{{ID: "team", Service: TeamService, Agent: "lead@v1", Workflows: []string{"c"}}}},
		{Name: "c", Steps: []Step{node("n", "a")}},
		leaf,
	}
	if err := ValidateSet(through); err == nil || !strings.Contains(err.Error(), `reaches itself through nested workflows: a@v1 -> b@v1 -> c@v1 -> a@v1`) {
		t.Errorf("cycle through a team: %v", err)
	}
}

func TestTeamNudgesALeadThatNeitherActsNorFinishes(t *testing.T) {
	// The lead ignores the task and the first nudge, and finishes on the second nudge.
	env, store, f := teamEnv(t, nil, func(ctx context.Context, f *fakeMembers, req *TeamTurnRequest) error {
		f.take(ctx, req.Team, req.Member)
		if f.count(req.Member) == 3 {
			f.finish(ctx, req.Team, "done", "code.py")
		}
		return nil
	})
	ctx := context.Background()
	_ = store.Put(ctx, "runs/t10/input", []byte("task"), Meta{})
	def := Definition{Name: "solo", Steps: []Step{{ID: "build", Service: TeamService, Agent: "lead@v1"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t10", Definition: def, Inputs: runInput("runs/t10/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	task := f.queue(ctx, "teams/t10/build", "lead--workflow", WorkflowSender)
	if len(task.Messages) != 3 || task.Messages[1].Text != NudgeText || task.Messages[2].Text != NudgeText ||
		task.Messages[2].Seq != 3 {
		t.Fatalf("task channel %+v", task)
	}
}

func TestTeamStallsAfterThreeNudges(t *testing.T) {
	env, store, f := teamEnv(t, nil, func(ctx context.Context, f *fakeMembers, req *TeamTurnRequest) error {
		f.count(req.Member)
		f.take(ctx, req.Team, req.Member)
		return nil
	})
	ctx := context.Background()
	_ = store.Put(ctx, "runs/t11/input", []byte("task"), Meta{})
	def := Definition{Name: "solo", Steps: []Step{{ID: "build", Service: TeamService, Agent: "lead@v1"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t11", Definition: def, Inputs: runInput("runs/t11/input")})
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), `team "build" stalled`) {
		t.Fatalf("error %v", err)
	}
	task := f.queue(ctx, "teams/t11/build", "lead--workflow", WorkflowSender)
	if len(task.Messages) != 4 || f.turns["lead"] != 4 {
		t.Fatalf("messages %d, turns %v", len(task.Messages), f.turns)
	}
}

// versioned is a team whose lead finishes on every message from the workflow, with the
// summary "v<n>" for its n-th finish.
func versioned(ctx context.Context, f *fakeMembers, req *TeamTurnRequest) error {
	for _, m := range f.take(ctx, req.Team, req.Member) {
		if m.From == WorkflowSender {
			f.finish(ctx, req.Team, fmt.Sprintf("v%d", f.count(req.Team)), "code.py")
		}
	}
	return nil
}

func summaryOf(t *testing.T, store *syncStore, artifact string) string {
	t.Helper()
	var out TeamOutput
	data, _ := store.Get(context.Background(), artifact)
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("artifact %s: %v", artifact, err)
	}
	return out.Summary
}

func TestRejectionMakesDependentTeamsWorkAgain(t *testing.T) {
	lead := func(id string, deps ...string) Step {
		return Step{ID: id, Service: TeamService, Agent: "lead@v1", DependsOn: deps}
	}
	inner := Definition{Name: "inner", Steps: []Step{lead("deep")}}
	def := Definition{Name: "chain", Steps: []Step{
		lead("build"),
		lead("polish", "build"),
		{ID: "wrap", Service: WorkflowService, Workflow: "inner", DependsOn: []string{"polish"}},
		{ID: "review", Service: GateService, DependsOn: []string{"wrap"}, OnReject: "build"},
	}}
	env, store, f := teamEnv(t, []Definition{inner, def}, versioned)
	ctx := context.Background()
	_ = store.Put(ctx, "runs/t12/input", []byte("task"), Meta{})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: "again", DecidedBy: testUser})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Approved, DecidedBy: testUser})
	}, 2*time.Second)
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t12", Definition: def, Inputs: runInput("runs/t12/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []string{"runs/t12/build", "runs/t12/polish", "runs/t12/wrap/deep"} {
		if got := summaryOf(t, store, artifact); got != "v2" {
			t.Errorf("%s was not produced again after the rejection: summary %q", artifact, got)
		}
	}
	// A dependent team is told that its inputs changed and receives the new input.
	polish := f.queue(ctx, "teams/t12/polish", "lead--workflow", WorkflowSender)
	if len(polish.Messages) != 2 || !strings.HasPrefix(polish.Messages[1].Text, "Run 2: the inputs of your team changed") ||
		!strings.Contains(polish.Messages[1].Text, `"summary": "v2"`) {
		t.Fatalf("polish task channel %+v", polish)
	}
	if got := string(store.m["teams/t12/polish/files/input-build.json"]); !strings.Contains(got, `"summary": "v2"`) {
		t.Errorf("polish input file was not refreshed: %s", got)
	}
	var manifest TeamManifest
	_, _ = f.a.getJSON(ctx, "teams/t12/wrap/deep/team.json", &manifest)
	if manifest.Round != 2 {
		t.Errorf("nested team round %d", manifest.Round)
	}
}

func TestOpenTeamTellsARerunFromARetry(t *testing.T) {
	store := newSyncStore()
	a := &Activities{Store: store}
	ctx := context.Background()
	_ = store.Put(ctx, "runs/t/input", []byte("task"), Meta{})
	req := OpenTeamRequest{TraceID: "t", Team: "teams/t/build", Step: Step{ID: "build", Agent: "lead@v1"}, Inputs: runInput("runs/t/input"), Run: "/0"}
	for i := 0; i < 2; i++ {
		if res, err := a.OpenTeam(ctx, &req); err != nil || res.Round != 1 {
			t.Fatalf("open %d: %+v %v", i, res, err)
		}
	}
	_ = store.Put(ctx, "teams/t/build/finished.json", []byte(`{"summary":"s","files":[]}`), Meta{})
	if res, err := a.OpenTeam(ctx, &req); err != nil || res.Round != 1 || !store.has("teams/t/build/finished.json") {
		t.Fatalf("a retry must leave the finished team alone: %+v %v", res, err)
	}
	req.Run = "/1"
	for i := 0; i < 2; i++ {
		if res, err := a.OpenTeam(ctx, &req); err != nil || res.Round != 2 || store.has("teams/t/build/finished.json") {
			t.Fatalf("rerun %d: %+v %v", i, res, err)
		}
	}
	var log TeamQueue
	_, _ = a.getJSON(ctx, "teams/t/build/channels/lead--workflow/workflow.json", &log)
	if len(log.Messages) != 2 {
		t.Fatalf("messages %+v", log.Messages)
	}
}

func TestRejectionRerunsDependentsThatAreStillInFlight(t *testing.T) {
	inner := Definition{Name: "inner", Steps: []Step{{ID: "ask", Service: GateService}}}
	def := Definition{Name: "busy", Steps: []Step{
		{ID: "build", Service: TeamService, Agent: "lead@v1"},
		{ID: "review", Service: GateService, DependsOn: []string{"build"}, OnReject: "build"},
		{ID: "talk", Service: InteractiveService, Agent: "helper@v1", DependsOn: []string{"build"}},
		{ID: "sub", Service: WorkflowService, Workflow: "inner", DependsOn: []string{"build"}},
		{ID: "slow", Service: "slow", Activity: "Slow", DependsOn: []string{"build"}},
	}}
	env, store, _ := teamEnv(t, []Definition{inner, def}, versioned)
	ctx := context.Background()
	_ = store.Put(ctx, "runs/t13/input", []byte("task"), Meta{})
	var mu sync.Mutex
	var saw []string
	// The first run of the slow step is still in flight when the rejection arrives.
	env.RegisterActivityWithOptions(func(ctx context.Context, req StepRequest) (StepResult, error) {
		summary := summaryOf(t, store, req.Inputs[0].Artifact)
		mu.Lock()
		saw = append(saw, summary)
		first := len(saw) == 1
		mu.Unlock()
		if first {
			env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: "again", DecidedBy: testUser})
			time.Sleep(200 * time.Millisecond)
		}
		_ = store.Put(ctx, req.OutputArtifact, []byte("saw "+summary), Meta{})
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: "Slow"})
	children := 0
	env.SetOnChildWorkflowStartedListener(func(info *workflow.Info, _ workflow.Context, _ converter.EncodedValues) {
		if info.WorkflowExecution.ID == "default-test-workflow-id/sub" {
			mu.Lock()
			children++
			mu.Unlock()
		}
	})
	env.RegisterDelayedCallback(func() {
		state := pendingState(t, env)
		if len(state.Gates) != 1 || state.Gates[0].Round != 2 || len(state.Sessions) != 1 || show(state.Sessions[0].Inputs) != "build=runs/t13/build#1" {
			t.Errorf("after the rejection %+v", state)
		}
		if got := string(store.m["user-sessions/t13/talk/files/input-build.json"]); !strings.Contains(got, `"summary": "v2"`) {
			t.Errorf("the session was not reopened with the new input: %s", got)
		}
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Approved, DecidedBy: testUser})
		env.SignalWorkflow(CompleteSignal, Completion{Step: "talk", File: "input-build.json", CompletedBy: testUser})
		if err := env.SignalWorkflowByID("default-test-workflow-id/sub", DecisionSignal, Decision{Step: "ask", Decision: Approved, DecidedBy: testUser}); err != nil {
			t.Error(err)
		}
	}, time.Second)
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t13", Definition: def, Inputs: runInput("runs/t13/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res Result
	_ = env.GetWorkflowResult(&res)
	if len(saw) != 2 || saw[1] != "v2" || string(store.m["runs/t13/slow"]) != "saw v2" {
		t.Errorf("the slow step did not run again with the new input: %v, artifact %q", saw, store.m["runs/t13/slow"])
	}
	if children != 2 {
		t.Errorf("the nested run in flight was started %d times", children)
	}
	for _, id := range []string{"build", "review", "talk", "sub", "sub/ask", "slow"} {
		if res.Artifacts[id] == "" {
			t.Errorf("artifact of %s is missing: %v", id, res.Artifacts)
		}
	}
}

func TestOpenSessionReopensACompletedSession(t *testing.T) {
	store := newSyncStore()
	a := &Activities{Store: store}
	ctx := context.Background()
	_ = store.Put(ctx, "runs/t/draft", []byte("text"), Meta{})
	_ = store.Put(ctx, "user-sessions/t/talk/completed.json", []byte(`{}`), Meta{})
	if _, err := a.OpenSession(ctx, &OpenSessionRequest{TraceID: "t", Session: "user-sessions/t/talk", Inputs: []Item{{Source: "draft", Artifact: "runs/t/draft", Round: 1}}}); err != nil {
		t.Fatal(err)
	}
	if store.has("user-sessions/t/talk/completed.json") || !store.has("user-sessions/t/talk/files/input-draft.md") {
		t.Fatal("an opened session must not be completed")
	}
}

func TestWorkflowMemberRunsOfALaterRoundKeepThoseOfAnEarlierOne(t *testing.T) {
	// The lead asks the echo workflow on every message from the workflow and finishes on its reply.
	env, store, f := teamEnv(t, []Definition{echoWorkflow}, func(ctx context.Context, f *fakeMembers, req *TeamTurnRequest) error {
		for _, m := range f.take(ctx, req.Team, req.Member) {
			if m.From == WorkflowSender {
				f.send(ctx, req.Team, "lead", "echo", fmt.Sprintf("ping %d", f.count("ping")))
			} else {
				f.finish(ctx, req.Team, m.Text, "code.py")
			}
		}
		return nil
	})
	ctx := context.Background()
	_ = store.Put(ctx, "runs/t14/input", []byte("task"), Meta{})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Rejected, Comment: "again", DecidedBy: testUser})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(DecisionSignal, Decision{Step: "review", Decision: Approved, DecidedBy: testUser})
	}, 2*time.Second)
	def := Definition{Name: "reviewed", Steps: []Step{
		{ID: "build", Service: TeamService, Agent: "lead@v1", Workflows: []string{"echo"}},
		{ID: "review", Service: GateService, DependsOn: []string{"build"}, OnReject: "build"},
	}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t14", Definition: def, Inputs: runInput("runs/t14/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if got := string(store.m["runs/t14/build.echo.r1.1/say"]); got != "echoed: ping 1" {
		t.Errorf("first round run %q", got)
	}
	if got := string(store.m["runs/t14/build.echo.r2.1/say"]); got != "echoed: ping 2" {
		t.Errorf("second round run %q", got)
	}
	if got := summaryOf(t, store, "runs/t14/build"); got != "echoed: ping 2" {
		t.Errorf("summary %q", got)
	}
	_ = f
}

func TestRouteChildCountsTowardTheNestingDepth(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	store := newSyncStore()
	small := Definition{Name: "small-fix", Steps: []Step{{ID: "fix", Service: "fix"}}}
	env.RegisterActivity(&Activities{Store: store, LoadDefinitions: func() ([]Definition, error) { return []Definition{small}, nil }})
	env.RegisterWorkflowWithOptions(Pipeline, workflow.RegisterOptions{Name: WorkflowName})
	env.RegisterActivityWithOptions(func(ctx context.Context, req StepRequest) (StepResult, error) {
		_ = store.Put(ctx, req.OutputArtifact, []byte(`{"route":"small-fix"}`), Meta{})
		return StepResult{Artifact: req.OutputArtifact}, nil
	}, activity.RegisterOptions{Name: DefaultActivity})
	def := Definition{Name: "intake", Steps: []Step{
		{ID: "triage", Service: "triage"},
		{ID: "route", Service: RouteService, DependsOn: []string{"triage"}},
	}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "t15", Definition: def, Inputs: runInput("runs/t15/input"), Depth: MaxDepth})
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), `workflow "small-fix" is nested deeper than 8 levels`) {
		t.Fatalf("error %v", err)
	}
}

func TestPostTeamMessageWritesOnlyTheSendersOwnQueue(t *testing.T) {
	store := newSyncStore()
	a := &Activities{Store: store}
	f := &fakeMembers{a: a}
	ctx := context.Background()
	team := "teams/t/build"
	channel := TeamChannel{ID: "echo--lead", Members: []string{"echo", "lead"}}
	_ = a.putJSON(ctx, team+"/team.json", TeamManifest{
		Lead: "lead", Channels: []TeamChannel{channel},
		Members: []TeamMember{{Name: "lead", Kind: AgentMember}, {Name: "echo", Kind: WorkflowMember}},
	}, Meta{})
	_ = store.Put(ctx, "runs/t/out", []byte("same answer"), Meta{})
	reply := func(read int) {
		t.Helper()
		req := PostTeamMessageRequest{Team: team, Channel: channel.ID, From: "echo", Artifacts: []string{"runs/t/out"}, Member: "echo", Ref: "echo", Read: read}
		// The activity runs twice, as after a retry.
		for i := 0; i < 2; i++ {
			if err := a.PostTeamMessage(ctx, &req); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.send(ctx, team, "lead", "echo", "first")
	before := string(store.m[queueID(team, channel.ID, "lead")])
	reply(1)
	time.Sleep(time.Millisecond)
	f.send(ctx, team, "lead", "echo", "second")
	lead := string(store.m[queueID(team, channel.ID, "lead")])
	reply(2)
	// The same answer to a later message is a new reply; the retries added nothing.
	echo := f.queue(ctx, team, channel.ID, "echo")
	if len(echo.Messages) != 2 || echo.Messages[1].Seq != 2 || echo.Messages[1].Text != "same answer" || echo.To != leadMember {
		t.Fatalf("echo queue %+v", echo)
	}
	if got := string(store.m[queueID(team, channel.ID, "lead")]); got != lead || before == lead {
		t.Error("a reply must not touch the other side's queue")
	}
	if state, err := a.TeamState(ctx, TeamStateRequest{Team: team}); err != nil || len(state.Unread) != 1 || state.Unread[0] != leadMember {
		t.Errorf("unread %v %v", state.Unread, err)
	}
	// The queue of an agent member belongs to the tool-gateway.
	err := a.PostTeamMessage(ctx, &PostTeamMessageRequest{Team: team, Channel: channel.ID, From: "lead", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), `does not write the queue of "lead"`) {
		t.Fatalf("error %v", err)
	}
}

func TestOneWayChannel(t *testing.T) {
	team := Step{
		ID: "team", Service: TeamService, Agent: "demo/lead@v1", Agents: []string{"demo/coder@v1"},
		Channels: [][]string{{"lead", "coder", OneWay}},
	}
	if err := Validate(&Definition{Name: "ok", Steps: []Step{team}}); err != nil {
		t.Fatal(err)
	}
	got := team.TeamChannels()
	if len(got) != 1 || got[0].ID != coderLeadChannel || got[0].From != leadMember || !got[0].sends("lead") || got[0].sends("coder") {
		t.Errorf("one-way channel %+v", got)
	}
	team.Channels = [][]string{{"coder", "lead", OneWay}}
	if got := team.TeamChannels(); got[0].From != coderMember {
		t.Errorf("the first member sends, got %+v", got)
	}
	team.Channels = [][]string{{"lead", "coder", "both"}}
	if err := Validate(&Definition{Name: "bad", Steps: []Step{team}}); err == nil || !strings.Contains(err.Error(), "third entry") {
		t.Errorf("want a refused third entry, got %v", err)
	}
	if two := (TeamChannel{ID: "a--b", Members: []string{"a", "b"}}); !two.sends("a") || !two.sends("b") {
		t.Error("both members send on a two-way channel")
	}
}
