package main

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type failingCloser struct{ err error }

func (c failingCloser) Close() error { return c.err }

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func newTestStore(t *testing.T) *store {
	t.Helper()
	root := t.TempDir()
	s := &store{
		objects:      filepath.Join(root, "objects"),
		meta:         filepath.Join(root, "meta"),
		traces:       filepath.Join(root, "traces"),
		transactions: filepath.Join(root, "transactions"),
		logger:       slog.New(slog.DiscardHandler),
		mu:           &sync.Mutex{},
		tx:           &transactionState{},
		indexed:      &atomic.Bool{},
	}
	for _, dir := range []string{s.objects, s.meta, s.traces, s.transactions} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	s.indexed.Store(true)
	return s
}

func TestHandlersFailWhenTransactionStateIsBroken(t *testing.T) {
	s := newTestStore(t)
	s.tx.err = errors.New("corrupt transaction state")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /blobs/{id...}", s.put)
	mux.HandleFunc("GET /blobs/{id...}", s.get)
	mux.HandleFunc("DELETE /blobs/{id...}", s.delete)
	mux.HandleFunc("GET /meta/{id...}", s.getMeta)
	mux.HandleFunc("GET /list/{prefix...}", s.list)
	mux.HandleFunc("GET /trace/{trace}", s.trace)

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPut, "/blobs/runs/a", "data"},
		{http.MethodGet, "/blobs/runs/a", ""},
		{http.MethodDelete, "/blobs/runs/a", ""},
		{http.MethodGet, "/meta/runs/a", ""},
		{http.MethodGet, "/list/", ""},
		{http.MethodGet, "/trace/t-1", ""},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s = %d, want 500", tc.method, tc.path, rec.Code)
		}
	}
}

