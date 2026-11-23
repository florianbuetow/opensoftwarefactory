package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opensoftwarefactory/orchestrator/pipeline"
)

// send calls the API as the given person and decodes a JSON answer into out when it is set.
func send(t *testing.T, method, url, creator, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if creator != "" {
		req.Header.Set("X-Creator", creator)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err = json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: status %d, body is not JSON: %v", method, url, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}

// serverWith starts a server whose stored workflows are the given YAML configuration.
func serverWith(t *testing.T, config string, agents func(context.Context, string) (AgentSettings, error)) (*httptest.Server, *fakeRunner, *memStore, *workflowStore) {
	t.Helper()
	workflows := storeWorkflows(t, config)
	runner, store := &fakeRunner{}, newMemStore()
	s := &server{workflows: workflows, blobs: store, runner: runner, agents: agents, logger: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return srv, runner, store, workflows
}

func TestSaveStoresTheNextVersionAndKeepsEarlierOnes(t *testing.T) {
	srv, _, _, workflows := newTestServer(t)
	var v1, v2, got workflowView
	// The client's name, version, time, and creator are ignored.
	first := `{"name":"other","version":7,"created":"2001-01-01T00:00:00Z","created_by":"mallory","versions":[9],"description":"one","steps":[{"id":"a","service":"svc"}]}`
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/flow/versions", "flo", first, &v1); code != http.StatusCreated {
		t.Fatalf("status %d", code)
	}
	if v1.Name != "flow" || v1.Version != 1 || v1.CreatedBy != "flo" || len(v1.Versions) != 1 || v1.Versions[0] != 1 {
		t.Fatalf("v1 %+v", v1)
	}
	if created, err := time.Parse(time.RFC3339, v1.Created); err != nil || time.Since(created) > time.Minute {
		t.Fatalf("created %q", v1.Created)
	}
	second := `{"description":"two","steps":[{"id":"a","service":"svc"},{"id":"b","service":"svc","depends_on":["a"]}]}`
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/flow/versions", "ana", second, &v2); code != http.StatusCreated {
		t.Fatalf("status %d", code)
	}
	if v2.Version != 2 || v2.CreatedBy != "ana" || len(v2.Versions) != 2 || len(v2.Steps) != 2 {
		t.Fatalf("v2 %+v", v2)
	}
	// The first version did not change.
	if code := send(t, http.MethodGet, srv.URL+"/api/workflows/flow/versions/1", "", "", &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if got.Version != 1 || got.Description != "one" || len(got.Steps) != 1 || got.CreatedBy != "flo" || got.Created != v1.Created || len(got.Versions) != 2 {
		t.Fatalf("v1 after v2 %+v", got)
	}
	for _, missing := range []string{"/api/workflows/flow/versions/3", "/api/workflows/flow/versions/0", "/api/workflows/flow/versions/x", "/api/workflows/none/versions/1"} {
		if code, _ := call(t, http.MethodGet, srv.URL+missing, ""); code != http.StatusNotFound {
			t.Errorf("%s: %d", missing, code)
		}
	}
	// The list holds the newest version of every workflow with its versions.
	var list struct {
		Workflows []workflowView `json:"workflows"`
	}
	if code := send(t, http.MethodGet, srv.URL+"/api/workflows", "", "", &list); code != http.StatusOK || len(list.Workflows) != 2 {
		t.Fatalf("list %d %+v", code, list)
	}
	if w := list.Workflows[0]; w.Name != "sample" || w.Version != 1 || len(w.Versions) != 1 {
		t.Errorf("sample %+v", w)
	}
	if w := list.Workflows[1]; w.Name != "flow" || w.Version != 2 || w.Description != "two" || len(w.Versions) != 2 || w.Versions[1] != 2 {
		t.Errorf("flow %+v", w)
	}
	// The file keeps every version.
	saved, err := storedConfig(workflows)
	if err != nil || len(saved.Workflows) != 3 {
		t.Fatalf("saved %+v %v", saved, err)
	}
	if code, _ := call(t, http.MethodPut, srv.URL+"/api/workflows", `{"workflows":[]}`); code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/workflows answered %d", code)
	}
}

func TestSavePinsBareReferencesAndRefusesUnknownVersions(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	save := func(name, body string, out any) int {
		return send(t, http.MethodPost, srv.URL+"/api/workflows/"+name+"/versions", "flo", body, out)
	}
	leaf := `{"steps":[{"id":"a","service":"svc"}]}`
	if save("leaf", leaf, nil) != http.StatusCreated || save("leaf", leaf, nil) != http.StatusCreated {
		t.Fatal("saving leaf failed")
	}
	var outer workflowView
	body := `{"steps":[{"id":"bare","service":"workflow","workflow":"leaf"},{"id":"old","service":"workflow","workflow":"leaf@v1"},` +
		`{"id":"team","service":"team","agent":"lead@v1","workflows":["sample"],"channels":[["lead","sample"]],"depends_on":["bare","old"]}]}`
	if code := save("outer", body, &outer); code != http.StatusCreated {
		t.Fatalf("status %d", code)
	}
	if outer.Steps[0].Workflow != "leaf@v2" || outer.Steps[1].Workflow != "leaf@v1" || outer.Steps[2].Workflows[0] != "sample@v1" {
		t.Fatalf("references %+v", outer.Steps)
	}
	var refused map[string]string
	if code := save("outer", `{"steps":[{"id":"n","service":"workflow","workflow":"leaf@v3"}]}`, &refused); code != http.StatusUnprocessableEntity || !strings.Contains(refused["error"], `unknown workflow "leaf@v3"`) {
		t.Fatalf("unknown version: %d %v", code, refused)
	}
	if code := save("outer", `{"steps":[{"id":"n","service":"workflow","workflow":"gone"}]}`, &refused); code != http.StatusUnprocessableEntity || !strings.Contains(refused["error"], `unknown workflow "gone"`) {
		t.Fatalf("unknown workflow: %d %v", code, refused)
	}
	// A name that differs only in case from a stored workflow is a duplicate.
	if code := save("Leaf", leaf, &refused); code != http.StatusUnprocessableEntity || !strings.Contains(refused["error"], "duplicate") {
		t.Fatalf("case duplicate: %d %v", code, refused)
	}
	if code := save("bad_name", leaf, &refused); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad name: %d", code)
	}
}

