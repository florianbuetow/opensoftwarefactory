package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func valueOf(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}

func TestUsageKeepsProviderDetailAndMissingCounts(t *testing.T) {
	var got usage
	raw := `{"prompt_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"provider_cost":0.012}`
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.PromptTokens == nil || *got.PromptTokens != 0 || got.CachedInputTokens == nil || *got.CachedInputTokens != 0 || got.CompletionTokens != nil {
		t.Fatalf("usage %+v", got)
	}
	stored, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored), `"provider_cost":0.012`) || strings.Contains(string(stored), `"completion_tokens":0`) {
		t.Fatalf("stored usage %s", stored)
	}
}

func TestAggregatedUsageStaysUnknownWhenAnyCallOmitsCount(t *testing.T) {
	one := int64(1)
	var total *usage
	addUsage(&total, &usage{PromptTokens: &one, CompletionTokens: &one, TotalTokens: &one})
	addUsage(&total, &usage{CompletionTokens: &one, TotalTokens: &one})
	if total.PromptTokens != nil {
		t.Fatalf("apparently complete prompt total %v", *total.PromptTokens)
	}
	if valueOf(total.CompletionTokens) != 2 || valueOf(total.TotalTokens) != 2 {
		t.Fatalf("known totals %+v", total)
	}
}

const settingsAgentJSON = `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"effort":"low","temperature":0.2,"tools":["docs/search"],"max_retries":1}`

// checkSettingsSent fails unless every model request carries the temperature.
func checkSettingsSent(t *testing.T, f *fakes, want int) {
	t.Helper()
	if len(f.llmReqs) != want {
		t.Fatalf("model requests = %d, want %d", len(f.llmReqs), want)
	}
	for i, req := range f.llmReqs {
		if req["temperature"] != 0.2 {
			t.Fatalf("request %d: temperature = %v", i, req["temperature"])
		}
	}
}

// checkNoSettingsSent fails when a model request names a temperature.
func checkNoSettingsSent(t *testing.T, f *fakes) {
	t.Helper()
	for i, req := range f.llmReqs {
		if _, ok := req["temperature"]; ok {
			t.Fatalf("request %d sent a temperature: %v", i, req)
		}
	}
}

// storedObject decodes a stored JSON object into a generic map, to check which fields exist.
func storedObject(t *testing.T, f *fakes, id string) map[string]any {
	t.Helper()
	f.mu.Lock()
	data := f.blobs[id]
	f.mu.Unlock()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("stored object %q: %v: %s", id, err, data)
	}
	return out
}

// checkToolRoundEvents checks the events of one tool round: a model call with a tool call,
// the tool call, and the final model call.
func checkToolRoundEvents(t *testing.T, events []TurnEvent) {
	t.Helper()
	if len(events) != 3 {
		t.Fatalf("events = %+v", events)
	}
	first, tool, last := events[0], events[1], events[2]
	if first.Kind != kindModelCall || first.Name != "chat completion" || first.Model != "m1" || first.Turn != 1 || first.Status != statusCompleted || first.Error != "" {
		t.Fatalf("first model call = %+v", first)
	}
	if first.Usage == nil || valueOf(first.Usage.PromptTokens) != 10 || valueOf(first.Usage.CompletionTokens) != 5 || valueOf(first.Usage.TotalTokens) != 15 {
		t.Fatalf("first model call usage = %+v", first.Usage)
	}
	if tool.Kind != kindToolCall || tool.Name != "docs__search" || tool.Turn != 1 || tool.Status != statusCompleted || tool.Model != "" || tool.Usage != nil {
		t.Fatalf("tool call = %+v", tool)
	}
	if want := estimateTokens("found 3 hits"); tool.EstimatedOutputTokens == nil || *tool.EstimatedOutputTokens != want {
		t.Fatalf("tool output estimate = %v, want %d", tool.EstimatedOutputTokens, want)
	}
	if want := estimateJSONTokens(map[string]string{"name": "docs__search", "arguments": `{"q":"auth"}`}); *tool.EstimatedInputTokens != want {
		t.Fatalf("tool input estimate = %d, want %d", *tool.EstimatedInputTokens, want)
	}
	if last.Kind != kindModelCall || last.Turn != 2 || last.Status != statusCompleted || last.Usage == nil || valueOf(last.Usage.TotalTokens) != 27 {
		t.Fatalf("last model call = %+v", last)
	}
	if want := estimateTokens(`{"role":"assistant","content":"the plan"}`); *last.EstimatedOutputTokens != want {
		t.Fatalf("model output estimate = %d, want %d", *last.EstimatedOutputTokens, want)
	}
	for i := range events {
		e := &events[i]
		if e.Started.IsZero() || e.Finished.Before(e.Started) {
			t.Fatalf("event %d times = %v %v", i, e.Started, e.Finished)
		}
		if e.EstimatedInputTokens == nil || *e.EstimatedInputTokens <= 0 || e.EstimatedOutputTokens == nil || e.EstimationMethod != estimationMethod {
			t.Fatalf("event %d estimates = %+v", i, *e)
		}
	}
	// The second request holds the first one plus two messages, so it is larger.
	if *last.EstimatedInputTokens <= *first.EstimatedInputTokens {
		t.Fatalf("input estimates did not grow: %d then %d", *first.EstimatedInputTokens, *last.EstimatedInputTokens)
	}
}

