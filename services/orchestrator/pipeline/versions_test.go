package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseWorkflowRef(t *testing.T) {
	for ref, want := range map[string]struct {
		name    string
		version int
		ok      bool
	}{
		"team-build@v2":   {"team-build", 2, true},
		"team-build":      {"team-build", 0, true},
		"API-review@v1":   {"API-review", 1, true},
		"x@v0":            {"", 0, false},
		"x@2":             {"", 0, false},
		"x@v":             {"", 0, false},
		"ecc/ECC-plan@v1": {"ecc/ECC-plan", 1, true},
		"a/b/c/d":         {"a/b/c/d", 0, true},
		"a/b/c/d/e@v1":    {"", 0, false},
		"a//b@v1":         {"", 0, false},
		"/a@v1":           {"", 0, false},
		"":                {"", 0, false},
	} {
		name, version, ok := ParseWorkflowRef(ref)
		if name != want.name || version != want.version || ok != want.ok {
			t.Errorf("%q: %q %d %v", ref, name, version, ok)
		}
	}
	if WorkflowRef("a", 3) != "a@v3" || WorkflowRef("a", 0) != "a" {
		t.Error("WorkflowRef")
	}
}

func TestValidWorkflowName(t *testing.T) {
	for name, want := range map[string]bool{
		"sample": true, "ecc/ECC-plan": true, "a/b/c/d": true,
		"a/b/c/d/e": false, "a//b": false, "a/": false, "/a": false, "ecc/v2": false, "v1": false, "a b": false,
	} {
		if got := ValidWorkflowName(name); got != want {
			t.Errorf("%q: %v", name, got)
		}
	}
}

// step returns a one-step definition whose step ID tells the versions apart.
func versionOf(name string, version int, step string) Definition {
	return Definition{Name: name, Version: version, Steps: []Step{{ID: step, Service: "echo"}}}
}

func TestFindDefinitionNewestAndPinReferences(t *testing.T) {
	defs := []Definition{
		versionOf("inner", 2, "new"),
		{Name: "legacy", Steps: []Step{{ID: "a", Service: "x"}}},
		versionOf("inner", 1, "old"),
		{Name: "outer", Version: 1, Steps: []Step{
			{ID: "n", Service: WorkflowService, Workflow: "inner"},
			{ID: "pinned", Service: WorkflowService, Workflow: "inner@v1"},
			{ID: "team", Service: TeamService, Agent: "lead@v1", Workflows: []string{"legacy", "gone"}},
		}},
	}
	if d, ok := FindDefinition(defs, "inner"); !ok || d.Version != 2 {
		t.Errorf("newest %+v %v", d, ok)
	}
	if d, ok := FindDefinition(defs, "inner@v1"); !ok || d.Steps[0].ID != oldStep {
		t.Errorf("pinned %+v %v", d, ok)
	}
	// A definition without a version is v1.
	if d, ok := FindDefinition(defs, "legacy@v1"); !ok || d.Name != legacyWorkflow {
		t.Errorf("unversioned %+v %v", d, ok)
	}
	if _, ok := FindDefinition(defs, "inner@v3"); ok {
		t.Error("found a version that does not exist")
	}
	if newest := Newest(defs); len(newest) != 3 || newest[0].Version != 2 || newest[1].Name != legacyWorkflow || newest[2].Name != "outer" {
		t.Errorf("newest %+v", newest)
	}
	PinReferences(defs)
	outer := defs[3].Steps
	if outer[0].Workflow != "inner@v2" || outer[1].Workflow != "inner@v1" || strings.Join(outer[2].Workflows, ",") != "legacy@v1,gone" {
		t.Errorf("pinned %+v", outer)
	}
	nested := NestedDefinitions(defs, &defs[3])
	if len(nested) != 3 || nested[0].Version != 2 || nested[1].Steps[0].ID != oldStep || nested[2].Name != legacyWorkflow {
		t.Errorf("nested %+v", nested)
	}
	if refs := AgentRefs(defs[3], defs[3]); len(refs) != 1 || refs[0] != "lead@v1" {
		t.Errorf("agents %v", refs)
	}
}

