package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTPUpstreamForwardsHeadersAndStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Mcp-Method") != "tools/call" {
			t.Fatalf("unexpected request: %s %q", r.Method, r.Header.Get("Mcp-Method"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\\\"jsonrpc\\\":\\\"2.0\\\"}\\n\\n")
	}))
	defer server.Close()
	u, err := newUpstream(&Connection{Transport: "streamable-http", URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := u.RoundTrip(context.Background(), []byte(`{"jsonrpc":"2.0"}`), http.Header{"Mcp-Method": {"tools/call"}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", response.Header.Get("Content-Type"))
	}
	got, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(got), "jsonrpc") {
		t.Fatalf("body %q", got)
	}
}

func TestHTTPUpstreamDoesNotFollowRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	u, err := newUpstream(&Connection{Transport: "streamable-http", URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := u.RoundTrip(context.Background(), []byte(`{"jsonrpc":"2.0"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)
	if response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status %d", response.StatusCode)
	}
}

func TestStdioUpstreamConcurrentRequestsAndIDRestore(t *testing.T) {
	u := helperUpstream(t, "echo")
	defer closeUpstream(t, u)
	const calls = 24
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(`{"jsonrpc":"2.0","id":` + string(rune('0'+i%10)) + `,"method":"tools/call","params":{"name":"echo"}}`)
			response, err := u.RoundTrip(context.Background(), body, nil)
			if err != nil {
				errs <- err
				return
			}
			defer closeResponse(t, response)
			var message struct {
				Result map[string]any `json:"result"`
				ID     int            `json:"id"`
			}
			if err := json.NewDecoder(response.Body).Decode(&message); err != nil || message.ID != i%10 || message.Result["echo"] == nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStdioUpstreamForwardsRequestScopedProgressAsSSE(t *testing.T) {
	u := helperUpstream(t, "progress")
	defer closeUpstream(t, u)
	body := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","_meta":{"progressToken":"job-7"}}}`)
	response, err := u.RoundTrip(context.Background(), body, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", response.Header.Get("Content-Type"))
	}
	stream, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSpace(string(stream)), "\n\n")
	if len(parts) != 2 || !strings.Contains(parts[0], "notifications/progress") || !strings.Contains(parts[1], `"id":7`) {
		t.Fatalf("unexpected stream %q", stream)
	}
}

func TestStdioUpstreamSendsCancellationAndCleansUp(t *testing.T) {
	u := helperUpstream(t, "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cancelled, err := u.RoundTrip(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait"}}`), nil)
	closeResponse(t, cancelled)
	if err == nil {
		t.Fatal("expected cancellation")
	}
	response, err := u.RoundTrip(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)
	var message struct {
		Result struct {
			Cancelled bool `json:"cancelled"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&message); err != nil {
		t.Fatal(err)
	}
	if !message.Result.Cancelled {
		t.Fatal("helper did not receive notifications/cancelled")
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStdioUpstreamMapsExternalCancellation(t *testing.T) {
	u := helperUpstream(t, "wait")
	defer closeUpstream(t, u)
	done := make(chan error, 1)
	go func() {
		response, err := u.RoundTrip(context.Background(), []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"wait"}}`), nil)
		closeResponse(t, response)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	accepted, err := u.RoundTrip(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":9}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	closeResponse(t, accepted)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tool call was not cancelled")
	}
}

func TestStdioUpstreamIgnoresAmbiguousExternalCancellation(t *testing.T) {
	u := helperUpstream(t, "wait")
	defer closeUpstream(t, u)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			response, err := u.RoundTrip(ctx, []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"wait"}}`), nil)
			closeResponse(t, response)
			results <- err
		}()
	}
	time.Sleep(100 * time.Millisecond)
	accepted, err := u.RoundTrip(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":9}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	closeResponse(t, accepted)
	for range 2 {
		if err := <-results; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want deadline exceeded", err)
		}
	}
}

func TestStdioUpstreamCancelsBlockedWrite(t *testing.T) {
	u := helperUpstream(t, "no-read")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	large := strings.Repeat("x", 1<<20)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"payload":"` + large + `"}}}`)
	started := time.Now()
	response, err := u.RoundTrip(ctx, body, nil)
	closeResponse(t, response)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("blocked stdio write did not stop promptly")
	}
}

func TestStdioUpstreamRestartsAfterExit(t *testing.T) {
	u := helperUpstream(t, "echo")
	defer closeUpstream(t, u)
	exited, err := u.RoundTrip(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exit"}}`), nil)
	closeResponse(t, exited)
	if err == nil {
		t.Fatal("expected process exit")
	}
	response, err := u.RoundTrip(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	closeResponse(t, response)
}

func TestStdioUpstreamClosePreventsRespawn(t *testing.T) {
	u := helperUpstream(t, "echo")
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := u.RoundTrip(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`), nil)
	closeResponse(t, response)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("got %v, want closed error", err)
	}
}

func TestStdioUpstreamRejectsWrongInitializationVersion(t *testing.T) {
	u := helperUpstream(t, "legacy")
	defer closeUpstream(t, u)
	response, err := u.RoundTrip(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`), nil)
	closeResponse(t, response)
	if err == nil || !strings.Contains(err.Error(), "negotiate") {
		t.Fatalf("got %v, want initialization error", err)
	}
}

