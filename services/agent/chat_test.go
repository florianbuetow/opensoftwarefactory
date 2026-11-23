package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	chatAgentJSON = `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1","m2"],"effort":"high","tools":[],"max_retries":0}`
	chatLogID     = "user-sessions/t1/talk/chats/main.json"
)

func chatBody() map[string]string {
	return map[string]string{
		"trace_id": "t1", "step_id": "talk", "session": "user-sessions/t1/talk", "tab": "main",
		"agent": "intake@v1", "message": "hello there", "created_by": "flo",
	}
}

// postChat sends one request to the chat API and returns the status and the decoded body.
func postChat(t *testing.T, f *fakes, body any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	f.svc.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(string(data))))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response %d is not JSON: %s", rec.Code, rec.Body.String())
	}
	return rec.Code, out
}

func storedChat(t *testing.T, f *fakes) (chatLog, []map[string]any) {
	t.Helper()
	f.mu.Lock()
	data := f.blobs[chatLogID]
	f.mu.Unlock()
	var log chatLog
	if err := json.Unmarshal(data, &log); err != nil {
		t.Fatalf("stored chat log: %v: %s", err, data)
	}
	messages := make([]map[string]any, len(log.Messages))
	for i, m := range log.Messages {
		if err := json.Unmarshal(m, &messages[i]); err != nil {
			t.Fatal(err)
		}
	}
	return log, messages
}

func TestChatHealthz(t *testing.T) {
	f := newFakes(t, chatAgentJSON, respond(finalResponse))
	rec := httptest.NewRecorder()
	f.svc.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
}

func TestChatNewTabStoresLogAndReplies(t *testing.T) {
	f := newFakes(t, chatAgentJSON, respond(finalResponse))
	f.prompt = "You are intake. {{policy::security/review}}"
	status, out := postChat(t, f, chatBody())
	if status != http.StatusOK || out["reply"] != "the plan" || out["chat"] != chatLogID || out["messages"] != float64(3) {
		t.Fatalf("response = %d %v", status, out)
	}
	log, messages := storedChat(t, f)
	if log.Session != "user-sessions/t1/talk" || log.Tab != "main" || log.Agent != "intake@v1" || log.TraceID != "t1" || log.StepID != "talk" {
		t.Fatalf("log header = %+v", log)
	}
	if log.Created.IsZero() || log.Updated.Before(log.Created) {
		t.Fatalf("log times = %v %v", log.Created, log.Updated)
	}
	if len(messages) != 3 || messages[0]["role"] != "system" || messages[1]["role"] != "user" || messages[2]["role"] != "assistant" {
		t.Fatalf("messages = %v", messages)
	}
	system := messages[0]["content"].(string)
	if !strings.HasPrefix(system, "You are intake. Follow the security policy.") {
		t.Fatalf("system prompt was not compiled: %q", system)
	}
	for _, want := range []string{"user-sessions/t1/talk/files/", "list_session_files", "read_session_file", "search_session_files", "write_session_file", "`session` = `user-sessions/t1/talk`"} {
		if !strings.Contains(system, want) {
			t.Fatalf("session note misses %q: %q", want, system)
		}
	}
	if messages[1]["content"] != "hello there" || messages[2]["content"] != "the plan" {
		t.Fatalf("messages = %v", messages)
	}
	if want := [2]string{"agent:intake@v1", "t1"}; f.putMeta[chatLogID] != want {
		t.Fatalf("put metadata = %v", f.putMeta[chatLogID])
	}
	if len(f.llmReqs) != 1 || f.llmReqs[0]["model"] != "m1" || f.llmReqs[0]["reasoning_effort"] != "high" {
		t.Fatalf("llm requests = %v", f.llmReqs)
	}
	sent := f.llmReqs[0]["messages"].([]any)
	if len(sent) != 2 || sent[0].(map[string]any)["content"] != system {
		t.Fatalf("sent messages = %v", sent)
	}
	head := f.llmHeads[0]
	if head.Get("X-Trace-Id") != "t1" || head.Get("X-Step-Id") != "talk" || head.Get("X-Agent-Ref") != "intake@v1" || head.Get("Traceparent") == "" {
		t.Fatalf("correlation headers = %v", head)
	}
}

