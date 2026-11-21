package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seen is what a fake backend received.
type seen struct {
	method, path, query, body, creator, protocol, session string
}

func fakeBackend(t *testing.T, got chan<- seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(body), r.Header.Get("X-Creator"), r.Header.Get("MCP-Protocol-Version"), r.Header.Get("Mcp-Session-Id")}
		w.Header().Set("Mcp-Session-Id", "upstream-session")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestUI(t *testing.T) (*httptest.Server, chan seen) {
	t.Helper()
	got := make(chan seen, 1)
	b := fakeBackend(t, got)
	h, err := newHandler(backends{orchestrator: b.URL, agents: b.URL, policies: b.URL, blobs: b.URL, llm: b.URL, mcp: b.URL, evaluations: b.URL, connectors: b.URL, gitHost: b.URL, agent: b.URL}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ui := httptest.NewServer(h)
	t.Cleanup(ui.Close)
	return ui, got
}

func TestProxyRewritesPaths(t *testing.T) {
	ui, got := newTestUI(t)
	cases := []struct {
		method, path, want, body string
	}{
		{"GET", "/api/orchestrator/workflows", "/api/workflows", ""},
		{"POST", "/api/orchestrator/workflows/intake/versions", "/api/workflows/intake/versions", `{"name":"intake","steps":[]}`},
		{"POST", "/api/orchestrator/workflows/intake/runs", "/api/workflows/intake/runs", "document"},
		{"GET", "/api/orchestrator/runs/abc", "/api/runs/abc", ""},
		{"GET", "/api/orchestrator/artifacts/runs/abc/intake", "/api/artifacts/runs/abc/intake", ""},
		{"POST", "/api/orchestrator/decisions", "/api/decisions", `{"step":"confirm"}`},
		{"GET", "/api/agents/agents", "/agents", ""},
		{"GET", "/api/agents/agents/intake/v1", "/agents/intake/v1", ""},
		{"POST", "/api/agents/agents/intake", "/agents/intake", `{"files":{}}`},
		{"GET", "/api/policies/policies", "/policies", ""},
		{"GET", "/api/evaluations/runs", "/api/runs", ""},
		{"POST", "/api/evaluations/sets", "/api/sets", `{"name":"baseline","run_ids":["abc"]}`},
		{"GET", "/api/connectors/git/repositories", "/api/git/repositories", ""},
		{"PUT", "/api/connectors/git/repositories/claims-build", "/api/git/repositories/claims-build", `{"kind":"plain","url":"http://git-host:8080/claims-build.git"}`},
		{"DELETE", "/api/connectors/devpods/abc", "/api/devpods/abc", ""},
		{"POST", "/api/connectors/devpods/abc/publish-requests/1/decision", "/api/devpods/abc/publish-requests/1/decision", `{"decision":"approve"}`},
		{"POST", "/api/policies/policies/security", "/policies/security", `{"files":{}}`},
		{"GET", "/api/blobs/list/", "/list/", ""},
		{"GET", "/api/blobs/list/agents", "/list/agents", ""},
		{"GET", "/api/blobs/trace/abc", "/trace/abc", ""},
		{"GET", "/api/blobs/blobs/runs/abc/input", "/blobs/runs/abc/input", ""},
		{"GET", "/api/blobs/meta/runs/abc/input", "/meta/runs/abc/input", ""},
		{"GET", "/api/llm/v1/models", "/v1/models", ""},
		{"GET", "/api/mcp-gateway/api/services", "/api/services", ""},
		{"PUT", "/api/mcp-gateway/api/services/context7", "/api/services/context7", `{"allowlist":[]}`},
		{"POST", "/api/mcp-gateway/api/services/context7/test-connection", "/api/services/context7/test-connection", ""},
		{"POST", "/api/mcp-gateway/mcp/context7", "/mcp/context7", `{"jsonrpc":"2.0"}`},
		{"DELETE", "/api/mcp-gateway/mcp/context7", "/mcp/context7", ""},
		{"GET", "/git/", "/", ""},
		{"GET", "/git/claims-review.git/info/refs", "/claims-review.git/info/refs", ""},
		{"POST", "/git/claims-review.git/git-receive-pack", "/claims-review.git/git-receive-pack", "pack data"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, ui.URL+c.path+"?x=1", strings.NewReader(c.body))
		req.Header.Set("X-Creator", "ada")
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		req.Header.Set("Mcp-Session-Id", "test-session")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("Mcp-Session-Id") != "upstream-session" {
			t.Fatal("UI proxy dropped upstream session header")
		}
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s %s: status %d", c.method, c.path, resp.StatusCode)
		}
		s := <-got
		want := seen{c.method, c.want, "x=1", c.body, "ada", "2025-11-25", "test-session"}
		if s != want {
			t.Errorf("%s %s: backend got %+v, want %+v", c.method, c.path, s, want)
		}
	}
}

