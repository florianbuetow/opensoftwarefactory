package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

import "opensoftwarefactory/orchestrator/pipeline"

type fakeRunner struct {
	started  []pipeline.Input
	pending  []PendingDecision
	decided  map[string]pipeline.Decision
	decision error

	sessions       []WaitingSession
	teams          []RunningTeam
	completed      map[string]pipeline.Completion
	completion     error
	failedSessions map[string]pipeline.SessionFailure
	failSessionErr error
	removed        []string
}

func (f *fakeRunner) Teams(context.Context) ([]RunningTeam, error) { return f.teams, nil }

func (f *fakeRunner) Sessions(context.Context) ([]WaitingSession, error) { return f.sessions, nil }

func (f *fakeRunner) Complete(_ context.Context, id string, c pipeline.Completion) error {
	if f.completion != nil {
		return f.completion
	}
	if f.completed == nil {
		f.completed = map[string]pipeline.Completion{}
	}
	f.completed[id] = c
	return nil
}

func (f *fakeRunner) FailSession(_ context.Context, id string, failure pipeline.SessionFailure) error {
	if f.failSessionErr != nil {
		return f.failSessionErr
	}
	if f.failedSessions == nil {
		f.failedSessions = map[string]pipeline.SessionFailure{}
	}
	f.failedSessions[id] = failure
	return nil
}

func (f *fakeRunner) Start(_ context.Context, in *pipeline.Input) error {
	f.started = append(f.started, *in)
	return nil
}

func (f *fakeRunner) StartExecution(_ context.Context, in *pipeline.Input) (string, error) {
	f.started = append(f.started, *in)
	return "execution-" + in.TraceID, nil
}

func (f *fakeRunner) Status(_ context.Context, trace string) (RunStatus, error) {
	for i := range f.started {
		in := &f.started[i]
		if in.TraceID == trace {
			return RunStatus{TraceID: trace, Status: "Completed", Artifacts: map[string]string{"a": pipeline.ArtifactID(in.Prefix, "a")}}, nil
		}
	}
	return RunStatus{}, errRunNotFound
}

func (f *fakeRunner) Pending(context.Context) ([]PendingDecision, error) { return f.pending, nil }

func (f *fakeRunner) Remove(_ context.Context, trace string) error {
	f.removed = append(f.removed, trace)
	return nil
}

func (f *fakeRunner) Decide(_ context.Context, id string, d pipeline.Decision) error {
	if f.decision != nil {
		return f.decision
	}
	if f.decided == nil {
		f.decided = map[string]pipeline.Decision{}
	}
	f.decided[id] = d
	return nil
}

type memStore struct {
	data map[string][]byte
	meta map[string]pipeline.Meta
}

// memStore lists and deletes the objects of a run, as the blobstore does, and fakeRunner
// reports the execution it started, as the Temporal runner does.
var (
	_ runObjects       = (*memStore)(nil)
	_ executionStarter = (*fakeRunner)(nil)
)

func newMemStore() *memStore {
	return &memStore{data: map[string][]byte{}, meta: map[string]pipeline.Meta{}}
}

func (m *memStore) Put(_ context.Context, id string, d []byte, meta pipeline.Meta) error {
	m.data[id] = d
	m.meta[id] = meta
	return nil
}

// PutIfAbsent stores an object only when none has the id, as the blobstore's create-only write.
func (m *memStore) PutIfAbsent(_ context.Context, id string, d []byte, meta pipeline.Meta) (string, error) {
	if _, ok := m.data[id]; ok {
		return "", pipeline.ErrPreconditionFailed
	}
	m.data[id] = d
	m.meta[id] = meta
	return "1", nil
}

func (m *memStore) Delete(_ context.Context, id string) error {
	delete(m.data, id)
	delete(m.meta, id)
	return nil
}