func TestChatContinuedTabReusesStoredMessages(t *testing.T) {
	f := newFakes(t, chatAgentJSON, respond(finalResponse))
	f.blobs[chatLogID] = []byte(`{"session":"user-sessions/t1/talk","tab":"main","agent":"intake@v1","trace_id":"t1","step_id":"talk",
		"created":"2026-01-02T03:04:05Z","updated":"2026-01-02T03:04:06Z",
		"messages":[{"role":"system","content":"stored system"},{"role":"user","content":"first"},{"role":"assistant","content":"answer one","extra":{"kept":true}}]}`)
	f.prompt = "a changed prompt {{policy::security/review}}"
	status, out := postChat(t, f, chatBody())
	if status != http.StatusOK || out["messages"] != float64(5) {
		t.Fatalf("response = %d %v", status, out)
	}
	if len(f.policyRequests) != 0 {
		t.Fatalf("an existing tab compiled the prompt again: %v", f.policyRequests)
	}
	sent := f.llmReqs[0]["messages"].([]any)
	if len(sent) != 4 || sent[0].(map[string]any)["content"] != "stored system" || sent[3].(map[string]any)["content"] != "hello there" {
		t.Fatalf("sent messages = %v", sent)
	}
	if kept := sent[2].(map[string]any)["extra"]; kept == nil {
		t.Fatalf("stored assistant message was not reused as is: %v", sent[2])
	}
	log, messages := storedChat(t, f)
	if log.Created.Format("2006-01-02T15:04:05Z") != "2026-01-02T03:04:05Z" || !log.Updated.After(log.Created) {
		t.Fatalf("log times = %v %v", log.Created, log.Updated)
	}
	if len(messages) != 5 || messages[0]["content"] != "stored system" || messages[4]["content"] != "the plan" {
		t.Fatalf("messages = %v", messages)
	}
}

func TestChatToolCallRound(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"],"max_retries":1}`,
		respond(toolCallResponse), respond(finalResponse))
	status, out := postChat(t, f, chatBody())
	if status != http.StatusOK || out["reply"] != "the plan" || out["messages"] != float64(5) {
		t.Fatalf("response = %d %v", status, out)
	}
	_, messages := storedChat(t, f)
	if len(messages) != 5 || messages[2]["tool_calls"] == nil || messages[3]["role"] != "tool" || messages[3]["tool_call_id"] != "call_1" || messages[3]["content"] != "found 3 hits" {
		t.Fatalf("messages = %v", messages)
	}
	if tools := f.llmReqs[0]["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	if sent := f.llmReqs[1]["messages"].([]any); len(sent) != 4 {
		t.Fatalf("second call messages = %v", sent)
	}
	var called bool
	for i, c := range f.mcpCalls {
		if c["method"] == "tools/call" {
			called = true
			if h := f.mcpHeads[i]; h.Get("X-Trace-Id") != "t1" || h.Get("X-Step-Id") != "talk" || h.Get("X-Agent-Ref") != "intake@v1" {
				t.Fatalf("mcp correlation headers = %v", h)
			}
		}
	}
	if !called {
		t.Fatalf("no tools/call: %v", f.mcpCalls)
	}
}

func TestChatBudgetExhaustedKeepsMessages(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"],"max_retries":0}`,
		respond(toolCallResponse))
	status, out := postChat(t, f, chatBody())
	if status != http.StatusBadGateway || !strings.Contains(out["error"].(string), "no final answer within 1 model calls") {
		t.Fatalf("response = %d %v", status, out)
	}
	if _, messages := storedChat(t, f); len(messages) != 4 || messages[3]["role"] != "tool" {
		t.Fatalf("messages = %v", messages)
	}
}