func TestRunStartsTheRequestedVersion(t *testing.T) {
	srv, runner, store, _ := newTestServer(t)
	second := `{"steps":[{"id":"only","service":"svc"}]}`
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/sample/versions", "", second, nil); code != http.StatusCreated {
		t.Fatalf("status %d", code)
	}
	var out struct {
		TraceID         string `json:"trace_id"`
		Workflow        string `json:"workflow"`
		WorkflowVersion int    `json:"workflow_version"`
	}
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs?version=1", "", "x", &out); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	if in := runner.started[0]; out.WorkflowVersion != 1 || out.Workflow != "sample" || in.Definition.Version != 1 || len(in.Definition.Steps) != 2 {
		t.Fatalf("older version: %+v %+v", out, in.Definition)
	}
	var record RunRecord
	_ = json.Unmarshal(store.data["runs/"+out.TraceID+"/run.json"], &record)
	if record.WorkflowVersion != 1 || !strings.Contains(string(store.data["runs/"+out.TraceID+"/run.json"]), `"workflow_version": 1`) {
		t.Fatalf("run.json %s", store.data["runs/"+out.TraceID+"/run.json"])
	}
	// Without a version the newest runs.
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs", "", "x", &out); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	if in := runner.started[1]; out.WorkflowVersion != 2 || in.Definition.Version != 2 || len(in.Definition.Steps) != 1 {
		t.Fatalf("newest version: %+v %+v", out, in.Definition)
	}
	if code, _ := call(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs?version=3", "x"); code != http.StatusNotFound {
		t.Errorf("unknown version %d", code)
	}
	for _, bad := range []string{"x", "0", "-1", "v1"} {
		if code, _ := call(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs?version="+bad, "x"); code != http.StatusBadRequest {
			t.Errorf("version %q: %d", bad, code)
		}
	}
	if len(runner.started) != 2 {
		t.Errorf("started %d runs", len(runner.started))
	}
}

