package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"opensoftwarefactory/orchestrator/pipeline"
)

func TestRunStatusShowsBlockedThePodAndThePullRequest(t *testing.T) {
	srv, runner, store, _ := newTestServer(t)
	ctx := context.Background()
	// A run whose history is gone is answered from its metadata, also when it ended blocked.
	const gone = "0b7e3c1e-1111-4a4a-9b9b-123456789abc"
	meta := `{"workflow":{"name":"build","version":1},"started":"2026-01-02T03:04:05Z","finished":"2026-01-02T03:05:05Z",` +
		`"status":"blocked","error":"the test service is down",` +
		`"dev_pod":{"id":"pod-1","repository":"claims-build","source_branch":"main","work_branch":"agent/payment-date-0b7e3c1e"}}`
	_ = store.Put(ctx, "runs/"+gone+"/metadata.json", []byte(meta), pipeline.Meta{})
	code, body := call(t, http.MethodGet, srv.URL+"/api/runs/"+gone, "")
	if code != http.StatusOK || !strings.Contains(body, `"status":"Blocked"`) || !strings.Contains(body, `"error":"the test service is down"`) ||
		!strings.Contains(body, `"dev_pod":{"id":"pod-1","repository":"claims-build","source_branch":"main","work_branch":"agent/payment-date-0b7e3c1e"}`) {
		t.Fatalf("blocked run %d %s", code, body)
	}
	if strings.Contains(body, "pull_request") {
		t.Fatalf("a run without a pull request record names one: %s", body)
	}
	// The pull request record the connector service stored is reported by its object ID.
	_ = store.Put(ctx, "runs/"+gone+"/pull-request.json", []byte(`{"title":"Add payment date"}`), pipeline.Meta{})
	if code, body = call(t, http.MethodGet, srv.URL+"/api/runs/"+gone, ""); code != http.StatusOK || !strings.Contains(body, `"pull_request":"runs/`+gone+`/pull-request.json"`) {
		t.Fatalf("pull request %d %s", code, body)
	}

	// A run with history gets the pod and the pull request the same way.
	const live = "1c8f4d2f-2222-4b5b-8c8c-23456789abcd"
	runner.started = []pipeline.Input{{TraceID: live, Prefix: pipeline.RunPrefix(live)}}
	_ = store.Put(ctx, "runs/"+live+"/metadata.json", []byte(`{"dev_pod":{"id":"pod-2","repository":"claims-review","source_branch":"main","work_branch":"agent/review"}}`), pipeline.Meta{})
	_ = store.Put(ctx, "runs/"+live+"/pull-request.json", []byte(`{}`), pipeline.Meta{})
	code, body = call(t, http.MethodGet, srv.URL+"/api/runs/"+live, "")
	if code != http.StatusOK || !strings.Contains(body, `"dev_pod":{"id":"pod-2"`) || !strings.Contains(body, `"pull_request":"runs/`+live+`/pull-request.json"`) {
		t.Fatalf("live run %d %s", code, body)
	}
	// A run without a pod names none.
	const plain = "2d9a5e3a-3333-4c6c-9d9d-3456789abcde"
	runner.started = append(runner.started, pipeline.Input{TraceID: plain, Prefix: pipeline.RunPrefix(plain)})
	if code, body = call(t, http.MethodGet, srv.URL+"/api/runs/"+plain, ""); code != http.StatusOK || strings.Contains(body, "dev_pod") || strings.Contains(body, "pull_request") {
		t.Fatalf("plain run %d %s", code, body)
	}
}

