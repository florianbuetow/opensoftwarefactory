package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	teamPath         = "teams/t1/build"
	teamMemberID     = teamPath + "/members/lead.json"
	leadMember       = "lead"
	coderMember      = "coder"
	coderLeadChannel = "coder--lead"
)

const teamJSON = `{"lead":"lead","members":[
	{"name":"lead","kind":"agent","ref":"intake@v1","description":"Coordinates the team."},
	{"name":"coder","kind":"agent","ref":"intake@v1","description":"Writes the\n implementation."},
	{"name":"tester","kind":"agent","ref":"intake@v1","description":"Writes the tests."}],
	"channels":[{"id":"coder--lead","members":["coder","lead"]},{"id":"lead--tester","members":["lead","tester"]},{"id":"coder--tester","members":["coder","tester"]}],
	"trace_id":"t1","step_id":"build"}`

func teamFakes(t *testing.T, agentJSON string, llm ...llmHandler) *fakes {
	f := newFakes(t, agentJSON, llm...)
	f.blobs[teamPath+"/team.json"] = []byte(teamJSON)
	// One queue per sender: a channel is <id>/<sender>.json for each side, seq counting per queue.
	f.blobs[teamPath+"/channels/lead--workflow/workflow.json"] = []byte(`{"channel":"lead--workflow","from":"workflow","to":"lead","messages":[
		{"seq":1,"text":"Build a parser.","sent":"2026-01-02T03:04:05.000000Z"}]}`)
	f.blobs[teamPath+"/channels/coder--lead/lead.json"] = []byte(`{"channel":"coder--lead","from":"lead","to":"coder","messages":[
		{"seq":1,"text":"write it","sent":"2026-01-02T03:04:06.000000Z"}]}`)
	f.blobs[teamPath+"/channels/coder--lead/coder.json"] = []byte(`{"channel":"coder--lead","from":"coder","to":"lead","messages":[
		{"seq":1,"text":"wrote parser.py","sent":"2026-01-02T03:04:09.000000Z"}]}`)
	f.blobs[teamPath+"/channels/lead--tester/tester.json"] = []byte(`{"channel":"lead--tester","from":"tester","to":"lead","messages":[
		{"seq":1,"text":"wrote test_parser.py","sent":"2026-01-02T03:04:07.500000Z"}]}`)
	f.blobs[teamPath+"/channels/coder--tester/coder.json"] = []byte(`{"channel":"coder--tester","from":"coder","to":"tester","messages":[
		{"seq":1,"text":"not for the lead","sent":"2026-01-02T03:04:01.000000Z"}]}`)
	return f
}

func leadTurn() *TeamTurnRequest {
	return &TeamTurnRequest{TraceID: "t1", StepID: "build", Team: teamPath, Member: leadMember, Agent: intakeRef}
}

func storedMember(t *testing.T, f *fakes, id string) (log teamMemberLog, messages []map[string]any) {
	t.Helper()
	f.mu.Lock()
	data := f.blobs[id]
	f.mu.Unlock()
	if err := json.Unmarshal(data, &log); err != nil {
		t.Fatalf("stored member history: %v: %s", err, data)
	}
	messages = make([]map[string]any, len(log.Messages))
	for i, m := range log.Messages {
		if err := json.Unmarshal(m, &messages[i]); err != nil {
			t.Fatal(err)
		}
	}
	return log, messages
}

