package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingCloser struct{ err error }

func (c failingCloser) Close() error { return c.err }

func TestEnvHelper(t *testing.T) {
	t.Setenv("REGISTRY_TEST_ENV_KEY", "")
	if got := env("REGISTRY_TEST_ENV_KEY", "fallback"); got != "fallback" {
		t.Fatalf("env unset = %q, want fallback", got)
	}
	t.Setenv("REGISTRY_TEST_ENV_KEY", "configured")
	if got := env("REGISTRY_TEST_ENV_KEY", "fallback"); got != "configured" {
		t.Fatalf("env set = %q, want configured", got)
	}
}

func TestCloseAfterJoinsCloseError(t *testing.T) {
	baseErr := errors.New("base")
	closeErr := errors.New("close")
	got := closeAfter(baseErr, failingCloser{err: closeErr})
	if !errors.Is(got, baseErr) || !errors.Is(got, closeErr) {
		t.Fatalf("closeAfter = %v, want joined base and close errors", got)
	}
}

func TestHealthzAndReadErrorBranches(t *testing.T) {
	blobs := newFakeBlobstore()
	blobSrv := httptest.NewServer(blobs)
	t.Cleanup(blobSrv.Close)

	reg := &registry{
		prefix: "agents",
		files:  []string{"systemprompt.md", "agent.json"},
		blobs:  blobClient{base: blobSrv.URL, client: http.DefaultClient},
		logger: slog.New(slog.DiscardHandler),
	}
	handler := reg.routes()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz: %d", rec.Code)
	}

	// Invalid names and missing entries/versions.
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/agents/BAD", http.StatusBadRequest},
		{http.MethodGet, "/agents/missing", http.StatusNotFound},
		{http.MethodGet, "/agents/BAD/v1", http.StatusBadRequest},
		{http.MethodGet, "/agents/missing/latest", http.StatusNotFound},
		{http.MethodGet, "/agents/missing/v1", http.StatusNotFound},
		{http.MethodDelete, "/agents/BAD", http.StatusBadRequest},
		{http.MethodDelete, "/agents/missing", http.StatusNotFound},
	} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, http.NoBody))
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}

	// Direct call to getVersion with a malformed version string.
	rec = httptest.NewRecorder()
	reg.getVersion(rec, httptest.NewRequest(http.MethodGet, "/agents/coder/badver", http.NoBody), "coder", "badver")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("getVersion malformed version = %d, want 400", rec.Code)
	}
}

func TestBlobstoreFailuresReturnBadGateway(t *testing.T) {
	failingBlobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(failingBlobs.Close)

	reg := &registry{
		prefix: "agents",
		files:  []string{"systemprompt.md", "agent.json"},
		blobs:  blobClient{base: failingBlobs.URL, client: http.DefaultClient},
		logger: slog.New(slog.DiscardHandler),
	}
	handler := reg.routes()

	validAgentBody := `{"files":{"systemprompt.md":"hi","agent.json":{"models":["m"],"unpack_inputs":false}}}`
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/agents", ""},
		{http.MethodGet, "/agents/coder", ""},
		{http.MethodGet, "/agents/coder/latest", ""},
		{http.MethodGet, "/agents/coder/v1", ""},
		{http.MethodPost, "/agents/coder", validAgentBody},
		{http.MethodDelete, "/agents/coder", ""},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("%s %s = %d, want 502", tc.method, tc.path, rec.Code)
		}
	}
}