const nestedConfig = `workflows:
  - name: leaf
    steps: [{id: a, service: agent, agent: demo/writer@v1}]
  - name: leaf
    version: 2
    steps: [{id: a, service: agent, agent: demo/writer@v2}]
  - name: mid
    steps:
      - id: team
        service: team
        agent: demo/lead@v1
        agents: [demo/coder@v1]
        workflows: [leaf@v1]
        channels: [[lead, coder], [lead, leaf, one-way]]
        member_positions: {leaf: {x: 10, y: 20}}
  - name: top
    steps:
      - {id: ask, service: interactive, agent: demo/triage@v1, agents: [demo/coder@v1]}
      - {id: run, service: workflow, workflow: mid, depends_on: [ask]}
  - name: free
    steps: [{id: a, service: svc}]
`

func TestLoadTreatsUnversionedWorkflowsAsV1AndPinsReferences(t *testing.T) {
	c, err := parseConfig([]byte(nestedConfig))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range c.Workflows {
		if want := map[bool]int{true: 2, false: 1}[w.Steps[0].Agent == "demo/writer@v2"]; w.Version != want {
			t.Errorf("%s loaded as v%d", w.Name, w.Version)
		}
	}
	top, _ := c.find("top", 0)
	mid, _ := c.find("mid", 1)
	if top.Steps[1].Workflow != "mid@v1" || mid.Steps[0].Workflows[0] != "leaf@v1" {
		t.Fatalf("references %q %q", top.Steps[1].Workflow, mid.Steps[0].Workflows[0])
	}
	// A bare reference names the newest version at the time of loading.
	bare, err := parseConfig([]byte(strings.Replace(nestedConfig, "workflows: [leaf@v1]", "workflows: [leaf]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if mid, _ = bare.find("mid", 0); mid.Steps[0].Workflows[0] != "leaf@v2" {
		t.Fatalf("bare reference pinned to %q", mid.Steps[0].Workflows[0])
	}
	// The next save stores versions and pinned references in the blobstore.
	srv, _, _, workflows := serverWith(t, nestedConfig, nil)
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/free/versions", "", `{"steps":[{"id":"a","service":"svc"}]}`, nil); code != http.StatusCreated {
		t.Fatalf("status %d", code)
	}
	file, _ := workflowObjects(workflows)
	if !strings.Contains(string(file), `"workflow": "mid@v1"`) || strings.Count(string(file), `"version": 1,`) != 4 || strings.Count(string(file), `"version": 2,`) != 2 {
		t.Fatalf("stored objects:\n%s", file)
	}
	if _, err = parseConfig([]byte("workflows:\n  - name: a\n    steps: [{id: a, service: s}]\n  - name: a\n    steps: [{id: a, service: s}]\n")); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("the same version twice: %v", err)
	}
}