func TestHandlerValidationAndDirectoryEdgeCases(t *testing.T) {
	srv, root := newTestServer(t)

	// Put a nested blob so "runs/dir" exists as a directory.
	if resp := do(t, http.MethodPut, srv.URL+"/blobs/runs/dir/item", "body", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}

	for _, tc := range []struct {
		headers map[string]string
		method  string
		path    string
		want    int
	}{
		{nil, http.MethodPut, "/blobs/.hidden", http.StatusBadRequest},
		{map[string]string{headerCreator: strings.Repeat("a", maxHeaderValue+1)}, http.MethodPut, "/blobs/runs/ctrl", http.StatusBadRequest},
		{nil, http.MethodGet, "/blobs/.hidden", http.StatusBadRequest},
		{nil, http.MethodGet, "/blobs/runs/dir", http.StatusNotFound},
		{nil, http.MethodDelete, "/blobs/.hidden", http.StatusBadRequest},
		{nil, http.MethodDelete, "/blobs/runs/dir", http.StatusNotFound},
		{nil, http.MethodGet, "/meta/.hidden", http.StatusBadRequest},
		{nil, http.MethodGet, "/meta/runs/dir", http.StatusNotFound},
		{nil, http.MethodGet, "/list/.hidden", http.StatusBadRequest},
		{nil, http.MethodGet, "/trace/" + strings.Repeat("t", maxHeaderValue+1), http.StatusBadRequest},
	} {
		if resp := do(t, tc.method, srv.URL+tc.path, "x", tc.headers); resp.StatusCode != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}

	// Dot-prefixed file in a non-skill directory is hidden from /list/.
	if err := os.WriteFile(filepath.Join(root, "objects", "runs", "dir", ".secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := listing(t, srv.URL+"/list/runs/dir")
	if len(l.Objects) != 1 || l.Objects[0].ID != "runs/dir/item" {
		t.Fatalf("listing exposed dot file: %+v", l)
	}
}

func TestParsePutConditionEdgeCases(t *testing.T) {
	reqMulti := httptest.NewRequest(http.MethodPut, "/blobs/a", http.NoBody)
	reqMulti.Header.Add("If-Match", `"rev1"`)
	reqMulti.Header.Add("If-Match", `"rev2"`)
	if _, err := parsePutCondition(reqMulti); err == nil {
		t.Fatal("expected error for multiple If-Match headers")
	}

	reqCtrl := httptest.NewRequest(http.MethodPut, "/blobs/a", http.NoBody)
	reqCtrl.Header.Set("If-Match", "\"rev\x01\"")
	if _, err := parsePutCondition(reqCtrl); err == nil {
		t.Fatal("expected error for control char in If-Match")
	}
}

func TestCommitPutBodyErrors(t *testing.T) {
	s := newTestStore(t)
	m := Meta{ID: "runs/err", Created: time.Now().UTC()}

	if _, _, err := s.commitPut("runs/err", errReader{err: errors.New("read boom")}, &m, putCondition{}); err == nil {
		t.Fatal("expected error from failing body reader")
	}
	_, status, err := s.commitPut("runs/too-large", errReader{err: &http.MaxBytesError{Limit: 10}}, &m, putCondition{})
	if err != nil || status != http.StatusRequestEntityTooLarge {
		t.Fatalf("MaxBytesError: status=%d err=%v, want 413 nil", status, err)
	}
}

func TestRecoveryAndReplayErrorBranches(t *testing.T) {
	s := newTestStore(t)

	// Non-json files and subdirectories in transactions are ignored.
	if err := os.MkdirAll(filepath.Join(s.transactions, "subdir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.transactions, "note.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverTransactions(); err != nil {
		t.Fatalf("recoverTransactions with ignored entries: %v", err)
	}

	// Corrupt journal fails recovery.
	badJournal := filepath.Join(s.transactions, "bad.json")
	if err := os.WriteFile(badJournal, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverTransactions(); err == nil {
		t.Fatal("expected error for corrupt transaction journal")
	}
	if err := os.Remove(badJournal); err != nil {
		t.Fatal(err)
	}

	// Replay rejects staged path outside transactions/object dir.
	j := &transactionJournal{
		Operation: "put",
		ID:        "runs/x",
		Stage:     filepath.Join(t.TempDir(), "not-upload"),
		Meta:      Meta{ID: "runs/x"},
	}
	if err := s.replayTransaction(filepath.Join(s.transactions, "j.json"), j); err == nil {
		t.Fatal("expected invalid staged object path error")
	}

	// Replay with PreviousTrace != Meta.TraceID drops old trace entry.
	oldEntry := s.traceEntry("old-trace", "runs/moved")
	if err := writeJSONAtomic(oldEntry, "runs/moved"); err != nil {
		t.Fatal(err)
	}
	objPath, _, _ := s.paths("runs/moved")
	if err := os.MkdirAll(filepath.Dir(objPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	moveJournal := &transactionJournal{
		Operation:     "put",
		ID:            "runs/moved",
		PreviousTrace: "old-trace",
		Meta:          Meta{ID: "runs/moved", TraceID: "new-trace", Revision: "rev-1", Created: time.Now().UTC()},
	}
	if err := s.replayTransaction(filepath.Join(s.transactions, "move.json"), moveJournal); err != nil {
		t.Fatalf("replayTransaction move: %v", err)
	}
	if _, err := os.Stat(oldEntry); !os.IsNotExist(err) {
		t.Fatalf("old trace entry should have been removed: %v", err)
	}

	// Missing transactions directory returns error.
	if err := os.RemoveAll(s.transactions); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverTransactions(); err == nil {
		t.Fatal("expected error when transactions directory is missing")
	}
}

func TestMetaAndTraceEdgeCases(t *testing.T) {
	s := newTestStore(t)

	// readMeta fallbacks: invalid ID, missing meta, corrupt meta.
	objPath := filepath.Join(s.objects, "runs", "corrupt")
	if err := os.MkdirAll(filepath.Dir(objPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(objPath)
	if err != nil {
		t.Fatal(err)
	}
	if m := s.readMeta(".invalid", info); m.ID != ".invalid" || m.Size != 3 {
		t.Fatalf("readMeta invalid ID = %+v", m)
	}
	if m := s.readMeta("runs/corrupt", info); m.ID != "runs/corrupt" || m.Size != 3 {
		t.Fatalf("readMeta missing file = %+v", m)
	}
	_, metaPath, _ := s.paths("runs/corrupt")
	if err = os.MkdirAll(filepath.Dir(metaPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(metaPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := s.readMeta("runs/corrupt", info); m.ID != "runs/corrupt" || m.Size != 3 {
		t.Fatalf("readMeta corrupt json = %+v", m)
	}
	if got := storedTrace(metaPath); got != "" {
		t.Fatalf("storedTrace corrupt = %q, want empty", got)
	}

	// tracedMeta: invalid ID -> stale; corrupt meta -> not found, not stale.
	if _, found, stale := s.tracedMeta("t-1", ".bad"); found || !stale {
		t.Fatalf("tracedMeta invalid ID: found=%v stale=%v", found, stale)
	}
	if _, found, stale := s.tracedMeta("t-1", "runs/corrupt"); found || stale {
		t.Fatalf("tracedMeta corrupt meta: found=%v stale=%v", found, stale)
	}

	// indexedTrace skips subdirs, dot files, and corrupt entry files.
	tDir := s.traceDir("t-edge")
	if err = os.MkdirAll(filepath.Join(tDir, "subdir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(tDir, ".hidden"), []byte(`"x"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(tDir, "badentry"), []byte(`{not-string`), 0o600); err != nil {
		t.Fatal(err)
	}
	objs, err := s.indexedTrace("t-edge")
	if err != nil || len(objs) != 0 {
		t.Fatalf("indexedTrace edge = %v, %v", objs, err)
	}

	// Trace sorting tie-breaker when Created timestamps are identical.
	sameTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"runs/same/b", "runs/same/a"} {
		_, mp, _ := s.paths(id)
		m := Meta{ID: id, Created: sameTime, TraceID: "t-tie"}
		if err := s.writeMeta(mp, &m); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /trace/{trace}", s.trace)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/trace/t-tie", http.NoBody))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runs/same/a"`) {
		t.Fatalf("trace tie-break = %d %s", rec.Code, rec.Body.String())
	}

	// buildTraceIndex with missing meta dir logs error and does not set indexed.
	broken := newTestStore(t)
	broken.indexed.Store(false)
	if err := os.RemoveAll(broken.meta); err != nil {
		t.Fatal(err)
	}
	broken.buildTraceIndex()
	if broken.indexed.Load() {
		t.Fatal("expected buildTraceIndex to leave indexed=false on missing meta dir")
	}
	if _, err := broken.scanTrace("t-1"); err == nil {
		t.Fatal("expected scanTrace error on missing meta dir")
	}
}

func TestAtomicWriteAndDurabilityHelpers(t *testing.T) {
	dir := t.TempDir()

	// Marshal error.
	if err := writeJSONAtomic(filepath.Join(dir, "bad.json"), make(chan int)); err == nil {
		t.Fatal("expected json.Marshal error")
	}

	// Parent path is a file -> MkdirAll fails.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(blocker, "child.json"), map[string]string{"a": "b"}); err == nil {
		t.Fatal("expected MkdirAll error")
	}

	// Target path is a non-empty directory -> os.Rename fails.
	targetDir := filepath.Join(dir, "targetdir")
	if err := os.MkdirAll(filepath.Join(targetDir, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(targetDir, map[string]string{"a": "b"}); err == nil {
		t.Fatal("expected Rename error over non-empty directory")
	}

	// ensureDirDurable rejects path outside root.
	if err := ensureDirDurable(t.TempDir(), dir); err == nil {
		t.Fatal("expected directory escapes root error")
	}

	// syncNearestDir walks up past nonexistent subdirectories to root.
	if err := syncNearestDir(filepath.Join(dir, "a", "b", "c"), dir); err != nil {
		t.Fatalf("syncNearestDir: %v", err)
	}

	// closeAfter joins close error.
	baseErr := errors.New("base")
	closeErr := errors.New("close")
	if got := closeAfter(baseErr, failingCloser{err: closeErr}); !errors.Is(got, baseErr) || !errors.Is(got, closeErr) {
		t.Fatalf("closeAfter = %v", got)
	}

	// removeAfter joins remove error on non-empty directory.
	if got := removeAfter(baseErr, targetDir); !errors.Is(got, baseErr) || got == baseErr {
		t.Fatalf("removeAfter = %v, want joined error", got)
	}

	// Unused import guard not needed; verify io.Closer compatibility.
	var _ io.Closer = failingCloser{}
}
