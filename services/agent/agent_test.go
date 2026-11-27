package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// fakes bundles httptest servers for the registry, blobstore, LLM gateway and MCP gateway.
type fakes struct {
	t                *testing.T
	svc              *Service
	agent            string // agent.json
	llm              []llmHandler
	blobs            map[string][]byte
	putMeta          map[string][2]string
	putMetricHeaders map[string]http.Header
	llmReqs          []map[string]any
	llmHeads         []http.Header
	mcpCalls         []map[string]any
	mcpHeads         []http.Header
	mcpFailMethod    string
	mcpToolText      string
	prompt           string
	policyContent    string
	policyVersion    string
	policyRequests   []string
	policyStatus     int
	mu               sync.Mutex
	mcpToolError     bool
}

// llmHandler answers one request to the fake LLM gateway. The fake reports the error it
// returns to the test.
type llmHandler func(w http.ResponseWriter, body map[string]any) error

// Values the tests use again and again.
const (
	intakeRef      = "intake@v1"
	intakePrompt   = "You are intake."
	planArtifact   = "runs/t1/plan"
	planAnswer     = "the plan"
	firstCallID    = "call_1"
	effortHigh     = "high"
	mcpDocsPath    = "/mcp/docs"
	llmGatewayName = "llm_gateway"
)

// jsonObject returns a decoded JSON value as an object and fails the test when it is none.
func jsonObject(t *testing.T, v any) map[string]any {
	t.Helper()
	object, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v (%T) is not a JSON object", v, v)
	}
	return object
}

// jsonList returns a decoded JSON value as an array and fails the test when it is none.
func jsonList(t *testing.T, v any) []any {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%v (%T) is not a JSON array", v, v)
	}
	return list
}

// jsonString returns a decoded JSON value as a string and fails the test when it is none.
func jsonString(t *testing.T, v any) string {
	t.Helper()
	text, ok := v.(string)
	if !ok {
		t.Fatalf("%v (%T) is not a JSON string", v, v)
	}
	return text
}

// writeBody writes a response body and reports a failed write to the test.
func writeBody(t *testing.T, w io.Writer, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Error(err)
	}
}