func TestLLMProxyOnlyListsModels(t *testing.T) {
	ui, got := newTestUI(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/llm/v1/chat/completions"},
		{"POST", "/api/llm/v1/models"},
		{"GET", "/api/llm/v1/models/some-model"},
	} {
		req, _ := http.NewRequest(c.method, ui.URL+c.path, strings.NewReader(`{"model":"some-model"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("%s %s: status %d, want an error", c.method, c.path, resp.StatusCode)
		}
	}
	select {
	case s := <-got:
		t.Errorf("gateway reached: %+v", s)
	default:
	}
}

func TestBlobsProxyIsReadOnly(t *testing.T) {
	ui, got := newTestUI(t)
	for _, method := range []string{"PUT", "DELETE", "POST"} {
		req, _ := http.NewRequest(method, ui.URL+"/api/blobs/blobs/x", strings.NewReader("data"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", method, resp.StatusCode)
		}
	}
	select {
	case s := <-got:
		t.Errorf("backend reached: %+v", s)
	default:
	}
}

func TestBackendDownReturnsJSONError(t *testing.T) {
	h, err := newHandler(backends{orchestrator: "http://127.0.0.1:1", agents: "http://127.0.0.1:1", policies: "http://127.0.0.1:1", blobs: "http://127.0.0.1:1"}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/orchestrator/workflows", nil))
	var body map[string]string
	if rec.Code != http.StatusBadGateway || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body["error"] == "" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestServesIndexAndHealth(t *testing.T) {
	ui, _ := newTestUI(t)
	for path, want := range map[string]string{"/": "Software Factory", "/healthz": `"status":"ok"`, "/app.js": "function api("} {
		resp, err := http.Get(ui.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET %s: %d, body lacks %q", path, resp.StatusCode, want)
		}
	}
}

func TestBlobsProxyAllowsTaskUploads(t *testing.T) {
	ui, got := newTestUI(t)
	req, _ := http.NewRequest(http.MethodPut, ui.URL+"/api/blobs/blobs/tasks/demo/example.md", strings.NewReader("task"))
	if r2, _ := http.NewRequest(http.MethodPut, ui.URL+"/api/blobs/blobs/templates/tasks/t.md", strings.NewReader("tpl")); r2 != nil {
		resp, err := http.DefaultClient.Do(r2)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusMethodNotAllowed {
			t.Fatal("template upload rejected")
		}
		<-got
	}
	req.Header.Set("X-Creator", "ada")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		t.Fatal("task upload rejected")
	}
	s := <-got
	if s.method != http.MethodPut || s.path != "/blobs/tasks/demo/example.md" || s.body != "task" || s.creator != "ada" {
		t.Fatalf("backend got %+v", s)
	}
	// A task outside a collection is refused before it reaches the backend.
	flat, _ := http.NewRequest(http.MethodPut, ui.URL+"/api/blobs/blobs/tasks/loose.md", strings.NewReader("x"))
	if resp, err := http.DefaultClient.Do(flat); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("flat task upload: %d, want 400", resp.StatusCode)
		}
	}
	del, _ := http.NewRequest(http.MethodDelete, ui.URL+"/api/blobs/blobs/tasks/demo/example.md", nil)
	if resp, err := http.DefaultClient.Do(del); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode == http.StatusMethodNotAllowed {
			t.Fatal("task delete rejected")
		}
		if s := <-got; s.method != http.MethodDelete || s.path != "/blobs/tasks/demo/example.md" {
			t.Fatalf("backend got %+v", s)
		}
	}
	for _, path := range []string{"/api/blobs/blobs/tasks/", "/api/blobs/blobs/other/x"} {
		req, _ := http.NewRequest(http.MethodPut, ui.URL+path, strings.NewReader("x"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}

func TestAgentProxyForwardsOnlyChat(t *testing.T) {
	ui, got := newTestUI(t)
	body := `{"trace_id":"abc","step_id":"talk","session":"user-sessions/abc/talk","tab":"triage","agent":"triage@v1","message":"hello","created_by":"ada"}`
	resp, err := http.Post(ui.URL+"/api/agent/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("chat: status %d", resp.StatusCode)
	}
	if s := <-got; s.method != http.MethodPost || s.path != "/chat" || s.body != body {
		t.Fatalf("agent got %+v", s)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/agent/chat"},
		{"PUT", "/api/agent/chat"},
		{"DELETE", "/api/agent/chat"},
		{"POST", "/api/agent/chat/extra"},
		{"POST", "/api/agent/healthz"},
		{"GET", "/api/agent/healthz"},
		{"POST", "/api/agent/"},
	} {
		req, _ := http.NewRequest(c.method, ui.URL+c.path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("%s %s: status %d, want an error", c.method, c.path, resp.StatusCode)
		}
	}
	select {
	case s := <-got:
		t.Errorf("agent reached: %+v", s)
	default:
	}
}

// A chat turn may take minutes: the proxy waits for a slow answer and gives up only at its deadline.
func TestAgentChatProxyWaitsUntilItsDeadline(t *testing.T) {
	old := chatTimeout
	t.Cleanup(func() { chatTimeout = old })
	release := make(chan struct{})
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"reply":"done"}`))
	}))
	t.Cleanup(agent.Close)
	h, err := newHandler(backends{orchestrator: agent.URL, agents: agent.URL, policies: agent.URL, blobs: agent.URL, agent: agent.URL}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ui := httptest.NewServer(h)
	t.Cleanup(ui.Close)

	chatTimeout = 50 * time.Millisecond
	resp, err := http.Post(ui.URL+"/api/agent/chat", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var failure map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&failure)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || failure["error"] == "" {
		t.Fatalf("past the deadline: status %d %v, want 502 with an error", resp.StatusCode, failure)
	}

	chatTimeout = time.Minute
	go func() { time.Sleep(300 * time.Millisecond); close(release) }()
	resp, err = http.Post(ui.URL+"/api/agent/chat", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(reply) != `{"reply":"done"}` {
		t.Fatalf("slow answer: status %d %q", resp.StatusCode, reply)
	}
}

func TestBlobsProxyAllowsSessionFiles(t *testing.T) {
	ui, got := newTestUI(t)
	long := strings.Repeat("a", 100)
	for _, c := range []struct {
		method, path string
		allowed      bool
	}{
		{"PUT", "user-sessions/abc/talk/files/notes.md", true},
		{"DELETE", "user-sessions/abc/talk/files/notes.md", true},
		{"PUT", "user-sessions/abc/talk/files/data.json", true},
		{"PUT", "user-sessions/abc/talk/files/plain.txt", true},
		{"PUT", "user-sessions/abc/route/talk/files/input-intake.md", true}, // a routed child
		{"PUT", "user-sessions/abc/talk/files/" + long + ".md", true},
		{"PUT", "user-sessions/abc/talk/files/" + long + "a.md", false}, // name too long
		{"POST", "user-sessions/abc/talk/files/notes.md", false},
		{"PUT", "user-sessions/abc/talk/chats/main.json", false},
		{"DELETE", "user-sessions/abc/talk/chats/main.json", false},
		{"PUT", "user-sessions/abc/talk/completed.json", false},
		{"DELETE", "user-sessions/abc/talk/completed.json", false},
		{"PUT", "user-sessions/abc/talk/files/picture.png", false},
		{"PUT", "user-sessions/abc/talk/files/config.yaml", false},
		{"PUT", "user-sessions/abc/talk/files/.hidden.md", false},
		{"PUT", "user-sessions/abc/talk/files/sub/notes.md", false},
		{"PUT", "user-sessions/abc/talk/files/notes.md.exe", false},
		{"PUT", "user-sessions/abc/talk/files/", false},
		{"PUT", "user-sessions/abc/files/notes.md", false}, // no step
		{"PUT", "user-sessions/files/notes.md", false},
		{"PUT", "runs/abc/talk/files/notes.md", false},
		{"DELETE", "runs/abc/user-sessions/abc/talk/files/notes.md", false},
	} {
		req, _ := http.NewRequest(c.method, ui.URL+"/api/blobs/blobs/"+c.path, strings.NewReader("text"))
		req.Header.Set("X-Creator", "ada")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if !c.allowed {
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status %d, want 405", c.method, c.path, resp.StatusCode)
			}
			select {
			case s := <-got:
				t.Errorf("%s %s reached the blobstore: %+v", c.method, c.path, s)
			default:
			}
			continue
		}
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("%s %s: status %d", c.method, c.path, resp.StatusCode)
			continue
		}
		if s := <-got; s.method != c.method || s.path != "/blobs/"+c.path || s.creator != "ada" {
			t.Errorf("%s %s: blobstore got %+v", c.method, c.path, s)
		}
	}
}

