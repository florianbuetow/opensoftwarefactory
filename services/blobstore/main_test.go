package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (srv *httptest.Server, root string) {
	t.Helper()
	root = t.TempDir()
	h, err := newHandler(root, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv = httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, root
}

func TestPutGetDelete(t *testing.T) {
	srv, root := newTestServer(t)
	url := srv.URL + "/blobs/runs/abc/top"

	if got := do(t, http.MethodGet, url, "", nil); got.StatusCode != http.StatusNotFound {
		t.Fatalf("GET missing: %d", got.StatusCode)
	}
	put := do(t, http.MethodPut, url, "hello", nil)
	if put.StatusCode != http.StatusNoContent || put.Header.Get("ETag") == "" {
		t.Fatalf("PUT: %d ETag %q", put.StatusCode, put.Header.Get("ETag"))
	}
	data, err := os.ReadFile(filepath.Clean(filepath.Join(root, "objects", "runs", "abc", "top")))
	if err != nil || string(data) != "hello" {
		t.Fatalf("file content %q: %v", data, err)
	}
	if _, err = os.Stat(filepath.Join(root, "meta", "runs", "abc", "top.json")); err != nil {
		t.Fatalf("metadata file: %v", err)
	}
	resp := do(t, http.MethodGet, url, "", nil)
	if resp.StatusCode != http.StatusOK || string(resp.Body) != "hello" {
		t.Fatalf("GET: %d %q", resp.StatusCode, resp.Body)
	}
	if resp.Header.Get(headerCreated) == "" {
		t.Fatal("GET lacks created header")
	}
	if resp.Header.Get("ETag") != put.Header.Get("ETag") {
		t.Fatalf("GET ETag %q, PUT ETag %q", resp.Header.Get("ETag"), put.Header.Get("ETag"))
	}
	if got := do(t, http.MethodDelete, url, "", nil); got.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %d", got.StatusCode)
	}
	if got := do(t, http.MethodDelete, url, "", nil); got.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE again: %d", got.StatusCode)
	}
	if _, err = os.Stat(filepath.Join(root, "meta", "runs", "abc", "top.json")); !os.IsNotExist(err) {
		t.Fatalf("metadata survived delete: %v", err)
	}
}

func TestMetadataFromHeaders(t *testing.T) {
	srv, _ := newTestServer(t)
	headers := map[string]string{headerCreator: "agent:intake@v1", headerTrace: "t-1", headerEstimatedInput: "12", headerEstimatedOutput: "3", headerProcessingTime: "456", headerEstimationMethod: "chars_div_4"}
	if got := do(t, http.MethodPut, srv.URL+"/blobs/runs/t-1/intake", "x", headers); got.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT: %d", got.StatusCode)
	}
	resp := do(t, http.MethodGet, srv.URL+"/meta/runs/t-1/intake", "", nil)
	var m Meta
	if err := json.Unmarshal(resp.Body, &m); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("meta: %d %v", resp.StatusCode, err)
	}
	if m.ID != "runs/t-1/intake" || m.Size != 1 || m.Creator != "agent:intake@v1" || m.TraceID != "t-1" || m.Created.IsZero() || m.EstimatedInputTokens == nil || *m.EstimatedInputTokens != 12 || m.EstimatedOutputTokens == nil || *m.EstimatedOutputTokens != 3 || m.ProcessingTimeMS == nil || *m.ProcessingTimeMS != 456 || m.EstimationMethod != "chars_div_4" {
		t.Fatalf("meta %+v", m)
	}
	get := do(t, http.MethodGet, srv.URL+"/blobs/runs/t-1/intake", "", nil)
	if get.Header.Get(headerCreator) != "agent:intake@v1" || get.Header.Get(headerTrace) != "t-1" || get.Header.Get(headerEstimatedInput) != "12" || get.Header.Get(headerEstimatedOutput) != "3" || get.Header.Get(headerProcessingTime) != "456" || get.Header.Get(headerEstimationMethod) != "chars_div_4" {
		t.Fatalf("GET headers %v", get.Header)
	}
	if got := do(t, http.MethodPut, srv.URL+"/blobs/runs/t-1/bad", "x", map[string]string{headerCreator: strings.Repeat("a", 300)}); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("long creator accepted: %d", got.StatusCode)
	}
	if got := do(t, http.MethodPut, srv.URL+"/blobs/runs/t-1/bad-metric", "x", map[string]string{headerEstimatedOutput: "1.5"}); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed estimate accepted: %d", got.StatusCode)
	}
	if got := do(t, http.MethodGet, srv.URL+"/meta/runs/t-1/missing", "", nil); got.StatusCode != http.StatusNotFound {
		t.Fatalf("missing meta: %d", got.StatusCode)
	}
}

