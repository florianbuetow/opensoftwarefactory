package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"
)

const (
	skillAgentJSON    = `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"max_retries":3}`
	skillRootText     = "---\nname: git\ndescription: Read the release checklist.\n---\nRead release/SKILL.md."
	skillScriptResult = `{"exit_code":0,"stdout":"tail","stderr":"","warning":"Standard output was longer than the limit."}`
)

func skillFakes(t *testing.T, f *fakes) *podGateway {
	t.Helper()
	g := &podGateway{}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/skills/tools/git/v3" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": map[string]string{"SKILL.md": skillRootText}})
	}))
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/skills" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req toolRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var result any
		switch req.Method {
		case mcpInitialize:
			w.Header().Set("Mcp-Session-Id", "skill-session")
			result = json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}`)
		case mcpToolsList:
			result = json.RawMessage(`{"tools":[
				{"name":"read_skill","inputSchema":{"type":"object","properties":{"skill":{"type":"string"},"file":{"type":"string"},"pod":{"type":"string"},"agent":{"type":"string"},"run":{"type":"string"},"version":{"type":"string"}},"required":["skill","pod","agent","run","version"]}},
				{"name":"run_skill_script","inputSchema":{"type":"object","properties":{"skill":{"type":"string"},"script":{"type":"string"},"args":{"type":"array"},"pod":{"type":"string"},"agent":{"type":"string"},"run":{"type":"string"},"version":{"type":"string"},"timeout_seconds":{"type":"integer"}},"required":["skill","script","pod","agent","run","version","timeout_seconds"]}}
			]}`)
		case mcpToolsCall:
			f.mu.Lock()
			g.names = append(g.names, req.Params.Name)
			g.calls = append(g.calls, req.Params.Arguments)
			f.mu.Unlock()
			text := skillRootText
			if req.Params.Name == skillScriptTool {
				text = skillScriptResult
			}
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}}
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(registry.Close)
	t.Cleanup(mcp.Close)
	f.svc.SkillRegistryURL = registry.URL
	f.svc.MCPGatewayURL = mcp.URL
	return g
}

func TestSkillPromptToolsAndEvidence(t *testing.T) {
	f := newFakes(t, skillAgentJSON, podCalls(
		[2]string{"skills__read_skill", `{"skill":"other"}`},
		[2]string{"skills__run_skill_script", `{"skill":"tools/git","script":"test.sh","pod":"spoof","agent":"spoof","run":"spoof","version":"v99","timeout_seconds":1}`},
	), respond(finalResponse))
	g := skillFakes(t, f)
	req := podRequest()
	req.Skills = map[string]string{"tools/git": "v3"}
	req.SkillScriptTimeoutSeconds = 75
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"skill": "tools/git", "script": "test.sh", "pod": testPod,
		"agent": intakeRef, "run": "t1", "version": "v3", "timeout_seconds": float64(75),
	}
	if len(g.calls) != 1 || !reflect.DeepEqual(g.calls[0], want) {
		t.Fatalf("forwarded calls: %v", g.calls)
	}
	schemas := shownSchemas(t, f.llmReqs[0])
	for name, keys := range map[string][]any{"skills__read_skill": {"skill"}, "skills__run_skill_script": {"skill", "script"}} {
		schema := schemas[name]
		if !reflect.DeepEqual(schema["required"], keys) {
			t.Fatalf("%s required: %v", name, schema["required"])
		}
		props := jsonObject(t, schema["properties"])
		for _, hidden := range skillIdentity {
			if _, ok := props[hidden]; ok {
				t.Errorf("%s exposes %s", name, hidden)
			}
		}
	}
	var record RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &record); err != nil {
		t.Fatal(err)
	}
	provenance := record.PromptEvidence
	if want := []SkillEvidence{{"tools/git", "v3", "skills/tools/git/v3/SKILL.md"}}; !reflect.DeepEqual(provenance.Skills, want) {
		t.Fatalf("skill evidence: %v", provenance.Skills)
	}
	prompt := string(f.blobs[provenance.EffectivePromptArtifact])
	if !strings.Contains(prompt, "## Skills\n\nYou carry") || !strings.Contains(prompt, "- tools/git: Read the release checklist.") ||
		!strings.Contains(prompt, "skills__read_skill") || !strings.Contains(prompt, "skills__run_skill_script") || strings.Contains(prompt, "v3") {
		t.Fatalf("effective prompt: %q", prompt)
	}
	var warnings []string
	for _, event := range record.Events {
		if event.Warning != "" {
			warnings = append(warnings, event.Name+": "+event.Warning)
		}
	}
	if len(warnings) != 1 || warnings[0] != "skills__run_skill_script: Standard output was longer than the limit." {
		t.Fatalf("warnings: %v", warnings)
	}
	data, _ := json.Marshal(record.Messages)
	for _, text := range []string{"spoof", "does not carry skill", "Standard output was longer"} {
		if !strings.Contains(string(data), text) {
			t.Errorf("record is missing %q: %s", text, data)
		}
	}
}

func TestNoSkillsMeansNoPromptSectionOrTools(t *testing.T) {
	f := newFakes(t, strings.TrimSuffix(skillAgentJSON, "}")+`,"tools":["skills/read_skill"]}`, respond(finalResponse))
	g := skillFakes(t, f)
	if _, err := f.svc.Process(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if f.llmReqs[0]["tools"] != nil || len(g.calls) != 0 || strings.Contains(string(f.blobs["runs/t1/plan.run.json"]), "## Skills") {
		t.Fatalf("unexpected skills in run: %v, %v", g.calls, f.llmReqs)
	}
}

func TestSkillScriptRequiresAPodInEveryTurn(t *testing.T) {
	const tool = "skills__run_skill_script"
	versions := map[string]string{"tools/git": "v3"}
	for _, kind := range []string{"process", "chat", "team"} {
		t.Run(kind, func(t *testing.T) {
			response := podCalls([2]string{tool, `{"skill":"tools/git","script":"check.sh"}`})
			var f *fakes
			if kind == "team" {
				f = teamFakes(t, skillAgentJSON, response)
			} else {
				f = newFakes(t, skillAgentJSON, response)
			}
			g := skillFakes(t, f)
			var err error
			switch kind {
			case "process":
				req := request()
				req.Skills = versions
				_, err = f.svc.Process(context.Background(), req)
			case "chat":
				body := chatBody()
				req := &sessionChatRequest{
					TraceID: body["trace_id"], StepID: body["step_id"], Session: body["session"],
					Tab: body["tab"], Agent: body["agent"], Message: body["message"], Skills: versions,
				}
				_, _, _, err = f.svc.Chat(context.Background(), req)
			case "team":
				req := coderTurn()
				req.Skills = versions
				_, err = f.svc.TeamTurn(context.Background(), req)
			}
			var app *temporal.ApplicationError
			if !errors.As(err, &app) || !app.NonRetryable() || app.Type() != "SkillScriptNeedsDevPod" || len(g.calls) != 0 {
				t.Fatalf("got %v with calls %v", err, g.calls)
			}
		})
	}
}

func TestReadSkillWorksWithoutPod(t *testing.T) {
	f := newFakes(t, skillAgentJSON,
		podCalls([2]string{"skills__read_skill", `{"skill":"tools/git","file":"release/SKILL.md","pod":"wrong","version":"v1","timeout_seconds":12}`}),
		respond(finalResponse))
	g := skillFakes(t, f)
	req := request()
	req.Skills = map[string]string{"tools/git": "v3"}
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"skill": "tools/git", "file": "release/SKILL.md", "pod": "",
		"agent": intakeRef, "run": "t1", "version": "v3",
	}
	if len(g.calls) != 1 || !reflect.DeepEqual(g.calls[0], want) {
		t.Fatalf("forwarded calls: %v", g.calls)
	}
}