func newFakes(t *testing.T, agentJSON string, llm ...llmHandler) *fakes {
	f := &fakes{t: t, agent: agentJSON, llm: llm, prompt: intakePrompt, policyContent: "Follow the security policy.", policyVersion: "v1", blobs: map[string][]byte{
		"runs/t1/input":  []byte("please build a thing"),
		"runs/t1/intake": []byte("intent record"),
	}, putMeta: map[string][2]string{}, putMetricHeaders: map[string]http.Header{}, mcpToolText: "found 3 hits"}

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/intake/v1" && r.URL.Path != "/agents/intake/triage/v1" {
			http.NotFound(w, r)
			return
		}
		data, _ := json.Marshal(map[string]any{"name": "intake", "version": "v1", "files": map[string]any{"systemprompt.md": f.prompt, "agent.json": json.RawMessage(f.agent)}})
		_, _ = w.Write(data)
	}))
	policySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.policyRequests = append(f.policyRequests, r.URL.Path)
		content, version, status := f.policyContent, f.policyVersion, f.policyStatus
		f.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			http.Error(w, "policy service unavailable", status)
			return
		}
		if r.URL.Path != "/policies/security/review/latest" || content == "" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": version, "files": map[string]string{"policy.md": content}})
	}))
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/list/") {
			prefix := strings.TrimPrefix(r.URL.Path, "/list/") + "/"
			objects := []map[string]string{}
			dirset := map[string]bool{}
			for id := range f.blobs {
				if strings.HasPrefix(id, prefix) {
					rest := strings.TrimPrefix(id, prefix)
					if strings.Contains(rest, "/") {
						dirset[strings.SplitN(rest, "/", 2)[0]] = true
					} else {
						objects = append(objects, map[string]string{"id": id})
					}
				}
			}
			dirs := []string{}
			for dir := range dirset {
				dirs = append(dirs, dir)
			}
			if len(objects) == 0 && len(dirs) == 0 {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"objects": objects, "directories": dirs})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		switch r.Method {
		case http.MethodGet:
			data, ok := f.blobs[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if _, err := w.Write(data); err != nil {
				t.Error(err)
			}
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			f.blobs[id] = data
			f.putMeta[id] = [2]string{r.Header.Get("X-Creator"), r.Header.Get("X-Trace-Id")}
			f.putMetricHeaders[id] = r.Header.Clone()
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			delete(f.blobs, id)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		f.mu.Lock()
		n := len(f.llmReqs)
		f.llmReqs = append(f.llmReqs, body)
		f.llmHeads = append(f.llmHeads, r.Header.Clone())
		f.mu.Unlock()
		if n >= len(f.llm) {
			n = len(f.llm) - 1
		}
		if err := f.llm[n](w, body); err != nil {
			t.Error(err)
		}
	}))
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != mcpDocsPath && r.URL.Path != "/mcp/code" {
			http.NotFound(w, r)
			return
		}
		session := "session-" + strings.TrimPrefix(r.URL.Path, "/mcp/")
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != session {
				t.Errorf("DELETE session = %q", r.Header.Get("Mcp-Session-Id"))
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var msg map[string]any
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		msg["_path"] = r.URL.Path
		f.mcpCalls = append(f.mcpCalls, msg)
		f.mcpHeads = append(f.mcpHeads, r.Header.Clone())
		f.mu.Unlock()
		method, _ := msg["method"].(string)
		if method == f.mcpFailMethod {
			http.Error(w, "forced MCP failure", http.StatusBadGateway)
			return
		}
		if method != mcpInitialize {
			if r.Header.Get("MCP-Protocol-Version") != mcpProtocolVersion || r.Header.Get("Mcp-Session-Id") != session {
				t.Errorf("%s headers: version=%q session=%q", method, r.Header.Get("MCP-Protocol-Version"), r.Header.Get("Mcp-Session-Id"))
			}
		}
		id, _ := json.Marshal(msg["id"])
		reply := func(result string) {
			w.Header().Set("Content-Type", "application/json")
			writeBody(t, w, `{"jsonrpc":"2.0","id":`+string(id)+`,"result":`+result+`}`)
		}
		switch msg["method"] {
		case mcpInitialize:
			w.Header().Set("Mcp-Session-Id", session)
			reply(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}`)
		case mcpInitialized, "notifications/cancelled":
			w.WriteHeader(http.StatusAccepted)
		case mcpToolsList:
			reply(`{"tools":[{"name":"search","description":"Search docs","inputSchema":{"type":"object","properties":{"q":{"type":"string"}}}},{"name":"delete_all","description":"Dangerous"},{"name":"noschema","description":"No schema"}]}`)
		case mcpToolsCall:
			// Answer as an event stream to exercise SSE parsing.
			w.Header().Set("Content-Type", "text/event-stream")
			text, _ := json.Marshal(f.mcpToolText)
			writeBody(t, w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":"+string(id)+",\"result\":{\"content\":[{\"type\":\"text\",\"text\":"+string(text)+"}],\"isError\":"+strconv.FormatBool(f.mcpToolError)+"}}\n\n")
		}
	}))
	for _, s := range []*httptest.Server{registry, policySrv, blob, llmSrv, mcpSrv} {
		t.Cleanup(s.Close)
	}
	f.svc = &Service{
		Store:             HTTPStore{BaseURL: blob.URL + "/blobs", Client: http.DefaultClient},
		RegistryURL:       registry.URL,
		PolicyRegistryURL: policySrv.URL,
		LLMURL:            llmSrv.URL + "/v1",
		MCPGatewayURL:     mcpSrv.URL,
		HTTP:              http.DefaultClient,
		Logger:            slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	return f
}

func request() *StepRequest {
	return &StepRequest{
		TraceID: "t1", Workflow: "feature", StepID: "plan", Agent: intakeRef,
		Inputs:         []InputItem{{Source: "input", Artifact: "runs/t1/input", Round: 1}, {Source: "intake", Artifact: "runs/t1/intake", Round: 1}},
		OutputArtifact: planArtifact,
	}
}

func respond(js string) llmHandler {
	return func(w http.ResponseWriter, _ map[string]any) error {
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, js)
		return err
	}
}

const (
	toolCallResponse = `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"docs__search","arguments":"{\"q\":\"auth\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	finalResponse    = `{"choices":[{"message":{"role":"assistant","content":"the plan"}}],"usage":{"prompt_tokens":20,"completion_tokens":7,"total_tokens":27}}`
)

func TestPromptEvidencePrefixPreservesNestedStepPath(t *testing.T) {
	if got := promptEvidencePrefix("runs/run-1/route/research", 3); got != "runs/run-1/_evidence/route/research/attempt-3" {
		t.Fatalf("nested evidence prefix = %q", got)
	}
}

func TestProcessCompilesLatestPoliciesThroughPolicyService(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":[],"max_turns":1}`,
		respond(finalResponse), respond(finalResponse))
	f.prompt = "Start\n{{policy::security/review}}\nAgain: {{policy::security/review}}\nEnd"
	f.policyContent = "Policy v1 contains {{policy::missing}} literally."
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	f.policyContent, f.policyVersion = "Policy v2 is fresh.", "v2"
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if got := len(f.policyRequests); got != 2 {
		t.Fatalf("policy requests = %d, want one per run despite repeated reference", got)
	}
	firstSystem := jsonObject(t, jsonList(t, f.llmReqs[0]["messages"])[0])["content"]
	secondSystem := jsonObject(t, jsonList(t, f.llmReqs[1]["messages"])[0])["content"]
	if firstSystem != "Start\nPolicy v1 contains {{policy::missing}} literally.\nAgain: Policy v1 contains {{policy::missing}} literally.\nEnd" {
		t.Fatalf("first compiled system = %q", firstSystem)
	}
	if secondSystem != "Start\nPolicy v2 is fresh.\nAgain: Policy v2 is fresh.\nEnd" {
		t.Fatalf("second compiled system = %q", secondSystem)
	}
	var rec RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Events) < 1 || rec.Events[0].Kind != "policy_compilation" || strings.Join(rec.Events[0].Policies, ",") != "security/review@v2" {
		t.Fatalf("policy provenance missing: %+v", rec.Events)
	}
	if rec.PromptEvidence == nil || rec.PromptEvidence.AgentSourceArtifact != "agents/intake/v1/systemprompt.md" || len(rec.PromptEvidence.Policies) != 1 {
		t.Fatalf("prompt evidence = %+v", rec.PromptEvidence)
	}
	if rec.PromptEvidence.SourcePromptArtifact != "runs/t1/_evidence/plan/attempt-1/systemprompt.source.md" || rec.PromptEvidence.EffectivePromptArtifact != "runs/t1/_evidence/plan/attempt-1/systemprompt.effective.md" {
		t.Fatalf("prompt evidence paths = %+v", rec.PromptEvidence)
	}
	policy := rec.PromptEvidence.Policies[0]
	if policy.ID != "security/review" || policy.Version != "v2" || policy.SourceArtifact != "policies/security/review/v2/policy.md" {
		t.Fatalf("policy evidence = %+v", policy)
	}
	if policy.SnapshotArtifact != "runs/t1/_evidence/plan/attempt-1/policies/security/review/v2/policy.md" {
		t.Fatalf("policy snapshot path = %q", policy.SnapshotArtifact)
	}
	if got := string(f.blobs[rec.PromptEvidence.SourcePromptArtifact]); got != f.prompt {
		t.Fatalf("source prompt snapshot = %q", got)
	}
	if got := string(f.blobs[rec.PromptEvidence.EffectivePromptArtifact]); got != secondSystem {
		t.Fatalf("effective prompt snapshot = %q", got)
	}
	if got := string(f.blobs[policy.SnapshotArtifact]); got != "Policy v2 is fresh." {
		t.Fatalf("policy snapshot = %q", got)
	}
}

