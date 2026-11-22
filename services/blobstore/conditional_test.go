package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentCreateIfAbsentHasOneWinner(t *testing.T) {
	srv, _ := newTestServer(t)
	const writers = 24
	statuses := make(chan int, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPut, srv.URL+"/blobs/receipts/one", strings.NewReader(string(rune('a'+i))))
			if err != nil {
				t.Errorf("request: %v", err)
				return
			}
			req.Header.Set("If-None-Match", "*")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("PUT: %v", err)
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(statuses)
	winners := 0
	for status := range statuses {
		switch status {
		case http.StatusNoContent:
			winners++
		case http.StatusPreconditionFailed:
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("create winners = %d, want 1", winners)
	}
}

func TestConditionalPutRevisionAndValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	url := srv.URL + "/blobs/accounting/current"
	created := do(t, http.MethodPut, url, "same", map[string]string{"If-None-Match": "*"})
	first := created.Header.Get("ETag")
	if created.StatusCode != http.StatusNoContent || first == "" || strings.HasPrefix(first, "W/") {
		t.Fatalf("create: %d ETag %q", created.StatusCode, first)
	}
	if got := do(t, http.MethodPut, url, "stale", map[string]string{"If-Match": `"stale"`}); got.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale replace: %d", got.StatusCode)
	}
	replaced := do(t, http.MethodPut, url, "same", map[string]string{"If-Match": first})
	second := replaced.Header.Get("ETag")
	if replaced.StatusCode != http.StatusNoContent || second == "" || second == first {
		t.Fatalf("replace: %d ETag %q after %q", replaced.StatusCode, second, first)
	}
	for name, headers := range map[string]map[string]string{
		"combined": {"If-None-Match": "*", "If-Match": second},
		"weak":     {"If-Match": "W/" + second},
		"list":     {"If-Match": second + `, "other"`},
		"bad-none": {"If-None-Match": `"anything"`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := do(t, http.MethodPut, url, "bad", headers); got.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d", got.StatusCode)
			}
		})
	}
	missing := do(t, http.MethodPut, srv.URL+"/blobs/accounting/missing", "x", map[string]string{"If-Match": second})
	if missing.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("replace missing: %d", missing.StatusCode)
	}
	assertObjectAndMetaRevision(t, srv.URL, "accounting/current", "same", second)
}

func TestStartupRecoversInterruptedPut(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-publish", true: "after-publish"}[published], func(t *testing.T) {
			root := t.TempDir()
			object := filepath.Join(root, "objects", "receipts", "event")
			if err := os.MkdirAll(filepath.Dir(object), 0o750); err != nil {
				t.Fatal(err)
			}
			stage, err := os.CreateTemp(filepath.Dir(object), ".upload-*")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stage.WriteString("recovered"); err != nil {
				t.Fatal(err)
			}
			if err := stage.Sync(); err != nil {
				t.Fatal(err)
			}
			if err := stage.Close(); err != nil {
				t.Fatal(err)
			}
			stagePath := stage.Name()
			if published {
				if err := os.Rename(stagePath, object); err != nil {
					t.Fatal(err)
				}
			}
			transactions := filepath.Join(root, "transactions")
			if err := os.MkdirAll(transactions, 0o750); err != nil {
				t.Fatal(err)
			}
			m := Meta{ID: "receipts/event", Size: int64(len("recovered")), Created: time.Now().UTC(), Revision: "recovered-revision", TraceID: "trace-recovered"}
			j := transactionJournal{Operation: "put", ID: m.ID, Stage: stagePath, Meta: m}
			if err := writeJSONAtomic(filepath.Join(transactions, "interrupted.json"), j); err != nil {
				t.Fatal(err)
			}
			h, err := newHandler(root, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(h)
			defer srv.Close()
			assertObjectAndMetaRevision(t, srv.URL, m.ID, "recovered", `"recovered-revision"`)
			waitForTraceIndex(t, root)
			if entries, err := os.ReadDir(transactions); err != nil || len(entries) != 0 {
				t.Fatalf("journals after recovery: %v %v", entries, err)
			}
		})
	}
}

func TestStartupRecoversInterruptedDelete(t *testing.T) {
	root := t.TempDir()
	h, err := newHandler(root, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	put := do(t, http.MethodPut, srv.URL+"/blobs/runs/delete-me", "old", map[string]string{headerTrace: "delete-trace"})
	if put.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT: %d", put.StatusCode)
	}
	srv.Close()
	j := transactionJournal{Operation: "delete", ID: "runs/delete-me", PreviousTrace: "delete-trace"}
	if err := writeJSONAtomic(filepath.Join(root, "transactions", "interrupted-delete.json"), j); err != nil {
		t.Fatal(err)
	}
	h, err = newHandler(root, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv = httptest.NewServer(h)
	defer srv.Close()
	if got := do(t, http.MethodGet, srv.URL+"/blobs/runs/delete-me", "", nil); got.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after recovery: %d", got.StatusCode)
	}
	if got := traced(t, srv.URL, "delete-trace"); len(got) != 0 {
		t.Fatalf("trace after recovery: %+v", got)
	}
	waitForTraceIndex(t, root)
}