func TestRenameRenamesEveryVersionAndTheReferences(t *testing.T) {
	srv, _, _, workflows := serverWith(t, nestedConfig, nil)
	var renamed workflowView
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/leaf/rename", "", `{"name":"sheet"}`, &renamed); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if renamed.Name != "sheet" || renamed.Version != 2 || len(renamed.Versions) != 2 {
		t.Fatalf("renamed %+v", renamed)
	}
	c, err := storedConfig(workflows)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.find("leaf", 0); ok {
		t.Error("the old name is still stored")
	}
	if v1, ok := c.find("sheet", 1); !ok || v1.Steps[0].Agent != "demo/writer@v1" {
		t.Errorf("v1 %+v", v1)
	}
	team := mustFind(t, c, "mid").Steps[0]
	if team.Workflows[0] != "sheet@v1" || team.Channels[1][1] != "sheet" || team.Channels[1][2] != pipeline.OneWay || team.Channels[0][1] != "coder" {
		t.Errorf("team %+v", team)
	}
	if _, moved := team.MemberPositions["sheet"]; !moved || len(team.MemberPositions) != 1 {
		t.Errorf("positions %+v", team.MemberPositions)
	}
	var refused map[string]string
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/sheet/rename", "", `{"name":"MID"}`, &refused); code != http.StatusConflict || !strings.Contains(refused["error"], "mid") {
		t.Errorf("existing name: %d %v", code, refused)
	}
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/gone/rename", "", `{"name":"x"}`, &refused); code != http.StatusNotFound {
		t.Errorf("unknown workflow: %d", code)
	}
	for _, bad := range []string{`{"name":""}`, `{"name":"a b"}`, `{"name":"x@v1"}`} {
		if code := send(t, http.MethodPost, srv.URL+"/api/workflows/sheet/rename", "", bad, &refused); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", bad, code)
		}
	}
	// A workflow member may not take the name of an agent member of the same team.
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/sheet/rename", "", `{"name":"coder"}`, &refused); code != http.StatusUnprocessableEntity || !strings.Contains(refused["error"], "two members") {
		t.Errorf("member name clash: %d %v", code, refused)
	}
	if after, _ := storedConfig(workflows); mustFind(t, after, "mid").Steps[0].Workflows[0] != "sheet@v1" {
		t.Error("a refused rename changed the file")
	}
}

func TestRenameMovesAWorkflowBetweenGroups(t *testing.T) {
	srv, _, _, workflows := serverWith(t, nestedConfig, nil)
	var moved workflowView
	// The UI sends the name with its group as one path segment.
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/leaf/rename", "", `{"name":"docs/leaf"}`, &moved); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/docs%2Fleaf/rename", "", `{"name":"lib/sheets/sheet"}`, &moved); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if moved.Name != "lib/sheets/sheet" || len(moved.Versions) != 2 {
		t.Fatalf("moved %+v", moved)
	}
	blobs := workflows.blobs.(*memStore)
	for _, id := range []string{"workflows/lib/sheets/sheet/v1/workflow.json", "workflows/lib/sheets/sheet/v2/workflow.json"} {
		if _, ok := blobs.data[id]; !ok {
			t.Errorf("%s is not stored", id)
		}
	}
	for id := range blobs.data {
		if strings.HasPrefix(id, "workflows/leaf/") || strings.HasPrefix(id, "workflows/docs/") {
			t.Errorf("%s is still stored", id)
		}
	}
	c, err := storedConfig(workflows)
	if err != nil {
		t.Fatal(err)
	}
	team := mustFind(t, c, "mid").Steps[0]
	if team.Workflows[0] != "lib/sheets/sheet@v1" || team.Channels[1][1] != "sheet" {
		t.Errorf("team %+v", team)
	}
	if _, ok := team.MemberPositions["sheet"]; !ok || len(team.MemberPositions) != 1 {
		t.Errorf("positions %+v", team.MemberPositions)
	}
	var view workflowView
	if code := send(t, http.MethodGet, srv.URL+"/api/workflows/lib%2Fsheets%2Fsheet/versions/1", "", "", &view); code != http.StatusOK || view.Name != "lib/sheets/sheet" {
		t.Errorf("read by grouped name: %d %+v", code, view)
	}
	var refused map[string]string
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/mid/rename", "", `{"name":"ecc/v2"}`, &refused); code != http.StatusUnprocessableEntity {
		t.Errorf("a version-like segment: %d", code)
	}
}

func mustFind(t *testing.T, c Config, name string) pipeline.Definition {
	t.Helper()
	d, ok := c.find(name, 0)
	if !ok {
		t.Fatalf("workflow %q is missing", name)
	}
	return d
}