func TestProcessMissingOrMalformedRequiredPolicyFailsBeforeModel(t *testing.T) {
	for _, test := range []struct {
		name, prompt string
		status       int
	}{
		{"missing", "{{policy::security/review}}", http.StatusNotFound},
		{"service unavailable", "{{policy::security/review}}", http.StatusServiceUnavailable},
		{"invalid id", "{{policy::Bad ID}}", 0},
		{"unclosed", "{{policy::security/review", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":[],"max_turns":1}`, respond(finalResponse))
			f.prompt, f.policyContent, f.policyStatus = test.prompt, "", test.status
			var logs bytes.Buffer
			f.svc.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			_, err := f.svc.Process(context.Background(), request())
			if err == nil || !strings.Contains(err.Error(), "compiling agent") {
				t.Fatalf("error = %v", err)
			}
			if len(f.llmReqs) != 0 || len(f.mcpCalls) != 0 {
				t.Fatalf("external execution started: llm=%v mcp=%v", f.llmReqs, f.mcpCalls)
			}
			var rec RunRecord
			if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Status != statusFailed || len(rec.Events) != 1 || rec.Events[0].Kind != "policy_compilation" || rec.Events[0].Status != statusFailed {
				t.Fatalf("failed record = %+v", rec)
			}
			if rec.PromptEvidence == nil || string(f.blobs[rec.PromptEvidence.SourcePromptArtifact]) != test.prompt || rec.PromptEvidence.EffectivePromptArtifact != "" {
				t.Fatalf("failed prompt evidence = %+v", rec.PromptEvidence)
			}
			if !strings.Contains(logs.String(), "policy compilation failed") || !strings.Contains(logs.String(), "agent failed") {
				t.Fatalf("failure logs = %s", logs.String())
			}
		})
	}
}

