package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPolicyAndConfig(t *testing.T) {
	c := Config{MCPConfig: Connection{Transport: "streamable-http", URL: "http://localhost/mcp"}, Allowlist: []string{"yes", "both"}, Denylist: []string{"no", "both"}}
	for name, want := range map[string]bool{"yes": true, "no": false, "both": false, "unspecified": false, "": false} {
		if c.allowed(name) != want {
			t.Errorf("%s", name)
		}
	}
	for _, name := range []string{"../escape", "a/b", "", ".hidden"} {
		if validateName(name) == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for _, raw := range []string{`{}`, `{"mcpconfig":{"transport":"stdio"}}`, `{"mcpconfig":{"transport":"streamable-http","url":"file:///x"}}`, `{"mcpconfig":{"transport":"stdio","command":"x"},"allowlist":{}}`, `{"mcpconfig":{"transport":"stdio","command":"x"},"denylist":[""]}`} {
		if _, err := decodeConfig([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	c, err := decodeConfig([]byte(`{"mcpconfig":{"transport":"stdio","command":"x"}}`))
	if err != nil || c.allowed("x") {
		t.Fatal(c, err)
	}
}

func TestConfigurationFailures(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	fail := false
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "PUT":
			if fail && strings.HasSuffix(r.URL.Path, "denylist.json") {
				http.Error(w, "failed", 500)
				return
			}
			objects[r.URL.Path], _ = io.ReadAll(r.Body)
			w.WriteHeader(204)
		case "GET":
			if r.URL.Path == "/list/mcp-gateway" {
				writeJSON(w, 200, map[string]any{"directories": []string{"sample"}})
				return
			}
			b, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Write(b)
		}
	}))
	defer blob.Close()
	g := newGateway(blobStore{blob.URL, blob.Client()}, slog.Default())
	defer g.close()
	srv := httptest.NewServer(g.routes())
	defer srv.Close()
	c := Config{MCPConfig: Connection{Transport: "streamable-http", URL: "http://example.invalid/mcp"}, Allowlist: []string{"echo"}, Denylist: []string{}}
	requestJSON(t, "PUT", srv.URL+"/api/services/sample", c, 200)
	mu.Lock()
	fail = true
	mu.Unlock()
	c.Denylist = []string{"echo"}
	requestJSON(t, "PUT", srv.URL+"/api/services/sample", c, 502)
	if !g.get("sample").config.allowed("echo") {
		t.Fatal("failed save replaced runtime")
	}
	mu.Lock()
	delete(objects, "/blobs/mcp-gateway/sample/allowlist.json")
	mu.Unlock()
	g2 := newGateway(g.store, slog.Default())
	defer g2.close()
	if err := g2.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g2.get("sample") != nil || g2.invalid["sample"] == "" {
		t.Fatal("incomplete config activated")
	}
}

