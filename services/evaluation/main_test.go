package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	runA = "11111111-1111-4111-8111-111111111111"
	runB = "22222222-2222-4222-8222-222222222222"
	runC = "33333333-3333-4333-8333-333333333333"
	runD = "44444444-4444-4444-8444-444444444444"
)

type fixture struct {
	runs              map[string]bool
	objects           map[string][]byte
	revisions         map[string]string
	statuses          map[string]any
	mu                sync.Mutex
	nextRevision      int
	failPriceList     bool
	failExecutionList bool
}

func newFixture(t *testing.T) (f *fixture, blob, orch *httptest.Server) {
	t.Helper()
	f = &fixture{runs: map[string]bool{runA: true, runB: true, runC: true, runD: true}, objects: map[string][]byte{}, revisions: map[string]string{}, statuses: map[string]any{}}
	started := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	put := func(id string, v any) { f.objects[id], _ = json.Marshal(v) }
	put("runs/"+runA+"/run.json", map[string]any{"trace_id": runA, "workflow": "feature", "started": started.Add(2 * time.Hour), "input_artifact": "runs/" + runA + "/input", "distributed_trace_id": strings.Repeat("a", 32)})
	put("runs/"+runA+"/code.run.json", map[string]any{"agent": "sdlc/implementation@v2"})
	put("runs/"+runA+"/route/review.run.json", map[string]any{"agent": "review/code@v1"})
	put("runs/"+runB+"/run.json", map[string]any{"trace_id": runB, "workflow": "small-fix", "started": started.Add(time.Hour), "input_artifact": "runs/" + runB + "/input"})
	put("runs/"+runB+"/fix.run.json", map[string]any{"agent": "sdlc/implementation@v1"})
	put("runs/"+runC+"/run.json", map[string]any{"trace_id": runC, "workflow": "feature", "started": started, "input_artifact": "runs/" + runC + "/input"})
	put("runs/"+runC+"/legacy.run.json", map[string]any{"agent": "legacy/worker@v1"})
	put("runs/"+runD+"/run.json", map[string]any{"trace_id": runD, "workflow": "feature", "started": started.Add(-time.Hour), "input_artifact": "runs/" + runD + "/input"})
	f.statuses[runA] = map[string]any{"status": "Completed", "started": started.Add(2*time.Hour + time.Second), "finished": started.Add(2*time.Hour + 5*time.Second), "duration_ms": 4000, "duration_known": true}
	f.statuses[runB] = map[string]any{"status": "Failed", "started": started.Add(time.Hour), "finished": started.Add(time.Hour), "duration_ms": 0, "duration_known": true}
	blob = httptest.NewServer(http.HandlerFunc(f.blobHandler))
	t.Cleanup(blob.Close)
	orch = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/runs/")
		if id == runC {
			http.NotFound(w, r)
			return
		}
		v, ok := f.statuses[id]
		if !ok {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, 200, v)
	}))
	t.Cleanup(orch.Close)
	return f, blob, orch
}