func TestProcessHappyPathWithToolCall(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1","m2"],"effort":"high","tools":["docs/search","docs/missing"],"max_turns":4,"extra":true}`,
		respond(toolCallResponse), respond(finalResponse))
	res, err := f.svc.Process(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if res.Artifact != planArtifact {
		t.Fatalf("artifact = %q", res.Artifact)
	}
	if got := string(f.blobs[planArtifact]); got != planAnswer {
		t.Fatalf("stored answer = %q", got)
	}
	want := [2]string{"agent:intake@v1", "t1"}
	if f.putMeta[planArtifact] != want || f.putMeta["runs/t1/plan.run.json"] != want {
		t.Fatalf("put metadata = %v", f.putMeta)
	}
	artifactHeaders := f.putMetricHeaders[planArtifact]
	if artifactHeaders.Get("X-Estimated-Input-Tokens") == "" || artifactHeaders.Get("X-Estimated-Output-Tokens") != "2" || artifactHeaders.Get("X-Processing-Time-Ms") == "" || artifactHeaders.Get("X-Estimation-Method") != estimationMethod {
		t.Fatalf("handoff metric headers = %v", artifactHeaders)
	}

	// MCP initialization, ready notification, tools/list, and tools/call.
	methods := make([]string, 0, len(f.mcpCalls))
	for _, c := range f.mcpCalls {
		methods = append(methods, jsonString(t, c["method"]))
	}
	if strings.Join(methods, ",") != "initialize,notifications/initialized,tools/list,tools/call" {
		t.Fatalf("mcp methods = %v", methods)
	}
	params := jsonObject(t, f.mcpCalls[3]["params"])
	if params["name"] != "search" || jsonObject(t, params["arguments"])["q"] != "auth" {
		t.Fatalf("tools/call params = %v", params)
	}

	// First model request: model, effort, filtered tools, system and user messages.
	first := f.llmReqs[0]
	if first["model"] != "m1" || first["reasoning_effort"] != effortHigh || first["stream"] != false {
		t.Fatalf("request = %v", first)
	}
	tools := jsonList(t, first["tools"])
	if len(tools) != 1 || jsonObject(t, jsonObject(t, tools[0])["function"])["name"] != "docs__search" {
		t.Fatalf("tools = %v", tools)
	}
	msgs := jsonList(t, first["messages"])
	if jsonObject(t, msgs[0])["content"] != intakePrompt {
		t.Fatalf("system = %v", msgs[0])
	}
	wantUser := "Trace: t1\nWorkflow: feature\nStep: plan\nOutput artifact: runs/t1/plan\n\n" +
		"## Input \"input\" (object runs/t1/input)\nplease build a thing\n\n" +
		"## Input \"intake\" (object runs/t1/intake)\nintent record"
	if got := jsonObject(t, msgs[1])["content"]; got != wantUser {
		t.Fatalf("user message =\n%q\nwant\n%q", got, wantUser)
	}

	// Second request carries the assistant tool call and the tool result.
	second := jsonList(t, f.llmReqs[1]["messages"])
	if len(second) != 4 {
		t.Fatalf("second request has %d messages", len(second))
	}
	tool := jsonObject(t, second[3])
	if tool["role"] != roleTool || tool["tool_call_id"] != firstCallID || tool["content"] != "found 3 hits" {
		t.Fatalf("tool message = %v", tool)
	}

	var rec struct {
		EstimatedInputTokens  *int64     `json:"estimated_input_tokens"`
		EstimatedOutputTokens *int64     `json:"estimated_output_tokens"`
		ProcessingTimeMS      *int64     `json:"processing_time_ms"`
		Agent                 string     `json:"agent"`
		Model                 string     `json:"model"`
		Effort                string     `json:"effort"`
		EstimationMethod      string     `json:"estimation_method"`
		Status                string     `json:"status"`
		TraceID               string     `json:"trace_id"`
		SpanID                string     `json:"span_id"`
		Messages              []any      `json:"messages"`
		Events                []RunEvent `json:"events"`
		Usage                 usage      `json:"usage"`
		ModelCalls            int        `json:"model_calls"`
		Attempt               int32      `json:"attempt"`
	}
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Agent != intakeRef || rec.Model != "m1" || rec.Effort != effortHigh || rec.ModelCalls != 2 || len(rec.Messages) != 5 {
		t.Fatalf("run record = %+v", rec)
	}
	if valueOf(rec.Usage.PromptTokens) != 30 || valueOf(rec.Usage.CompletionTokens) != 12 || valueOf(rec.Usage.TotalTokens) != 42 {
		t.Fatalf("usage = %+v", rec.Usage)
	}
	if rec.Events[1].EstimatedInputTokens == nil || rec.Events[1].EstimatedOutputTokens == nil || rec.Events[1].ProcessingTimeMS == nil || rec.Events[2].EstimatedInputTokens == nil || rec.Events[2].EstimatedOutputTokens == nil || rec.Events[2].ProcessingTimeMS == nil || rec.Events[3].EstimatedOutputTokens == nil {
		t.Fatalf("operation metrics missing: %+v", rec.Events)
	}
	if rec.EstimatedInputTokens == nil || rec.EstimatedOutputTokens == nil || rec.EstimationMethod != estimationMethod || rec.ProcessingTimeMS == nil {
		t.Fatalf("run estimate metrics missing: %+v", rec)
	}
	if *rec.EstimatedInputTokens != *rec.Events[1].EstimatedInputTokens+*rec.Events[3].EstimatedInputTokens || *rec.EstimatedOutputTokens != *rec.Events[1].EstimatedOutputTokens+*rec.Events[3].EstimatedOutputTokens {
		t.Fatalf("run aggregates include a non-model operation: totals=%d/%d events=%+v", *rec.EstimatedInputTokens, *rec.EstimatedOutputTokens, rec.Events)
	}
	if artifactHeaders.Get("X-Estimated-Input-Tokens") != strconv.FormatInt(*rec.Events[1].EstimatedInputTokens, 10) {
		t.Fatalf("artifact initial request estimate = %q, want %d", artifactHeaders.Get("X-Estimated-Input-Tokens"), *rec.Events[1].EstimatedInputTokens)
	}
	if rec.Status != statusCompleted || rec.Attempt != 1 || rec.TraceID != "" || rec.SpanID != "" || len(rec.Events) != 4 {
		t.Fatalf("record observability = status %q attempt %d trace/span %q/%q events %+v", rec.Status, rec.Attempt, rec.TraceID, rec.SpanID, rec.Events)
	}
	if _, ok := f.blobs["runs/t1/plan.attempt-1.run.json"]; !ok {
		t.Fatal("immutable attempt record missing")
	}
	for _, headers := range append(f.llmHeads, f.mcpHeads...) {
		if headers.Get("X-Trace-Id") != "t1" || headers.Get("X-Step-Id") != "plan" || headers.Get("X-Agent-Ref") != intakeRef || !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`).MatchString(headers.Get("Traceparent")) {
			t.Fatalf("correlation headers = %v", headers)
		}
	}
}

func TestNoToolsNeverCallsMCP(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":[]}`, respond(finalResponse))
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(f.mcpCalls) != 0 {
		t.Fatalf("mcp called: %v", f.mcpCalls)
	}
	req := f.llmReqs[0]
	if _, ok := req["tools"]; ok {
		t.Fatal("tools sent without an allowlist")
	}
	if _, ok := req["reasoning_effort"]; ok {
		t.Fatal("reasoning_effort sent without effort")
	}
}

