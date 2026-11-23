package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPClientInitializationSessionAndCleanup(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != "session-1" || r.Header.Get("MCP-Protocol-Version") != mcpProtocolVersion {
				t.Errorf("DELETE headers = %v", r.Header)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		methods = append(methods, request.Method)
		if request.Method == "initialize" {
			if r.Header.Get("MCP-Protocol-Version") != "" || r.Header.Get("Mcp-Session-Id") != "" {
				t.Errorf("initialize headers = %v", r.Header)
			}
			w.Header().Set("Mcp-Session-Id", "session-1")
			writeRPCResult(w, request.ID, `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"test","version":"1"}}`)
			return
		}
		if r.Header.Get("MCP-Protocol-Version") != mcpProtocolVersion || r.Header.Get("Mcp-Session-Id") != "session-1" {
			t.Errorf("%s headers = %v", request.Method, r.Header)
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPCResult(w, request.ID, `{"tools":[]}`)
	}))
	defer server.Close()
	client := &mcpClient{url: server.URL, http: server.Client()}
	if err := client.connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.listTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "initialize,notifications/initialized,tools/list" {
		t.Fatalf("methods = %v", methods)
	}
}

func TestMCPClientRejectsWrongVersionOrMissingTools(t *testing.T) {
	for _, result := range []string{
		`{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}`,
		`{"protocolVersion":"2025-11-25","capabilities":{}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			writeRPCResult(w, request.ID, result)
		}))
		client := &mcpClient{url: server.URL, http: server.Client()}
		if err := client.connect(context.Background()); err == nil {
			t.Fatalf("accepted initialize result %s", result)
		}
		server.Close()
	}
}

func TestMCPClientCancelsActiveCall(t *testing.T) {
	cancelled := make(chan json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				RequestID json.RawMessage `json:"requestId"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-cancel")
			writeRPCResult(w, request.ID, `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "notifications/cancelled":
			cancelled <- request.Params.RequestID
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	client := &mcpClient{url: server.URL, http: server.Client()}
	if err := client.connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if _, _, err := client.callTool(ctx, "wait", json.RawMessage(`{}`)); err == nil {
		t.Fatal("cancelled call succeeded")
	}
	select {
	case id := <-cancelled:
		if string(id) != "2" {
			t.Fatalf("cancelled request id = %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive cancellation notification")
	}
}

func TestNextEventStopsAtMatchingResponse(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":99,\"result\":{}}\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[]}}\n\n" +
		"data: this would be invalid JSON\n\n"
	msg, err := nextEvent(strings.NewReader(stream), json.RawMessage("2"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(msg.ID) != "2" {
		t.Fatalf("id = %s", msg.ID)
	}
}

func TestModelToolNameDisambiguatesNormalizedCollisions(t *testing.T) {
	used := map[string]string{}
	first := modelToolName("docs/a.b", used)
	used[first] = "docs/a.b"
	second := modelToolName("docs/a:b", used)
	if first == second || len(second) > 64 {
		t.Fatalf("aliases = %q and %q", first, second)
	}
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(id)+`,"result":`+result+`}`)
}