func TestWorkflowStepRunsThePinnedVersion(t *testing.T) {
	defs := []Definition{versionOf("inner", 1, "old"), versionOf("inner", 2, "new")}
	for ref, want := range map[string]string{"inner@v1": "old", "inner@v2": "new", "inner": "new"} {
		env, store, _ := teamEnv(t, defs, cooperative)
		_ = store.Put(context.Background(), "runs/p1/input", []byte("hello"), Meta{})
		outer := Definition{Name: "outer", Steps: []Step{{ID: "nested", Service: WorkflowService, Workflow: ref}}}
		env.ExecuteWorkflow(Pipeline, &Input{TraceID: "p1", Definition: outer, Inputs: runInput("runs/p1/input")})
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		var res Result
		_ = env.GetWorkflowResult(&res)
		if res.Artifacts["nested"] != "runs/p1/nested/"+want {
			t.Errorf("%s: artifacts %v", ref, res.Artifacts)
		}
	}
	env, store, _ := teamEnv(t, defs, cooperative)
	_ = store.Put(context.Background(), "runs/p2/input", []byte("hello"), Meta{})
	outer := Definition{Name: "outer", Steps: []Step{{ID: "nested", Service: WorkflowService, Workflow: "inner@v3"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "p2", Definition: outer, Inputs: runInput("runs/p2/input")})
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), `workflow "inner@v3" is not a configured workflow`) {
		t.Fatalf("error %v", err)
	}
}