func TestStartupAssignsRevisionToLegacyObject(t *testing.T) {
	root := t.TempDir()
	object := filepath.Join(root, "objects", "runs", "legacy", "output")
	metaPath := filepath.Join(root, "meta", "runs", "legacy", "output.json")
	if err := os.MkdirAll(filepath.Dir(object), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, []byte("legacy bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := Meta{ID: "runs/legacy/output", Size: int64(len("legacy bytes")), Created: time.Now().UTC(), Creator: "legacy-agent", TraceID: "legacy-trace"}
	if err := writeJSONAtomic(metaPath, legacy); err != nil {
		t.Fatal(err)
	}
	h, err := newHandler(root, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	get := do(t, http.MethodGet, srv.URL+"/blobs/"+legacy.ID, "", nil)
	body, _ := io.ReadAll(get.Body)
	if get.StatusCode != http.StatusOK || string(body) != "legacy bytes" || get.Header.Get("ETag") == "" || get.Header.Get(headerCreator) != legacy.Creator {
		t.Fatalf("legacy GET: %d body %q ETag %q creator %q", get.StatusCode, body, get.Header.Get("ETag"), get.Header.Get(headerCreator))
	}
	metaResp := do(t, http.MethodGet, srv.URL+"/meta/"+legacy.ID, "", nil)
	var upgraded Meta
	if err := json.NewDecoder(metaResp.Body).Decode(&upgraded); err != nil {
		t.Fatal(err)
	}
	if upgraded.Revision == "" || upgraded.Creator != legacy.Creator || upgraded.TraceID != legacy.TraceID || !upgraded.Created.Equal(legacy.Created) {
		t.Fatalf("upgraded metadata %+v", upgraded)
	}
	waitForTraceIndex(t, root)
}

func assertObjectAndMetaRevision(t *testing.T, base, id, wantBody, wantETag string) {
	t.Helper()
	get := do(t, http.MethodGet, base+"/blobs/"+id, "", nil)
	body, err := io.ReadAll(get.Body)
	if err != nil || get.StatusCode != http.StatusOK || string(body) != wantBody || get.Header.Get("ETag") != wantETag {
		t.Fatalf("GET: %d body %q ETag %q err %v", get.StatusCode, body, get.Header.Get("ETag"), err)
	}
	metaResp := do(t, http.MethodGet, base+"/meta/"+id, "", nil)
	var m Meta
	if err := json.NewDecoder(metaResp.Body).Decode(&m); err != nil || metaResp.StatusCode != http.StatusOK {
		t.Fatalf("meta: %d %v", metaResp.StatusCode, err)
	}
	if quoteETag(m.Revision) != wantETag || m.Size != int64(len(wantBody)) {
		t.Fatalf("meta %+v, GET ETag %q", m, wantETag)
	}
	head := do(t, http.MethodHead, base+"/blobs/"+id, "", nil)
	if head.StatusCode != http.StatusOK || head.Header.Get("ETag") != wantETag {
		t.Fatalf("HEAD: %d ETag %q", head.StatusCode, head.Header.Get("ETag"))
	}
}

func TestSlowDownloadDoesNotBlockMutation(t *testing.T) {
	srv, _ := newTestServer(t)
	url := srv.URL + "/blobs/large"
	response := do(t, http.MethodPut, url, strings.Repeat("a", 16<<20), nil)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal(response.Status)
	}
	download, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	oldRevision := download.Header.Get("ETag")
	request, err := http.NewRequest(http.MethodPut, url, strings.NewReader("replacement"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("If-Match", oldRevision)
	client := &http.Client{Timeout: 2 * time.Second}
	changed, err := client.Do(request)
	if err != nil {
		t.Fatalf("slow download blocked the writer: %v", err)
	}
	changed.Body.Close()
	if changed.StatusCode != http.StatusNoContent || changed.Header.Get("ETag") == oldRevision {
		t.Fatalf("replacement failed: %s", changed.Status)
	}
	prefix := make([]byte, 8)
	if _, err := io.ReadFull(download.Body, prefix); err != nil || string(prefix) != "aaaaaaaa" {
		t.Fatalf("download lost its original snapshot: %q, %v", prefix, err)
	}
}