func TestDeleteRemovesEveryVersionUnlessTheWorkflowIsUsed(t *testing.T) {
	srv, _, _, workflows := serverWith(t, nestedConfig, nil)
	var refused struct {
		Error     string   `json:"error"`
		Workflows []string `json:"workflows"`
	}
	// mid runs leaf@v1 as a team member, and top reaches it through mid.
	if code := send(t, http.MethodDelete, srv.URL+"/api/workflows/leaf", "", "", &refused); code != http.StatusConflict {
		t.Fatalf("status %d", code)
	}
	if strings.Join(refused.Workflows, ",") != "mid,top" || !strings.Contains(refused.Error, "mid, top") {
		t.Fatalf("refused %+v", refused)
	}
	if code := send(t, http.MethodDelete, srv.URL+"/api/workflows/mid", "", "", &refused); code != http.StatusConflict || strings.Join(refused.Workflows, ",") != "top" {
		t.Fatalf("mid: %d %+v", code, refused)
	}
	if code, _ := call(t, http.MethodDelete, srv.URL+"/api/workflows/gone", ""); code != http.StatusNotFound {
		t.Errorf("unknown workflow %d", code)
	}
	// Only the newest version counts: a new version of mid without the member frees leaf.
	for _, step := range []struct{ name, body string }{
		{"top", `{"steps":[{"id":"a","service":"svc"}]}`},
		{"mid", `{"steps":[{"id":"a","service":"svc"}]}`},
	} {
		if code := send(t, http.MethodPost, srv.URL+"/api/workflows/"+step.name+"/versions", "", step.body, nil); code != http.StatusCreated {
			t.Fatalf("%s: %d", step.name, code)
		}
	}
	if code, body := call(t, http.MethodDelete, srv.URL+"/api/workflows/leaf", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, body)
	}
	c, err := storedConfig(workflows)
	if err != nil {
		t.Fatalf("the configuration no longer loads: %v", err)
	}
	if _, ok := c.find("leaf", 0); ok || len(c.versions("leaf")) != 0 {
		t.Error("a version of leaf is left")
	}
	// The older version of mid still names the deleted workflow and still loads.
	if old, ok := c.find("mid", 1); !ok || old.Steps[0].Workflows[0] != "leaf@v1" {
		t.Errorf("mid v1 %+v", old)
	}
}