func TestModelCallCheckpointIsDurableBeforeExternalCallCompletes(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":[]}`, func(w http.ResponseWriter, _ map[string]any) error {
		close(started)
		<-release
		return respond(finalResponse)(w, nil)
	})
	done := make(chan error, 1)
	go func() { _, err := f.svc.Process(context.Background(), request()); done <- err }()
	<-started
	f.mu.Lock()
	checkpoint := append([]byte(nil), f.blobs["runs/t1/plan.attempt-1.run.json"]...)
	f.mu.Unlock()
	var rec RunRecord
	if err := json.Unmarshal(checkpoint, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != statusRunning || len(rec.Events) != 1 || rec.Events[0].Status != statusRunning || rec.Events[0].Kind != kindModelCall || len(rec.Messages) != 2 {
		t.Fatalf("in-flight checkpoint = %+v", rec)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRecordAndEventsUseExportedTraceContext(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":[]}`, respond(finalResponse))
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	var record RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Events) != 1 || record.TraceID == "" || record.Events[0].TraceID != record.TraceID || record.Events[0].SpanID == "" {
		t.Fatalf("record/event trace correlation = %+v", record)
	}
	ended := recorder.Ended()
	foundProcess, foundEvent := false, false
	for _, span := range ended {
		if span.SpanContext().TraceID().String() != record.TraceID {
			continue
		}
		if span.Name() == "agent.process" && span.SpanContext().SpanID().String() == record.SpanID {
			foundProcess = true
		}
		if span.Name() == "agent.model_call" && span.SpanContext().SpanID().String() == record.Events[0].SpanID {
			foundEvent = true
		}
	}
	if !foundProcess || !foundEvent {
		t.Fatalf("exported spans missing: process=%v event=%v spans=%d", foundProcess, foundEvent, len(ended))
	}
}

func TestDuplicateToolNamesRouteToSelectedServers(t *testing.T) {
	calls := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"docs__search","arguments":"{\"q\":\"docs\"}"}},` +
		`{"id":"b","type":"function","function":{"name":"code__search","arguments":"{\"q\":\"code\"}"}}]}}]}`
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search","code/search"],"max_turns":2}`,
		respond(calls), respond(finalResponse))
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	called := map[string]bool{}
	for _, call := range f.mcpCalls {
		if call["method"] == mcpToolsCall {
			called[jsonString(t, call["_path"])] = true
		}
	}
	if !called[mcpDocsPath] || !called["/mcp/code"] || len(called) != 2 {
		t.Fatalf("tool calls routed to %v", called)
	}
}