func TestListShowsDirectoriesAndObjects(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, id := range []string{"agents/intake/v1/systemprompt.md", "agents/intake/v2/systemprompt.md", "agents/triage/v1/systemprompt.md", "policies/p1/v1/policy.md", "top-level"} {
		if got := do(t, http.MethodPut, srv.URL+"/blobs/"+id, id, nil); got.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s: %d", id, got.StatusCode)
		}
	}
	root := listing(t, srv.URL+"/list/")
	if strings.Join(root.Directories, ",") != "agents,policies" || len(root.Objects) != 1 || root.Objects[0].ID != "top-level" {
		t.Fatalf("root %+v", root)
	}
	agents := listing(t, srv.URL+"/list/agents")
	if agents.Prefix != "agents" || strings.Join(agents.Directories, ",") != "intake,triage" || len(agents.Objects) != 0 {
		t.Fatalf("agents %+v", agents)
	}
	v1 := listing(t, srv.URL+"/list/agents/intake/v1/")
	if len(v1.Objects) != 1 || v1.Objects[0].ID != "agents/intake/v1/systemprompt.md" || v1.Objects[0].Size != int64(len("agents/intake/v1/systemprompt.md")) {
		t.Fatalf("v1 %+v", v1)
	}
	if got := do(t, http.MethodGet, srv.URL+"/list/nowhere", "", nil); got.StatusCode != http.StatusNotFound {
		t.Fatalf("missing prefix: %d", got.StatusCode)
	}
	if got := do(t, http.MethodGet, srv.URL+"/list/../x", "", nil); got.StatusCode == http.StatusOK {
		t.Fatal("escaping prefix accepted")
	}
}

func TestDotPrefixedSkillPathsAreStoredAndListed(t *testing.T) {
	srv, root := newTestServer(t)
	ids := []string{"skills/git/v1/.config", "skills/git/v1/scripts/.helper"}
	for _, id := range ids {
		if got := do(t, http.MethodPut, srv.URL+"/blobs/"+id, id, nil); got.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s: %d", id, got.StatusCode)
		}
		got := do(t, http.MethodGet, srv.URL+"/blobs/"+id, "", nil)
		if got.StatusCode != http.StatusOK || string(got.Body) != id {
			t.Fatalf("GET %s: %d %q", id, got.StatusCode, got.Body)
		}
	}
	version := listing(t, srv.URL+"/list/skills/git/v1")
	if strings.Join(version.Directories, ",") != "scripts" || len(version.Objects) != 1 || version.Objects[0].ID != ids[0] {
		t.Fatalf("version listing %+v", version)
	}
	scripts := listing(t, srv.URL+"/list/skills/git/v1/scripts")
	if len(scripts.Objects) != 1 || scripts.Objects[0].ID != ids[1] {
		t.Fatalf("scripts listing %+v", scripts)
	}

	// A legacy interrupted upload has no metadata and must remain private.
	orphan := filepath.Join(root, "objects", "skills", "git", "v1", ".upload-orphan")
	if err := os.WriteFile(orphan, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	version = listing(t, srv.URL+"/list/skills/git/v1")
	for _, object := range version.Objects {
		if object.ID == "skills/git/v1/.upload-orphan" {
			t.Fatalf("internal upload exposed: %+v", version)
		}
	}
}

func TestValidIDAllowsSafeDotSegments(t *testing.T) {
	for _, id := range []string{"skills/git/v1/.config", "skills/git/v1/a..b", "skills/git/v1/scripts/.helper"} {
		if !validID(id) {
			t.Errorf("valid dot path rejected: %q", id)
		}
	}
	for _, id := range []string{"skills/./v1/file", "skills/../v1/file", "skills//v1/file"} {
		if validID(id) {
			t.Errorf("unsafe dot path accepted: %q", id)
		}
	}
}