func TestTeamTurnFirstTurnDeliversUnreadOldestFirst(t *testing.T) {
	f := teamFakes(t, chatAgentJSON, respond(finalResponse))
	f.prompt = "You are the lead. {{policy::security/review}}"
	res, err := f.svc.TeamTurn(context.Background(), leadTurn())
	if err != nil || res.ModelCalls != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	log, messages := storedMember(t, f, teamMemberID)
	if log.Member != leadMember || log.Agent != intakeRef {
		t.Fatalf("header = %+v", log)
	}
	if len(log.Read) != 3 || log.Read["lead--workflow"] != 1 || log.Read[coderLeadChannel] != 1 || log.Read["lead--tester"] != 1 {
		t.Fatalf("read markers = %v", log.Read)
	}
	if len(messages) != 5 || messages[0]["role"] != roleSystem || messages[4]["role"] != roleAssistant {
		t.Fatalf("messages = %v", messages)
	}
	for i, want := range []string{
		"[Message from workflow on channel lead--workflow]\nBuild a parser.",
		"[Message from tester on channel lead--tester]\nwrote test_parser.py",
		"[Message from coder on channel coder--lead]\nwrote parser.py",
	} {
		if messages[i+1]["role"] != roleUser || messages[i+1]["content"] != want {
			t.Fatalf("message %d = %v, want %q", i+1, messages[i+1], want)
		}
	}
	system := jsonString(t, messages[0]["content"])
	if !strings.HasPrefix(system, "You are the lead. Follow the security policy.") {
		t.Fatalf("system prompt was not compiled: %q", system)
	}
	for _, want := range []string{
		"team member `lead` in the team `teams/t1/build`", "You are the lead of this team",
		"channel `coder--lead` -> reaches `coder`. Writes the implementation.",
		"channel `lead--tester` -> reaches `tester`. Writes the tests.",
		"channel `lead--workflow` -> reaches `workflow`.",
		"[Message from <name> on channel <channel id>]",
		"filled in automatically", "`member` is always your own name `lead`, never the recipient", "`channel` selects who receives the message",
		"Never send messages that only say you are waiting.", "You are woken automatically when a reply arrives.",
		"list_session_files", "read_session_file", "search_session_files", "write_session_file", "`session` = `teams/t1/build`",
		"finish_team with `files`", "`summary`",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("team note misses %q:\n%s", want, system)
		}
	}
	if strings.Contains(system, "coder--tester") {
		t.Fatalf("team note lists a channel of other members:\n%s", system)
	}
	if want := [2]string{"agent:intake@v1", "t1"}; f.putMeta[teamMemberID] != want {
		t.Fatalf("put metadata = %v", f.putMeta[teamMemberID])
	}
	if sent := jsonList(t, f.llmReqs[0]["messages"]); len(sent) != 4 {
		t.Fatalf("sent messages = %v", sent)
	}
	head := f.llmHeads[0]
	if head.Get("X-Trace-Id") != "t1" || head.Get("X-Step-Id") != "build" || head.Get("X-Agent-Ref") != intakeRef || head.Get("Traceparent") == "" {
		t.Fatalf("correlation headers = %v", head)
	}
}

func TestTeamTurnNoteForAMemberThatIsNotTheLead(t *testing.T) {
	f := teamFakes(t, teamToolsJSON, respond(finalResponse))
	factoryMCP(t, f, true)
	req := leadTurn()
	req.Member = coderMember
	res, err := f.svc.TeamTurn(context.Background(), req)
	if err != nil || res.ModelCalls != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	log, messages := storedMember(t, f, teamPath+"/members/coder.json")
	if len(messages) != 3 || messages[1]["content"] != "[Message from lead on channel coder--lead]\nwrite it" {
		t.Fatalf("messages = %v", messages)
	}
	if len(log.Read) != 1 || log.Read[coderLeadChannel] != 1 {
		t.Fatalf("read markers = %v", log.Read)
	}
	system := jsonString(t, messages[0]["content"])
	for _, want := range []string{"The lead of this team is `lead`", "channel `coder--tester` -> reaches `tester`", "Never send messages that only say you are waiting.", "You cannot finish the team's work", "If you end your turn without calling send_message, your final answer is sent as your reply"} {
		if !strings.Contains(system, want) {
			t.Fatalf("team note misses %q:\n%s", want, system)
		}
	}
	if strings.Contains(system, "finish_team") || strings.Contains(system, "lead--workflow") {
		t.Fatalf("a member that is not the lead was told about finish_team or the workflow channel:\n%s", system)
	}
}