func requestJSON(t *testing.T, method, url string, value any, status int) []byte {
	t.Helper()
	data, _ := json.Marshal(value)
	req, _ := http.NewRequest(method, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != status {
		t.Fatalf("%s %s: %d want %d: %s", method, url, res.StatusCode, status, body)
	}
	return body
}

func mcpRequest(t *testing.T, url, method, name, cursor string, status int) []byte {
	t.Helper()
	init := initializeRequest("test-init")
	initReq, _ := http.NewRequest("POST", url, bytes.NewReader(init))
	initReq.Header = protocolHeaders(true)
	initRes, err := http.DefaultClient.Do(initReq)
	if err != nil {
		t.Fatal(err)
	}
	initBody, _ := io.ReadAll(initRes.Body)
	initRes.Body.Close()
	if initRes.StatusCode != http.StatusOK {
		if initRes.StatusCode == status {
			return initBody
		}
		t.Fatalf("initialize: %d", initRes.StatusCode)
	}
	session := initRes.Header.Get("Mcp-Session-Id")
	notify, _ := http.NewRequest("POST", url, bytes.NewReader(initializedNotification()))
	notify.Header = protocolHeaders(false)
	if session != "" {
		notify.Header.Set("Mcp-Session-Id", session)
	}
	notifyRes, err := http.DefaultClient.Do(notify)
	if err != nil {
		t.Fatal(err)
	}
	notifyRes.Body.Close()
	if notifyRes.StatusCode != http.StatusAccepted {
		t.Fatalf("initialized: %d", notifyRes.StatusCode)
	}
	payload := map[string]any{"jsonrpc": "2.0", "id": "connection-test", "method": method, "params": map[string]any{}}
	params := payload["params"].(map[string]any)
	if name != "" {
		params["name"] = name
		params["arguments"] = map[string]any{"text": "hello"}
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(data))
	req.Header = protocolHeaders(false)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != status {
		t.Fatalf("%s %s: %d want %d: %s", method, name, res.StatusCode, status, body)
	}
	return body
}

func buildFixture(t *testing.T, dir, target string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "server")
	cmd := exec.Command("go", "build", "-o", out, target)
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v %s", err, b)
	}
	return out
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func launch(t *testing.T, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var log bytes.Buffer
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		r, err := http.Get(url)
		if err == nil {
			r.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not start: %s", url)
}

func TestGatewayRealBlobstoreAndExternalServers(t *testing.T) {
	blobBin := buildFixture(t, "../blobstore", ".")
	blobAddr := freeAddr(t)
	launch(t, blobBin, "-addr", blobAddr, "-data", t.TempDir())
	blobURL := "http://" + blobAddr
	waitHTTP(t, blobURL+"/healthz")
	mockBin := buildFixture(t, ".", "./testdata/mock")
	mockAddr := freeAddr(t)
	launch(t, mockBin, "-http", mockAddr)
	mockURL := "http://" + mockAddr
	waitHTTP(t, mockURL+"/calls")
	store := blobStore{blobURL, &http.Client{Timeout: 5 * time.Second}}
	g := newGateway(store, slog.Default())
	if err := g.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer g.close()
	srv := httptest.NewServer(g.routes())
	defer srv.Close()
	callsFile := filepath.Join(t.TempDir(), "calls")
	connections := map[string]Connection{"http": {Transport: "streamable-http", URL: mockURL + "/mcp"}, "stdio": {Transport: "stdio", Command: mockBin, Env: map[string]string{"MOCK_CALLS_FILE": callsFile}}}
	for name, conn := range connections {
		t.Run(name, func(t *testing.T) {
			c := Config{MCPConfig: conn, Allowlist: []string{"echo", "both", "stream"}, Denylist: []string{"both", "blocked"}}
			requestJSON(t, "PUT", srv.URL+"/api/services/"+name, c, 200)
			requestJSON(t, "POST", srv.URL+"/api/services/"+name+"/test-connection", nil, 200)
			for _, file := range configFiles {
				requestJSON(t, "GET", blobURL+"/blobs/mcp-gateway/"+name+"/"+file, nil, 200)
			}
			path := srv.URL + "/mcp/" + name
			first := mcpRequest(t, path, "tools/list", "", "", 200)
			if !bytes.Contains(first, []byte(`"tools":[]`)) || !bytes.Contains(first, []byte(`"nextCursor":"page2"`)) {
				t.Fatalf("filtered pagination lost: %s", first)
			}
			second := mcpRequest(t, path, "tools/list", "", "page2", 200)
			if bytes.Contains(second, []byte(`"name":"both"`)) {
				t.Fatal(string(second))
			}
			for _, denied := range []string{"both", "blocked", "unlisted"} {
				mcpRequest(t, path, "tools/call", denied, "", 403)
			}
			result := mcpRequest(t, path, "tools/call", "echo", "", 200)
			if !bytes.Contains(result, []byte(`"structuredContent":{"text":"hello"}`)) {
				t.Fatal(string(result))
			}
			mcpRequest(t, path, "resources/list", "", "", 400)
			old := g.get(name).upstream
			c.Denylist = append(c.Denylist, "echo")
			requestJSON(t, "PUT", srv.URL+"/api/services/"+name, c, 200)
			if old != g.get(name).upstream {
				t.Fatal("policy update replaced transport")
			}
			mcpRequest(t, path, "tools/call", "echo", "", 403)
		})
	}
	// Only permitted calls reached either external server.
	calls := requestJSON(t, "GET", mockURL+"/calls", nil, 200)
	if string(bytes.TrimSpace(calls)) != `["echo"]` {
		t.Fatal(string(calls))
	}
	data, err := os.ReadFile(callsFile)
	if err != nil || string(data) != "echo\n" {
		t.Fatalf("stdio calls %q: %v", data, err)
	}
	// Persisted deny policies are in force after recreating the gateway runtime.
	g2 := newGateway(store, slog.Default())
	if err := g2.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer g2.close()
	srv2 := httptest.NewServer(g2.routes())
	defer srv2.Close()
	for name := range connections {
		mcpRequest(t, srv2.URL+"/mcp/"+name, "tools/call", "echo", "", 403)
		requestJSON(t, "DELETE", srv2.URL+"/api/services/"+name, nil, 204)
		mcpRequest(t, srv2.URL+"/mcp/"+name, "tools/list", "", "", 404)
	}
}

func TestSSEFilteringAndIncrementalDelivery(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","id":"connection-test","result":{"tools":[{"name":"echo"},{"name":"blocked"}],"nextCursor":"next"}}`)
		fmt.Fprintln(w)
		w.(http.Flusher).Flush()
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	u, err := newUpstream(Connection{Transport: "streamable-http", URL: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	g := newGateway(blobStore{}, slog.Default())
	g.entries["test"] = &entry{Config{Allowlist: []string{"echo"}}, u}
	defer g.close()
	srv := httptest.NewServer(g.routes())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	payload := []byte(`{"jsonrpc":"2.0","id":"connection-test","method":"tools/list","params":{}}`)
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/mcp/test", bytes.NewReader(payload))
	req.Header = protocolHeaders(false)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	result, err := readRPCResponse(res, json.RawMessage(`"connection-test"`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(result["tools"], []byte("blocked")) || !bytes.Contains(result["tools"], []byte("echo")) {
		t.Fatal(string(result["tools"]))
	}
}

func TestRequestValidation(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	h := protocolHeaders(false)
	if _, _, err := parseRequest(data, h); err != nil {
		t.Fatal(err)
	}
	h = protocolHeaders(false)
	h.Set("MCP-Protocol-Version", "2025-06-18")
	if _, _, err := parseRequest(data, h); err == nil {
		t.Fatal("accepted wrong version")
	}
	if _, _, err := parseRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28"}}`), protocolHeaders(true)); err == nil {
		t.Fatal("accepted modern initialize")
	}
}

func TestProxyLogsCorrelatedToolOutcomeWithoutArguments(t *testing.T) {
	var upstreamHeaders http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"private failure detail"}]}}`)
	}))
	defer up.Close()
	u, err := newUpstream(Connection{Transport: "streamable-http", URL: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	g := newGateway(blobStore{}, slog.New(slog.NewJSONHandler(&logs, nil)))
	g.entries["docs"] = &entry{config: Config{Allowlist: []string{"lookup"}}, upstream: u}
	defer g.close()
	srv := httptest.NewServer(g.routes())
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"secret":"do not log"}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/docs", strings.NewReader(body))
	req.Header = protocolHeaders(false)
	req.Header.Set("Traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	req.Header.Set("X-Trace-Id", "run-123")
	req.Header.Set("X-Step-Id", "research")
	req.Header.Set("X-Agent-Ref", "test/mcp@v1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if upstreamHeaders.Get("Traceparent") != req.Header.Get("Traceparent") || upstreamHeaders.Get("X-Trace-Id") != "run-123" {
		t.Fatalf("correlation not forwarded: %v", upstreamHeaders)
	}
	line := logs.String()
	for _, want := range []string{`"service":"docs"`, `"method":"tools/call"`, `"tool":"lookup"`, `"outcome":"error"`, `"run_id":"run-123"`, `"step":"research"`, `"agent":"test/mcp@v1"`, `"trace_id":"0123456789abcdef0123456789abcdef"`, `"span_id":"0123456789abcdef"`} {
		if !strings.Contains(line, want) {
			t.Errorf("log missing %s: %s", want, line)
		}
	}
	for _, secret := range []string{"do not log", "private failure detail"} {
		if strings.Contains(line, secret) {
			t.Errorf("log leaked %q: %s", secret, line)
		}
	}
}

func TestResponseOutcome(t *testing.T) {
	for _, test := range []struct{ body, method, outcome string }{
		{`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`, "tools/call", "success"},
		{`{"jsonrpc":"2.0","id":1,"result":{"isError":true}}`, "tools/call", "error"},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"failed"}}`, "tools/list", "error"},
	} {
		got, _ := responseOutcome([]byte(test.body), test.method)
		if got != test.outcome {
			t.Errorf("%s: got %s want %s", test.body, got, test.outcome)
		}
	}
}