func (m *memStore) TraceObjects(_ context.Context, trace string) ([]string, error) {
	var ids []string
	for id, meta := range m.meta {
		if meta.TraceID == trace {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (m *memStore) Get(_ context.Context, id string) ([]byte, error) {
	d, ok := m.data[id]
	if !ok {
		return nil, pipeline.ErrNotFound
	}
	return d, nil
}

// Values the tests of this package share.
const (
	// sampleWorkflow is the name of the workflow in sampleConfig.
	sampleWorkflow = "sample"
	// testUser is the person who starts runs, decides gates, and completes sessions.
	testUser = "flo"
	// chatStep is the ID of the interactive step of the session tests.
	chatStep = "chat"
)

const sampleConfig = `workflows:
  - name: sample
    steps:
      - id: draft
        service: draft
      - id: review
        service: review
        depends_on: [draft]
`

// List returns the immediate child directories and object IDs of a prefix.
func (m *memStore) List(_ context.Context, prefix string) (dirs, objects []string, err error) {
	seen := map[string]bool{}
	dirs, objects = []string{}, []string{}
	for id := range m.data {
		rest, ok := strings.CutPrefix(id, prefix+"/")
		if !ok {
			continue
		}
		if dir, _, nested := strings.Cut(rest, "/"); nested {
			if !seen[dir] {
				seen[dir] = true
				dirs = append(dirs, dir)
			}
			continue
		}
		objects = append(objects, id)
	}
	sort.Strings(dirs)
	sort.Strings(objects)
	return dirs, objects, nil
}

// storeWorkflows stores the YAML configuration in a blobstore of its own and opens the
// workflow store on it.
func storeWorkflows(t *testing.T, config string) *workflowStore {
	t.Helper()
	c, err := parseConfig([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	s := &workflowStore{blobs: newMemStore(), config: Config{Workflows: []pipeline.Definition{}}}
	if err = s.update(context.Background(), func(next *Config) error { *next = c; return nil }); err != nil {
		t.Fatal(err)
	}
	return s
}

// storedConfig reads the workflows back from the blobstore, as the orchestrator does when it
// starts.
func storedConfig(s *workflowStore) (Config, error) {
	reopened, err := openWorkflowStore(context.Background(), s.blobs)
	if err != nil {
		return Config{}, err
	}
	return reopened.config, nil
}

// workflowObjects returns every stored workflow object, in ID order, so a test can tell
// whether anything was written.
func workflowObjects(s *workflowStore) ([]byte, error) {
	m, ok := s.blobs.(*memStore)
	if !ok {
		return nil, fmt.Errorf("the workflow store keeps its objects in a %T, not in a memStore", s.blobs)
	}
	ids := make([]string, 0, len(m.data))
	for id := range m.data {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []byte
	for _, id := range ids {
		out = append(out, id+"\n"...)
		out = append(out, m.data[id]...)
	}
	return out, nil
}

func newTestServer(t *testing.T) (*httptest.Server, *fakeRunner, *memStore, *workflowStore) {
	t.Helper()
	workflows := storeWorkflows(t, sampleConfig)
	runner, store := &fakeRunner{}, newMemStore()
	s := &server{workflows: workflows, blobs: store, runner: runner, logger: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return srv, runner, store, workflows
}

func call(t *testing.T, method, url, body string) (status int, answer string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// closeBody closes the body of a response and reports a failed Close as a test error.
func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}
}

func TestStartRunAssignsTraceIDAndStoresInput(t *testing.T) {
	srv, runner, store, _ := newTestServer(t)
	code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs", "hello")
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
	var out map[string]string
	_ = json.Unmarshal([]byte(body), &out)
	if uuid.Validate(out["trace_id"]) != nil {
		t.Fatalf("trace id %q", out["trace_id"])
	}
	in := runner.started[0]
	if len(runner.started) != 1 || in.TraceID != out["trace_id"] || in.Definition.Name != sampleWorkflow || in.Prefix != "runs/"+in.TraceID {
		t.Fatalf("started %+v", runner.started)
	}
	if string(store.data[in.Inputs[0].Artifact]) != "hello" {
		t.Fatalf("input not stored: %v", store.data)
	}
	if m := store.meta[in.Inputs[0].Artifact]; m.Creator != "orchestrator" || m.TraceID != in.TraceID {
		t.Fatalf("input metadata %+v", m)
	}

	code, _ = call(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs", "again")
	if code != http.StatusAccepted || runner.started[1].TraceID == runner.started[0].TraceID {
		t.Fatal("second run reused the trace id")
	}

	code, body = call(t, http.MethodGet, srv.URL+"/api/runs/"+out["trace_id"], "")
	if code != http.StatusOK || !strings.Contains(body, "Completed") {
		t.Fatalf("status %d %s", code, body)
	}
	if code, _ = call(t, http.MethodGet, srv.URL+"/api/runs/"+uuid.NewString(), ""); code != http.StatusNotFound {
		t.Fatalf("unknown run %d", code)
	}
	if code, _ = call(t, http.MethodPost, srv.URL+"/api/workflows/missing/runs", "x"); code != http.StatusNotFound {
		t.Fatalf("unknown workflow %d", code)
	}
}

func TestSaveVersionWritesYAML(t *testing.T) {
	srv, _, _, workflows := newTestServer(t)
	def := `{"name":"two","steps":[{"id":"a","service":"agent","agent":"intake@v1","position":{"x":10,"y":20}},{"id":"g","service":"gate","question":"ok?","depends_on":["a"]},{"id":"b","service":"svc","depends_on":["g"],"timeout":"30s"}]}`
	if code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/two/versions", def); code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	saved, err := storedConfig(workflows)
	if err != nil {
		t.Fatal(err)
	}
	two, ok := saved.find("two", 1)
	if !ok || len(saved.Workflows) != 2 {
		t.Fatalf("saved %+v", saved)
	}
	steps := two.Steps
	if steps[0].Agent != "intake@v1" || steps[1].Question != "ok?" || steps[2].DependsOn[0] != "g" || steps[0].Position.X != 10 {
		t.Fatalf("saved %+v", saved)
	}
	code, body := call(t, http.MethodGet, srv.URL+"/api/workflows", "")
	if code != http.StatusOK || !strings.Contains(body, `"name":"two"`) || !strings.Contains(body, `"agent":"intake@v1"`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestSaveVersionRejectsInvalidGraph(t *testing.T) {
	srv, _, _, workflows := newTestServer(t)
	before, _ := workflowObjects(workflows)
	cyc := `{"steps":[{"id":"a","service":"s","depends_on":["b"]},{"id":"b","service":"s","depends_on":["a"]}]}`
	if code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/w/versions", cyc); code != http.StatusUnprocessableEntity || !strings.Contains(body, "cycle") {
		t.Fatalf("%d %s", code, body)
	}
	noAgent := `{"steps":[{"id":"a","service":"agent"}]}`
	if code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/w/versions", noAgent); code != http.StatusUnprocessableEntity || !strings.Contains(body, "agent") {
		t.Fatalf("agent step without agent accepted: %d %s", code, body)
	}
	if code, _ := call(t, http.MethodPost, srv.URL+"/api/workflows/w/versions", `{"steps":[],"extra":1}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", code)
	}
	after, _ := workflowObjects(workflows)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid configuration overwrote config.yaml")
	}
}

func TestArtifactProxy(t *testing.T) {
	srv, _, store, _ := newTestServer(t)
	store.data["runs/x/a"] = []byte("result")
	if code, body := call(t, http.MethodGet, srv.URL+"/api/artifacts/runs/x/a", ""); code != http.StatusOK || body != "result" {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := call(t, http.MethodGet, srv.URL+"/api/artifacts/runs/x/missing", ""); code != http.StatusNotFound {
		t.Fatalf("missing artifact %d", code)
	}
}

func TestDecisions(t *testing.T) {
	srv, runner, _, _ := newTestServer(t)
	code, body := call(t, http.MethodGet, srv.URL+"/api/decisions", "")
	if code != http.StatusOK || !strings.Contains(body, `"decisions":[]`) {
		t.Fatalf("empty decisions %d %s", code, body)
	}
	runner.pending = []PendingDecision{{WorkflowID: "t/route", TraceID: "t", Workflow: "feature", PendingGate: pipeline.PendingGate{Step: "approve", Question: "ok?", Inputs: []pipeline.Item{{Source: "spec", Artifact: "runs/t/route/spec", Round: 1}}, Artifact: "runs/t/route/approve"}}}
	code, body = call(t, http.MethodGet, srv.URL+"/api/decisions", "")
	if code != http.StatusOK || !strings.Contains(body, `"workflow_id":"t/route"`) || !strings.Contains(body, `"question":"ok?"`) {
		t.Fatalf("decisions %d %s", code, body)
	}

	ok := `{"workflow_id":"t/route","step":"approve","decision":"approved","comment":"go","decided_by":"flo"}`
	if code, body = call(t, http.MethodPost, srv.URL+"/api/decisions", ok); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
	if d := runner.decided["t/route"]; d.Step != "approve" || d.Decision != pipeline.Approved || d.DecidedBy != testUser || d.Comment != "go" {
		t.Fatalf("decided %+v", runner.decided)
	}
	for name, bad := range map[string]string{
		"no workflow": `{"step":"approve","decision":"approved","decided_by":"flo"}`,
		"no step":     `{"workflow_id":"t","decision":"approved","decided_by":"flo"}`,
		"no person":   `{"workflow_id":"t","step":"approve","decision":"approved"}`,
		"bad verdict": `{"workflow_id":"t","step":"approve","decision":"maybe","decided_by":"flo"}`,
		"unknown key": `{"workflow_id":"t","step":"approve","decision":"approved","decided_by":"flo","extra":1}`,
	} {
		if code, _ = call(t, http.MethodPost, srv.URL+"/api/decisions", bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	runner.decision = errRunNotFound
	if code, _ = call(t, http.MethodPost, srv.URL+"/api/decisions", ok); code != http.StatusNotFound {
		t.Fatalf("finished workflow %d", code)
	}
}

func TestSessions(t *testing.T) {
	srv, runner, store, _ := newTestServer(t)
	code, body := call(t, http.MethodGet, srv.URL+"/api/sessions", "")
	if code != http.StatusOK || !strings.Contains(body, `"sessions":[]`) {
		t.Fatalf("empty sessions %d %s", code, body)
	}
	runner.sessions = []WaitingSession{{WorkflowID: "t/route", TraceID: "t", Workflow: "feature", PendingSession: pipeline.PendingSession{
		Step: chatStep, Agent: "triage@v1", Agents: []string{"triage@v1", "plain@v2"},
		Inputs: []pipeline.Item{{Source: "spec", Artifact: "runs/t/route/spec", Round: 1}}, Artifact: "runs/t/route/chat", Session: "user-sessions/t/route/chat",
		Skills: map[string]string{"git": "v3"}, SkillScriptTimeoutSeconds: 300,
		AgentSettings: map[string]pipeline.AgentExecutionSettings{"triage@v1": {Skills: map[string]string{"git": "v3"}, SkillScriptTimeoutSeconds: 300}},
	}}}
	code, body = call(t, http.MethodGet, srv.URL+"/api/sessions", "")
	want := `{"sessions":[{"workflow_id":"t/route","trace_id":"t","workflow":"feature","step":"chat","agent":"triage@v1","agents":["triage@v1","plain@v2"],"artifact":"runs/t/route/chat","session":"user-sessions/t/route/chat","inputs":[{"source":"spec","artifact":"runs/t/route/spec","round":1}]}]}`
	if code != http.StatusOK || strings.TrimSpace(body) != want {
		t.Fatalf("sessions %d %s", code, body)
	}

	store.data["user-sessions/t/route/chat/files/result.md"] = []byte("# Result")
	ok := `{"workflow_id":"t/route","step":"chat","file":"result.md","completed_by":"flo"}`
	if code, body = call(t, http.MethodPost, srv.URL+"/api/sessions/complete", ok); code != http.StatusAccepted || strings.TrimSpace(body) != ok {
		t.Fatalf("%d %s", code, body)
	}
	if c := runner.completed["t/route"]; c.Step != chatStep || c.File != "result.md" || c.CompletedBy != testUser {
		t.Fatalf("completed %+v", runner.completed)
	}
	for name, bad := range map[string]string{
		"no workflow": `{"step":"chat","file":"result.md","completed_by":"flo"}`,
		"no step":     `{"workflow_id":"t/route","file":"result.md","completed_by":"flo"}`,
		"no person":   `{"workflow_id":"t/route","step":"chat","file":"result.md"}`,
		"no file":     `{"workflow_id":"t/route","step":"chat","completed_by":"flo"}`,
		"bad type":    `{"workflow_id":"t/route","step":"chat","file":"result.yaml","completed_by":"flo"}`,
		"path":        `{"workflow_id":"t/route","step":"chat","file":"../result.md","completed_by":"flo"}`,
		"unknown key": `{"workflow_id":"t/route","step":"chat","file":"result.md","completed_by":"flo","extra":1}`,
	} {
		if code, _ = call(t, http.MethodPost, srv.URL+"/api/sessions/complete", bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	for name, missing := range map[string]string{
		"unknown step":     `{"workflow_id":"t/route","step":"other","file":"result.md","completed_by":"flo"}`,
		"unknown workflow": `{"workflow_id":"t","step":"chat","file":"result.md","completed_by":"flo"}`,
		"missing file":     `{"workflow_id":"t/route","step":"chat","file":"absent.md","completed_by":"flo"}`,
	} {
		if code, _ = call(t, http.MethodPost, srv.URL+"/api/sessions/complete", missing); code != http.StatusNotFound {
			t.Errorf("%s: %d", name, code)
		}
	}
	runner.completion = errRunNotFound
	if code, _ = call(t, http.MethodPost, srv.URL+"/api/sessions/complete", ok); code != http.StatusNotFound {
		t.Fatalf("finished workflow %d", code)
	}
}

func TestSessionChatUsesPinnedServerSettingsAndStopsOnlyForMissingDevPod(t *testing.T) {
	var received []SessionChatRequest
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req SessionChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		received = append(received, req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Message {
		case "needs pod":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"The workflow needs a devpod step before this step to run a skill script.","type":"SkillScriptNeedsDevPod"}`))
		case "ordinary failure":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"bad input","type":"InvalidAgent"}`))
		default:
			_, _ = w.Write([]byte(`{"reply":"done"}`))
		}
	}))
	defer agent.Close()

	workflows := storeWorkflows(t, sampleConfig)
	runner, store := &fakeRunner{}, newMemStore()
	runner.sessions = []WaitingSession{{WorkflowID: "workflow-1", TraceID: "trace-1", Workflow: "feature", PendingSession: pipeline.PendingSession{
		Step: chatStep, Pass: 2, Agent: "triage@v1", Agents: []string{"triage@v1", "plain@v2"}, Session: "user-sessions/trace-1/chat.pass-2", DevPod: "pod-1",
		AgentSettings: map[string]pipeline.AgentExecutionSettings{"plain@v2": {Skills: map[string]string{"git": "v3"}, SkillScriptTimeoutSeconds: 300}},
	}}}
	s := &server{workflows: workflows, blobs: store, runner: runner, logger: slog.New(slog.DiscardHandler), agentURL: agent.URL, httpClient: http.DefaultClient}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	post := func(message string) (int, string) {
		body := fmt.Sprintf(`{"workflow_id":"workflow-1","trace_id":"forged","step_id":"chat","session":"user-sessions/trace-1/chat.pass-2","tab":"plain","agent":"plain@v2","message":%q,"created_by":"ada","dev_pod":"forged","skills":{"evil":"v9"},"skill_script_timeout_seconds":1}`, message)
		return call(t, http.MethodPost, srv.URL+"/api/sessions/chat", body)
	}
	if code, body := post("hello"); code != http.StatusOK || strings.TrimSpace(body) != `{"reply":"done"}` {
		t.Fatalf("success %d %s", code, body)
	}
	got := received[0]
	if got.TraceID != "trace-1" || got.StepID != chatStep || got.Session != "user-sessions/trace-1/chat.pass-2" || got.DevPod != "pod-1" || got.Skills["git"] != "v3" || len(got.Skills) != 1 || got.SkillScriptTimeoutSeconds != 300 {
		t.Fatalf("agent request %+v", got)
	}
	runner.sessions[0].AgentSettings = nil
	if code, _ := post("legacy session"); code != http.StatusOK || received[1].Skills != nil || received[1].SkillScriptTimeoutSeconds != 0 {
		t.Fatalf("legacy session status %d request %+v", code, received[1])
	}
	if code, _ := post("ordinary failure"); code != http.StatusUnprocessableEntity || len(runner.failedSessions) != 0 {
		t.Fatalf("ordinary failure status %d signals %+v", code, runner.failedSessions)
	}
	if code, _ := post("needs pod"); code != http.StatusUnprocessableEntity {
		t.Fatalf("needs pod status %d", code)
	}
	if failed := runner.failedSessions["workflow-1"]; failed.Step != chatStep || failed.Type != "SkillScriptNeedsDevPod" || !strings.Contains(failed.Error, "needs a devpod step") {
		t.Fatalf("workflow failure %+v", failed)
	}
	badAgent := `{"workflow_id":"workflow-1","step_id":"chat","session":"user-sessions/trace-1/chat.pass-2","tab":"x","agent":"other@v1","message":"hello","created_by":"ada"}`
	if code, _ := call(t, http.MethodPost, srv.URL+"/api/sessions/chat", badAgent); code != http.StatusUnprocessableEntity || len(received) != 4 {
		t.Fatalf("unknown agent status %d calls %d", code, len(received))
	}
}

func TestInteractiveStepRoundTripsThroughTheConfiguration(t *testing.T) {
	srv, _, _, workflows := newTestServer(t)
	def := `{"steps":[{"id":"chat","service":"interactive","agent":"triage@v1","agents":["plain@v2","folder/helper@v3"]},{"id":"use","service":"agent","agent":"intake@v1","depends_on":["chat"]}]}`
	if code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/talk/versions", def); code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	saved, err := storedConfig(workflows)
	if err != nil {
		t.Fatal(err)
	}
	talk, _ := saved.find("talk", 0)
	chat := talk.Steps[0]
	if chat.Service != pipeline.InteractiveService || chat.Agent != "triage@v1" || strings.Join(chat.Agents, ",") != "plain@v2,folder/helper@v3" {
		t.Fatalf("saved %+v", chat)
	}
	code, body := call(t, http.MethodGet, srv.URL+"/api/workflows", "")
	if code != http.StatusOK || !strings.Contains(body, `"agents":["plain@v2","folder/helper@v3"]`) {
		t.Fatalf("%d %s", code, body)
	}
	bad := `{"steps":[{"id":"chat","service":"interactive","agent":"triage@v1","agents":["plain"]}]}`
	if code, body = call(t, http.MethodPost, srv.URL+"/api/workflows/talk/versions", bad); code != http.StatusUnprocessableEntity || !strings.Contains(body, "plain") {
		t.Fatalf("unpinned agent accepted: %d %s", code, body)
	}
}

func TestConfigRejectsUnknownFields(t *testing.T) {
	if _, err := parseConfig([]byte("workflows: []\nextra: 1\n")); err == nil {
		t.Fatal("accepted unknown field")
	}
	if c, err := parseConfig([]byte("")); err != nil || len(c.Workflows) != 0 {
		t.Fatalf("empty config: %v", err)
	}
}

func TestSavingMergeAndBatchSteps(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	ok := `{"steps":[{"id":"a","service":"x"},{"id":"b","service":"x"},
		{"id":"collect","service":"merge","depends_on":["a","b"],"capacity":4,"input_timeout":30},
		{"id":"gather","service":"batch","depends_on":["collect"],"size":2},
		{"id":"after","service":"x","depends_on":["gather"]}]}`
	code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/nodes/versions", ok)
	if code != http.StatusCreated || !strings.Contains(body, `"service":"merge","depends_on":["a","b"],"capacity":4,"input_timeout":30`) ||
		!strings.Contains(body, `"service":"batch","depends_on":["collect"],"size":2`) {
		t.Fatalf("%d %s", code, body)
	}
	for name, def := range map[string]string{
		"needs a size of 1 or more": `{"steps":[{"id":"a","service":"x"},{"id":"g","service":"batch","depends_on":["a"]}]}`,
		"cannot depend on merge":    `{"steps":[{"id":"a","service":"x"},{"id":"m","service":"merge","depends_on":["a"]},{"id":"r","service":"route","depends_on":["m"]}]}`,
		"at least one source":       `{"steps":[{"id":"m","service":"merge"}]}`,
	} {
		if code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/bad/versions", def); code != http.StatusUnprocessableEntity || !strings.Contains(body, name) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}

func TestStartRunFromExistingArtifact(t *testing.T) {
	srv, runner, store, _ := newTestServer(t)
	store.data["tasks/example.md"] = []byte("predefined task")
	code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs?input=tasks/example.md", "")
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
	if got := string(store.data[runner.started[0].Inputs[0].Artifact]); got != "predefined task" {
		t.Fatalf("input copied as %q", got)
	}
	if code, _ = call(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs?input=tasks/missing.md", ""); code != http.StatusNotFound {
		t.Fatalf("missing input %d", code)
	}
	if code, _ = call(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs?input=../x", ""); code != http.StatusBadRequest {
		t.Fatalf("invalid input id %d", code)
	}
}

func TestStartRunWritesRunRecord(t *testing.T) {
	srv, runner, store, _ := newTestServer(t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/workflows/sample/runs", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Creator", testUser)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	trace := runner.started[0].TraceID
	var rec RunRecord
	if err = json.Unmarshal(store.data["runs/"+trace+"/run.json"], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Workflow != sampleWorkflow || rec.StartedBy != testUser || rec.TraceID != trace || rec.Started.IsZero() || rec.InputArtifact != "runs/"+trace+"/input" {
		t.Fatalf("record %+v", rec)
	}
	if m := store.meta["runs/"+trace+"/input"]; m.Creator != testUser {
		t.Fatalf("input creator %q", m.Creator)
	}
}

func TestDeleteRunRemovesHistoryAndArtifacts(t *testing.T) {
	srv, runner, store, _ := newTestServer(t)
	trace := "5a13a94f-78a1-4b72-be78-88278806b594"
	other := "9e8bd793-9647-4d2d-8d2d-2d75fe44d000"
	for _, id := range []string{"runs/" + trace + "/input", "runs/" + trace + "/intake", "runs/" + trace + "/route/fix"} {
		_ = store.Put(context.Background(), id, []byte("x"), pipeline.Meta{TraceID: trace})
	}
	_ = store.Put(context.Background(), "runs/"+other+"/input", []byte("y"), pipeline.Meta{TraceID: other})
	_ = store.Put(context.Background(), "notes/keep", []byte("z"), pipeline.Meta{TraceID: trace})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, srv.URL+"/api/runs/"+trace, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(runner.removed) != 1 || runner.removed[0] != trace {
		t.Fatalf("history not removed: %v", runner.removed)
	}
	for id := range store.data {
		if strings.HasPrefix(id, "runs/"+trace+"/") {
			t.Errorf("artifact left: %s", id)
		}
	}
	if _, ok := store.data["runs/"+other+"/input"]; !ok {
		t.Error("another run's artifact was removed")
	}
	if _, ok := store.data["notes/keep"]; !ok {
		t.Error("an object outside runs/ was removed")
	}
	req, err = http.NewRequestWithContext(context.Background(), http.MethodDelete, srv.URL+"/api/runs/not-a-uuid", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err = http.DefaultClient.Do(req); err == nil {
		closeBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("bad trace: %d", resp.StatusCode)
		}
	}
}