func TestTeamTurnWithNothingUnreadMakesNoModelCall(t *testing.T) {
	f := teamFakes(t, chatAgentJSON, respond(finalResponse))
	if _, err := f.svc.TeamTurn(context.Background(), leadTurn()); err != nil {
		t.Fatal(err)
	}
	stored := string(f.blobs[teamMemberID])
	res, err := f.svc.TeamTurn(context.Background(), leadTurn())
	if err != nil || res.ModelCalls != 0 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(f.llmReqs) != 1 || string(f.blobs[teamMemberID]) != stored {
		t.Fatalf("a turn without unread messages called the model or rewrote the history")
	}

	// The tester has sent messages but received none: the queues of the other sides do not exist yet.
	delete(f.blobs, teamPath+"/channels/coder--tester/coder.json")
	req := leadTurn()
	req.Member = "tester"
	if res, err := f.svc.TeamTurn(context.Background(), req); err != nil || res.ModelCalls != 0 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if _, ok := f.blobs[teamPath+"/members/tester.json"]; ok || len(f.llmReqs) != 1 {
		t.Fatal("a first turn without unread messages stored a history or called the model")
	}
}

func TestTeamTurnLaterTurnDeliversOnlyNewMessages(t *testing.T) {
	f := teamFakes(t, chatAgentJSON, respond(finalResponse))
	f.blobs[teamMemberID] = []byte(`{"member":"lead","agent":"intake@v1","read":{"lead--workflow":1,"lead--tester":1},
		"messages":[{"role":"system","content":"stored system"},{"role":"user","content":"earlier"},{"role":"assistant","content":"ok","extra":{"kept":true}}]}`)
	f.prompt = "a changed prompt {{policy::security/review}}"
	res, err := f.svc.TeamTurn(context.Background(), leadTurn())
	if err != nil || res.ModelCalls != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(f.policyRequests) != 0 {
		t.Fatalf("a later turn compiled the prompt again: %v", f.policyRequests)
	}
	log, messages := storedMember(t, f, teamMemberID)
	if len(messages) != 5 || messages[0]["content"] != storedSystem || messages[2]["extra"] == nil ||
		messages[3]["content"] != "[Message from coder on channel coder--lead]\nwrote parser.py" {
		t.Fatalf("messages = %v", messages)
	}
	if log.Read[coderLeadChannel] != 1 || log.Read["lead--workflow"] != 1 || log.Read["lead--tester"] != 1 {
		t.Fatalf("read markers = %v", log.Read)
	}
}

func TestTeamTurnToolRoundCountsModelCalls(t *testing.T) {
	f := teamFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"],"max_turns":2}`,
		respond(toolCallResponse), respond(finalResponse))
	res, err := f.svc.TeamTurn(context.Background(), leadTurn())
	if err != nil || res.ModelCalls != 2 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if _, messages := storedMember(t, f, teamMemberID); len(messages) != 7 || messages[5]["role"] != roleTool {
		t.Fatalf("messages = %v", messages)
	}
	for i, c := range f.mcpCalls {
		if h := f.mcpHeads[i]; c["method"] == mcpToolsCall && (h.Get("X-Trace-Id") != "t1" || h.Get("X-Step-Id") != "build" || h.Get("X-Agent-Ref") != intakeRef) {
			t.Fatalf("mcp correlation headers = %v", h)
		}
	}
}

func TestTeamTurnBudgetEndsTheTurnAndKeepsTheMarkers(t *testing.T) {
	f := teamFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"],"max_turns":1}`,
		respond(toolCallResponse))
	res, err := f.svc.TeamTurn(context.Background(), leadTurn())
	if err != nil || res.ModelCalls != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	log, messages := storedMember(t, f, teamMemberID)
	if len(messages) != 6 || messages[5]["role"] != roleTool || log.Read[coderLeadChannel] != 1 {
		t.Fatalf("history after the budget = %v %v", log.Read, messages)
	}
	if res, err := f.svc.TeamTurn(context.Background(), leadTurn()); err != nil || res.ModelCalls != 0 || len(f.llmReqs) != 1 {
		t.Fatalf("messages were delivered twice: %+v, %v, %d model calls", res, err, len(f.llmReqs))
	}
}