func TestResponseOutcomeDoesNotLogJSONRPCMessage(t *testing.T) {
	outcome, detail := responseOutcome([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"token secret-value rejected"}}`), "tools/call")
	if outcome != "error" || detail != "JSON-RPC error -32000" {
		t.Fatalf("outcome/detail = %q / %q", outcome, detail)
	}
	if strings.Contains(detail, "secret-value") {
		t.Fatalf("JSON-RPC response message leaked into log detail: %q", detail)
	}
}

func TestHTTPGatewayForwardsAndClosesSession(t *testing.T) {
	var deleted bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != "s1" {
				t.Fatalf("delete session %q", r.Header.Get("Mcp-Session-Id"))
			}
			deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method %s", r.Method)
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "s1")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": "test-init", "result": map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}})
			return
		}
		if r.Header.Get("MCP-Protocol-Version") != protocolVersion || r.Header.Get("Mcp-Session-Id") != "s1" {
			t.Fatalf("missing legacy session/header")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	u, err := newUpstream(Connection{Transport: "streamable-http", URL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	g := newGateway(blobStore{}, slog.New(slog.DiscardHandler))
	g.entries["remote"] = &entry{config: Config{MCPConfig: Connection{Transport: "streamable-http", URL: upstream.URL}}, upstream: u}
	defer g.close()
	srv := httptest.NewServer(g.routes())
	defer srv.Close()
	init := initializeRequest("test-init")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/remote", bytes.NewReader(init))
	req.Header = protocolHeaders(true)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Mcp-Session-Id") != "s1" {
		t.Fatal("session was not returned")
	}
	closeReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/mcp/remote", nil)
	closeReq.Header.Set("Mcp-Session-Id", "s1")
	closeRes, err := http.DefaultClient.Do(closeReq)
	if err != nil {
		t.Fatal(err)
	}
	closeRes.Body.Close()
	if closeRes.StatusCode != http.StatusNoContent || !deleted {
		t.Fatalf("delete status=%d deleted=%v", closeRes.StatusCode, deleted)
	}
	getRes, err := http.Get(srv.URL + "/mcp/remote")
	if err != nil {
		t.Fatal(err)
	}
	getRes.Body.Close()
	if getRes.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status %d", getRes.StatusCode)
	}
}