func TestDeleteEntryErrorBranches(t *testing.T) {
	blobs := newFakeBlobstore()
	var failPut, failDelete bool
	blobSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failPut && r.Method == http.MethodPut {
			http.Error(w, "put failed", http.StatusInternalServerError)
			return
		}
		if failDelete && r.Method == http.MethodDelete {
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		blobs.ServeHTTP(w, r)
	}))
	t.Cleanup(blobSrv.Close)

	reg := &registry{
		prefix:    "skills",
		files:     []string{"SKILL.md"},
		openFiles: true,
		blobs:     blobClient{base: blobSrv.URL, client: http.DefaultClient},
		logger:    slog.New(slog.DiscardHandler),
	}
	handler := reg.routes()

	body := `{"files":{"SKILL.md":"---\nname: git\ndescription: git skill\n---\nbody\n"}}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/skills/git", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /skills/git = %d: %s", rec.Code, rec.Body.String())
	}

	// Unknown version returns 404.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/skills/git/v99", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown version = %d, want 404", rec.Code)
	}

	// Failure writing retired version marker returns 502.
	failPut = true
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/skills/git/v1", http.NoBody))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("DELETE with failed retired put = %d, want 502", rec.Code)
	}
	failPut = false

	// Failure deleting file returns 502.
	failDelete = true
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/skills/git/v1", http.NoBody))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("DELETE with failed blob delete = %d, want 502", rec.Code)
	}
}

func TestSeedErrorAndEdgeBranches(t *testing.T) {
	blobs := newFakeBlobstore()
	blobSrv := httptest.NewServer(blobs)
	t.Cleanup(blobSrv.Close)

	reg := &registry{
		prefix: "agents",
		files:  []string{"systemprompt.md", "agent.json"},
		blobs:  blobClient{base: blobSrv.URL, client: http.DefaultClient},
		logger: slog.New(slog.DiscardHandler),
	}

	// Nonexistent seed directory is a no-op.
	if err := reg.seed(t.Context(), filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatalf("seed missing dir: %v", err)
	}

	// Regular file passed as seed dir returns an error.
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reg.seed(t.Context(), notADir); err == nil {
		t.Fatal("expected error when seed dir is a file")
	}

	// Top-level files and top-level version directories (name == "") are ignored.
	edgeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(edgeDir, "README.md"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(edgeDir, "v1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := reg.seed(t.Context(), edgeDir); err != nil {
		t.Fatalf("seed edgeDir: %v", err)
	}

	// Missing required file fails seeding.
	missingFileDir := t.TempDir()
	v1Dir := filepath.Join(missingFileDir, "coder", "v1")
	if err := os.MkdirAll(v1Dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1Dir, "systemprompt.md"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reg.seed(t.Context(), missingFileDir); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("expected missing required file error, got %v", err)
	}

	// Invalid output_contract in agent seed fails seeding.
	badContractDir := t.TempDir()
	v1Bad := filepath.Join(badContractDir, "judge", "v1")
	if err := os.MkdirAll(v1Bad, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1Bad, "systemprompt.md"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1Bad, "agent.json"), []byte(`{"output_contract":{"format":"bad"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reg.seed(t.Context(), badContractDir); err == nil {
		t.Fatal("expected invalid output_contract error")
	}

	// seedWithRetry returns error immediately when deadline already passed.
	if err := reg.seedWithRetry(t.Context(), badContractDir, 0); err == nil {
		t.Fatal("expected seedWithRetry error when deadline is 0")
	}

	// Invalid skill front matter in openFiles seed fails seeding.
	skillReg := &registry{
		prefix:    "skills",
		files:     []string{"SKILL.md"},
		openFiles: true,
		blobs:     blobClient{base: blobSrv.URL, client: http.DefaultClient},
		logger:    slog.New(slog.DiscardHandler),
	}
	badSkillDir := t.TempDir()
	v1Skill := filepath.Join(badSkillDir, "git", "v1")
	if err := os.MkdirAll(v1Skill, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1Skill, "SKILL.md"), []byte("no front matter"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := skillReg.seed(t.Context(), badSkillDir); err == nil {
		t.Fatal("expected invalid skill front matter error")
	}
}

func TestBlobClientDoErrorsAndValidPolicyID(t *testing.T) {
	badURL := blobClient{base: "://invalid", client: http.DefaultClient}
	if _, err := badURL.list(t.Context(), "agents"); err == nil {
		t.Fatal("expected url.Parse error")
	}

	closedSrv := httptest.NewServer(http.NotFoundHandler())
	closedURL := closedSrv.URL
	closedSrv.Close()
	unreachable := blobClient{base: closedURL, client: http.DefaultClient}
	if _, err := unreachable.list(t.Context(), "agents"); err == nil {
		t.Fatal("expected connection error")
	}

	for _, id := range []string{"a/b/c/d/e", "latest", "group/v1", ""} {
		if validPolicyID(id) {
			t.Errorf("validPolicyID(%q) = true, want false", id)
		}
	}
}

func TestValidationUpstreamErrors(t *testing.T) {
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream failure", http.StatusInternalServerError)
	}))
	t.Cleanup(errSrv.Close)

	reg := &registry{
		prefix:      "agents",
		files:       []string{"systemprompt.md", "agent.json"},
		policiesURL: errSrv.URL,
		skillsURL:   errSrv.URL,
		http:        http.DefaultClient,
		logger:      slog.New(slog.DiscardHandler),
	}

	if err := reg.validatePolicyReferences(t.Context(), "a {{policy::sec}} and {{policy::sec}}"); err == nil {
		t.Fatal("expected policy upstream error")
	}
	if err := reg.validateSkillReferences(t.Context(), []byte(`{"skills":["git@v1"]}`)); err == nil {
		t.Fatal("expected skill upstream error")
	}
	if err := reg.validateSkillReferences(t.Context(), []byte(`not-json`)); err == nil {
		t.Fatal("expected invalid JSON error")
	}
	if err := reg.validateSkillReferences(t.Context(), []byte(`{"skills":"not-a-list"}`)); err == nil {
		t.Fatal("expected invalid skills list error")
	}
	if err := validateModelSettings([]byte(`not-json`)); err == nil {
		t.Fatal("expected invalid JSON error from validateModelSettings")
	}
	if err := validateUnpackInputs([]byte(`not-json`)); err == nil {
		t.Fatal("expected invalid JSON error from validateUnpackInputs")
	}
}