// checkEventFields fails when a stored event has a field outside the contract.
func checkEventFields(t *testing.T, stored map[string]any) {
	t.Helper()
	allowed := map[string]bool{
		"kind": true, "name": true, "status": true, "started": true, "finished": true, "model": true, "turn": true,
		"usage": true, "estimated_input_tokens": true, "estimated_output_tokens": true, "estimation_method": true, "error": true,
	}
	events, _ := stored["events"].([]any)
	if len(events) == 0 {
		t.Fatalf("no events stored: %v", stored)
	}
	for _, e := range events {
		for field := range jsonObject(t, e) {
			if !allowed[field] {
				t.Fatalf("event has the field %q: %v", field, e)
			}
		}
	}
}

func TestProcessSendsAndRecordsModelSettings(t *testing.T) {
	f := newFakes(t, settingsAgentJSON, respond(toolCallResponse), respond(finalResponse))
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	checkSettingsSent(t, f, 2)
	rec := storedObject(t, f, "runs/t1/plan.run.json")
	if rec["temperature"] != 0.2 || rec["estimation_method"] != estimationMethod {
		t.Fatalf("record settings = %v %v", rec["temperature"], rec["estimation_method"])
	}
	var typed RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &typed); err != nil {
		t.Fatal(err)
	}
	// Events: tool discovery, model call, tool call, model call.
	if len(typed.Events) != 4 || typed.Events[1].Usage == nil || valueOf(typed.Events[1].Usage.TotalTokens) != 15 || typed.Events[3].Usage == nil || valueOf(typed.Events[3].Usage.TotalTokens) != 27 {
		t.Fatalf("model call events miss the reported usage: %+v", typed.Events)
	}
	if typed.Events[0].Usage != nil || typed.Events[2].Usage != nil {
		t.Fatalf("an event that is not a model call has usage: %+v", typed.Events)
	}
	if want := estimateTokens(`{"role":"assistant","content":"the plan"}`); *typed.Events[3].EstimatedOutputTokens != want {
		t.Fatalf("model output estimate = %d, want %d", *typed.Events[3].EstimatedOutputTokens, want)
	}
	for _, i := range []int{1, 2, 3} {
		if typed.Events[i].EstimationMethod != estimationMethod {
			t.Fatalf("event %d estimation method = %q", i, typed.Events[i].EstimationMethod)
		}
	}
}

func TestProcessOmitsUnsetModelSettings(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, respond(finalResponse))
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	checkNoSettingsSent(t, f)
	rec := storedObject(t, f, "runs/t1/plan.run.json")
	if _, ok := rec["temperature"]; ok {
		t.Fatalf("record has a temperature: %v", rec["temperature"])
	}
}