func TestStdioUpstreamDeleteAndProcessHelpers(t *testing.T) {
	u := &stdioUpstream{}
	resp, err := u.Delete(context.Background(), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	closeResponse(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("Delete status = %d, want 405", resp.StatusCode)
	}

	// progressToken edge cases.
	for _, raw := range []string{
		`{}`,
		`{"params":{"_meta":{"progressToken":null}}}`,
		`{"params":{"_meta":{"progressToken":true}}}`,
		`{"params":{"_meta":{"progressToken":{}}}}`,
	} {
		var msg map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &msg)
		if tok := progressToken(msg); tok != nil {
			t.Errorf("progressToken(%s) = %s, want nil", raw, tok)
		}
	}
	var numMsg map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"params":{"_meta":{"progressToken":42}}}`), &numMsg)
	if tok := progressToken(numMsg); string(tok) != "42" {
		t.Fatalf("numeric progressToken = %q, want 42", tok)
	}

	// register duplicate progress token, remove, and closed process.
	p := &stdioProcess{owner: u, pending: map[string]stdioPending{}, done: make(chan struct{}), exited: make(chan struct{})}
	if err := p.register("id-1", stdioPending{progress: json.RawMessage(`"tok-1"`), finished: make(chan struct{})}); err != nil {
		t.Fatal(err)
	}
	if err := p.register("id-2", stdioPending{progress: json.RawMessage(`"tok-1"`), finished: make(chan struct{})}); err == nil {
		t.Fatal("expected duplicate progress token error")
	}
	if !p.remove("id-1") || p.remove("id-1") {
		t.Fatal("remove did not toggle presence")
	}
	p.closed = true
	if err := p.register("id-3", stdioPending{}); !errors.Is(err, errProcessClosed) {
		t.Fatalf("register on closed = %v", err)
	}

	// read ignores invalid JSON, non-string IDs, unknown IDs, and non-progress notifications.
	p2 := &stdioProcess{owner: u, pending: map[string]stdioPending{}, done: make(chan struct{}), exited: make(chan struct{})}
	input := strings.Join([]string{
		"not-json",
		`{"jsonrpc":"2.0","method":"notifications/other"}`,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`,
		`{"jsonrpc":"2.0","id":123,"result":{}}`,
		`{"jsonrpc":"2.0","id":"unknown","result":{}}`,
	}, "\n") + "\n"
	p2.read(strings.NewReader(input))

	// stop returns non-ErrClosed stdin close error.
	p3 := &stdioProcess{stdin: errWriteCloser{err: errors.New("close failed")}, exited: make(chan struct{})}
	if err := p3.stop(); err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("stop err = %v", err)
	}
}

type errWriteCloser struct{ err error }

func (errWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (e errWriteCloser) Close() error              { return e.err }

func TestStdioUpstreamCancelsStreamingProgressOnContextDone(t *testing.T) {
	u := helperUpstream(t, "wait")
	defer closeUpstream(t, u)
	ctx, cancel := context.WithCancel(context.Background())
	body := []byte(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"wait","_meta":{"progressToken":"stream-11"}}}`)
	resp, err := u.RoundTrip(ctx, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, resp)
	cancel()
	_, readErr := io.ReadAll(resp.Body)
	if !errors.Is(readErr, context.Canceled) {
		t.Fatalf("stream read err = %v, want context.Canceled", readErr)
	}
}

func helperUpstream(t *testing.T, mode string) *stdioUpstream {
	t.Helper()
	u, err := newUpstream(&Connection{Transport: "stdio", Command: os.Args[0], Args: []string{"-test.run=TestStdioHelperProcess", "--"}, Env: map[string]string{"GO_WANT_MCP_HELPER": "1", "MCP_HELPER_MODE": mode}})
	if err != nil {
		t.Fatal(err)
	}
	stdio, ok := u.(*stdioUpstream)
	if !ok {
		t.Fatalf("stdio transport built %T", u)
	}
	return stdio
}

func TestStdioHelperProcess(*testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		return
	}
	mode := os.Getenv("MCP_HELPER_MODE")
	scanner := bufio.NewScanner(os.Stdin)
	cancelled := false
	for scanner.Scan() {
		var message map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			os.Exit(2)
		}
		var method string
		_ = json.Unmarshal(message["method"], &method)
		if method == "notifications/cancelled" {
			cancelled = true
			continue
		}
		if method == "notifications/initialized" {
			continue
		}
		if method == "initialize" {
			var id any
			_ = json.Unmarshal(message["id"], &id)
			if mode == "legacy" {
				response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": "2026-07-28", "capabilities": map[string]any{"tools": map[string]any{}}}})
				_, _ = os.Stdout.Write(append(response, '\n'))
				continue
			}
			response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}})
			_, _ = os.Stdout.Write(append(response, '\n'))
			if mode == "no-read" {
				time.Sleep(time.Hour)
			}
			continue
		}
		var params struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(message["params"], &params)
		if params.Name == "exit" {
			os.Exit(3)
		}
		if mode == "wait" && params.Name == "wait" {
			continue
		}
		if mode == "progress" {
			var meta map[string]json.RawMessage
			_ = json.Unmarshal(message["params"], &meta)
			var requestMeta map[string]json.RawMessage
			_ = json.Unmarshal(meta["_meta"], &requestMeta)
			progress, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progressToken": requestMeta["progressToken"], "progress": 1}})
			_, _ = os.Stdout.Write(append(progress, '\n'))
		}
		var id any
		_ = json.Unmarshal(message["id"], &id)
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"echo": true, "cancelled": cancelled}})
		_, _ = os.Stdout.Write(append(response, '\n'))
	}
	os.Exit(0)
}