func TestInvalidToolReferenceMakesNoGatewayCall(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["missing-server"]}`, respond(finalResponse))
	if _, err := f.svc.Process(context.Background(), request()); err == nil || !strings.Contains(err.Error(), "<server>/<tool>") {
		t.Fatalf("err = %v", err)
	}
	if len(f.mcpCalls) != 0 {
		t.Fatalf("MCP called for invalid reference: %v", f.mcpCalls)
	}
}

func TestAllowlistFiltersAndDefaultsSchema(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/noschema"]}`, respond(finalResponse))
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	tools := jsonList(t, f.llmReqs[0]["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	fn := jsonObject(t, jsonObject(t, tools[0])["function"])
	if fn["name"] != "docs__noschema" || jsonObject(t, fn["parameters"])["type"] != "object" {
		t.Fatalf("function = %v", fn)
	}
}

func TestDisallowedAndInvalidToolCallsBecomeToolErrors(t *testing.T) {
	calls := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"docs__delete_all","arguments":"{}"}},` +
		`{"id":"b","type":"function","function":{"name":"docs__search","arguments":"{not json"}}]}}]}`
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"],"max_turns":2}`, respond(calls), respond(finalResponse))
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.mcpCalls {
		if c["method"] == mcpToolsCall {
			t.Fatalf("unexpected tools/call: %v", c)
		}
	}
	msgs := jsonList(t, f.llmReqs[1]["messages"])
	for _, i := range []int{3, 4} {
		if content := jsonString(t, jsonObject(t, msgs[i])["content"]); !strings.HasPrefix(content, "Error: ") {
			t.Fatalf("tool message %d = %q", i, content)
		}
	}
}

func TestMaxTurnsExceeded(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"],"max_turns":2}`, respond(toolCallResponse))
	_, err := f.svc.Process(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "max_turns=2") {
		t.Fatalf("err = %v", err)
	}
	if len(f.llmReqs) != 2 {
		t.Fatalf("model calls = %d", len(f.llmReqs))
	}
	if _, ok := f.blobs[planArtifact]; ok {
		t.Fatal("output stored despite failure")
	}
	var rec RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != statusFailed || !strings.Contains(rec.Error, "max_turns=2") || rec.ModelCalls != 2 || len(rec.Messages) < 4 {
		t.Fatalf("failure record = %+v", rec)
	}
	if _, ok := f.blobs["runs/t1/plan.attempt-1.run.json"]; !ok {
		t.Fatal("failure attempt record missing")
	}
}