func TestTeamWorkflowMemberRunsThePinnedVersionUnderItsPlainName(t *testing.T) {
	newer := Definition{Name: "echo", Version: 2, Description: "Newer.", Steps: []Step{{ID: "shout", Service: "echo"}}}
	env, store, f := teamEnv(t, []Definition{echoWorkflow, newer}, cooperative)
	ctx := context.Background()
	_ = store.Put(ctx, "runs/p3/input", []byte("Build a parser."), Meta{})
	step := teamStep()
	step.Workflows = []string{"echo@v1"}
	step.Channels = [][]string{{"lead", "coder"}, {"lead", "echo"}}
	def := Definition{Name: "solo", Steps: []Step{step}}
	if err := Validate(&def); err != nil {
		t.Fatal(err)
	}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "p3", Definition: def, Inputs: runInput("runs/p3/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var manifest TeamManifest
	_, _ = f.a.getJSON(ctx, "teams/p3/build/team.json", &manifest)
	if m := manifest.Members[2]; m != (TeamMember{Name: "echo", Kind: WorkflowMember, Ref: "echo@v1", Description: "Echoes its input."}) {
		t.Errorf("workflow member %+v", m)
	}
	if !store.has("runs/p3/build.echo.r1.1/say") || store.has("runs/p3/build.echo.r1.1/shout") {
		t.Error("the workflow member did not run its pinned version")
	}
	var member teamMemberFile
	_, _ = f.a.getJSON(ctx, "teams/p3/build/members/echo.json", &member)
	if member.Member != echoMember || member.Agent != "echo@v1" {
		t.Errorf("member file %+v", member)
	}
}

func TestValidatePinnedReferences(t *testing.T) {
	node := func(id, ref string) Step { return Step{ID: id, Service: WorkflowService, Workflow: ref} }
	for _, bad := range []string{"inner@1", "inner@v0", "inner@", "a//b@v1", "a/b/c/d/e@v1"} {
		if err := Validate(&Definition{Name: "w", Steps: []Step{node("n", bad)}}); err == nil {
			t.Errorf("workflow step accepted %q", bad)
		}
		team := Step{ID: "t", Service: TeamService, Agent: "lead@v1", Workflows: []string{bad}}
		if err := Validate(&Definition{Name: "w", Steps: []Step{team}}); err == nil {
			t.Errorf("team accepted %q", bad)
		}
	}
	// Two versions of one workflow would be two members of the same name.
	twice := Step{ID: "t", Service: TeamService, Agent: "lead@v1", Workflows: []string{"docs@v1", "docs@v2"}}
	if err := Validate(&Definition{Name: "w", Steps: []Step{twice}}); err == nil || !strings.Contains(err.Error(), `two members named "docs"`) {
		t.Errorf("two versions as members: %v", err)
	}

	leaf1, leaf2 := versionOf("leaf", 1, "a"), versionOf("leaf", 2, "a")
	if err := ValidateSet([]Definition{leaf1, leaf2, {Name: "root", Version: 1, Steps: []Step{node("n", "leaf@v3")}}}); err == nil ||
		!strings.Contains(err.Error(), `step "n" names unknown workflow "leaf@v3"`) {
		t.Errorf("missing pinned version: %v", err)
	}
	// The newest version of a may run an older version of b that ran an older version of a.
	spiral := []Definition{
		versionOf("a", 1, "x"),
		{Name: "b", Version: 1, Steps: []Step{node("n", "a@v1")}},
		{Name: "a", Version: 2, Steps: []Step{node("n", "b@v1")}},
	}
	if err := ValidateSet(spiral); err != nil {
		t.Errorf("no version reaches itself: %v", err)
	}
	// A version that names itself is a cycle.
	self := []Definition{versionOf("a", 1, "x"), {Name: "a", Version: 2, Steps: []Step{node("n", "a@v2")}}}
	if err := ValidateSet(self); err == nil || !strings.Contains(err.Error(), "a@v2 -> a@v2") {
		t.Errorf("self: %v", err)
	}
	// The final-step rule applies to the pinned version, not the newest.
	two := Definition{Name: "leaf", Version: 3, Steps: []Step{{ID: "a", Service: "x"}, {ID: "b", Service: "x"}}}
	root := Definition{Name: "root", Version: 1, Steps: []Step{node("n", "leaf@v1")}}
	if err := ValidateSet([]Definition{leaf1, two, root}); err != nil {
		t.Errorf("pinned version has one final step: %v", err)
	}
	root.Steps[0].Workflow = "leaf@v3"
	if err := ValidateSet([]Definition{leaf1, two, root}); err == nil || !strings.Contains(err.Error(), "has 2 (a, b)") {
		t.Errorf("two final steps: %v", err)
	}
}

func TestResolveRouteAcceptsANameOrAPinnedVersion(t *testing.T) {
	defs := []Definition{versionOf("fix", 1, "old"), versionOf("fix", 2, "new")}
	for route, want := range map[string]string{"fix": "new", "fix@v1": "old", "fix@v2": "new"} {
		store := memStore{"runs/r/triage": []byte(`{"route":"` + route + `"}`)}
		a := &Activities{Store: store, LoadDefinitions: func() ([]Definition, error) { return defs, nil }}
		res, err := a.ResolveRoute(context.Background(), RouteRequest{Inputs: runInput("runs/r/triage")})
		if err != nil || res.Definition.Steps[0].ID != want {
			t.Errorf("%s: %+v %v", route, res, err)
		}
	}
	store := memStore{"runs/r/triage": []byte(`{"route":"fix@v9"}`)}
	a := &Activities{Store: store, LoadDefinitions: func() ([]Definition, error) { return defs, nil }}
	if _, err := a.ResolveRoute(context.Background(), RouteRequest{Inputs: runInput("runs/r/triage")}); err == nil || !strings.Contains(err.Error(), "not a configured workflow") {
		t.Errorf("unknown version: %v", err)
	}
}

// startedMetadata is what the server writes when a run starts. The max_turns number does
// not fit a float64, so it shows whether FinishRun keeps the other fields exactly.
const startedMetadata = `{"schema_version":1,"run_id":"RUN","workflow":{"name":"w","version":2},"agents":{"a@v1":{"max_turns":9007199254740993}}}`

func finishedMetadata(t *testing.T, data []byte) map[string]any {
	t.Helper()
	if !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("FinishRun changed a field it does not own: %s", data)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	workflow, ok := m["workflow"].(map[string]any)
	if !ok || m["schema_version"] != float64(1) || workflow["version"] != float64(2) {
		t.Fatalf("metadata %s", data)
	}
	finished, ok := m["finished"].(string)
	if !ok {
		t.Fatalf("finished %v", m["finished"])
	}
	if _, err := time.Parse(time.RFC3339, finished); err != nil {
		t.Fatalf("finished %v", m["finished"])
	}
	return m
}

func TestTopLevelRunCompletesItsMetadata(t *testing.T) {
	env, store, _ := teamEnv(t, nil, cooperative)
	ctx := context.Background()
	_ = store.Put(ctx, "runs/m1/input", []byte("hello"), Meta{})
	_ = store.Put(ctx, "runs/m1/metadata.json", []byte(startedMetadata), Meta{})
	def := Definition{Name: "w", Steps: []Step{{ID: "say", Service: "echo"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "m1", Definition: def, Inputs: runInput("runs/m1/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	m := finishedMetadata(t, store.m["runs/m1/metadata.json"])
	if _, has := m["error"]; has || m["status"] != RunCompleted {
		t.Fatalf("metadata %v", m)
	}
}

func TestFailedRunRecordsStatusAndError(t *testing.T) {
	env, store, _ := teamEnv(t, nil, cooperative)
	ctx := context.Background()
	_ = store.Put(ctx, "runs/m2/metadata.json", []byte(startedMetadata), Meta{})
	// The input artifact is missing, so the step fails.
	def := Definition{Name: "w", Steps: []Step{{ID: "say", Service: "echo"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "m2", Definition: def, Inputs: runInput("runs/m2/input")})
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("the run did not fail")
	}
	m := finishedMetadata(t, store.m["runs/m2/metadata.json"])
	if failure, ok := m["error"].(string); m["status"] != RunFailed || !ok || !strings.Contains(failure, `step "say" failed`) {
		t.Fatalf("metadata %v", m)
	}
}

func TestCancelledRunRecordsCancelled(t *testing.T) {
	env, store, _ := teamEnv(t, nil, cooperative)
	ctx := context.Background()
	_ = store.Put(ctx, "runs/m3/metadata.json", []byte(startedMetadata), Meta{})
	def := Definition{Name: "w", Steps: []Step{{ID: "ask", Service: GateService}}}
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Second)
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "m3", Definition: def, Inputs: runInput("runs/m3/input")})
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("the run did not end with an error")
	}
	m := finishedMetadata(t, store.m["runs/m3/metadata.json"])
	if m["status"] != RunCancelled || m["error"] == nil {
		t.Fatalf("metadata %v", m)
	}
}

func TestNestedRunDoesNotFinishTheMetadata(t *testing.T) {
	defs := []Definition{versionOf("inner", 1, "old")}
	env, store, _ := teamEnv(t, defs, cooperative)
	ctx := context.Background()
	_ = store.Put(ctx, "runs/m4/input", []byte("hello"), Meta{})
	// A nested run stores its artifacts under the step, where no metadata belongs.
	_ = store.Put(ctx, "runs/m4/nested/metadata.json", []byte(startedMetadata), Meta{})
	outer := Definition{Name: "outer", Steps: []Step{{ID: "nested", Service: WorkflowService, Workflow: "inner@v1"}}}
	env.ExecuteWorkflow(Pipeline, &Input{TraceID: "m4", Definition: outer, Inputs: runInput("runs/m4/input")})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if string(store.m["runs/m4/nested/metadata.json"]) != startedMetadata {
		t.Error("a nested run finished metadata")
	}
	// A run without metadata gets none.
	if store.has("runs/m4/metadata.json") {
		t.Error("metadata was created at the end of the run")
	}
}

func TestFinishRunKeepsTheFirstResult(t *testing.T) {
	store := memStore{"runs/m5/metadata.json": []byte(startedMetadata)}
	a := &Activities{Store: store}
	ctx := context.Background()
	req := FinishRunRequest{TraceID: "m5", Metadata: "runs/m5/metadata.json", Status: RunFailed, Error: "boom"}
	if err := a.FinishRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	first := string(store["runs/m5/metadata.json"])
	if err := a.FinishRun(ctx, FinishRunRequest{TraceID: "m5", Metadata: "runs/m5/metadata.json", Status: RunCompleted}); err != nil {
		t.Fatal(err)
	}
	if string(store["runs/m5/metadata.json"]) != first || !strings.Contains(first, `"status": "failed"`) || !strings.Contains(first, `"error": "boom"`) {
		t.Fatalf("metadata %s", first)
	}
}
