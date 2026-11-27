package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDevPodNoteOnlyRequestsAllowedCommits(t *testing.T) {
	for _, commands := range [][]string{nil, {"./verify.sh"}, {"git add *"}, {"git commit *"}} {
		if strings.Contains(devPodNote(commands), "Commit your work") {
			t.Errorf("requested commits without both permissions: %v", commands)
		}
	}
	if !strings.Contains(devPodNote([]string{"git add *", "git commit *"}), "Commit your work") {
		t.Error("agents with both permissions lost commit instructions")
	}
}

const devPodAgentJSON = `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"max_turns":6,
	"tools":["devpod/read_file","devpod/push_branch","devpod/report_blocked","factory/send_message"],
	"commands":["./verify.sh","git add *","git commit *"]}`

const devPodNoteText = "\n\n## Dev pod\n" +
	"You work in the project folder of a Git repository, on its own work branch.\n" +
	"- Use the tools to list, read, search, write and edit files and to run commands. Tool names may carry a prefix such as `devpod__`.\n" +
	"- All paths are relative to the project folder.\n" +
	"- You can only run these commands (`*` stands for any text): `./verify.sh`, `git add *`, `git commit *`. Every other command is refused.\n" +
	"- Commit your work with the command tool: first `git add <files>`, then `git commit -m \"<what you changed>\"`.\n" +
	"- Never try to push or to switch branches.\n" +
	"- If you cannot go on, call the tool report_blocked with the reason."

// podGateway records the tool calls that reached the MCP services `devpod` and `factory`.
type podGateway struct {
	names []string
	calls []map[string]any
	fail  bool // tool calls answer with a tool error
}

// toolRequest is the part of an MCP request the fake gateways read.
type toolRequest struct {
	Method string `json:"method"`
	Params struct {
		Arguments map[string]any `json:"arguments"`
		Name      string         `json:"name"`
	} `json:"params"`
	ID json.RawMessage `json:"id"`
}

// Values the dev pod tests use again and again.
const (
	testPod        = "pod-1"
	serviceDown    = "The test service is down."
	mcpFactoryPath = "/mcp/factory"
)