func TestOneModelCallByDefault(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"]}`, respond(toolCallResponse))
	if _, err := f.svc.Process(context.Background(), request()); err == nil || !strings.Contains(err.Error(), "within its turn limit (max_turns=1)") {
		t.Fatalf("err = %v", err)
	}
	if len(f.llmReqs) != 1 {
		t.Fatalf("model calls = %d, want 1", len(f.llmReqs))
	}
}

func TestGatewayErrorIncludesUpstreamMessage(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, func(w http.ResponseWriter, _ map[string]any) error {
		w.WriteHeader(http.StatusInternalServerError)
		_, err := io.WriteString(w, `{"error":{"message":"upstream exploded","type":"api_error"}}`)
		return err
	})
	_, err := f.svc.Process(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "upstream exploded") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
	var rec RunRecord
	if json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec) != nil || rec.Status != statusFailed || len(rec.Events) != 1 || rec.Events[0].Status != statusFailed || rec.Events[0].Category != llmGatewayName {
		t.Fatalf("gateway failure record = %+v", rec)
	}
	if rec.Events[0].EstimatedInputTokens == nil || rec.Events[0].EstimatedOutputTokens != nil || rec.Events[0].ProcessingTimeMS == nil || rec.EstimatedInputTokens == nil || rec.EstimatedOutputTokens != nil || rec.ProcessingTimeMS == nil {
		t.Fatalf("failed model call lost known metrics or invented output: %+v", rec)
	}
}

func TestErrorCategoriesDistinguishGatewayAndTransport(t *testing.T) {
	if got := errorCategory(categorizedError{llmGatewayName, "404"}); got != llmGatewayName {
		t.Fatalf("gateway category = %q", got)
	}
	if got := errorCategory(errors.New("connection reset")); got != "transport" {
		t.Fatalf("network category = %q", got)
	}
}

func TestMCPTransportFailurePersistsTimeline(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"],"max_turns":2}`, respond(toolCallResponse))
	f.mcpFailMethod = mcpToolsCall
	_, err := f.svc.Process(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
	var rec RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != statusFailed || len(rec.Events) != 3 || rec.Events[2].Kind != kindToolCall || rec.Events[2].Status != statusFailed || rec.Events[2].ToolCallID != firstCallID {
		t.Fatalf("MCP failure record = %+v", rec)
	}
}

func TestSuccessfulToolTextStartingWithErrorIsNotMisclassified(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"],"max_turns":2}`, respond(toolCallResponse), respond(finalResponse))
	f.mcpToolText = "Error: this is documentation, not a failure"
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	var rec RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Events) != 4 || rec.Events[2].Status != statusCompleted || rec.Events[2].Error != "" {
		t.Fatalf("successful tool response was misclassified: %+v", rec.Events)
	}
}

func TestCorrelationSpanDistinguishesNestedStepsWithSameID(t *testing.T) {
	first := request()
	second := request()
	second.OutputArtifact = "runs/t1/route/plan"
	one, ok := withCorrelation(context.Background(), first, first.Agent, 1).Value(correlationKey{}).(correlation)
	if !ok {
		t.Fatal("first context carries no correlation")
	}
	two, ok := withCorrelation(context.Background(), second, second.Agent, 1).Value(correlationKey{}).(correlation)
	if !ok {
		t.Fatal("second context carries no correlation")
	}
	if strings.Split(one.traceparent, "-")[1] != strings.Split(two.traceparent, "-")[1] {
		t.Fatal("one run must share one trace ID")
	}
	if strings.Split(one.traceparent, "-")[2] == strings.Split(two.traceparent, "-")[2] {
		t.Fatal("nested steps with the same ID reused a span ID")
	}
}

