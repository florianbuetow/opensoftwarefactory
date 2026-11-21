package main

import (
	"net/http"
	"strings"
	"testing"

	"opensoftwarefactory/orchestrator/pipeline"
)

func TestTeamsListsRunningTeamSteps(t *testing.T) {
	srv, runner, _, _ := newTestServer(t)
	code, body := call(t, http.MethodGet, srv.URL+"/api/teams", "")
	if code != http.StatusOK || !strings.Contains(body, `"teams":[]`) {
		t.Fatalf("empty teams %d %s", code, body)
	}
	runner.teams = []RunningTeam{{WorkflowID: "t/build", TraceID: "t", Workflow: "team-build", PendingTeam: pipeline.PendingTeam{Step: "team", Team: "teams/t/build/team"}}}
	code, body = call(t, http.MethodGet, srv.URL+"/api/teams", "")
	want := `{"workflow_id":"t/build","trace_id":"t","workflow":"team-build","step":"team","team":"teams/t/build/team"}`
	if code != http.StatusOK || !strings.Contains(body, want) {
		t.Fatalf("teams %d %s", code, body)
	}
}

func TestRejectionAtAGateWithOnRejectNeedsAComment(t *testing.T) {
	srv, runner, _, _ := newTestServer(t)
	runner.pending = []PendingDecision{
		{WorkflowID: "t/build", TraceID: "t", Workflow: "team-build", PendingGate: pipeline.PendingGate{Step: "review", OnReject: "team", Round: 1}},
		{WorkflowID: "t", TraceID: "t", Workflow: "plain", PendingGate: pipeline.PendingGate{Step: "review", Round: 1}},
	}
	code, body := call(t, http.MethodGet, srv.URL+"/api/decisions", "")
	if code != http.StatusOK || !strings.Contains(body, `"on_reject":"team","round":1`) || !strings.Contains(body, `"on_reject":"","round":1`) {
		t.Fatalf("decisions %d %s", code, body)
	}
	for _, comment := range []string{``, `,"comment":"  "`} {
		bad := `{"workflow_id":"t/build","step":"review","decision":"rejected","decided_by":"flo"` + comment + `}`
		if code, body = call(t, http.MethodPost, srv.URL+"/api/decisions", bad); code != http.StatusBadRequest || !strings.Contains(body, "comment") {
			t.Fatalf("rejection without a comment %d %s", code, body)
		}
	}
	if len(runner.decided) != 0 {
		t.Fatalf("decided %+v", runner.decided)
	}
	for _, ok := range []string{
		`{"workflow_id":"t/build","step":"review","decision":"rejected","comment":"redo","decided_by":"flo"}`,
		`{"workflow_id":"t/build","step":"review","decision":"approved","decided_by":"flo"}`,
		`{"workflow_id":"t","step":"review","decision":"rejected","decided_by":"flo"}`,
	} {
		if code, body = call(t, http.MethodPost, srv.URL+"/api/decisions", ok); code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", ok, code, body)
		}
	}
}

func TestSaveRefusesAVersionThatReachesItself(t *testing.T) {
	srv, _, _, workflows := newTestServer(t)
	before, _ := workflowObjects(workflows)
	// The first version of a workflow that names itself would run itself.
	body := `{"steps":[{"id":"n","service":"workflow","workflow":"a"}]}`
	code, out := call(t, http.MethodPost, srv.URL+"/api/workflows/a/versions", body)
	if code != http.StatusUnprocessableEntity || !strings.Contains(out, "a@v1 -\\u003e a@v1") {
		t.Fatalf("%d %s", code, out)
	}
	if after, _ := workflowObjects(workflows); string(before) != string(after) {
		t.Fatal("an invalid version changed config.yaml")
	}
}

func TestLoadRefusesUnversionedWorkflowsThatReachEachOther(t *testing.T) {
	_, err := parseConfig([]byte(`workflows:
  - name: a
    steps: [{id: n, service: workflow, workflow: b}]
  - name: b
    steps: [{id: n, service: workflow, workflow: a}]
`))
	if err == nil || !strings.Contains(err.Error(), "a@v1 -> b@v1 -> a@v1") {
		t.Fatalf("error %v", err)
	}
}