func TestStartRunWritesMetadata(t *testing.T) {
	temperature := 0.2
	agents := func(_ context.Context, ref string) (AgentSettings, error) {
		switch ref {
		case "demo/coder@v1":
			return AgentSettings{
				Model: "qwen3.5-9b-mlx", Effort: "low", Temperature: &temperature, Tools: []string{"factory/send_message"}, MaxRetries: 10,
				Skills: map[string]string{"git": "v3"}, SkillScriptTimeoutSeconds: 300,
			}, nil
		case "demo/triage@v1":
			return AgentSettings{}, errors.New("the agent registry does not know this agent")
		}
		return AgentSettings{Model: "plain", Tools: []string{}}, nil
	}
	srv, runner, store, _ := serverWith(t, nestedConfig, agents)
	var out struct {
		TraceID string `json:"trace_id"`
	}
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/top/runs", "flo", "task", &out); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	id := "runs/" + out.TraceID + "/metadata.json"
	var meta struct {
		SchemaVersion int               `json:"schema_version"`
		RunID         string            `json:"run_id"`
		Workflow      WorkflowVersion   `json:"workflow"`
		Workflows     []WorkflowVersion `json:"workflows"`
		Started       string            `json:"started"`
		StartedBy     string            `json:"started_by"`
		InputArtifact string            `json:"input_artifact"`
		Agents        map[string]map[string]any
		Finished      *string `json:"finished"`
		Status        *string `json:"status"`
	}
	if err := json.Unmarshal(store.data[id], &meta); err != nil {
		t.Fatalf("%v: %s", err, store.data[id])
	}
	if meta.SchemaVersion != 1 || meta.RunID != out.TraceID || meta.Workflow != (WorkflowVersion{"top", 1}) || meta.StartedBy != "flo" || meta.InputArtifact != "runs/"+out.TraceID+"/input" {
		t.Fatalf("metadata %s", store.data[id])
	}
	if _, err := time.Parse(time.RFC3339, meta.Started); err != nil || meta.Finished != nil || meta.Status != nil {
		t.Fatalf("times %s", store.data[id])
	}
	// The nested versions are the pinned ones: leaf@v1, although leaf has a v2.
	if len(meta.Workflows) != 2 || meta.Workflows[0] != (WorkflowVersion{"mid", 1}) || meta.Workflows[1] != (WorkflowVersion{"leaf", 1}) {
		t.Fatalf("workflows %+v", meta.Workflows)
	}
	if len(meta.Agents) != 4 || meta.Agents["demo/writer@v2"] != nil {
		t.Fatalf("agents %v", meta.Agents)
	}
	coder, _ := json.Marshal(meta.Agents["demo/coder@v1"])
	if string(coder) != `{"effort":"low","max_retries":10,"model":"qwen3.5-9b-mlx","skill_script_timeout_seconds":300,"skills":{"git":"v3"},"temperature":0.2,"tools":["factory/send_message"]}` {
		t.Errorf("coder %s", coder)
	}
	// Temperature is left out when the agent sets none.
	lead, _ := json.Marshal(meta.Agents["demo/lead@v1"])
	if string(lead) != `{"effort":"","max_retries":0,"model":"plain","tools":[]}` {
		t.Errorf("lead %s", lead)
	}
	unread, _ := json.Marshal(meta.Agents["demo/triage@v1"])
	if string(unread) != `{"error":"the agent registry does not know this agent"}` {
		t.Errorf("unreadable agent %s", unread)
	}
	if m := store.meta[id]; m.Creator != "flo" || m.TraceID != out.TraceID {
		t.Errorf("blob metadata %+v", m)
	}
	if in := runner.started[0]; in.Depth != 0 || in.Prefix != "runs/"+out.TraceID {
		t.Errorf("input %+v", in)
	}

	// The pipeline's activity completes the same object at the end of the run.
	a := &pipeline.Activities{Store: store}
	if err := a.FinishRun(context.Background(), pipeline.FinishRunRequest{TraceID: out.TraceID, Metadata: id, Status: pipeline.RunFailed, Error: `step "ask" failed`}); err != nil {
		t.Fatal(err)
	}
	var done map[string]any
	_ = json.Unmarshal(store.data[id], &done)
	if done["status"] != "failed" || done["error"] != `step "ask" failed` || done["started"] != meta.Started || done["run_id"] != out.TraceID {
		t.Fatalf("finished metadata %s", store.data[id])
	}
	if _, err := time.Parse(time.RFC3339, done["finished"].(string)); err != nil {
		t.Fatalf("finished %v", done["finished"])
	}
	if agents := done["agents"].(map[string]any); len(agents) != 4 || agents["demo/coder@v1"].(map[string]any)["temperature"] != 0.2 {
		t.Errorf("agents after the end %v", done["agents"])
	}
}

func TestStartRunWithoutARegistryListsTheAgentsWithAnError(t *testing.T) {
	srv, _, store, _ := serverWith(t, nestedConfig, nil)
	var out struct {
		TraceID string `json:"trace_id"`
	}
	if code := send(t, http.MethodPost, srv.URL+"/api/workflows/leaf/runs?version=1", "", "task", &out); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	data := string(store.data["runs/"+out.TraceID+"/metadata.json"])
	if !strings.Contains(data, `"demo/writer@v1": {`) || !strings.Contains(data, `"error": "the agent registry is not configured"`) ||
		!strings.Contains(data, `"workflows": []`) || !strings.Contains(data, `"started_by": "orchestrator"`) || strings.Contains(data, "writer@v2") {
		t.Fatalf("metadata %s", data)
	}
}