func TestTeamTurnModelFailureFailsTheTurnAndKeepsTheMarkers(t *testing.T) {
	f := teamFakes(t, chatAgentJSON, func(w http.ResponseWriter, _ map[string]any) error {
		http.Error(w, `{"error":{"message":"model overloaded"}}`, http.StatusServiceUnavailable)
		return nil
	})
	res, err := f.svc.TeamTurn(context.Background(), leadTurn())
	if err == nil || !strings.Contains(err.Error(), "model overloaded") || res.ModelCalls != 0 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	log, messages := storedMember(t, f, teamMemberID)
	if len(messages) != 4 || log.Read["lead--workflow"] != 1 || log.Read[coderLeadChannel] != 1 {
		t.Fatalf("history after the failure = %v %v", log.Read, messages)
	}
}

func TestTeamTurnRefusesInvalidRequests(t *testing.T) {
	for name, change := range map[string]func(*TeamTurnRequest){
		"missing trace":       func(r *TeamTurnRequest) { r.TraceID = "" },
		"missing step":        func(r *TeamTurnRequest) { r.StepID = "" },
		"team outside teams/": func(r *TeamTurnRequest) { r.Team = "runs/t1/build" },
		"team escapes":        func(r *TeamTurnRequest) { r.Team = "teams/t1/../x" },
		"member with a path":  func(r *TeamTurnRequest) { r.Member = "a/b" },
		"member workflow":     func(r *TeamTurnRequest) { r.Member = "workflow" },
		"unknown member":      func(r *TeamTurnRequest) { r.Member = "ghost" },
		"unpinned agent":      func(r *TeamTurnRequest) { r.Agent = "intake" },
		"unknown team":        func(r *TeamTurnRequest) { r.Team = "teams/t1/other" },
	} {
		t.Run(name, func(t *testing.T) {
			f := teamFakes(t, chatAgentJSON, respond(finalResponse))
			req := leadTurn()
			change(req)
			if _, err := f.svc.TeamTurn(context.Background(), req); err == nil {
				t.Fatal("no error")
			}
			if len(f.llmReqs) != 0 || len(f.putMeta) != 0 {
				t.Fatal("an invalid request reached the model or the store")
			}
		})
	}
	f := teamFakes(t, chatAgentJSON, respond(finalResponse))
	stored := `{"member":"lead","agent":"other@v2","read":{},"messages":[{"role":"system","content":"s"}]}`
	f.blobs[teamMemberID] = []byte(stored)
	if _, err := f.svc.TeamTurn(context.Background(), leadTurn()); err == nil || !strings.Contains(err.Error(), "other@v2") {
		t.Fatalf("agent mismatch: %v", err)
	}
	if string(f.blobs[teamMemberID]) != stored || len(f.llmReqs) != 0 {
		t.Fatal("a mismatching request changed state")
	}
}

const (
	teamToolsJSON    = `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["factory/send_message"],"max_turns":2}`
	sendCallResponse = `{"choices":[{"message":{"role":"assistant","content":"I report now.","tool_calls":[{"id":"call_s","type":"function","function":{"name":"factory__send_message","arguments":"{\"team\":\"teams/t1/build\",\"member\":\"coder\",\"channel\":\"coder--lead\",\"text\":\"wrote parser.py\"}"}}]}}]}`
)

// factorySends records the arguments of the send_message calls that reached the gateway.
type factorySends struct {
	calls []map[string]any
	names []string
	fail  bool
}