func TestChatAgentMismatchConflicts(t *testing.T) {
	f := newFakes(t, chatAgentJSON, respond(finalResponse))
	stored := `{"session":"user-sessions/t1/talk","tab":"main","agent":"other@v2","trace_id":"t1","step_id":"talk","messages":[{"role":"system","content":"s"}]}`
	f.blobs[chatLogID] = []byte(stored)
	status, out := postChat(t, f, chatBody())
	if status != http.StatusConflict || !strings.Contains(out["error"].(string), "other@v2") {
		t.Fatalf("response = %d %v", status, out)
	}
	if string(f.blobs[chatLogID]) != stored || len(f.llmReqs) != 0 {
		t.Fatalf("a conflicting request changed state: %s", f.blobs[chatLogID])
	}
}

func TestChatUnknownAgentIsNotFound(t *testing.T) {
	f := newFakes(t, chatAgentJSON, respond(finalResponse))
	body := chatBody()
	body["agent"] = "missing@v1"
	status, out := postChat(t, f, body)
	if status != http.StatusNotFound || out["error"] == "" {
		t.Fatalf("response = %d %v", status, out)
	}
	if _, ok := f.blobs[chatLogID]; ok {
		t.Fatal("a chat log was stored for an unknown agent")
	}
}

func TestChatValidation(t *testing.T) {
	for name, change := range map[string]func(map[string]string){
		"missing trace":          func(b map[string]string) { b["trace_id"] = "" },
		"missing step":           func(b map[string]string) { b["step_id"] = "" },
		"session of another run": func(b map[string]string) { b["session"] = "user-sessions/t2/talk" },
		"session outside":        func(b map[string]string) { b["session"] = "runs/t1/talk" },
		"session is the prefix":  func(b map[string]string) { b["session"] = "user-sessions/t1/" },
		"session escapes":        func(b map[string]string) { b["session"] = "user-sessions/t1/../x" },
		"bad tab":                func(b map[string]string) { b["tab"] = "Main_1" },
		"long tab":               func(b map[string]string) { b["tab"] = strings.Repeat("a", 41) },
		"unpinned agent":         func(b map[string]string) { b["agent"] = "intake" },
		"empty message":          func(b map[string]string) { b["message"] = "  " },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakes(t, chatAgentJSON, respond(finalResponse))
			body := chatBody()
			change(body)
			status, out := postChat(t, f, body)
			if status != http.StatusBadRequest || out["error"] == "" {
				t.Fatalf("response = %d %v", status, out)
			}
			if len(f.llmReqs) != 0 || len(f.putMeta) != 0 {
				t.Fatal("an invalid request reached the model or the store")
			}
		})
	}
	f := newFakes(t, chatAgentJSON, respond(finalResponse))
	if status, _ := postChat(t, f, "not an object"); status != http.StatusBadRequest {
		t.Fatalf("non-object body = %d", status)
	}
}

func TestChatModelFailurePersistsUserMessage(t *testing.T) {
	f := newFakes(t, chatAgentJSON, func(w http.ResponseWriter, _ map[string]any) {
		http.Error(w, `{"error":{"message":"model overloaded"}}`, http.StatusServiceUnavailable)
	}, respond(finalResponse))
	status, out := postChat(t, f, chatBody())
	if status != http.StatusBadGateway || !strings.Contains(out["error"].(string), "model overloaded") {
		t.Fatalf("response = %d %v", status, out)
	}
	_, messages := storedChat(t, f)
	if len(messages) != 2 || messages[0]["role"] != "system" || messages[1]["content"] != "hello there" {
		t.Fatalf("messages after failure = %v", messages)
	}
	// The next message continues the same log.
	body := chatBody()
	body["message"] = "again"
	if status, out = postChat(t, f, body); status != http.StatusOK || out["messages"] != float64(4) {
		t.Fatalf("retry response = %d %v", status, out)
	}
}

func TestChatStoreFailureDoesNotStartNewLog(t *testing.T) {
	f := newFakes(t, chatAgentJSON, respond(finalResponse))
	f.svc.Store = HTTPStore{BaseURL: "http://127.0.0.1:1/blobs", Client: http.DefaultClient}
	if status, _ := postChat(t, f, chatBody()); status != http.StatusBadGateway {
		t.Fatalf("response = %d", status)
	}
	if len(f.llmReqs) != 0 {
		t.Fatal("the model was called although the chat log could not be read")
	}
}