func TestSkillFileMetadataDoesNotConflictWithNestedJSONPaths(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, id := range []string{"skills/git/v1/config", "skills/git/v1/config.json/values.txt"} {
		if resp := do(t, http.MethodPut, srv.URL+"/blobs/"+id, "value", map[string]string{"X-Creator": "skill-author"}); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s: %d", id, resp.StatusCode)
		}
		resp := do(t, http.MethodGet, srv.URL+"/blobs/"+id, "", nil)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Creator") != "skill-author" {
			t.Fatalf("GET %s: %d %v", id, resp.StatusCode, resp.Header)
		}
	}
}

func TestTraceListsObjectsInCreationOrder(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, id := range []string{"runs/t-9/input", "runs/t-9/intake", "runs/t-9/triage"} {
		if got := do(t, http.MethodPut, srv.URL+"/blobs/"+id, "x", map[string]string{headerTrace: "t-9", headerCreator: "c"}); got.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s: %d", id, got.StatusCode)
		}
	}
	do(t, http.MethodPut, srv.URL+"/blobs/runs/t-8/input", "x", map[string]string{headerTrace: "t-8"})
	resp := do(t, http.MethodGet, srv.URL+"/trace/t-9", "", nil)
	var out struct {
		TraceID string `json:"trace_id"`
		Objects []Meta `json:"objects"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("trace: %d %v", resp.StatusCode, err)
	}
	if out.TraceID != "t-9" || len(out.Objects) != 3 {
		t.Fatalf("trace %+v", out)
	}
	for i := 1; i < len(out.Objects); i++ {
		if out.Objects[i].Created.Before(out.Objects[i-1].Created) {
			t.Fatalf("not in creation order: %+v", out.Objects)
		}
	}
	if out.Objects[0].ID != "runs/t-9/input" || out.Objects[2].ID != "runs/t-9/triage" {
		t.Fatalf("order %+v", out.Objects)
	}
}

func TestTraceIndexMatchesScanAcrossPrefixes(t *testing.T) {
	srv, root := newTestServer(t)
	ids := []string{"decisions/d-1/record.json", "memory/notes/a/b/c/note.md", "runs/t-1/input", "top-level"}
	for _, id := range ids {
		if got := do(t, http.MethodPut, srv.URL+"/blobs/"+id, "x", map[string]string{headerTrace: "t-1", headerCreator: "c"}); got.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s: %d", id, got.StatusCode)
		}
	}
	do(t, http.MethodPut, srv.URL+"/blobs/runs/t-2/input", "x", map[string]string{headerTrace: "t-2"})
	do(t, http.MethodPut, srv.URL+"/blobs/runs/t-1/untraced", "x", nil)
	got := traced(t, srv.URL, "t-1")
	if strings.Join(objectIDs(got), ",") != strings.Join(ids, ",") {
		t.Fatalf("trace %+v", got)
	}
	// The index answers exactly what the scan over all metadata answered before.
	scanned, err := (&store{meta: filepath.Join(root, "meta")}).scanTrace("t-1")
	if err != nil || len(scanned) != len(got) {
		t.Fatalf("scan: %v %+v", err, scanned)
	}
	for _, want := range scanned {
		found := false
		for _, m := range got {
			found = found || (m.ID == want.ID && m.Size == want.Size && m.Creator == want.Creator && m.TraceID == want.TraceID && m.Created.Equal(want.Created))
		}
		if !found {
			t.Fatalf("index lacks %+v", want)
		}
	}
	if got := traced(t, srv.URL, "t-0"); len(got) != 0 {
		t.Fatalf("unknown trace %+v", got)
	}
	resp := do(t, http.MethodGet, srv.URL+"/trace/t-0", "", nil)
	if strings.TrimSpace(string(resp.Body)) != `{"objects":[],"trace_id":"t-0"}` {
		t.Fatalf("empty trace body %s", resp.Body)
	}
}

func TestOverwriteMovesTraceEntry(t *testing.T) {
	srv, root := newTestServer(t)
	s := store{traces: filepath.Join(root, "traces")}
	url := srv.URL + "/blobs/runs/shared/output"
	do(t, http.MethodPut, url, "x", map[string]string{headerTrace: "t-old"})
	do(t, http.MethodPut, srv.URL+"/blobs/runs/shared/other", "x", map[string]string{headerTrace: "t-old"})
	if got := do(t, http.MethodPut, url, "y", map[string]string{headerTrace: "t-new"}); got.StatusCode != http.StatusNoContent {
		t.Fatalf("overwrite: %d", got.StatusCode)
	}
	if got := objectIDs(traced(t, srv.URL, "t-old")); strings.Join(got, ",") != "runs/shared/other" {
		t.Fatalf("old trace %v", got)
	}
	if got := objectIDs(traced(t, srv.URL, "t-new")); strings.Join(got, ",") != "runs/shared/output" {
		t.Fatalf("new trace %v", got)
	}
	if _, err := os.Stat(s.traceEntry("t-old", "runs/shared/output")); !os.IsNotExist(err) {
		t.Fatalf("old entry survived overwrite: %v", err)
	}
	// Overwriting without a trace ID leaves the object in no trace.
	do(t, http.MethodPut, url, "z", nil)
	if got := traced(t, srv.URL, "t-new"); len(got) != 0 {
		t.Fatalf("trace after untraced overwrite %+v", got)
	}
	if _, err := os.Stat(s.traceDir("t-new")); !os.IsNotExist(err) {
		t.Fatalf("empty trace directory survived: %v", err)
	}
}

func TestDeleteRemovesTraceEntry(t *testing.T) {
	srv, root := newTestServer(t)
	s := store{traces: filepath.Join(root, "traces")}
	for _, id := range []string{"runs/t-3/input", "runs/t-3/intake"} {
		do(t, http.MethodPut, srv.URL+"/blobs/"+id, "x", map[string]string{headerTrace: "t-3"})
	}
	if got := do(t, http.MethodDelete, srv.URL+"/blobs/runs/t-3/input", "", nil); got.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %d", got.StatusCode)
	}
	if got := objectIDs(traced(t, srv.URL, "t-3")); strings.Join(got, ",") != "runs/t-3/intake" {
		t.Fatalf("trace after delete %v", got)
	}
	if _, err := os.Stat(s.traceEntry("t-3", "runs/t-3/input")); !os.IsNotExist(err) {
		t.Fatalf("entry survived delete: %v", err)
	}
	do(t, http.MethodDelete, srv.URL+"/blobs/runs/t-3/intake", "", nil)
	if _, err := os.Stat(s.traceDir("t-3")); !os.IsNotExist(err) {
		t.Fatalf("empty trace directory survived: %v", err)
	}
}

func TestBuildIndexesExistingObjects(t *testing.T) {
	srv, root := newTestServer(t)
	ids := []string{"agents/intake/v1/systemprompt.md", "runs/t-4/input", "runs/t-4/intake"}
	for _, id := range ids {
		do(t, http.MethodPut, srv.URL+"/blobs/"+id, "x", map[string]string{headerTrace: "t-4"})
	}
	do(t, http.MethodPut, srv.URL+"/blobs/runs/t-5/input", "x", nil)
	srv.Close()
	// A store written before the index existed has no index at all; a build that was
	// interrupted has entries but no marker. Both are completed by the next start.
	for _, interrupted := range []bool{false, true} {
		s := store{traces: filepath.Join(root, "traces")}
		if err := os.Remove(filepath.Join(s.traces, traceIndexMarker)); err != nil {
			t.Fatal(err)
		}
		if interrupted {
			if err := os.Remove(s.traceEntry("t-4", "runs/t-4/input")); err != nil {
				t.Fatal(err)
			}
		} else if err := os.RemoveAll(s.traces); err != nil {
			t.Fatal(err)
		}
		h, err := newHandler(root, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		again := httptest.NewServer(h)
		// Before the build completes the scan answers; afterwards the index does.
		if got := objectIDs(traced(t, again.URL, "t-4")); strings.Join(got, ",") != strings.Join(ids, ",") {
			t.Fatalf("trace during build (interrupted %v) %v", interrupted, got)
		}
		waitForTraceIndex(t, root)
		for _, id := range ids {
			if data, _ := os.ReadFile(s.traceEntry("t-4", id)); string(data) != `"`+id+`"` {
				t.Fatalf("entry of %s (interrupted %v): %q", id, interrupted, data)
			}
		}
		if got := objectIDs(traced(t, again.URL, "t-4")); strings.Join(got, ",") != strings.Join(ids, ",") {
			t.Fatalf("trace after build (interrupted %v) %v", interrupted, got)
		}
		again.Close()
	}
}

func TestTraceSkipsAndRemovesStaleEntry(t *testing.T) {
	srv, root := newTestServer(t)
	s := store{traces: filepath.Join(root, "traces")}
	for _, id := range []string{"runs/t-6/gone", "runs/t-6/kept", "runs/t-6/moved"} {
		do(t, http.MethodPut, srv.URL+"/blobs/"+id, "x", map[string]string{headerTrace: "t-6"})
	}
	// One object vanishes and one changes its trace behind the index's back.
	if err := os.Remove(filepath.Join(root, "meta", "runs", "t-6", "gone.json")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(root, "meta", "runs", "t-6", "moved.json"), Meta{ID: "runs/t-6/moved", TraceID: "t-7"}); err != nil {
		t.Fatal(err)
	}
	if got := objectIDs(traced(t, srv.URL, "t-6")); strings.Join(got, ",") != "runs/t-6/kept" {
		t.Fatalf("trace with stale entries %v", got)
	}
	for _, id := range []string{"runs/t-6/gone", "runs/t-6/moved"} {
		if _, err := os.Stat(s.traceEntry("t-6", id)); !os.IsNotExist(err) {
			t.Fatalf("stale entry of %s survived: %v", id, err)
		}
	}
	if _, err := os.Stat(s.traceEntry("t-6", "runs/t-6/kept")); err != nil {
		t.Fatalf("live entry removed: %v", err)
	}
}

func TestConcurrentWritesKeepEveryTraceEntry(t *testing.T) {
	srv, _ := newTestServer(t)
	const writers = 32
	request := func(method, id string) {
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+"/blobs/"+id, strings.NewReader("x"))
		if err != nil {
			t.Errorf("%s %s: %v", method, id, err)
			return
		}
		req.Header.Set(headerTrace, "t-8")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("%s %s: %v", method, id, err)
			return
		}
		if err = resp.Body.Close(); err != nil {
			t.Errorf("%s %s: %v", method, id, err)
		}
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s %s: %d", method, id, resp.StatusCode)
		}
	}
	each := func(method, prefix string) {
		var wg sync.WaitGroup
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				request(method, prefix+strconv.Itoa(i))
			}()
		}
		wg.Wait()
	}
	each(http.MethodPut, "runs/t-8/first-")
	if got := traced(t, srv.URL, "t-8"); len(got) != writers {
		t.Fatalf("after concurrent PUTs: %d of %d", len(got), writers)
	}
	// Deleting one half while writing another keeps exactly the written half.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); each(http.MethodDelete, "runs/t-8/first-") }()
	go func() { defer wg.Done(); each(http.MethodPut, "other/second-") }()
	wg.Wait()
	got := objectIDs(traced(t, srv.URL, "t-8"))
	if len(got) != writers {
		t.Fatalf("after concurrent PUTs and DELETEs: %v", got)
	}
	for _, id := range got {
		if !strings.HasPrefix(id, "other/second-") {
			t.Fatalf("deleted object still traced: %v", got)
		}
	}
}

func TestTraceIndexIsNotAnObject(t *testing.T) {
	srv, root := newTestServer(t)
	do(t, http.MethodPut, srv.URL+"/blobs/runs/t-9/input", "x", map[string]string{headerTrace: "t-9"})
	entry := (&store{traces: filepath.Join(root, "traces")}).traceEntry("t-9", "runs/t-9/input")
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("index entry: %v", err)
	}
	top := listing(t, srv.URL+"/list/")
	if strings.Join(top.Directories, ",") != "runs" || len(top.Objects) != 0 {
		t.Fatalf("root %+v", top)
	}
	rel, _ := filepath.Rel(root, entry)
	for _, path := range []string{"/list/traces", "/blobs/" + filepath.ToSlash(rel), "/meta/" + filepath.ToSlash(rel), "/blobs/traces/" + traceIndexMarker} {
		if got := do(t, http.MethodGet, srv.URL+path, "", nil); got.StatusCode == http.StatusOK {
			t.Fatalf("index reachable through %s", path)
		}
	}
}

// BenchmarkTrace looks up a small trace in a store that holds many other objects.
func BenchmarkTrace(b *testing.B) {
	root := b.TempDir()
	h, err := newHandler(root, slog.New(slog.DiscardHandler))
	if err != nil {
		b.Fatal(err)
	}
	for i := range 2000 {
		trace := "other-" + strconv.Itoa(i/10)
		if i < 10 {
			trace = "wanted"
		}
		req := httptest.NewRequest(http.MethodPut, "/blobs/runs/"+trace+"/step-"+strconv.Itoa(i), strings.NewReader("x"))
		req.Header.Set(headerTrace, trace)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/trace/wanted", http.NoBody))
		if rec.Code != http.StatusOK {
			b.Fatalf("trace: %d", rec.Code)
		}
	}
}

func TestRejectsEscapingIDs(t *testing.T) {
	s := store{objects: t.TempDir(), meta: t.TempDir()}
	for _, id := range []string{"", "..", "a/../b", "a//b", ".hidden", "a/.x", "a\\b", strings.Repeat("a", 600)} {
		if _, _, ok := s.paths(id); ok {
			t.Errorf("accepted %q", id)
		}
	}
	if _, _, ok := s.paths("runs/0b7e/step-1.json"); !ok {
		t.Error("rejected valid id")
	}
	if _, _, ok := s.paths("skills/git/v1/.hidden"); !ok {
		t.Error("rejected valid dot-prefixed skill path")
	}
}

func listing(t *testing.T, url string) Listing {
	t.Helper()
	resp := do(t, http.MethodGet, url, "", nil)
	var out Listing
	if err := json.Unmarshal(resp.Body, &out); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("list %s: %d %v", url, resp.StatusCode, err)
	}
	return out
}

// traced returns the objects of GET /trace/<trace> in the order the service gives them.
func traced(t *testing.T, base, trace string) []Meta {
	t.Helper()
	resp := do(t, http.MethodGet, base+"/trace/"+trace, "", nil)
	var out struct {
		TraceID string `json:"trace_id"`
		Objects []Meta `json:"objects"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil || resp.StatusCode != http.StatusOK || out.TraceID != trace || out.Objects == nil {
		t.Fatalf("trace %s: %d %v %+v", trace, resp.StatusCode, err, out)
	}
	return out.Objects
}

