package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"
)

const header = "Trace: t1\nWorkflow: feature\nStep: plan\nOutput artifact: runs/t1/plan"

// firstUserMessage returns the user message of the first model request.
func firstUserMessage(t *testing.T, f *fakes) string {
	t.Helper()
	if len(f.llmReqs) == 0 {
		t.Fatal("no model request")
	}
	return f.llmReqs[0]["messages"].([]any)[1].(map[string]any)["content"].(string)
}

func TestStepRequestReadsTheItemList(t *testing.T) {
	var req StepRequest
	body := `{"trace_id":"t1","workflow":"feature","step_id":"plan","agent":"intake@v1","output_artifact":"runs/t1/plan",
		"inputs":[{"source":"short","artifact":"runs/t1/short","round":1},{"source":"a","artifact":"runs/t1/a.pass-2","round":2}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	want := []InputItem{{Source: "short", Artifact: "runs/t1/short", Round: 1}, {Source: "a", Artifact: "runs/t1/a.pass-2", Round: 2}}
	if len(req.Inputs) != 2 || req.Inputs[0] != want[0] || req.Inputs[1] != want[1] {
		t.Fatalf("inputs = %+v", req.Inputs)
	}
	// The map form no longer exists.
	if err := json.Unmarshal([]byte(`{"inputs":{"input":"runs/t1/input"}}`), &req); err == nil {
		t.Fatal("the map form of inputs was accepted")
	}
}

func TestFirstMessageUnpacksInputsInListOrder(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, respond(finalResponse))
	req := request()
	// The list order is kept: "intake" comes before "input".
	req.Inputs = []InputItem{{Source: "intake", Artifact: "runs/t1/intake", Round: 1}, {Source: "input", Artifact: "runs/t1/input", Round: 1}}
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := header + "\n\n## Input \"intake\" (object runs/t1/intake)\nintent record" +
		"\n\n## Input \"input\" (object runs/t1/input)\nplease build a thing"
	if got := firstUserMessage(t, f); got != want {
		t.Fatalf("first message =\n%s\nwant\n%s", got, want)
	}
	var rec RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.Events[0].Inputs, ","); got != "runs/t1/intake,runs/t1/input" {
		t.Fatalf("event input artifacts = %q", got)
	}
}

func TestFirstMessageWritesTheRoundOnlyForARepeatedSource(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, respond(finalResponse))
	f.blobs["runs/t1/a.pass-1"] = []byte("first a")
	f.blobs["runs/t1/a.pass-2"] = []byte("second a")
	req := request()
	req.Inputs = []InputItem{{Source: "a", Artifact: "runs/t1/a.pass-1", Round: 1}, {Source: "input", Artifact: "runs/t1/input", Round: 1}, {Source: "a", Artifact: "runs/t1/a.pass-2", Round: 2}}
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := header + "\n\n## Input \"a\" round 1 (object runs/t1/a.pass-1)\nfirst a" +
		"\n\n## Input \"input\" (object runs/t1/input)\nplease build a thing" +
		"\n\n## Input \"a\" round 2 (object runs/t1/a.pass-2)\nsecond a"
	if got := firstUserMessage(t, f); got != want {
		t.Fatalf("first message =\n%s\nwant\n%s", got, want)
	}
}

func TestFirstMessageListsReferencesWithoutUnpacking(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":false,"models":["m1"]}`, respond(finalResponse))
	req := request()
	// The artifacts are not read, so they do not have to exist.
	req.Inputs = []InputItem{{Source: "short", Artifact: "merge-demo/short", Round: 1}, {Source: "long-b", Artifact: "merge-demo/long-b", Round: 1}}
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := header + "\n\nInputs (read each with get_artifact):\n" +
		"- \"short\" round 1: object merge-demo/short\n" +
		"- \"long-b\" round 1: object merge-demo/long-b"
	if got := firstUserMessage(t, f); got != want {
		t.Fatalf("first message =\n%s\nwant\n%s", got, want)
	}
	if system := f.llmReqs[0]["messages"].([]any)[0].(map[string]any)["content"]; system != "You are intake." {
		t.Fatalf("system message changed: %q", system)
	}
}

const (
	noUnpackJSON  = `{"schema_version":1,"name":"intake","models":["m1"],"tools":[]}`
	noUnpackError = `agent intake@v1: agent.json has no "unpack_inputs"; it must be true or false`
)

func assertMissingUnpack(t *testing.T, f *fakes, err error) {
	t.Helper()
	var app *temporal.ApplicationError
	if err == nil || !strings.Contains(err.Error(), noUnpackError) {
		t.Fatalf("error = %v", err)
	}
	if !temporal.IsApplicationError(err) {
		t.Fatalf("error is not an application error: %v", err)
	}
	if app, _ = err.(*temporal.ApplicationError); app == nil || !app.NonRetryable() || app.Type() != "InvalidAgent" {
		t.Fatalf("error is retryable or has the wrong type: %#v", err)
	}
	if len(f.llmReqs) != 0 {
		t.Fatal("the model was called")
	}
}

func TestProcessFailsWithoutUnpackInputs(t *testing.T) {
	f := newFakes(t, noUnpackJSON, respond(finalResponse))
	_, err := f.svc.Process(context.Background(), request())
	assertMissingUnpack(t, f, err)
	var rec RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != "failed" || !strings.Contains(rec.Error, noUnpackError) {
		t.Fatalf("record = %+v", rec)
	}
}

func TestTeamTurnFailsWithoutUnpackInputs(t *testing.T) {
	f := teamFakes(t, noUnpackJSON, respond(finalResponse))
	_, err := f.svc.TeamTurn(context.Background(), leadTurn())
	assertMissingUnpack(t, f, err)
}

func TestChatFailsWithoutUnpackInputs(t *testing.T) {
	f := newFakes(t, noUnpackJSON, respond(finalResponse))
	code, out := postChat(t, f, chatBody())
	if code != http.StatusUnprocessableEntity || out["error"] != noUnpackError {
		t.Fatalf("chat = %d %v", code, out)
	}
	if len(f.llmReqs) != 0 {
		t.Fatal("the model was called")
	}
}