func TestDecisionsListPublishRequests(t *testing.T) {
	srv, runner, _, _ := newTestServer(t)
	runner.pending = []PendingDecision{{WorkflowID: "t", TraceID: "t", Workflow: "build", PendingGate: pipeline.PendingGate{
		Step: "deliver.publish-2", Question: "Publish branch agent/x of claims-build and open the pull request 'Add payment date'?",
		Inputs: []pipeline.Item{{Source: "deliver", Artifact: "runs/t/deliver", Round: 1}}, Artifact: "runs/t/deliver.publish-2", Round: 1,
		Publish: &pipeline.PublishInfo{Step: "deliver", Pod: "pod-1", N: 2, Kind: pipeline.PublishPullRequest, Title: "Add payment date", Body: "Shows the date.", Repository: "claims-build", Branch: "agent/x"},
	}}}
	code, body := call(t, http.MethodGet, srv.URL+"/api/decisions", "")
	want := `"step":"deliver.publish-2","question":"Publish branch agent/x of claims-build and open the pull request 'Add payment date'?",` +
		`"inputs":[{"source":"deliver","artifact":"runs/t/deliver","round":1}],"artifact":"runs/t/deliver.publish-2","on_reject":"","round":1,` +
		`"publish":{"step":"deliver","pod":"pod-1","n":2,"kind":"pull_request","title":"Add payment date","body":"Shows the date.","repository":"claims-build","branch":"agent/x"}`
	if code != http.StatusOK || !strings.Contains(body, want) {
		t.Fatalf("decisions %d %s", code, body)
	}
	// A publish request is decided like a gate; rejecting it needs no reason.
	for _, decision := range []string{
		`{"workflow_id":"t","step":"deliver.publish-2","decision":"approved","decided_by":"flo"}`,
		`{"workflow_id":"t","step":"deliver.publish-2","decision":"rejected","decided_by":"flo"}`,
	} {
		if code, body = call(t, http.MethodPost, srv.URL+"/api/decisions", decision); code != http.StatusAccepted {
			t.Fatalf("%s: %d %s", decision, code, body)
		}
	}
	if d := runner.decided["t"]; d.Step != "deliver.publish-2" || d.Decision != pipeline.Rejected {
		t.Fatalf("decided %+v", runner.decided)
	}
}