func (f *fixture) blobHandler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/list/runs":
		var dirs []string
		for id := range f.runs {
			dirs = append(dirs, id)
		}
		sort.Strings(dirs)
		writeJSON(w, 200, map[string]any{"directories": dirs, "objects": []any{}})
	case r.Method == http.MethodGet && r.URL.Path == "/list/evaluation-sets":
		var objects []map[string]any
		for id := range f.objects {
			if strings.HasPrefix(id, "evaluation-sets/") {
				objects = append(objects, map[string]any{"id": id})
			}
		}
		writeJSON(w, 200, map[string]any{"directories": []any{}, "objects": objects})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/list/"):
		prefix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/list/"), "/") + "/"
		if f.failPriceList && prefix == pricePrefix+"/" {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if f.failExecutionList && prefix == "workflow-executions/" {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		var objects []map[string]any
		for id := range f.objects {
			if strings.HasPrefix(id, prefix) && !strings.Contains(strings.TrimPrefix(id, prefix), "/") {
				objects = append(objects, map[string]any{"id": id})
			}
		}
		writeJSON(w, 200, map[string]any{"directories": []any{}, "objects": objects})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/trace/"):
		id := strings.TrimPrefix(r.URL.Path, "/trace/")
		var objects []map[string]any
		for key := range f.objects {
			if strings.HasPrefix(key, "runs/"+id+"/") {
				objects = append(objects, map[string]any{"id": key})
			}
		}
		writeJSON(w, 200, map[string]any{"objects": objects})
	case strings.HasPrefix(r.URL.Path, "/blobs/"):
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		if r.Method == http.MethodPut {
			if r.Header.Get("If-None-Match") == "*" {
				if _, exists := f.objects[id]; exists {
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
			}
			if match := strings.Trim(r.Header.Get("If-Match"), `"`); match != "" && match != f.revisions[id] {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			f.objects[id], _ = io.ReadAll(r.Body)
			f.nextRevision++
			f.revisions[id] = fmt.Sprintf("r%d", f.nextRevision)
			w.Header().Set("ETag", `"`+f.revisions[id]+`"`)
			w.WriteHeader(204)
			return
		}
		data, ok := f.objects[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("X-Created", "2026-11-01T12:00:00Z")
		if f.revisions[id] == "" {
			f.nextRevision++
			f.revisions[id] = fmt.Sprintf("r%d", f.nextRevision)
		}
		w.Header().Set("ETag", `"`+f.revisions[id]+`"`)
		_, _ = w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func testEvaluation(t *testing.T, blob, orch string) *httptest.Server {
	t.Helper()
	s := &service{blobs: blob, orchestrator: orch, http: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.projectorLoop(ctx)
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return srv
}

func request(t *testing.T, method, url, body string) (status int, data []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Creator", "tester")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, res.Body)
	data, err = io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, data
}

// get sends a GET request that ends with the test.
func get(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// closeBody closes what a test has read and reports a failed Close.
func closeBody(t *testing.T, body io.Closer) {
	t.Helper()
	if err := body.Close(); err != nil {
		t.Error(err)
	}
}

func TestRunFiltersPaginationAndIncompleteEvidence(t *testing.T) {
	_, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)
	code, data := request(t, "GET", srv.URL+"/api/runs?workflow=feature&agent=review/code&started_from=2026-11-01T00:00:00Z&started_to=2026-11-02T00:00:00Z&duration_min_ms=4000&duration_max_ms=6000", "")
	if code != 200 {
		t.Fatalf("%d %s", code, data)
	}
	var page struct {
		Runs       []RunSnapshot `json:"runs"`
		Incomplete int           `json:"incomplete_runs"`
	}
	_ = json.Unmarshal(data, &page)
	wantStarted := time.Date(2026, 11, 1, 14, 0, 1, 0, time.UTC)
	if len(page.Runs) != 1 || page.Runs[0].TraceID != runA || !page.Runs[0].AgentsComplete || !page.Runs[0].Started.Equal(wantStarted) {
		t.Fatalf("runs %+v", page)
	}
	code, data = request(t, "GET", srv.URL+"/api/runs?duration_min_ms=0", "")
	_ = json.Unmarshal(data, &page)
	if code != 200 || len(page.Runs) != 2 || page.Incomplete != 1 {
		t.Fatalf("duration known filter %d %+v", code, page)
	}
	code, data = request(t, "GET", srv.URL+"/api/runs?limit=1", "")
	var first struct {
		Next string        `json:"next_cursor"`
		Runs []RunSnapshot `json:"runs"`
	}
	_ = json.Unmarshal(data, &first)
	if code != 200 || len(first.Runs) != 1 || first.Next == "" {
		t.Fatalf("first page %d %s", code, data)
	}
	code, data = request(t, "GET", srv.URL+"/api/runs?limit=1&cursor="+first.Next, "")
	var second struct {
		Runs []RunSnapshot `json:"runs"`
	}
	_ = json.Unmarshal(data, &second)
	if code != 200 || len(second.Runs) != 1 || second.Runs[0].TraceID == first.Runs[0].TraceID {
		t.Fatalf("second page %d %s", code, data)
	}
}

func TestMissingHistoryIsExpectedAndWarningsAreScoped(t *testing.T) {
	_, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)
	code, data := request(t, http.MethodGet, srv.URL+"/api/runs", "")
	var page struct {
		Runs       []RunSnapshot `json:"runs"`
		Incomplete int           `json:"incomplete_runs"`
		History    int           `json:"history_unavailable"`
		Omitted    int           `json:"history_unavailable_omitted"`
	}
	_ = json.Unmarshal(data, &page)
	if code != http.StatusOK || page.Incomplete != 1 || page.History != 1 || page.Omitted != 0 {
		t.Fatalf("unfiltered page %d %+v", code, page)
	}
	found := false
	for _, run := range page.Runs {
		if run.TraceID == runC {
			found = run.HistoryUnavailable && run.IndexError == "" && run.AgentsComplete && len(run.Agents) == 1
		}
	}
	if !found {
		t.Fatalf("legacy run not retained: %+v", page.Runs)
	}
	code, data = request(t, http.MethodGet, srv.URL+"/api/runs?status=Completed", "")
	_ = json.Unmarshal(data, &page)
	if code != http.StatusOK || page.History != 1 || page.Omitted != 1 {
		t.Fatalf("status-filtered page %d %+v", code, page)
	}
	code, data = request(t, http.MethodGet, srv.URL+"/api/runs?workflow=small-fix&status=Completed", "")
	_ = json.Unmarshal(data, &page)
	if code != http.StatusOK || page.History != 0 || page.Omitted != 0 {
		t.Fatalf("scoped page %d %+v", code, page)
	}
}

func TestEvaluationSetPersistsSnapshotsAndFlagsDeletedSources(t *testing.T) {
	f, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)
	code, data := request(t, "POST", srv.URL+"/api/sets", `{"name":"Release candidates","description":"same tasks","run_ids":["`+runA+`","`+runB+`"]}`)
	if code != 201 {
		t.Fatalf("create %d %s", code, data)
	}
	var set EvaluationSet
	_ = json.Unmarshal(data, &set)
	if set.Revision != 1 || len(set.Runs) != 2 || set.Runs[1].DurationMS != 0 || !set.Runs[1].DurationKnown {
		t.Fatalf("set %+v", set)
	}
	f.mu.Lock()
	delete(f.runs, runA)
	f.mu.Unlock()
	// A new service instance proves the set is in the blobstore rather than process memory.
	srv2 := testEvaluation(t, blob.URL, orch.URL)
	code, data = request(t, "GET", srv2.URL+"/api/sets/"+set.ID, "")
	_ = json.Unmarshal(data, &set)
	if code != 200 || set.Runs[0].SourcePresent || set.Runs[0].Workflow != "feature" {
		t.Fatalf("deleted source snapshot %d %+v", code, set.Runs[0])
	}
	code, _ = request(t, "PUT", srv2.URL+"/api/sets/"+set.ID, `{"name":"Release candidates","run_ids":["`+runA+`"],"expected_revision":99}`)
	if code != 409 {
		t.Fatalf("stale update %d", code)
	}
	code, data = request(t, "PUT", srv2.URL+"/api/sets/"+set.ID, `{"name":"Release candidates","run_ids":["`+runA+`"],"expected_revision":1}`)
	_ = json.Unmarshal(data, &set)
	if code != 200 || set.Revision != 2 || len(set.Runs) != 1 || set.Runs[0].SourcePresent {
		t.Fatalf("update %d %+v", code, set)
	}
}

func TestConcurrentSetNamesAreSerialized(t *testing.T) {
	_, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)
	start := make(chan struct{})
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			code, _ := request(t, http.MethodPost, srv.URL+"/api/sets", `{"name":"Same set","run_ids":["`+runA+`"]}`)
			codes <- code
		}()
	}
	close(start)
	got := []int{<-codes, <-codes}
	sort.Ints(got)
	if got[0] != http.StatusCreated || got[1] != http.StatusConflict {
		t.Fatalf("concurrent create statuses %v", got)
	}
}

func TestClassifyNamesEveryKindOfRunObject(t *testing.T) {
	cases := map[string][2]string{
		"runs/t/input":                            {"input", "input"},
		"runs/t/run.json":                         {"run.json", "run record"},
		"runs/t/metadata.json":                    {"metadata.json", "run metadata"},
		"runs/t/code":                             {"steps/code/output", "step output"},
		"runs/t/build/team":                       {"steps/build/team/output", "step output"},
		"runs/t/code.run.json":                    {"steps/code/agent-record.json", "agent record"},
		"runs/t/code.attempt-2.run.json":          {"steps/code/attempt-2.agent-record.json", "agent record"},
		"runs/t/review.1":                         {"steps/review/decision-round-1", "gate decision"},
		"runs/t/_evidence/code/attempt-1/a.md":    {"steps/code/evidence/attempt-1/a.md", "prompt evidence"},
		"user-sessions/t/refine/chats/a.json":     {"sessions/refine/chats/a.json", "session chat"},
		"user-sessions/t/refine/files/input-x.md": {"sessions/refine/files/input-x.md", "session input"},
		"user-sessions/t/refine/completed.json":   {"sessions/refine/completed.json", "session completion"},
		"teams/t/build/team/channels/a--b/a.json": {"teams/build/team/channels/a--b/a.json", "channel queue"},
		"teams/t/team/members/coder.json":         {"teams/team/members/coder.json", "member history"},
		"teams/t/team/finished.json":              {"teams/team/finished.json", "team result"},
		"evaluation-sets/unrelated.json":          {"other/evaluation-sets/unrelated.json", "other"},
	}
	for id, want := range cases {
		file, role, description, _ := classify("t", id)
		if file != want[0] || role != want[1] || description == "" {
			t.Errorf("%s: got %q %q %q, want %q %q", id, file, role, description, want[0], want[1])
		}
	}
	if role, _ := refine("step output", "x", []byte(`{"decision":"approved"}`)); role != "gate decision" {
		t.Errorf("a decision record is a gate decision, got %q", role)
	}
	if role, _ := refine("step output", "x", []byte(`{"team":"teams/t/team","files":[]}`)); role != "team result" {
		t.Errorf("a team summary is a team result, got %q", role)
	}
	if got := withExtension("steps/code/output", []byte("# Title")); got != "steps/code/output.md" {
		t.Errorf("text gets .md, got %q", got)
	}
	if got := withExtension("steps/review/decision-round-1", []byte(`{"a":1}`)); got != "steps/review/decision-round-1.json" {
		t.Errorf("JSON gets .json, got %q", got)
	}
	if got := withExtension("teams/team/files/main.py", []byte("x")); got != "teams/team/files/main.py" {
		t.Errorf("an extension is kept, got %q", got)
	}
}

// A step after a merge or batch node runs once per forwarded message. Each pass is
// stored as <step>.pass-<n> and exported into its own folder under the step.
func TestClassifyPutsEveryPassIntoItsOwnFolder(t *testing.T) {
	cases := map[string][3]string{
		"runs/t/compare.pass-1":                          {"steps/compare/pass-1/output", "step output", "pass 1"},
		"runs/t/compare.pass-2":                          {"steps/compare/pass-2/output", "step output", "pass 2"},
		"runs/t/compare.pass-2.run.json":                 {"steps/compare/pass-2/agent-record.json", "agent record", "pass 2"},
		"runs/t/compare.pass-2.attempt-3.run.json":       {"steps/compare/pass-2/attempt-3.agent-record.json", "agent record", "pass 2"},
		"runs/t/build/compare.pass-12":                   {"steps/build/compare/pass-12/output", "step output", "pass 12"},
		"runs/t/review.pass-2.1":                         {"steps/review/pass-2/decision-round-1", "gate decision", "pass 2"},
		"runs/t/_evidence/compare.pass-2/attempt-1/a.md": {"steps/compare/pass-2/evidence/attempt-1/a.md", "prompt evidence", ""},
	}
	for id, want := range cases {
		file, role, description, step := classify("t", id)
		if file != want[0] || role != want[1] || !strings.Contains(description, want[2]) {
			t.Errorf("%s: got %q %q %q, want %q %q naming %q", id, file, role, description, want[0], want[1], want[2])
		}
		if strings.Contains(step, ".pass-") {
			t.Errorf("%s: the step %q still carries its pass", id, step)
		}
	}
	// A step that is not after a merge or batch keeps its description and names no pass.
	if _, _, description, step := classify("t", "runs/t/code"); strings.Contains(description, "pass") || step != "code" {
		t.Errorf("a plain step: %q %q", description, step)
	}
	if got := withExtension("steps/compare/pass-2/output", []byte("# Title")); got != "steps/compare/pass-2/output.md" {
		t.Errorf("a pass output gets its extension by content, got %q", got)
	}
	if !strings.Contains(exportReadme, "steps/<step>/pass-<n>/") {
		t.Error("the README does not describe the pass folders")
	}
}

func TestExportIsAZipWithReadmeSetAndRunFolders(t *testing.T) {
	_, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)
	code, data := request(t, "POST", srv.URL+"/api/sets", `{"name":"Base line","run_ids":["`+runA+`"]}`)
	if code != 201 {
		t.Fatalf("%d %s", code, data)
	}
	var set EvaluationSet
	if err := json.Unmarshal(data, &set); err != nil {
		t.Fatal(err)
	}
	res := get(t, srv.URL+"/api/sets/"+set.ID+"/export")
	defer closeBody(t, res.Body)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/zip" || !strings.Contains(res.Header.Get("Content-Disposition"), `filename="Base-line.zip"`) {
		t.Fatalf("%d %v", res.StatusCode, res.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]*zip.File{}
	for _, f := range zr.File {
		names[f.Name] = f
	}
	for _, want := range []string{"Base-line/README.md", "Base-line/set.json"} {
		if names[want] == nil {
			t.Errorf("missing %s in %v", want, len(names))
		}
	}
	var manifest *zip.File
	for name, f := range names {
		if strings.HasPrefix(name, "Base-line/runs/01-") && strings.HasSuffix(name, "/manifest.json") {
			manifest = f
		}
		if strings.Contains(name, "EXPORT-FAILED") {
			t.Errorf("the export failed: %s", name)
		}
	}
	if manifest == nil {
		t.Fatalf("no manifest among %d files", len(names))
	}
	rc, err := manifest.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, rc)
	var m exportManifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.TraceID != runA || len(m.Files) == 0 {
		t.Fatalf("manifest %+v", m)
	}
	dir := strings.TrimSuffix(manifest.Name, "manifest.json")
	for _, f := range m.Files {
		if names[dir+f.Path] == nil || f.Role == "" || f.Object == "" {
			t.Errorf("manifest entry %+v has no file", f)
		}
	}
	if code, _ := request(t, "GET", srv.URL+"/api/sets/missing/export", ""); code != 404 {
		t.Errorf("an unknown set is 404, got %d", code)
	}
}