func objectIDs(objects []Meta) []string {
	ids := []string{}
	for i := range objects {
		ids = append(ids, objects[i].ID)
	}
	return ids
}

// waitForTraceIndex blocks until the one-time build of the trace index has finished.
func waitForTraceIndex(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "traces", traceIndexMarker)); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("trace index was not built")
		}
		time.Sleep(time.Millisecond)
	}
}

// httpResult is one finished HTTP exchange: do has read the whole body and closed it.
type httpResult struct {
	Header     http.Header
	Status     string
	Body       []byte
	StatusCode int
}

func do(t *testing.T, method, url, body string, headers map[string]string) httpResult {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(resp.Body)
	if err = resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return httpResult{Header: resp.Header, Status: resp.Status, Body: data, StatusCode: resp.StatusCode}
}

func TestDeleteRemovesEmptyFolders(t *testing.T) {
	srv, dir := newTestServer(t)
	for _, id := range []string{"agents/a/v1/systemprompt.md", "agents/b/v1/systemprompt.md"} {
		do(t, http.MethodPut, srv.URL+"/blobs/"+id, "x", nil)
	}
	do(t, http.MethodDelete, srv.URL+"/blobs/agents/a/v1/systemprompt.md", "", nil)
	body := do(t, http.MethodGet, srv.URL+"/list/agents", "", nil).Body
	if strings.Contains(string(body), `"a"`) || !strings.Contains(string(body), `"b"`) {
		t.Fatalf("listing after delete: %s", body)
	}
	if _, err := os.Stat(filepath.Join(dir, "objects", "agents")); err != nil {
		t.Fatalf("root folder of the remaining object removed: %v", err)
	}
}