// devPodMCP points the service at a gateway with the MCP services `devpod` and `factory`.
func devPodMCP(t *testing.T, f *fakes) *podGateway {
	t.Helper()
	g := &podGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/devpod" && r.URL.Path != mcpFactoryPath {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var msg toolRequest
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Error(err)
			return
		}
		reply := func(result string) {
			w.Header().Set("Content-Type", "application/json")
			writeBody(t, w, `{"jsonrpc":"2.0","id":`+string(msg.ID)+`,"result":`+result+`}`)
		}
		switch msg.Method {
		case mcpInitialize:
			w.Header().Set("Mcp-Session-Id", "session-pod")
			reply(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}`)
		case mcpToolsList:
			if r.URL.Path == mcpFactoryPath {
				// A tool of another service keeps an argument named like a dev pod one.
				reply(`{"tools":[{"name":"send_message","description":"Send","inputSchema":{"type":"object","properties":{"run":{"type":"string"},"text":{"type":"string"}},"required":["run","text"]}}]}`)
				return
			}
			identity := `"pod":{"type":"string"},"agent":{"type":"string"},"run":{"type":"string"}`
			reply(`{"tools":[
				{"name":"read_file","description":"Read a file","inputSchema":{"type":"object","properties":{` + identity + `,"path":{"type":"string"},"start_line":{"type":"integer"}},"required":["pod","agent","run","path"]}},
				{"name":"push_branch","description":"Push","inputSchema":{"type":"object","properties":{` + identity + `},"required":["pod","agent","run"]}},
				{"name":"report_blocked","description":"Report blocked","inputSchema":{"type":"object","properties":{` + identity + `,"reason":{"type":"string"}},"required":["pod","agent","run","reason"]}}]}`)
		case mcpToolsCall:
			f.mu.Lock()
			g.names = append(g.names, msg.Params.Name)
			g.calls = append(g.calls, msg.Params.Arguments)
			fail := g.fail
			f.mu.Unlock()
			if fail {
				reply(`{"content":[{"type":"text","text":"refused by the pod"}],"isError":true}`)
			} else {
				reply(`{"content":[{"type":"text","text":"ok"}],"isError":false}`)
			}
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	f.svc.MCPGatewayURL = srv.URL
	return g
}

// podCalls is a model answer that calls the named tools; each call is {alias, arguments}.
func podCalls(calls ...[2]string) llmHandler {
	list := make([]map[string]any, len(calls))
	for i, c := range calls {
		list[i] = map[string]any{"id": "c" + string(rune('a'+i)), "type": "function", "function": map[string]string{"name": c[0], "arguments": c[1]}}
	}
	data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": roleAssistant, "content": nil, "tool_calls": list}}}})
	return respond(string(data))
}

func podRequest() *StepRequest {
	req := request()
	req.DevPod = testPod
	return req
}

// shownSchemas returns the parameter schemas of the tools in a model request by tool name.
func shownSchemas(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, tool := range jsonList(t, body["tools"]) {
		fn := jsonObject(t, jsonObject(t, tool)["function"])
		out[jsonString(t, fn["name"])] = jsonObject(t, fn["parameters"])
	}
	return out
}

func TestRequestsReadTheDevPod(t *testing.T) {
	var step StepRequest
	var chat sessionChatRequest
	var turn TeamTurnRequest
	for _, v := range []any{&step, &chat, &turn} {
		if err := json.Unmarshal([]byte(`{"dev_pod":"pod-1"}`), v); err != nil {
			t.Fatal(err)
		}
	}
	if step.DevPod != testPod || chat.DevPod != testPod || turn.DevPod != testPod {
		t.Fatalf("dev_pod = %q %q %q", step.DevPod, chat.DevPod, turn.DevPod)
	}
}

// The model does not choose the pod or who it is: the harness sets pod, agent and run.
func TestDevPodToolCallsCarryPodAgentAndRun(t *testing.T) {
	f := newFakes(t, devPodAgentJSON,
		podCalls(
			[2]string{"devpod__read_file", `{"path":"src/panel.py","pod":"someone-elses-pod","agent":"admin@v9","run":"other-run"}`},
			[2]string{"devpod__push_branch", ``},
			[2]string{"factory__send_message", `{"run":"mine","text":"hi"}`},
		), respond(finalResponse))
	g := devPodMCP(t, f)
	res, err := f.svc.Process(context.Background(), podRequest())
	if err != nil || res.Blocked != "" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if strings.Join(g.names, ",") != "read_file,push_branch,send_message" {
		t.Fatalf("forwarded tools = %v", g.names)
	}
	if c := g.calls[0]; c["pod"] != testPod || c["agent"] != intakeRef || c["run"] != "t1" || c["path"] != "src/panel.py" || len(c) != 4 {
		t.Fatalf("read_file arguments = %v", c)
	}
	if c := g.calls[1]; c["pod"] != testPod || c["agent"] != intakeRef || c["run"] != "t1" || len(c) != 3 {
		t.Fatalf("push_branch arguments = %v", c)
	}
	if c := g.calls[2]; c["run"] != "mine" || c["pod"] != nil || c["agent"] != nil {
		t.Fatalf("a tool of another service was changed: %v", c)
	}
	// The stored tool call is what the model wrote.
	var rec RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if stored, _ := json.Marshal(rec.Messages[2]); !strings.Contains(string(stored), "someone-elses-pod") {
		t.Fatalf("stored tool call = %s", stored)
	}
	if rec.Status != statusCompleted {
		t.Fatalf("record status = %q", rec.Status)
	}
}

func TestDevPodToolSchemasHideTheHarnessArguments(t *testing.T) {
	f := newFakes(t, devPodAgentJSON, respond(finalResponse))
	devPodMCP(t, f)
	if _, err := f.svc.Process(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	schemas := shownSchemas(t, f.llmReqs[0])
	read := schemas["devpod__read_file"]
	props := jsonObject(t, read["properties"])
	if len(props) != 2 || props["path"] == nil || props["start_line"] == nil {
		t.Fatalf("read_file properties = %v", props)
	}
	if required, _ := json.Marshal(read["required"]); string(required) != `["path"]` {
		t.Fatalf("read_file required = %s", required)
	}
	push := schemas["devpod__push_branch"]
	if _, has := push["required"]; has || len(jsonObject(t, push["properties"])) != 0 || push["type"] != "object" {
		t.Fatalf("push_branch schema = %v", push)
	}
	if required, _ := json.Marshal(schemas["devpod__report_blocked"]["required"]); string(required) != `["reason"]` {
		t.Fatalf("report_blocked required = %s", required)
	}
	// Another service's schema stays as offered.
	send := schemas["factory__send_message"]
	if required, _ := json.Marshal(send["required"]); string(required) != `["run","text"]` || jsonObject(t, send["properties"])["run"] == nil {
		t.Fatalf("send_message schema = %v", send)
	}
}

func TestTrimDevPodSchemaKeepsWhatItCannotRead(t *testing.T) {
	for _, raw := range []string{`"text"`, `[1]`, `{"type":"object","properties":3,"required":"pod"}`} {
		var got, want any
		if err := json.Unmarshal(trimDevPodSchema(json.RawMessage(raw)), &got); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		_ = json.Unmarshal([]byte(raw), &want)
		x, _ := json.Marshal(got)
		y, _ := json.Marshal(want)
		if !bytes.Equal(x, y) {
			t.Fatalf("%s became %s", raw, x)
		}
	}
}

func TestDevPodToolWithoutADevPodIsAToolError(t *testing.T) {
	f := newFakes(t, devPodAgentJSON,
		podCalls([2]string{"devpod__read_file", `{"path":"a.py","pod":"guessed"}`}, [2]string{"devpod__report_blocked", `{"reason":"x"}`}),
		respond(finalResponse))
	g := devPodMCP(t, f)
	res, err := f.svc.Process(context.Background(), request())
	if err != nil || res.Blocked != "" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(g.calls) != 0 {
		t.Fatalf("a dev pod tool was forwarded without a dev pod: %v", g.calls)
	}
	sent := jsonList(t, f.llmReqs[1]["messages"])
	want := `Error: tool "devpod__read_file" works in a dev pod, and this run has none. The workflow needs a devpod step before this step.`
	if got := jsonObject(t, sent[3])["content"]; got != want {
		t.Fatalf("tool message = %q", got)
	}
	if got := string(f.blobs[planArtifact]); got != planAnswer {
		t.Fatalf("stored answer = %q", got)
	}
	if strings.Contains(firstUserMessage(t, f), "Dev pod") {
		t.Fatalf("a run without a dev pod got the note: %s", firstUserMessage(t, f))
	}
}

func TestProcessFirstMessageEndsWithTheDevPodNote(t *testing.T) {
	f := newFakes(t, devPodAgentJSON, respond(finalResponse))
	devPodMCP(t, f)
	if _, err := f.svc.Process(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	want := header + "\n\n## Input \"input\" (object runs/t1/input)\nplease build a thing" +
		"\n\n## Input \"intake\" (object runs/t1/intake)\nintent record" + devPodNoteText
	if got := firstUserMessage(t, f); got != want {
		t.Fatalf("first message =\n%s\nwant\n%s", got, want)
	}
	if system := jsonObject(t, jsonList(t, f.llmReqs[0]["messages"])[0])["content"]; system != intakePrompt {
		t.Fatalf("system message changed: %q", system)
	}
}

func TestDevPodNoteWithoutCommands(t *testing.T) {
	for _, commands := range [][]string{nil, {}, {" "}} {
		note := devPodNote(commands)
		if !strings.Contains(note, "- You are not allowed to run any command.\n") || strings.Contains(note, "You can only run") {
			t.Fatalf("note = %s", note)
		}
	}
}

func TestProcessBlockedEndsTheWorkAtOnce(t *testing.T) {
	f := newFakes(t, devPodAgentJSON,
		podCalls(
			[2]string{"devpod__report_blocked", `{"reason":"The test service is down.","pod":"x"}`},
			[2]string{"devpod__read_file", `{"path":"a.py"}`},
		), respond(finalResponse))
	g := devPodMCP(t, f)
	res, err := f.svc.Process(context.Background(), podRequest())
	if err != nil {
		t.Fatal(err)
	}
	if res.Artifact != planArtifact || res.Blocked != serviceDown {
		t.Fatalf("result = %+v", res)
	}
	data, _ := json.Marshal(res)
	if string(data) != `{"artifact":"runs/t1/plan","blocked":"The test service is down."}` {
		t.Fatalf("result JSON = %s", data)
	}
	if got := string(f.blobs[planArtifact]); got != serviceDown {
		t.Fatalf("output document = %q", got)
	}
	if len(f.llmReqs) != 1 || strings.Join(g.names, ",") != "report_blocked" {
		t.Fatalf("model calls = %d, forwarded tools = %v", len(f.llmReqs), g.names)
	}
	if c := g.calls[0]; c["pod"] != testPod || c["agent"] != intakeRef || c["run"] != "t1" || c["reason"] != serviceDown {
		t.Fatalf("report_blocked arguments = %v", c)
	}
	for _, id := range []string{"runs/t1/plan.run.json", "runs/t1/plan.attempt-1.run.json"} {
		var rec RunRecord
		if err := json.Unmarshal(f.blobs[id], &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Status != statusBlocked || rec.Error != serviceDown || rec.ModelCalls != 1 {
			t.Fatalf("%s: status %q, error %q, model calls %d", id, rec.Status, rec.Error, rec.ModelCalls)
		}
	}
}

func TestProcessResultWithoutBlockedHasNoBlockedField(t *testing.T) {
	data, _ := json.Marshal(StepResult{Artifact: planArtifact})
	if string(data) != `{"artifact":"runs/t1/plan"}` {
		t.Fatalf("result JSON = %s", data)
	}
}

func TestProcessBlockedWithoutAReason(t *testing.T) {
	f := newFakes(t, devPodAgentJSON, podCalls([2]string{"devpod__report_blocked", `{}`}), respond(finalResponse))
	devPodMCP(t, f)
	res, err := f.svc.Process(context.Background(), podRequest())
	if err != nil || res.Blocked != noBlockedReason || string(f.blobs[planArtifact]) != noBlockedReason {
		t.Fatalf("result = %+v, %v, document %q", res, err, f.blobs[planArtifact])
	}
}

// A report_blocked the connector service refused does not end the work.
func TestProcessARefusedReportBlockedGoesOn(t *testing.T) {
	f := newFakes(t, devPodAgentJSON, podCalls([2]string{"devpod__report_blocked", `{"reason":"stuck"}`}), respond(finalResponse))
	g := devPodMCP(t, f)
	g.fail = true
	res, err := f.svc.Process(context.Background(), podRequest())
	if err != nil || res.Blocked != "" || len(f.llmReqs) != 2 {
		t.Fatalf("result = %+v, %v, model calls %d", res, err, len(f.llmReqs))
	}
	if got := string(f.blobs[planArtifact]); got != planAnswer {
		t.Fatalf("stored answer = %q", got)
	}
	var rec RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil || rec.Status != statusCompleted {
		t.Fatalf("record status = %q, %v", rec.Status, err)
	}
}

func podChatBody() map[string]string {
	body := chatBody()
	body["dev_pod"] = testPod
	return body
}

func TestChatDevPodNoteAndArguments(t *testing.T) {
	f := newFakes(t, devPodAgentJSON, podCalls([2]string{"devpod__read_file", `{"path":"a.py","run":"x"}`}), respond(finalResponse))
	g := devPodMCP(t, f)
	status, out := postChat(t, f, podChatBody())
	if status != http.StatusOK || out["reply"] != planAnswer {
		t.Fatalf("response = %d %v", status, out)
	}
	if c := g.calls[0]; c["pod"] != testPod || c["agent"] != intakeRef || c["run"] != "t1" || c["path"] != "a.py" {
		t.Fatalf("read_file arguments = %v", c)
	}
	_, messages := storedChat(t, f)
	want := intakePrompt + sessionNote("user-sessions/t1/talk") + devPodNoteText
	if messages[0]["content"] != want {
		t.Fatalf("system message =\n%s\nwant\n%s", messages[0]["content"], want)
	}
	if messages[1]["content"] != chatMessage {
		t.Fatalf("user message = %q", messages[1]["content"])
	}
	if required, _ := json.Marshal(shownSchemas(t, f.llmReqs[0])["devpod__read_file"]["required"]); string(required) != `["path"]` {
		t.Fatalf("read_file required = %s", required)
	}
}

func TestChatWithoutADevPod(t *testing.T) {
	f := newFakes(t, devPodAgentJSON, podCalls([2]string{"devpod__read_file", `{"path":"a.py"}`}), respond(finalResponse))
	g := devPodMCP(t, f)
	if status, out := postChat(t, f, chatBody()); status != http.StatusOK {
		t.Fatalf("response = %d %v", status, out)
	}
	_, messages := storedChat(t, f)
	if strings.Contains(jsonString(t, messages[0]["content"]), "Dev pod") {
		t.Fatalf("system message = %s", messages[0]["content"])
	}
	if len(g.calls) != 0 || !strings.HasPrefix(jsonString(t, messages[3]["content"]), `Error: tool "devpod__read_file" works in a dev pod`) {
		t.Fatalf("calls = %v, tool message = %v", g.calls, messages[3]["content"])
	}
}

func TestChatBlockedReasonIsTheReply(t *testing.T) {
	f := newFakes(t, devPodAgentJSON,
		podCalls(
			[2]string{"devpod__read_file", `{"path":"a.py"}`},
			[2]string{"devpod__report_blocked", `{"reason":"I may not change the tests."}`},
			[2]string{"devpod__read_file", `{"path":"b.py"}`},
		), respond(finalResponse))
	g := devPodMCP(t, f)
	status, out := postChat(t, f, podChatBody())
	if status != http.StatusOK || out["reply"] != "I may not change the tests." {
		t.Fatalf("response = %d %v", status, out)
	}
	if len(f.llmReqs) != 1 || strings.Join(g.names, ",") != "read_file,report_blocked" {
		t.Fatalf("model calls = %d, forwarded tools = %v", len(f.llmReqs), g.names)
	}
	// Every tool call has its answer, so the log can be sent again.
	_, messages := storedChat(t, f)
	if len(messages) != 6 || messages[3]["tool_call_id"] != "ca" || messages[4]["tool_call_id"] != "cb" || messages[5]["tool_call_id"] != "cc" ||
		messages[5]["content"] != "Not run: the turn ended because you reported that you are blocked." {
		t.Fatalf("messages = %v", messages)
	}
}

func podCoderTurn() *TeamTurnRequest {
	req := coderTurn()
	req.DevPod = testPod
	return req
}

func TestTeamTurnDevPodNoteAndArguments(t *testing.T) {
	f := teamFakes(t, devPodAgentJSON, podCalls([2]string{"devpod__read_file", `{"path":"a.py","pod":"x","team":"kept"}`}), respond(finalResponse))
	g := devPodMCP(t, f)
	res, err := f.svc.TeamTurn(context.Background(), podCoderTurn())
	if err != nil || res.Blocked != "" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if c := g.calls[0]; g.names[0] != "read_file" || c["pod"] != testPod || c["agent"] != intakeRef || c["run"] != "t1" || c["team"] != "kept" {
		t.Fatalf("read_file arguments = %v", c)
	}
	_, messages := storedMember(t, f, teamPath+"/members/coder.json")
	if want := "[Message from lead on channel coder--lead]\nwrite it" + devPodNoteText; messages[1]["content"] != want {
		t.Fatalf("first user message =\n%s\nwant\n%s", messages[1]["content"], want)
	}
	if strings.Contains(jsonString(t, messages[0]["content"]), "Dev pod") {
		t.Fatalf("system message = %s", messages[0]["content"])
	}

	// A later turn does not repeat the note.
	f.blobs[teamPath+"/channels/coder--lead/lead.json"] = []byte(`{"messages":[
		{"seq":1,"text":"write it","sent":"2026-01-02T03:04:06.000000Z"},{"seq":2,"text":"go on","sent":"2026-01-02T03:05:06.000000Z"}]}`)
	if _, err := f.svc.TeamTurn(context.Background(), podCoderTurn()); err != nil {
		t.Fatal(err)
	}
	_, messages = storedMember(t, f, teamPath+"/members/coder.json")
	last := messages[len(messages)-2]
	if last["content"] != "[Message from lead on channel coder--lead]\ngo on" {
		t.Fatalf("later user message = %q", last["content"])
	}
}

func TestTeamTurnWithoutADevPodHasNoNote(t *testing.T) {
	f := teamFakes(t, devPodAgentJSON, podCalls([2]string{"devpod__read_file", `{"path":"a.py"}`}), respond(finalResponse))
	g := devPodMCP(t, f)
	if _, err := f.svc.TeamTurn(context.Background(), coderTurn()); err != nil {
		t.Fatal(err)
	}
	_, messages := storedMember(t, f, teamPath+"/members/coder.json")
	if messages[1]["content"] != "[Message from lead on channel coder--lead]\nwrite it" {
		t.Fatalf("first user message = %q", messages[1]["content"])
	}
	if len(g.names) != 1 || g.names[0] != teamSendTool || !strings.HasPrefix(jsonString(t, messages[3]["content"]), `Error: tool "devpod__read_file" works in a dev pod`) {
		t.Fatalf("forwarded tools = %v, tool message = %v", g.names, messages[3]["content"])
	}
}

func TestTeamTurnBlockedReasonIsTheAnswer(t *testing.T) {
	f := teamFakes(t, devPodAgentJSON,
		podCalls(
			[2]string{"devpod__report_blocked", `{"reason":"The repository has no tests folder."}`},
			[2]string{"devpod__read_file", `{"path":"a.py"}`},
		), respond(finalResponse))
	g := devPodMCP(t, f)
	res, err := f.svc.TeamTurn(context.Background(), podCoderTurn())
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocked != "The repository has no tests folder." || res.ModelCalls != 1 || len(f.llmReqs) != 1 {
		t.Fatalf("result = %+v, model calls %d", res, len(f.llmReqs))
	}
	// The reason is the member's answer: it is sent as its reply to the member who wrote to it.
	if strings.Join(g.names, ",") != "report_blocked,send_message" {
		t.Fatalf("forwarded tools = %v", g.names)
	}
	if c := g.calls[1]; c["channel"] != coderLeadChannel || c["text"] != "The repository has no tests folder." || c["member"] != coderMember {
		t.Fatalf("send_message arguments = %v", c)
	}
	_, messages := storedMember(t, f, teamPath+"/members/coder.json")
	if len(messages) != 5 || messages[3]["tool_call_id"] != "ca" || messages[4]["tool_call_id"] != "cb" || !strings.HasPrefix(jsonString(t, messages[4]["content"]), "Not run:") {
		t.Fatalf("messages = %v", messages)
	}
}

func rejectionRequest(f *fakes) *StepRequest {
	f.blobs["runs/t1/review.1"] = []byte(`{"step":"review","decision":"reject","comment":"The payment date is missing.","decided_by":"flo"}`)
	f.blobs["runs/t1/plan.round-1"] = []byte("old plan")
	f.blobs["runs/t1/check.round-1"] = []byte("check report")
	req := request()
	// The marked items are rendered after the normal inputs, wherever they stand in the list.
	req.Inputs = []InputItem{
		{Source: "input", Artifact: "runs/t1/input", Round: 1},
		{Source: "review", Artifact: "runs/t1/review.1", Round: 1, Kind: inputKindRejection},
		{Source: "plan", Artifact: "runs/t1/plan.round-1", Round: 1, Kind: inputKindPrevious},
		{Source: "check", Artifact: "runs/t1/check.round-1", Round: 1, Kind: inputKindReviewed},
		{Source: "intake", Artifact: "runs/t1/intake", Round: 1},
	}
	return req
}

func TestStepRequestReadsTheItemKind(t *testing.T) {
	var req StepRequest
	body := `{"inputs":[{"source":"input","artifact":"runs/t1/input","round":1},{"source":"review","artifact":"runs/t1/review.1","round":1,"kind":"rejection"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Inputs[0].Kind != "" || req.Inputs[1].Kind != inputKindRejection {
		t.Fatalf("inputs = %+v", req.Inputs)
	}
}

func TestFirstMessageRendersARejection(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, respond(finalResponse))
	if _, err := f.svc.Process(context.Background(), rejectionRequest(f)); err != nil {
		t.Fatal(err)
	}
	want := header + "\n\n## Input \"input\" (object runs/t1/input)\nplease build a thing" +
		"\n\n## Input \"intake\" (object runs/t1/intake)\nintent record" +
		"\n\n## Rejection from \"review\" (round 1)\nThe payment date is missing." +
		"\n\n## Your previous result (round 1) (object runs/t1/plan.round-1)\nold plan" +
		"\n\n## Input \"check\" round 1 (object runs/t1/check.round-1)\ncheck report" +
		"\n\nChange your previous result so that the rejection no longer applies. Keep what was not criticised."
	if got := firstUserMessage(t, f); got != want {
		t.Fatalf("first message =\n%s\nwant\n%s", got, want)
	}
}

func TestFirstMessageRendersARejectionRecordItCannotRead(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, respond(finalResponse))
	req := rejectionRequest(f)
	f.blobs["runs/t1/review.1"] = []byte("Too long.")
	req.Inputs = req.Inputs[1:2]
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := header + "\n\n## Rejection from \"review\" (round 1)\nToo long.\n\n" + rejectionSentence
	if got := firstUserMessage(t, f); got != want {
		t.Fatalf("first message =\n%s\nwant\n%s", got, want)
	}
}

func TestFirstMessageListsARejectionWithoutUnpacking(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":false,"models":["m1"]}`, respond(finalResponse))
	req := rejectionRequest(f)
	// The artifacts are not read, so they do not have to exist.
	delete(f.blobs, "runs/t1/review.1")
	delete(f.blobs, "runs/t1/plan.round-1")
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := header + "\n\nInputs (read each with get_artifact):\n" +
		"- \"input\" round 1: object runs/t1/input\n" +
		"- \"intake\" round 1: object runs/t1/intake" +
		"\n\nRejection (read each with get_artifact):\n" +
		"- rejection from \"review\" round 1: object runs/t1/review.1\n" +
		"- your previous result round 1: object runs/t1/plan.round-1\n" +
		"- reviewed input \"check\" round 1: object runs/t1/check.round-1" +
		"\n\nChange your previous result so that the rejection no longer applies. Keep what was not criticised."
	if got := firstUserMessage(t, f); got != want {
		t.Fatalf("first message =\n%s\nwant\n%s", got, want)
	}
}

func TestFirstMessageWithARejectionEndsWithTheDevPodNote(t *testing.T) {
	f := newFakes(t, devPodAgentJSON, respond(finalResponse))
	devPodMCP(t, f)
	req := rejectionRequest(f)
	req.DevPod = testPod
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := firstUserMessage(t, f); !strings.HasSuffix(got, rejectionSentence+devPodNoteText) {
		t.Fatalf("first message =\n%s", got)
	}
}