func TestProcessSendsATemperatureOfZero(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"temperature":0}`, respond(finalResponse))
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if req := f.llmReqs[0]; req["temperature"] != float64(0) {
		t.Fatalf("a temperature of zero was not sent: %v", req)
	}
	if rec := storedObject(t, f, "runs/t1/plan.run.json"); rec["temperature"] != float64(0) {
		t.Fatalf("a temperature of zero was not recorded: %v", rec["temperature"])
	}
}

func TestChatRecordsModelSettingsAndEvents(t *testing.T) {
	f := newFakes(t, settingsAgentJSON, respond(toolCallResponse), respond(finalResponse))
	if status, out := postChat(t, f, chatBody()); status != http.StatusOK {
		t.Fatalf("response = %d %v", status, out)
	}
	checkSettingsSent(t, f, 2)
	log, messages := storedChat(t, f)
	if log.Model != "m1" || log.Temperature == nil || *log.Temperature != 0.2 {
		t.Fatalf("log settings = %q %v", log.Model, log.Temperature)
	}
	if len(messages) != 5 {
		t.Fatalf("messages = %v", messages)
	}
	checkToolRoundEvents(t, log.Events)
	stored := storedObject(t, f, chatLogID)
	checkEventFields(t, stored)
	if stored["temperature"] != 0.2 || stored["model"] != "m1" {
		t.Fatalf("stored settings = %v", stored)
	}

	// A second message adds its events to the stored ones.
	if status, out := postChat(t, f, chatBody()); status != http.StatusOK {
		t.Fatalf("response = %d %v", status, out)
	}
	log, _ = storedChat(t, f)
	if len(log.Events) != 4 || log.Events[3].Kind != kindModelCall || log.Events[3].Turn != 1 {
		t.Fatalf("events after the second message = %+v", log.Events)
	}
	if !log.Events[3].Started.After(log.Events[0].Started) {
		t.Fatalf("events are not in the order of the calls: %+v", log.Events)
	}
}

func TestChatLogWithoutEventsStillLoads(t *testing.T) {
	f := newFakes(t, chatAgentJSON, respond(`{"choices":[{"message":{"role":"assistant","content":"the plan"}}]}`))
	f.blobs[chatLogID] = []byte(`{"session":"user-sessions/t1/talk","tab":"main","agent":"intake@v1","trace_id":"t1","step_id":"talk",
		"created":"2026-01-02T03:04:05Z","updated":"2026-01-02T03:04:06Z",
		"messages":[{"role":"system","content":"stored system"},{"role":"user","content":"first"},{"role":"assistant","content":"answer one"}]}`)
	if status, out := postChat(t, f, chatBody()); status != http.StatusOK || out["messages"] != float64(5) {
		t.Fatalf("response = %d %v", status, out)
	}
	checkNoSettingsSent(t, f)
	log, messages := storedChat(t, f)
	if len(messages) != 5 || messages[0]["content"] != storedSystem || log.Model != "m1" || log.Temperature != nil {
		t.Fatalf("log = %+v %v", log, messages)
	}
	// The model reported no usage, so the event has only the estimates.
	if len(log.Events) != 1 || log.Events[0].Usage != nil || log.Events[0].EstimatedInputTokens == nil || log.Events[0].EstimatedOutputTokens == nil {
		t.Fatalf("events = %+v", log.Events)
	}
	stored := storedObject(t, f, chatLogID)
	if _, ok := stored["temperature"]; ok {
		t.Fatalf("log of an agent without settings has a temperature: %v", stored["temperature"])
	}
	if _, ok := jsonObject(t, jsonList(t, stored["events"])[0])["usage"]; ok {
		t.Fatalf("event has usage the model did not report: %v", stored["events"])
	}
}

func TestChatFailedModelCallIsRecorded(t *testing.T) {
	f := newFakes(t, chatAgentJSON, func(w http.ResponseWriter, _ map[string]any) error {
		http.Error(w, `{"error":{"message":"model overloaded"}}`, http.StatusServiceUnavailable)
		return nil
	})
	if status, _ := postChat(t, f, chatBody()); status != http.StatusBadGateway {
		t.Fatalf("status = %d", status)
	}
	log, _ := storedChat(t, f)
	if len(log.Events) != 1 {
		t.Fatalf("events = %+v", log.Events)
	}
	e := log.Events[0]
	if e.Kind != kindModelCall || e.Status != statusFailed || !strings.Contains(e.Error, "model overloaded") || e.EstimatedInputTokens == nil || e.EstimatedOutputTokens != nil || e.Finished.Before(e.Started) {
		t.Fatalf("failed model call = %+v", e)
	}
}

func TestChatFailedToolCallsAreRecorded(t *testing.T) {
	// The tool reports an error: the model gets it and the event is failed.
	f := newFakes(t, settingsAgentJSON, respond(toolCallResponse), respond(finalResponse))
	f.mcpToolText, f.mcpToolError = "index offline", true
	if status, out := postChat(t, f, chatBody()); status != http.StatusOK {
		t.Fatalf("response = %d %v", status, out)
	}
	log, _ := storedChat(t, f)
	if len(log.Events) != 3 || log.Events[1].Status != statusFailed || log.Events[1].Error != "Error: index offline" || log.Events[1].EstimatedOutputTokens == nil {
		t.Fatalf("events = %+v", log.Events)
	}

	// The gateway fails: the turn fails and the event says why.
	f = newFakes(t, settingsAgentJSON, respond(toolCallResponse), respond(finalResponse))
	f.mcpFailMethod = mcpToolsCall
	if status, _ := postChat(t, f, chatBody()); status != http.StatusBadGateway {
		t.Fatalf("status = %d", status)
	}
	log, _ = storedChat(t, f)
	if len(log.Events) != 2 || log.Events[1].Kind != kindToolCall || log.Events[1].Status != statusFailed || log.Events[1].Error == "" || log.Events[1].EstimatedOutputTokens != nil {
		t.Fatalf("events = %+v", log.Events)
	}
}

func TestTeamTurnRecordsModelSettingsAndEvents(t *testing.T) {
	f := teamFakes(t, settingsAgentJSON, respond(toolCallResponse), respond(finalResponse))
	if res, err := f.svc.TeamTurn(context.Background(), leadTurn()); err != nil || res.ModelCalls != 2 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	checkSettingsSent(t, f, 2)
	log, messages := storedMember(t, f, teamMemberID)
	if log.Model != "m1" || log.Temperature == nil || *log.Temperature != 0.2 {
		t.Fatalf("history settings = %q %v", log.Model, log.Temperature)
	}
	if len(messages) != 7 || len(log.Read) != 3 {
		t.Fatalf("history = %v %v", log.Read, messages)
	}
	checkToolRoundEvents(t, log.Events)
	checkEventFields(t, storedObject(t, f, teamMemberID))

	// A later turn adds its events to the stored ones.
	f.blobs[teamPath+"/channels/coder--lead/coder.json"] = []byte(`{"messages":[
		{"seq":1,"text":"wrote parser.py","sent":"2026-01-02T03:04:09.000000Z"},
		{"seq":2,"text":"fixed it","sent":"2026-01-02T03:05:00.000000Z"}]}`)
	if res, err := f.svc.TeamTurn(context.Background(), leadTurn()); err != nil || res.ModelCalls != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	log, _ = storedMember(t, f, teamMemberID)
	if len(log.Events) != 4 || log.Events[3].Kind != kindModelCall || log.Events[3].Turn != 1 {
		t.Fatalf("events after the second turn = %+v", log.Events)
	}
}

func TestTeamMemberHistoryWithoutEventsStillLoads(t *testing.T) {
	f := teamFakes(t, chatAgentJSON, respond(finalResponse))
	f.blobs[teamMemberID] = []byte(`{"member":"lead","agent":"intake@v1","read":{"lead--workflow":1,"lead--tester":1},
		"messages":[{"role":"system","content":"stored system"},{"role":"user","content":"earlier"},{"role":"assistant","content":"ok"}]}`)
	if res, err := f.svc.TeamTurn(context.Background(), leadTurn()); err != nil || res.ModelCalls != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	checkNoSettingsSent(t, f)
	log, messages := storedMember(t, f, teamMemberID)
	if len(messages) != 5 || messages[0]["content"] != storedSystem || log.Model != "m1" || log.Temperature != nil {
		t.Fatalf("history = %+v %v", log, messages)
	}
	if len(log.Events) != 1 || log.Events[0].Kind != kindModelCall || log.Events[0].Status != statusCompleted {
		t.Fatalf("events = %+v", log.Events)
	}
}

func TestTeamTurnFailedModelCallIsRecorded(t *testing.T) {
	f := teamFakes(t, chatAgentJSON, func(w http.ResponseWriter, _ map[string]any) error {
		http.Error(w, `{"error":{"message":"model overloaded"}}`, http.StatusServiceUnavailable)
		return nil
	})
	if _, err := f.svc.TeamTurn(context.Background(), leadTurn()); err == nil {
		t.Fatal("the turn did not fail")
	}
	log, _ := storedMember(t, f, teamMemberID)
	if len(log.Events) != 1 || log.Events[0].Status != statusFailed || !strings.Contains(log.Events[0].Error, "model overloaded") {
		t.Fatalf("events = %+v", log.Events)
	}
}

func TestEffortNoneEndsTheRequestWithAnEmptyThinkingBlock(t *testing.T) {
	messages := []any{map[string]string{"role": roleUser, "content": "hi"}}
	none := (&Definition{Models: []string{"m1"}, Effort: "none"}).chatRequest(messages, nil)
	last, ok := none.Messages[len(none.Messages)-1].(map[string]string)
	if len(none.Messages) != 2 || !ok || last["role"] != roleAssistant || last["content"] != noThinking || none.ReasoningEffort != "" {
		t.Fatalf("effort none: %+v", none)
	}
	if len(messages) != 1 {
		t.Fatal("the conversation itself must not grow")
	}
	low := (&Definition{Models: []string{"m1"}, Effort: "low"}).chatRequest(messages, nil)
	if len(low.Messages) != 1 || low.ReasoningEffort != "low" {
		t.Fatalf("effort low: %+v", low)
	}
}