// factoryMCP points the service at a gateway with the MCP service `factory`.
func factoryMCP(t *testing.T, f *fakes, offerSend bool) *factorySends {
	t.Helper()
	sends := &factorySends{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != mcpFactoryPath {
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
			w.Header().Set("Mcp-Session-Id", "session-factory")
			reply(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}`)
		case mcpToolsList:
			if offerSend {
				reply(`{"tools":[{"name":"send_message","description":"Send a message","inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}},{"name":"write_session_file","description":"Write"},{"name":"finish_team","description":"Finish"},{"name":"other","description":"Other"}]}`)
			} else {
				reply(`{"tools":[]}`)
			}
		case mcpToolsCall:
			f.mu.Lock()
			sends.calls = append(sends.calls, msg.Params.Arguments)
			sends.names = append(sends.names, msg.Params.Name)
			fail := sends.fail
			f.mu.Unlock()
			if fail {
				reply(`{"content":[{"type":"text","text":"channel refused"}],"isError":true}`)
			} else {
				reply(`{"content":[{"type":"text","text":"sent"}],"isError":false}`)
			}
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	f.svc.MCPGatewayURL = srv.URL
	return sends
}

func coderTurn() *TeamTurnRequest {
	req := leadTurn()
	req.Member = coderMember
	return req
}

func TestTeamTurnAutoReplySendsTheFinalAnswer(t *testing.T) {
	f := teamFakes(t, teamToolsJSON, respond(finalResponse))
	sends := factoryMCP(t, f, true)
	// The tester wrote to the coder after the lead did, so the reply goes to the tester.
	f.blobs[teamPath+"/channels/coder--tester/tester.json"] = []byte(`{"messages":[{"seq":1,"text":"which names?","sent":"2026-01-02T03:04:08.000000Z"}]}`)
	res, err := f.svc.TeamTurn(context.Background(), coderTurn())
	if err != nil || res.ModelCalls != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(sends.calls) != 1 {
		t.Fatalf("send_message calls = %v", sends.calls)
	}
	if c := sends.calls[0]; c["team"] != teamPath || c["member"] != coderMember || c["channel"] != "coder--tester" || c["text"] != planAnswer {
		t.Fatalf("send_message arguments = %v", c)
	}
	// The history keeps the assistant message as it is and gains nothing else.
	if _, messages := storedMember(t, f, teamPath+"/members/coder.json"); len(messages) != 4 || messages[3]["role"] != roleAssistant || messages[3]["content"] != planAnswer {
		t.Fatalf("messages = %v", messages)
	}
}

func TestTeamTurnAutoReplyIsSkippedAfterASentMessage(t *testing.T) {
	f := teamFakes(t, teamToolsJSON, respond(sendCallResponse), respond(finalResponse))
	sends := factoryMCP(t, f, true)
	res, err := f.svc.TeamTurn(context.Background(), coderTurn())
	if err != nil || res.ModelCalls != 2 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(sends.calls) != 1 || sends.calls[0]["text"] != "wrote parser.py" {
		t.Fatalf("send_message calls = %v", sends.calls)
	}
}

func TestTeamTurnAutoReplyAfterAFailedSendMessage(t *testing.T) {
	f := teamFakes(t, teamToolsJSON, respond(sendCallResponse), respond(finalResponse))
	sends := factoryMCP(t, f, true)
	sends.fail = true
	if _, err := f.svc.TeamTurn(context.Background(), coderTurn()); err == nil || !strings.Contains(err.Error(), "channel refused") {
		t.Fatalf("error = %v", err)
	}
	// The model's own call failed, so the harness tried to send the final answer.
	if len(sends.calls) != 2 || sends.calls[1]["text"] != planAnswer || sends.calls[1]["channel"] != coderLeadChannel {
		t.Fatalf("send_message calls = %v", sends.calls)
	}
	if log, _ := storedMember(t, f, teamPath+"/members/coder.json"); log.Read[coderLeadChannel] != 1 {
		t.Fatalf("read markers = %v", log.Read)
	}
}

func TestTeamTurnAutoReplyAfterBudgetExhaustion(t *testing.T) {
	f := teamFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["factory/send_message","factory/write_session_file"],"max_turns":1}`,
		respond(`{"choices":[{"message":{"role":"assistant","content":"I wrote parser.py.","tool_calls":[{"id":"c1","type":"function","function":{"name":"factory__write_session_file","arguments":"{}"}}]}}]}`))
	sends := factoryMCP(t, f, true)
	res, err := f.svc.TeamTurn(context.Background(), coderTurn())
	if err != nil || res.ModelCalls != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(sends.calls) != 2 || sends.names[1] != teamSendTool || sends.calls[1]["text"] != "I wrote parser.py." || sends.calls[1]["channel"] != coderLeadChannel {
		t.Fatalf("send_message calls = %v", sends.calls)
	}
}

func TestTeamTurnAutoReplyNeedsTheSendMessageTool(t *testing.T) {
	f := teamFakes(t, chatAgentJSON, respond(finalResponse))
	if _, err := f.svc.TeamTurn(context.Background(), coderTurn()); err == nil || !strings.Contains(err.Error(), "has no tool send_message") {
		t.Fatalf("agent without the tool: %v", err)
	}
	f = teamFakes(t, teamToolsJSON, respond(finalResponse))
	factoryMCP(t, f, false)
	if _, err := f.svc.TeamTurn(context.Background(), coderTurn()); err == nil || !strings.Contains(err.Error(), "factory/send_message is not available") {
		t.Fatalf("gateway without the tool: %v", err)
	}
}

func TestTeamTurnLeadGetsNoAutoReply(t *testing.T) {
	f := teamFakes(t, teamToolsJSON, respond(finalResponse))
	sends := factoryMCP(t, f, true)
	if _, err := f.svc.TeamTurn(context.Background(), leadTurn()); err != nil {
		t.Fatal(err)
	}
	if len(sends.calls) != 0 {
		t.Fatalf("the lead's final answer was sent: %v", sends.calls)
	}
}

// toolCalls is a model response that calls the given tools of the service `factory`.
func toolCalls(calls ...[2]string) llmHandler {
	list := make([]map[string]any, len(calls))
	for i, c := range calls {
		list[i] = map[string]any{"id": "c" + string(rune('a'+i)), "type": "function", "function": map[string]string{"name": "factory__" + c[0], "arguments": c[1]}}
	}
	data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": roleAssistant, "content": nil, "tool_calls": list}}}})
	return respond(string(data))
}

const teamAllToolsJSON = `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["factory/send_message","factory/write_session_file","factory/finish_team","factory/other"],"max_turns":6}`

// The model does not decide who it is: the harness sets team, member and session.
func TestTeamTurnOverwritesIdentityArguments(t *testing.T) {
	f := teamFakes(t, teamAllToolsJSON, toolCalls(
		[2]string{"send_message", `{"team":"teams/t1/other","member":"lead","channel":"coder--lead","text":"done"}`},
		[2]string{"write_session_file", `{"session":"user-sessions/x/y","name":"a.py","content":"x"}`},
		[2]string{"finish_team", ``},
		[2]string{"other", `{"member":"lead"}`},
	), respond(finalResponse))
	sends := factoryMCP(t, f, true)
	if _, err := f.svc.TeamTurn(context.Background(), coderTurn()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sends.names, ",") != "send_message,write_session_file,finish_team,other" {
		t.Fatalf("forwarded tools = %v", sends.names)
	}
	if c := sends.calls[0]; c["team"] != teamPath || c["member"] != coderMember || c["channel"] != coderLeadChannel || c["text"] != "done" {
		t.Fatalf("send_message arguments = %v", c)
	}
	if c := sends.calls[1]; c["session"] != teamPath || c["name"] != "a.py" || c["member"] != nil {
		t.Fatalf("write_session_file arguments = %v", c)
	}
	if c := sends.calls[2]; c["team"] != teamPath || c["member"] != coderMember {
		t.Fatalf("finish_team arguments = %v", c)
	}
	if c := sends.calls[3]; c["member"] != leadMember || c["team"] != nil {
		t.Fatalf("a tool that is not a team tool was changed: %v", c)
	}
	// The stored tool call is what the model wrote.
	_, messages := storedMember(t, f, teamPath+"/members/coder.json")
	stored, _ := json.Marshal(messages[2]["tool_calls"])
	if !strings.Contains(string(stored), `\"member\":\"lead\"`) || !strings.Contains(string(stored), "teams/t1/other") {
		t.Fatalf("stored tool call = %s", stored)
	}
}

func TestTeamTurnForwardsOneMessagePerChannelAndEndsAfterTwoRefusals(t *testing.T) {
	send := func(channel, text string) [2]string {
		return [2]string{"send_message", `{"channel":"` + channel + `","text":"` + text + `"}`}
	}
	f := teamFakes(t, teamAllToolsJSON,
		toolCalls(send("coder--lead", "write it"), send("lead--tester", "test it"), send("coder--lead", "waiting")),
		toolCalls(send("coder--lead", "still waiting"), send("lead--tester", "waiting too")),
		respond(finalResponse))
	sends := factoryMCP(t, f, true)
	res, err := f.svc.TeamTurn(context.Background(), leadTurn())
	if err != nil || res.ModelCalls != 2 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(sends.calls) != 2 || sends.calls[0]["text"] != "write it" || sends.calls[1]["text"] != "test it" {
		t.Fatalf("forwarded messages = %v", sends.calls)
	}
	log, messages := storedMember(t, f, teamMemberID)
	// system, 3 delivered, assistant, 3 tool answers, assistant, 2 tool answers
	if len(messages) != 11 || log.Read[coderLeadChannel] != 1 {
		t.Fatalf("history = %v %v", log.Read, messages)
	}
	want := "Error: you already sent a message on channel coder--lead in this turn. End your turn now by answering without a tool call; you are woken when a reply arrives."
	if messages[7]["content"] != want || messages[9]["content"] != want || messages[10]["role"] != roleTool || messages[5]["content"] != "sent" {
		t.Fatalf("tool answers = %v", messages[5:])
	}
}

func TestTeamTurnAFailedSendDoesNotUseUpTheChannel(t *testing.T) {
	f := teamFakes(t, teamAllToolsJSON,
		toolCalls([2]string{"send_message", `{"channel":"coder--lead","text":"first"}`}),
		toolCalls([2]string{"send_message", `{"channel":"coder--lead","text":"second"}`}),
		respond(finalResponse))
	sends := factoryMCP(t, f, true)
	sends.fail = true
	if _, err := f.svc.TeamTurn(context.Background(), leadTurn()); err != nil {
		t.Fatal(err)
	}
	if len(sends.calls) != 2 {
		t.Fatalf("forwarded messages = %v", sends.calls)
	}
}

// The read markers are stored before the model runs, so tools called during the turn
// see the delivered messages as read.
func TestTeamTurnStoresTheMarkersBeforeTheModelRuns(t *testing.T) {
	var during teamMemberLog
	var f *fakes
	f = teamFakes(t, chatAgentJSON, func(w http.ResponseWriter, body map[string]any) error {
		f.mu.Lock()
		err := json.Unmarshal(f.blobs[teamMemberID], &during)
		f.mu.Unlock()
		if err != nil {
			return err
		}
		return respond(finalResponse)(w, body)
	})
	if _, err := f.svc.TeamTurn(context.Background(), leadTurn()); err != nil {
		t.Fatal(err)
	}
	if during.Read["lead--workflow"] != 1 || during.Read[coderLeadChannel] != 1 || during.Read["lead--tester"] != 1 || len(during.Messages) != 4 {
		t.Fatalf("member file during the model call: read = %v, %d messages", during.Read, len(during.Messages))
	}
	if _, messages := storedMember(t, f, teamMemberID); len(messages) != 5 {
		t.Fatalf("messages after the turn = %v", messages)
	}
}

// A failed tool call's message reaches the model as "Error: <what the tool raised>", so the
// model can correct its call.
func TestTeamTurnAFailedToolCallsMessageReachesTheModel(t *testing.T) {
	f := teamFakes(t, teamAllToolsJSON,
		toolCalls([2]string{"finish_team", `{"files":["main.py"],"summary":"done"}`}),
		respond(finalResponse))
	sends := factoryMCP(t, f, true)
	sends.fail = true
	if _, err := f.svc.TeamTurn(context.Background(), leadTurn()); err != nil {
		t.Fatal(err)
	}
	sent := jsonList(t, f.llmReqs[1]["messages"])
	last := jsonObject(t, sent[len(sent)-1])
	if last["role"] != roleTool || last["content"] != "Error: channel refused" {
		t.Fatalf("tool message = %v", last)
	}
}