// Coverage: encoded and malformed session paths never reach the blobstore as a write.
func TestBlobsProxyRejectsDisguisedSessionPaths(t *testing.T) {
	ui, got := newTestUI(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{
		"user-sessions/abc/talk/files/notes.md/extra",
		"user-sessions/abc/talk/files/notes.md/",
		"user-sessions/abc/talk/files/notes.md%0A",
		"user-sessions/abc/talk/files/notes.md%00",
		"user-sessions/abc/talk/files/sub%2Fnotes.md",
		"user-sessions/abc/talk/files/%2e%2e/completed.json",
		"user-sessions/abc/talk/files/..%2Fchats%2Fmain.json",
		"user-sessions/abc/talk/files/../chats/main.json",
		"user-sessions/abc/talk//files/notes.md",
		"user-sessions/abc/%2e%2e/files/notes.md",
		"user-sessions/abc/.hidden/files/notes.md",
		"User-Sessions/abc/talk/files/notes.md",
		"user-sessions/abc/talk/Files/notes.md",
		"user-sessions/abc/talk/files/notes.MD",
		"user-sessions/abc/talk/chats%2Ffiles/../main.json",
	} {
		for _, method := range []string{"PUT", "DELETE"} {
			req, err := http.NewRequest(method, ui.URL+"/api/blobs/blobs/"+path, strings.NewReader("text"))
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode < 300 {
				t.Errorf("%s %s: status %d, want a refusal", method, path, resp.StatusCode)
			}
			select {
			case s := <-got:
				t.Errorf("%s %s reached the blobstore: %+v", method, path, s)
			default:
			}
		}
	}
}

// The vendored Mermaid file is served from the UI itself, with nothing that would stop it
// from running, and may be cached only when the request names its version.
func TestServesVendoredMermaid(t *testing.T) {
	ui, _ := newTestUI(t)
	for query, want := range map[string]string{"": "no-store", "?v=1": "public, max-age=31536000, immutable"} {
		resp, err := http.Get(ui.URL + "/vendor/mermaid.min.js" + query)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "mermaid") {
			t.Fatalf("GET /vendor/mermaid.min.js%s: status %d, %d bytes", query, resp.StatusCode, len(body))
		}
		if got := resp.Header.Get("Cache-Control"); got != want {
			t.Errorf("Cache-Control%s = %q, want %q", query, got, want)
		}
		if !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
			t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
		}
		if csp := resp.Header.Get("Content-Security-Policy"); csp != "" {
			t.Errorf("unexpected Content-Security-Policy %q", csp)
		}
	}
	resp, err := http.Get(ui.URL + "/app.js?v=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("app.js Cache-Control = %q, want no-store", got)
	}
}
