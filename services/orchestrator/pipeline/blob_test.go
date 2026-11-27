package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type errCloser struct{ err error }

func (e errCloser) Close() error { return e.err }

func TestNewHTTPStoreAndConnectorsFromEnv(t *testing.T) {
	t.Setenv("BLOB_URL", "")
	if s := NewHTTPStoreFromEnv(); s.BaseURL != "http://blobstore:8080/blobs" || s.Client != http.DefaultClient {
		t.Fatalf("default store = %+v", s)
	}
	t.Setenv("BLOB_URL", "http://custom:9000/blobs/")
	if s := NewHTTPStoreFromEnv(); s.BaseURL != "http://custom:9000/blobs" {
		t.Fatalf("custom store = %+v", s)
	}

	t.Setenv("CONNECTORS_URL", "")
	if c := NewHTTPConnectorsFromEnv(nil); c.BaseURL != "http://connectors:8080" || c.Client != http.DefaultClient {
		t.Fatalf("default connectors = %+v", c)
	}
	t.Setenv("CONNECTORS_URL", "http://conn:9090/")
	customClient := &http.Client{}
	if c := NewHTTPConnectorsFromEnv(customClient); c.BaseURL != "http://conn:9090" || c.Client != customClient {
		t.Fatalf("custom connectors = %+v", c)
	}

	if got := RoundName("step", 2); got != "step.round-2" {
		t.Fatalf("RoundName = %q", got)
	}
	if got := evidenceBase("other/path"); got != "" {
		t.Fatalf("evidenceBase non-runs = %q", got)
	}
	if err := closeAfter(errors.New("a"), errCloser{err: errors.New("b")}); err == nil {
		t.Fatal("expected joined closeAfter error")
	}
}