// Coverage: a tool call whose transport fails still gets a tool answer, so the next message
// can send the stored history again.
func TestChatFailedToolCallLeavesAResendableLog(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"],"max_retries":1}`,
		respond(toolCallResponse), respond(finalResponse))
	f.mcpFailMethod = "tools/call"
	if status, out := postChat(t, f, chatBody()); status != http.StatusBadGateway {
		t.Fatalf("response = %d %v", status, out)
	}
	_, messages := storedChat(t, f)
	if len(messages) != 4 || messages[2]["tool_calls"] == nil || messages[3]["role"] != "tool" || messages[3]["tool_call_id"] != "call_1" {
		t.Fatalf("messages after the failed tool call = %v", messages)
	}
	f.mcpFailMethod = ""
	body := chatBody()
	body["message"] = "again"
	if status, out := postChat(t, f, body); status != http.StatusOK || out["messages"] != float64(6) {
		t.Fatalf("retry response = %d %v", status, out)
	}
	sent := f.llmReqs[1]["messages"].([]any)
	if len(sent) != 5 || sent[3].(map[string]any)["role"] != "tool" || sent[4].(map[string]any)["content"] != "again" {
		t.Fatalf("messages sent after the failure = %v", sent)
	}
}

// Coverage: messages sent to one tab at the same time are all kept, one turn after the other.
func TestChatConcurrentMessagesToOneTabAreAllKept(t *testing.T) {
	f := newFakes(t, chatAgentJSON, respond(finalResponse))
	const n = 8
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			body := chatBody()
			body["message"] = "message " + strings.Repeat("x", i+1)
			data, _ := json.Marshal(body)
			rec := httptest.NewRecorder()
			f.svc.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(string(data))))
			codes <- rec.Code
		}()
	}
	for i := 0; i < n; i++ {
		if code := <-codes; code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
	}
	_, messages := storedChat(t, f)
	if len(messages) != 1+2*n {
		t.Fatalf("stored %d messages, want %d", len(messages), 1+2*n)
	}
	users := map[any]bool{}
	for i, m := range messages[1:] {
		want := "user"
		if i%2 == 1 {
			want = "assistant"
		}
		if m["role"] != want {
			t.Fatalf("message %d has role %v, want %s", i+1, m["role"], want)
		}
		if want == "user" {
			users[m["content"]] = true
		}
	}
	if len(users) != n {
		t.Fatalf("kept %d distinct user messages, want %d", len(users), n)
	}
	chatLocks.mu.Lock()
	left := len(chatLocks.items)
	chatLocks.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d chat locks left behind", left)
	}
}

// Coverage: when the person's request ends in the middle of a turn, the message is still stored.
func TestChatClientDisconnectKeepsTheMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFakes(t, chatAgentJSON, func(w http.ResponseWriter, _ map[string]any) {
		cancel()
		time.Sleep(50 * time.Millisecond)
	})
	data, _ := json.Marshal(chatBody())
	rec := httptest.NewRecorder()
	f.svc.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(string(data))).WithContext(ctx))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	_, messages := storedChat(t, f)
	if len(messages) != 2 || messages[1]["content"] != "hello there" {
		t.Fatalf("messages after the disconnect = %v", messages)
	}
}

// Coverage: a chat log that cannot be parsed is reported and left untouched.
func TestChatCorruptLogIsNotOverwritten(t *testing.T) {
	f := newFakes(t, chatAgentJSON, respond(finalResponse))
	f.blobs[chatLogID] = []byte(`{"agent":"intake@v1","messages":[{"role":"system"`)
	if status, _ := postChat(t, f, chatBody()); status != http.StatusBadGateway {
		t.Fatalf("status = %d", status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if string(f.blobs[chatLogID]) != `{"agent":"intake@v1","messages":[{"role":"system"` || len(f.llmReqs) != 0 {
		t.Fatalf("corrupt log was replaced or the model was called: %s", f.blobs[chatLogID])
	}
}