func TestAgentRegistryReadsTheSettingsOfAnAgent(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents/demo/coder/v1":
			_, _ = w.Write([]byte(`{"files":{"agent.json":{"description":"Writes code.","models":["first","second"],"effort":"low","temperature":0,"tools":["factory/send_message"],"max_retries":10,"skills":["git@v3"],"skill_script_timeout_seconds":300}}}`))
		case "/skills/git/v3":
			_, _ = w.Write([]byte(`{"files":{"SKILL.md":"---\nname: git\ndescription: Git procedures\n---\n"}}`))
		case "/agents/plain/v2":
			_, _ = w.Write([]byte(`{"files":{"agent.json":{"models":["only"]}}}`))
		case "/agents/missing-skill/v1":
			_, _ = w.Write([]byte(`{"files":{"agent.json":{"skills":["gone@v9"]}}}`))
		case "/agents/broken/v1":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer registry.Close()
	t.Setenv("AGENT_REGISTRY_URL", registry.URL+"/")
	t.Setenv("SKILL_REGISTRY_URL", registry.URL+"/")
	agents := newAgentRegistry(http.DefaultClient)
	ctx := context.Background()
	coder, err := agents.settings(ctx, "demo/coder@v1")
	if err != nil {
		t.Fatal(err)
	}
	// A temperature of 0 is set, so it is recorded.
	data, _ := json.Marshal(coder)
	if string(data) != `{"model":"first","effort":"low","temperature":0,"tools":["factory/send_message"],"max_retries":10,"skills":{"git":"v3"},"skill_script_timeout_seconds":300}` {
		t.Errorf("coder %s", data)
	}
	plain, err := agents.settings(ctx, "plain@v2")
	data, _ = json.Marshal(plain)
	if err != nil || string(data) != `{"model":"only","effort":"","tools":[],"max_retries":0}` {
		t.Errorf("plain %s %v", data, err)
	}
	for ref, want := range map[string]string{"gone@v1": "does not know", "broken@v1": "500", "unpinned": "not pinned"} {
		if _, err = agents.settings(ctx, ref); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", ref, err)
		}
	}
	if _, err = agents.settings(ctx, "missing-skill@v1"); err == nil || !strings.Contains(err.Error(), `does not know "gone@v9"`) {
		t.Errorf("missing skill: %v", err)
	}
	if got := agentDescriber(agents, slog.New(slog.DiscardHandler))(ctx, "demo/coder@v1"); got != "Writes code." {
		t.Errorf("description %q", got)
	}
	registry.Close()
	if _, err = agents.settings(ctx, "demo/coder@v1"); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("closed registry: %v", err)
	}
}

func TestStoredStatusAnswersAFinishedRunWithoutHistory(t *testing.T) {
	cases := map[string]string{"completed": "Completed", "failed": "Failed", "cancelled": "Canceled"}
	for stored, want := range cases {
		blobs := newMemStore()
		s := &server{blobs: blobs}
		trace := "11111111-1111-4111-8111-111111111111"
		meta := `{"workflow":{"name":"demo","version":2},"started":"2026-01-01T00:00:00Z","finished":"2026-01-01T00:00:05Z","status":"` + stored + `","error":"boom"}`
		if err := blobs.Put(context.Background(), "runs/"+trace+"/metadata.json", []byte(meta), pipeline.Meta{}); err != nil {
			t.Fatal(err)
		}
		st, ok := s.storedStatus(context.Background(), trace)
		if !ok || st.Status != want || st.Workflow != "demo" || st.DurationMS != 5000 || !st.DurationKnown || st.Error != "boom" {
			t.Errorf("%s: got %+v %v", stored, st, ok)
		}
	}
	blobs := newMemStore()
	s := &server{blobs: blobs}
	_ = blobs.Put(context.Background(), "runs/x/metadata.json", []byte(`{"workflow":{"name":"demo","version":1}}`), pipeline.Meta{})
	if _, ok := s.storedStatus(context.Background(), "x"); ok {
		t.Error("metadata without an outcome gives no status")
	}
	if _, ok := s.storedStatus(context.Background(), "missing"); ok {
		t.Error("a run without metadata gives no status")
	}
}