func TestSavingDevPodStepsAndBackEdges(t *testing.T) {
	srv, _, _, workflows := newTestServer(t)
	def := `{"steps":[` +
		`{"id":"pod","service":"devpod","repository":"claims-build","source_branch":"main","work_branch":"agent/payment-date-{run}","name":"payment",` +
		`"protected":["tests/**","verify.sh"],"rules":[{"tool":"write_file","path":"src/panel/**","matches":"rejection ?reason","message":"Blocked by CL-7: fix it."}]},` +
		`{"id":"baseline","service":"command","depends_on":["pod"],"command":"./verify.sh","allow_failure":true},` +
		`{"id":"build","service":"agent","agent":"course/feature-builder@v1","depends_on":["baseline"]},` +
		`{"id":"verify","service":"command","depends_on":["build"],"command":"./verify.sh","timeout":"5m"},` +
		`{"id":"decide","service":"gate","depends_on":["verify"],"question":"Good enough?","on_reject":"build","max_rejections":3},` +
		`{"id":"stop","service":"devpod-stop","depends_on":["decide"]}]}`
	code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/ex06/versions", def)
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	for _, field := range []string{
		`"repository":"claims-build"`, `"source_branch":"main"`, `"work_branch":"agent/payment-date-{run}"`, `"name":"payment"`,
		`"protected":["tests/**","verify.sh"]`, `"rules":[{"tool":"write_file","path":"src/panel/**","matches":"rejection ?reason","message":"Blocked by CL-7: fix it."}]`,
		`"command":"./verify.sh","allow_failure":true`, `"on_reject":"build","max_rejections":3`, `"service":"devpod-stop"`,
	} {
		if !strings.Contains(body, field) {
			t.Errorf("saved version lacks %s: %s", field, body)
		}
	}
	// The stored YAML reads back to the same steps.
	saved, err := storedConfig(workflows)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := saved.find("ex06", 1)
	if !ok || len(stored.Steps) != 6 {
		t.Fatalf("stored %+v", stored)
	}
	pod, baseline, decide := stored.Steps[0], stored.Steps[1], stored.Steps[4]
	if pod.Repository != "claims-build" || pod.WorkBranch != "agent/payment-date-{run}" || pod.Name != "payment" || len(pod.Protected) != 2 ||
		len(pod.Rules) != 1 || pod.Rules[0] != (pipeline.Rule{Tool: "write_file", Path: "src/panel/**", Matches: "rejection ?reason", Message: "Blocked by CL-7: fix it."}) {
		t.Errorf("stored pod step %+v", pod)
	}
	if baseline.Command != "./verify.sh" || !baseline.AllowFailure || decide.OnReject != "build" || decide.MaxRejections != 3 {
		t.Errorf("stored steps %+v %+v", baseline, decide)
	}
	objects, _ := workflowObjects(workflows)
	for _, line := range []string{`"repository": "claims-build"`, `"work_branch": "agent/payment-date-{run}"`, `"allow_failure": true`, `"on_reject": "build"`, `"max_rejections": 3`, `"service": "devpod-stop"`} {
		if !strings.Contains(string(objects), line) {
			t.Errorf("the stored workflow lacks %s", line)
		}
	}
	// Each of these is refused on save, and nothing is stored.
	before, _ := workflowObjects(workflows)
	podStep := `{"id":"pod","service":"devpod","repository":"claims-build","source_branch":"main","work_branch":"agent/x"}`
	for name, tc := range map[string]struct{ steps, want string }{
		"two pods":        {podStep + `,{"id":"pod2","service":"devpod","repository":"claims-build","source_branch":"main","work_branch":"agent/y"}`, "only one"},
		"command no pod":  {`{"id":"verify","service":"command","command":"./verify.sh"}`, "must depend on the devpod step"},
		"stop no pod":     {podStep + `,{"id":"stop","service":"devpod-stop"}`, "must depend on the devpod step"},
		"same branch":     {`{"id":"pod","service":"devpod","repository":"claims-build","source_branch":"main","work_branch":"main"}`, "must differ"},
		"back to command": {podStep + `,{"id":"verify","service":"command","command":"ls","depends_on":["pod"]},{"id":"decide","service":"gate","depends_on":["verify"],"on_reject":"verify"}`, "on_reject must name an agent, interactive, team, or workflow step"},
		"back to later":   {`{"id":"a","service":"agent","agent":"a@v1"},{"id":"decide","service":"gate","depends_on":["a"],"on_reject":"b"},{"id":"b","service":"agent","agent":"b@v1","depends_on":["decide"]}`, "on_reject must name an agent, interactive, team, or workflow step"},
		"across a merge":  {`{"id":"a","service":"agent","agent":"a@v1"},{"id":"m","service":"merge","depends_on":["a"]},{"id":"b","service":"agent","agent":"b@v1","depends_on":["m"]},{"id":"decide","service":"gate","depends_on":["b"],"on_reject":"a"}`, "merge step"},
		"across a batch":  {`{"id":"a","service":"agent","agent":"a@v1"},{"id":"m","service":"batch","size":1,"depends_on":["a"]},{"id":"b","service":"agent","agent":"b@v1","depends_on":["m"]},{"id":"decide","service":"gate","depends_on":["b"],"on_reject":"a"}`, "batch step"},
	} {
		if code, body = call(t, http.MethodPost, srv.URL+"/api/workflows/bad/versions", `{"steps":[`+tc.steps+`]}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if after, _ := workflowObjects(workflows); !bytes.Equal(before, after) {
		t.Fatal("a refused version changed config.yaml")
	}
}

func TestRejectionAtAGateWithABackEdgeToAnAgentNeedsAComment(t *testing.T) {
	srv, runner, _, _ := newTestServer(t)
	runner.pending = []PendingDecision{{WorkflowID: "t", TraceID: "t", Workflow: "build", PendingGate: pipeline.PendingGate{Step: "decide", OnReject: "build", Round: 1, MaxRejections: 5}}}
	code, body := call(t, http.MethodGet, srv.URL+"/api/decisions", "")
	if code != http.StatusOK || !strings.Contains(body, `"on_reject":"build","round":1,"max_rejections":5`) {
		t.Fatalf("decisions %d %s", code, body)
	}
	bad := `{"workflow_id":"t","step":"decide","decision":"rejected","decided_by":"flo"}`
	if code, body = call(t, http.MethodPost, srv.URL+"/api/decisions", bad); code != http.StatusBadRequest || !strings.Contains(body, "needs a comment with the reason") {
		t.Fatalf("rejection without a reason %d %s", code, body)
	}
	ok := `{"workflow_id":"t","step":"decide","decision":"rejected","comment":"the date is missing","decided_by":"flo"}`
	if code, body = call(t, http.MethodPost, srv.URL+"/api/decisions", ok); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
}