func TestCorrelationHeadersCarryExecutionActivityAndTurn(t *testing.T) {
	req := request()
	req.ExecutionID = "execution-1"
	req.ParentExecutionID = "parent-1"
	ctx := withModelTurn(withCorrelation(context.Background(), req, "judge@v3", 2), 4)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.test", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	setCorrelationHeaders(httpReq)
	if httpReq.Header.Get("X-Execution-Id") != "execution-1" || httpReq.Header.Get("X-Parent-Execution-Id") != "parent-1" || httpReq.Header.Get("X-Activity-Attempt") != "2" || httpReq.Header.Get("X-Turn") != "4" {
		t.Fatalf("headers %v", httpReq.Header)
	}
}

func TestCanceledAttemptPersistsWithBackgroundContext(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, respond(finalResponse))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.svc.Process(ctx, request())
	if err == nil {
		t.Fatal("canceled process succeeded")
	}
	var rec RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != statusCanceled || rec.Error == "" {
		t.Fatalf("cancel record = %+v", rec)
	}
}

type failingPutStore struct{ Store }

func (f failingPutStore) Put(context.Context, string, []byte, Meta) error {
	return errors.New("blobstore unavailable")
}

type failAfterStore struct {
	Store
}

func (s *failAfterStore) Put(ctx context.Context, id string, body []byte, meta Meta) error {
	if strings.HasSuffix(id, ".run.json") && !strings.Contains(id, ".attempt-") && bytes.Contains(body, []byte(`"status": "completed"`)) {
		return errors.New("final record unavailable")
	}
	return s.Store.Put(ctx, id, body, meta)
}

func TestSuccessfulWorkWithRecordFailureLogsFailedStatus(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, respond(finalResponse))
	store := &failAfterStore{Store: f.svc.Store}
	f.svc.Store = store
	var logs bytes.Buffer
	f.svc.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	_, err := f.svc.Process(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "final record unavailable") {
		t.Fatalf("record persistence error = %v", err)
	}
	var terminal map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var entry map[string]any
		if json.Unmarshal(line, &entry) == nil && entry["msg"] == "agent failed" {
			terminal = entry
		}
	}
	if terminal["status"] != statusFailed || !strings.Contains(fmt.Sprint(terminal["error"]), "final record unavailable") {
		t.Fatalf("terminal log = %v", terminal)
	}
}

func TestStartCheckpointFailureStopsBeforeExternalCall(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, func(w http.ResponseWriter, _ map[string]any) error {
		w.WriteHeader(http.StatusBadGateway)
		_, err := io.WriteString(w, `{"error":{"message":"original model failure"}}`)
		return err
	})
	f.svc.Store = failingPutStore{Store: f.svc.Store}
	_, err := f.svc.Process(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "persisting agent start checkpoint") || !strings.Contains(err.Error(), "blobstore unavailable") {
		t.Fatalf("checkpoint error = %v", err)
	}
	if len(f.llmReqs) != 0 {
		t.Fatalf("model called despite missing durable checkpoint: %v", f.llmReqs)
	}
}

func TestInvalidAgentRef(t *testing.T) {
	f := newFakes(t, `{}`, respond(finalResponse))
	for _, ref := range []string{"", "intake", "intake@", "@v1", "a@b@c", "../x@v1", "/intake@v1", "intake/@v1", "a//b@v1", "a/b/c/d/e@v1"} {
		req := request()
		req.Agent = ref
		if _, err := f.svc.Process(context.Background(), req); err == nil {
			t.Fatalf("ref %q accepted", ref)
		}
	}
}

// TestGroupedAgentRef runs an agent that sits in a group, like intake/triage@v1.
func TestGroupedAgentRef(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake/triage","unpack_inputs":true,"models":["m"],"tools":[]}`, respond(finalResponse))
	req := request()
	req.Agent = "intake/triage@v1"
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatalf("grouped agent: %v", err)
	}
}

func TestDefinitionWithoutModels(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","tools":[]}`, respond(finalResponse))
	_, err := f.svc.Process(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "no models") {
		t.Fatalf("err = %v", err)
	}
	if len(f.llmReqs) != 0 {
		t.Fatal("model called without models")
	}
}

// A version stored before max_turns names its limit max_retries: the model calls after the
// first one. It keeps running with the same number of calls.
func TestStoredMaxRetriesStillLimitsTheTurns(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"tools":["docs/search"],"max_retries":1}`, respond(toolCallResponse))
	_, err := f.svc.Process(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "within its turn limit (max_turns=2)") {
		t.Fatalf("err = %v", err)
	}
	if len(f.llmReqs) != 2 {
		t.Fatalf("model calls = %d", len(f.llmReqs))
	}
}
