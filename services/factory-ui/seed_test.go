package main

import (
	"context"
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

type seedStore struct {
	objects map[string]string
	puts    map[string]int
	mu      sync.Mutex
}

func newSeedStore(t *testing.T, initial map[string]string) (*seedStore, *httptest.Server) {
	t.Helper()
	store := &seedStore{objects: initial, puts: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/meta/")
		if r.Method == http.MethodPut {
			id = strings.TrimPrefix(r.URL.Path, "/blobs/")
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/meta/"):
			if _, ok := store.objects[id]; ok {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/blobs/"):
			if got := r.Header.Get("X-Creator"); got != "factory" {
				t.Errorf("creator = %q, want factory", got)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			store.objects[id] = string(body)
			store.puts[id]++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	return store, server
}

func TestSeedDirectoryStoresNestedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "templates", "tasks", "default.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{{task}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, server := newSeedStore(t, map[string]string{})
	defer server.Close()

	if err := seedDirectory(context.Background(), server.URL, dir); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.objects["templates/tasks/default.md"]; got != "{{task}}\n" {
		t.Fatalf("stored body = %q", got)
	}
}

func TestSeedDirectoryAllowsMissingAndEmptyDirectories(t *testing.T) {
	_, server := newSeedStore(t, map[string]string{})
	defer server.Close()
	if err := seedDirectory(context.Background(), server.URL, filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("missing directory: %v", err)
	}
	if err := seedDirectory(context.Background(), server.URL, t.TempDir()); err != nil {
		t.Fatalf("empty directory: %v", err)
	}
}

func TestSeedDirectoryDoesNotOverwriteExistingObjects(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.md"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, server := newSeedStore(t, map[string]string{"existing.md": "original"})
	defer server.Close()

	if err := seedDirectory(context.Background(), server.URL, dir); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.objects["existing.md"]; got != "original" {
		t.Fatalf("existing object = %q, want original", got)
	}
	if got := store.puts["existing.md"]; got != 0 {
		t.Fatalf("PUT count = %d, want 0", got)
	}
}

func TestSeedDefaultsSucceedsAndCancelsOnRetry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.md"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, server := newSeedStore(t, map[string]string{})
	defer server.Close()

	seedDefaults(context.Background(), server.URL, dir, slog.New(slog.DiscardHandler))
	store.mu.Lock()
	got := store.objects["ok.md"]
	store.mu.Unlock()
	if got != "hello" {
		t.Fatalf("seedDefaults stored %q", got)
	}

	// Failing blobstore retries until context is cancelled.
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	seedDefaults(ctx, badServer.URL, dir, slog.New(slog.DiscardHandler))
}

func TestEnsureObjectAndSeedDirectoryErrors(t *testing.T) {
	// Non-404/non-200 meta response fails.
	metaFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer metaFail.Close()
	if err := ensureObject(context.Background(), metaFail.URL, "a.md", "body"); err == nil {
		t.Fatal("expected error for 500 meta response")
	}

	// 404 meta followed by 500 PUT fails.
	putFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer putFail.Close()
	if err := ensureObject(context.Background(), putFail.URL, "a.md", "body"); err == nil {
		t.Fatal("expected error for 500 PUT response")
	}

	// Invalid URL fails.
	if err := ensureObject(context.Background(), "://bad-url", "a.md", "body"); err == nil {
		t.Fatal("expected error for invalid URL")
	}

	// seedDirectory skips non-regular files (symlinks).
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(target, []byte("skip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	store, server := newSeedStore(t, map[string]string{})
	defer server.Close()
	if err := seedDirectory(context.Background(), server.URL, dir); err != nil {
		t.Fatalf("seedDirectory with symlink: %v", err)
	}
	store.mu.Lock()
	count := len(store.objects)
	store.mu.Unlock()
	if count != 0 {
		t.Fatalf("symlink was uploaded: %d objects", count)
	}

	// seedDirectory returns error when dir is a regular file.
	fileAsDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedDirectory(context.Background(), server.URL, fileAsDir); err == nil {
		t.Fatal("expected error when dir is a file")
	}
}
