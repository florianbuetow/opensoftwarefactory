package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type seedStore struct {
	mu      sync.Mutex
	objects map[string]string
	puts    map[string]int
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{{task}}\n"), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(dir, "existing.md"), []byte("replacement"), 0o644); err != nil {
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