func TestHTTPStoreOperations(t *testing.T) {
	blobs := map[string][]byte{}
	etags := map[string]string{}
	creators := map[string]string{}
	var lastCreator, lastTrace string

	const (
		failID  = "fail/500"
		failKey = "fail"
		badJSON = "badjson"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/blobs/"):
			id := strings.TrimPrefix(r.URL.Path, "/blobs/")
			switch r.Method {
			case http.MethodPut:
				if id == failID {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if id == "fail/no-etag" {
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.Header.Get("If-None-Match") == "*" {
					if _, exists := blobs[id]; exists {
						w.WriteHeader(http.StatusPreconditionFailed)
						return
					}
				}
				if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
					if etags[id] == "" || `"`+etags[id]+`"` != ifMatch {
						w.WriteHeader(http.StatusPreconditionFailed)
						return
					}
				}
				body, _ := io.ReadAll(r.Body)
				blobs[id] = body
				etags[id] = "rev-" + string(body)
				lastCreator = r.Header.Get("X-Creator")
				lastTrace = r.Header.Get("X-Trace-Id")
				if lastCreator != "" {
					creators[id] = lastCreator
				}
				w.Header().Set("ETag", `"`+etags[id]+`"`)
				w.WriteHeader(http.StatusOK)
			case http.MethodGet:
				if id == failID {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if id == "fail/no-etag" {
					_, _ = w.Write([]byte("x"))
					return
				}
				data, ok := blobs[id]
				if !ok {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("ETag", `"`+etags[id]+`"`)
				_, _ = w.Write(data)
			case http.MethodDelete:
				if id == failID {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if _, ok := blobs[id]; !ok {
					http.NotFound(w, r)
					return
				}
				delete(blobs, id)
				delete(etags, id)
				w.WriteHeader(http.StatusNoContent)
			}
		case strings.HasPrefix(r.URL.Path, "/list/"):
			prefix := strings.TrimPrefix(r.URL.Path, "/list/")
			switch prefix {
			case "missing":
				http.NotFound(w, r)
			case failKey:
				w.WriteHeader(http.StatusInternalServerError)
			case badJSON:
				_, _ = io.WriteString(w, "{bad")
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"directories": []string{"sub"},
					"objects":     []map[string]string{{"id": prefix + "/a.txt"}},
				})
			}
		case strings.HasPrefix(r.URL.Path, "/meta/"):
			id := strings.TrimPrefix(r.URL.Path, "/meta/")
			switch id {
			case failKey:
				w.WriteHeader(http.StatusInternalServerError)
			case badJSON:
				_, _ = io.WriteString(w, "{bad")
			default:
				_ = json.NewEncoder(w).Encode(map[string]string{"creator": creators[id]})
			}
		case strings.HasPrefix(r.URL.Path, "/trace/"):
			tid := strings.TrimPrefix(r.URL.Path, "/trace/")
			switch tid {
			case "missing":
				http.NotFound(w, r)
			case failKey:
				w.WriteHeader(http.StatusInternalServerError)
			case badJSON:
				_, _ = io.WriteString(w, "{bad")
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"objects": []map[string]string{{"id": "runs/" + tid + "/input"}},
				})
			}
		}
	}))
	defer srv.Close()

	store := HTTPStore{BaseURL: srv.URL + "/blobs", Client: srv.Client()}
	ctx := context.Background()

	// Put / Get / Creator / Delete.
	if err := store.Put(ctx, "a/b", []byte("hello"), Meta{Creator: "alice", TraceID: "t1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if lastCreator != "alice" || lastTrace != "t1" {
		t.Fatalf("meta headers = (%q, %q)", lastCreator, lastTrace)
	}
	if got, err := store.Get(ctx, "a/b"); err != nil || string(got) != "hello" {
		t.Fatalf("Get = (%q, %v)", got, err)
	}
	if _, err := store.Get(ctx, "does/not/exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get err = %v", err)
	}
	if _, err := store.Get(ctx, failID); err == nil {
		t.Fatal("expected 500 Get error")
	}
	if err := store.Put(ctx, "../bad", nil, Meta{}); err == nil {
		t.Fatal("expected invalid ID Put error")
	}

	// PutIfAbsent / PutIfMatch / GetWithRevision.
	rev, err := store.PutIfAbsent(ctx, "cond/1", []byte("v1"), Meta{Creator: "bob", TraceID: "t2"})
	if err != nil || rev != "rev-v1" {
		t.Fatalf("PutIfAbsent = (%q, %v)", rev, err)
	}
	if _, err = store.PutIfAbsent(ctx, "cond/1", []byte("v2"), Meta{}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("duplicate PutIfAbsent err = %v", err)
	}
	if _, err = store.PutIfAbsent(ctx, "../bad", nil, Meta{}); err == nil {
		t.Fatal("expected invalid ID PutIfAbsent error")
	}
	if _, err = store.PutIfAbsent(ctx, failID, nil, Meta{}); err == nil {
		t.Fatal("expected 500 PutIfAbsent error")
	}
	if _, err = store.PutIfAbsent(ctx, "fail/no-etag", nil, Meta{}); err == nil {
		t.Fatal("expected no-etag PutIfAbsent error")
	}

	rev2, err := store.PutIfMatch(ctx, "cond/1", []byte("v2"), Meta{}, rev)
	if err != nil || rev2 != "rev-v2" {
		t.Fatalf("PutIfMatch = (%q, %v)", rev2, err)
	}
	if _, err = store.PutIfMatch(ctx, "cond/1", []byte("v3"), Meta{}, "wrong"); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale PutIfMatch err = %v", err)
	}
	if _, err = store.PutIfMatch(ctx, "cond/1", []byte("v3"), Meta{}, ""); err == nil {
		t.Fatal("expected empty revision error")
	}

	gotData, gotRev, err := store.GetWithRevision(ctx, "cond/1")
	if err != nil || string(gotData) != "v2" || gotRev != "rev-v2" {
		t.Fatalf("GetWithRevision = (%q, %q, %v)", gotData, gotRev, err)
	}
	if _, _, err = store.GetWithRevision(ctx, "../bad"); err == nil {
		t.Fatal("expected invalid ID GetWithRevision error")
	}
	if _, _, err = store.GetWithRevision(ctx, "missing/id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing GetWithRevision err = %v", err)
	}
	if _, _, err = store.GetWithRevision(ctx, failID); err == nil {
		t.Fatal("expected 500 GetWithRevision error")
	}
	if _, _, err = store.GetWithRevision(ctx, "fail/no-etag"); err == nil {
		t.Fatal("expected no-etag GetWithRevision error")
	}

	// Delete existing, missing (returns nil), and 500.
	if err = store.Delete(ctx, "a/b"); err != nil {
		t.Fatalf("Delete existing: %v", err)
	}
	if err = store.Delete(ctx, "a/b"); err != nil {
		t.Fatalf("Delete missing should succeed: %v", err)
	}
	if err = store.Delete(ctx, failID); err == nil {
		t.Fatal("expected 500 Delete error")
	}

	// List.
	dirs, objs, err := store.List(ctx, "runs")
	if err != nil || len(dirs) != 1 || len(objs) != 1 {
		t.Fatalf("List = (%v, %v, %v)", dirs, objs, err)
	}
	if dirs, objs, err = store.List(ctx, "missing"); err != nil || dirs != nil || objs != nil {
		t.Fatalf("List 404 = (%v, %v, %v)", dirs, objs, err)
	}
	if _, _, err = store.List(ctx, "../bad"); err == nil {
		t.Fatal("expected invalid prefix List error")
	}
	if _, _, err = store.List(ctx, failKey); err == nil {
		t.Fatal("expected 500 List error")
	}
	if _, _, err = store.List(ctx, badJSON); err == nil {
		t.Fatal("expected badjson List error")
	}

	// Creator.
	var creator string
	if creator, err = store.Creator(ctx, "cond/1"); err != nil || creator != "bob" {
		t.Fatalf("Creator = (%q, %v)", creator, err)
	}
	if _, err = store.Creator(ctx, "../bad"); err == nil {
		t.Fatal("expected invalid ID Creator error")
	}
	if _, err = store.Creator(ctx, failKey); err == nil {
		t.Fatal("expected 500 Creator error")
	}
	if _, err = store.Creator(ctx, badJSON); err == nil {
		t.Fatal("expected badjson Creator error")
	}

	// TraceObjects.
	ids, err := store.TraceObjects(ctx, "t1")
	if err != nil || len(ids) != 1 || ids[0] != "runs/t1/input" {
		t.Fatalf("TraceObjects = (%v, %v)", ids, err)
	}
	if ids, err = store.TraceObjects(ctx, "missing"); err != nil || ids != nil {
		t.Fatalf("TraceObjects 404 = (%v, %v)", ids, err)
	}
	if _, err = store.TraceObjects(ctx, failKey); err == nil {
		t.Fatal("expected 500 TraceObjects error")
	}
	if _, err = store.TraceObjects(ctx, badJSON); err == nil {
		t.Fatal("expected badjson TraceObjects error")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := WriteFileAtomic(path, []byte("atomic"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	got, err := root.ReadFile("file.txt")
	if err != nil || string(got) != "atomic" {
		t.Fatalf("read = (%q, %v)", got, err)
	}
	if err = WriteFileAtomic(filepath.Join(dir, "no-such-dir", "file.txt"), []byte("x"), 0o600); err == nil {
		t.Fatal("expected error for missing parent directory")
	}
}