func TestListSetsValidationAndExportEdgeCases(t *testing.T) {
	f, blob, orch := newFixture(t)
	srv := testEvaluation(t, blob.URL, orch.URL)

	// Health check.
	if code, _ := request(t, http.MethodGet, srv.URL+"/healthz", ""); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}

	// listRuns error paths.
	for _, query := range []string{
		"started_from=bad",
		"started_to=bad",
		"duration_min_ms=-1",
		"duration_max_ms=abc",
		"duration_min_ms=100&duration_max_ms=50",
		"cursor=!!!",
		"limit=0",
	} {
		if code, _ := request(t, http.MethodGet, srv.URL+"/api/runs?"+query, ""); code != http.StatusBadRequest {
			t.Fatalf("listRuns(%s) = %d", query, code)
		}
	}

	// Create two sets, remove one run source, and list sets.
	code, data := request(t, http.MethodPost, srv.URL+"/api/sets", `{"name":"Set One","run_ids":["`+runA+`","`+runB+`"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create set 1 = %d %s", code, data)
	}
	var set1 EvaluationSet
	_ = json.Unmarshal(data, &set1)

	code, data = request(t, http.MethodPost, srv.URL+"/api/sets", `{"name":"Set Two","run_ids":["`+runB+`"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create set 2 = %d %s", code, data)
	}
	var set2 EvaluationSet
	_ = json.Unmarshal(data, &set2)

	f.mu.Lock()
	delete(f.runs, runA)
	f.mu.Unlock()

	code, data = request(t, http.MethodGet, srv.URL+"/api/sets", "")
	if code != http.StatusOK {
		t.Fatalf("list sets = %d %s", code, data)
	}
	var listed struct {
		Sets []setSummary `json:"sets"`
	}
	if err := json.Unmarshal(data, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sets) != 2 {
		t.Fatalf("sets count = %d", len(listed.Sets))
	}
	missingCounts := map[string]int{}
	for _, summary := range listed.Sets {
		missingCounts[summary.ID] = summary.MissingRunCount
	}
	if missingCounts[set1.ID] != 1 || missingCounts[set2.ID] != 0 {
		t.Fatalf("missing counts = %+v", missingCounts)
	}

	// getSet / createSet / updateSet error paths.
	if code, _ := request(t, http.MethodGet, srv.URL+"/api/sets/not-a-uuid", ""); code != http.StatusNotFound {
		t.Fatalf("getSet invalid id = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/sets", `{"name":"","run_ids":[]}`); code != http.StatusBadRequest {
		t.Fatalf("empty name = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/sets", `{"name":"Bad UUID","run_ids":["not-a-uuid"]}`); code != http.StatusBadRequest {
		t.Fatalf("bad run uuid = %d", code)
	}
	if code, _ := request(t, http.MethodPost, srv.URL+"/api/sets", `{"name":"Missing run","run_ids":["`+runA+`"]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("missing run = %d", code)
	}
	if code, _ := request(t, http.MethodPut, srv.URL+"/api/sets/not-a-uuid", `{"name":"X","run_ids":[]}`); code != http.StatusBadRequest {
		t.Fatalf("update invalid id = %d", code)
	}
	if code, _ := request(t, http.MethodPut, srv.URL+"/api/sets/99999999-9999-4999-8999-999999999999", `{"name":"X","run_ids":[]}`); code != http.StatusNotFound {
		t.Fatalf("update missing set = %d", code)
	}
	if code, _ := request(t, http.MethodPut, srv.URL+"/api/sets/"+set2.ID, `{"name":"Set One","run_ids":[]}`); code != http.StatusConflict {
		t.Fatalf("update duplicate name = %d", code)
	}

	// Export set with a deleted source run includes manifest note.
	res := get(t, srv.URL+"/api/sets/"+set1.ID+"/export")
	defer closeBody(t, res.Body)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("PK")) {
		t.Fatalf("export with deleted source = %d", res.StatusCode)
	}

	// Export helper edge cases: safeName, classify, refine.
	if safeName("---") != "set" {
		t.Fatalf("safeName = %q", safeName("---"))
	}
	for id, wantRole := range map[string]string{
		"user-sessions/t/s/files/note.md": "session file",
		"user-sessions/t/s/other":         "session data",
		"teams/t/s/team.json":             "team manifest",
		"teams/t/s/files/input-1.md":      "team input",
		"teams/t/s/files/out.md":          "team file",
		"teams/t/s/other":                 "team data",
	} {
		if _, role, _, _ := classify("t", id); role != wantRole {
			t.Errorf("classify(%s) role = %q, want %q", id, role, wantRole)
		}
	}
	if role, _ := refine(roleStepOutput, "d", []byte(`{"other":1}`)); role != roleStepOutput {
		t.Fatalf("refine non-matching JSON = %q", role)
	}
	if role, _ := refine(roleStepOutput, "d", []byte(`plain text`)); role != roleStepOutput {
		t.Fatalf("refine plain text = %q", role)
	}
}
