package main

import (
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type kubeRequest struct {
	path, query, auth string
}

// fakeCluster starts a TLS Kubernetes stand-in and a service account directory that trusts it.
func fakeCluster(t *testing.T) (proxy *clusterProxy, got chan kubeRequest) {
	t.Helper()
	got = make(chan kubeRequest, 1)
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- kubeRequest{r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization")}
		if r.URL.Path == "/api/v1/namespaces/factory/pods/missing/log" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"pods \"missing\" not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(api.Close)
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	for name, data := range map[string][]byte{"ca.crt": ca, "token": []byte("secret-1\n"), "namespace": []byte("factory")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &clusterProxy{apiBase: api.URL, dir: dir, logger: slog.New(slog.DiscardHandler)}, got
}

func clusterUI(t *testing.T, c *clusterProxy) http.Handler {
	t.Helper()
	h, err := newHandler(&backends{orchestrator: "http://x", agents: "http://x", policies: "http://x", blobs: "http://x", cluster: c}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestClusterPathsAndQueries(t *testing.T) {
	c, got := fakeCluster(t)
	h := clusterUI(t, c)
	cases := []struct{ path, wantPath, wantQuery string }{
		{"/api/cluster/nodes", "/api/v1/nodes", ""},
		{"/api/cluster/deployments", "/apis/apps/v1/namespaces/factory/deployments", ""},
		{"/api/cluster/pods?watch=true", "/api/v1/namespaces/factory/pods", ""},
		{"/api/cluster/services", "/api/v1/namespaces/factory/services", ""},
		{"/api/cluster/events", "/api/v1/namespaces/factory/events", ""},
		{"/api/cluster/pods/api-7d9f-x2/log", "/api/v1/namespaces/factory/pods/api-7d9f-x2/log", ""},
		{
			"/api/cluster/pods/api-1/log?container=app&tailLines=100&sinceSeconds=60&previous=true&timestamps=true&follow=true&limitBytes=1",
			"/api/v1/namespaces/factory/pods/api-1/log",
			"container=app&previous=true&sinceSeconds=60&tailLines=100&timestamps=true",
		},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", tc.path, http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d %s", tc.path, rec.Code, rec.Body)
		}
		r := <-got
		if r.path != tc.wantPath || r.query != tc.wantQuery || r.auth != "Bearer secret-1" {
			t.Errorf("GET %s reached %+v, want path %q query %q", tc.path, r, tc.wantPath, tc.wantQuery)
		}
	}
}

func TestClusterRereadsToken(t *testing.T) {
	c, got := fakeCluster(t)
	h := clusterUI(t, c)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/cluster/nodes", http.NoBody))
	<-got
	if err := os.WriteFile(filepath.Join(c.dir, "token"), []byte("secret-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/cluster/nodes", http.NoBody))
	if r := <-got; r.auth != "Bearer secret-2" {
		t.Errorf("auth %q, want the rotated token", r.auth)
	}
}

// Kubernetes mounts each service account file as a symlink into a dated directory that it
// swaps on rotation; reads follow those links and nothing that points out of the directory.
func TestClusterReadsServiceAccountLinks(t *testing.T) {
	dir := t.TempDir()
	dated := filepath.Join(dir, "..2026_11_23_10_00_00.1")
	if err := os.Mkdir(dated, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	for path, data := range map[string]string{filepath.Join(dated, "token"): "secret-1\n", outside: "elsewhere"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{"..data": filepath.Base(dated), "token": "..data/token", "escape": outside} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	c := &clusterProxy{dir: dir}
	if token, err := c.read("token"); err != nil || token != "secret-1" {
		t.Errorf("token = %q, %v", token, err)
	}
	for _, name := range []string{"escape", "../" + filepath.Base(filepath.Dir(outside)) + "/secret"} {
		if data, err := c.read(name); err == nil {
			t.Errorf("read %q returned %q, want a refusal", name, data)
		}
	}
}

func TestClusterAllowlist(t *testing.T) {
	c, got := fakeCluster(t)
	h := clusterUI(t, c)
	for _, path := range []string{
		"/api/cluster/secrets",
		"/api/cluster/pods/api-1",
		"/api/cluster/pods/api-1/exec",
		"/api/cluster/pods/../secrets/log",
		"/api/cluster/pods/API/log",
		"/api/cluster/nodes/node-1",
		"/api/cluster/",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, http.NoBody))
		// ServeMux redirects unclean paths such as ".." before the proxy sees them.
		if rec.Code != http.StatusNotFound && rec.Code/100 != 3 {
			t.Errorf("GET %s: status %d, want 404", path, rec.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/cluster/pods", http.NoBody))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", method, rec.Code)
		}
	}
	select {
	case r := <-got:
		t.Errorf("kubernetes reached: %+v", r)
	default:
	}
}

func TestClusterErrorsBecomeJSON(t *testing.T) {
	c, got := fakeCluster(t)
	rec := httptest.NewRecorder()
	clusterUI(t, c).ServeHTTP(rec, httptest.NewRequest("GET", "/api/cluster/pods/missing/log", http.NoBody))
	<-got
	var body map[string]string
	if rec.Code != http.StatusNotFound || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body["error"] != `pods "missing" not found` {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestClusterOutsideCluster(t *testing.T) {
	c := &clusterProxy{apiBase: "https://127.0.0.1:1", dir: t.TempDir(), logger: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(clusterUI(t, c))
	defer srv.Close()
	resp := get(t, srv.URL+"/api/cluster/pods")
	if resp.status != http.StatusServiceUnavailable || resp.body != "{\"error\":\"not running in a cluster\"}\n" {
		t.Fatalf("got %d %s", resp.status, resp.body)
	}
}
