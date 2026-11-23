// Test-only external MCP server. Never built into the gateway production image.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const version = "2025-11-25"

var mu sync.Mutex
var calls []string
var httpInitialized bool

func response(raw []byte, initialized *bool) map[string]any {
	var req struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name            string         `json:"name"`
			Cursor          string         `json:"cursor"`
			Arguments       map[string]any `json:"arguments"`
			ProtocolVersion string         `json:"protocolVersion"`
		} `json:"params"`
	}
	_ = json.Unmarshal(raw, &req)
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if req.Method == "initialize" {
		if req.Params.ProtocolVersion != version {
			out["error"] = map[string]any{"code": -32602, "message": "legacy initialize required"}
			return out
		}
		*initialized = true
		out["result"] = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}, "resources": map[string]any{}}, "serverInfo": map[string]string{"name": "mock", "version": "1"}}
		return out
	}
	if req.Method == "notifications/initialized" {
		return nil
	}
	if !*initialized {
		out["error"] = map[string]any{"code": -32002, "message": "initialize first"}
		return out
	}
	switch req.Method {
	case "tools/list":
		names := []string{"blocked", "unlisted"}
		result := map[string]any{}
		if req.Params.Cursor == "page2" {
			names = []string{"echo", "both", "stream"}
		} else {
			result["nextCursor"] = "page2"
		}
		tools := []any{}
		for _, name := range names {
			tools = append(tools, map[string]any{"name": name, "description": "Integration test tool", "inputSchema": map[string]any{"type": "object"}})
		}
		result["tools"] = tools
		out["result"] = result
	case "tools/call":
		mu.Lock()
		calls = append(calls, req.Params.Name)
		if path := os.Getenv("MOCK_CALLS_FILE"); path != "" {
			if root, err := os.OpenRoot(filepath.Dir(path)); err == nil {
				if f, err := root.OpenFile(filepath.Base(path), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
					_, _ = fmt.Fprintln(f, req.Params.Name)
					_ = f.Close()
				}
				_ = root.Close()
			}
		}
		mu.Unlock()
		out["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": req.Params.Arguments["text"]}}, "structuredContent": req.Params.Arguments}
	default:
		out["error"] = map[string]any{"code": -32601, "message": "unknown method"}
	}
	return out
}

func main() {
	addr := flag.String("http", "", "HTTP listen address; omit for stdio")
	flag.Parse()
	if *addr != "" {
		http.HandleFunc("/calls", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(calls)
		})
		http.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				if r.Header.Get("Mcp-Session-Id") != "test-session" {
					http.Error(w, "session required", 400)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.Method != http.MethodPost {
				http.Error(w, "POST required", 405)
				return
			}
			var raw json.RawMessage
			if json.NewDecoder(r.Body).Decode(&raw) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			var req struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &req)
			if req.Method != "initialize" && r.Header.Get("MCP-Protocol-Version") != version {
				http.Error(w, "legacy protocol header required", 400)
				return
			}
			if req.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "test-session" {
				http.Error(w, "session required", 400)
				return
			}
			mu.Lock()
			if req.Method == "initialize" {
				httpInitialized = false
			}
			if req.Method == "notifications/initialized" {
				httpInitialized = true
				mu.Unlock()
				w.WriteHeader(http.StatusAccepted)
				return
			}
			ready := httpInitialized
			mu.Unlock()
			if req.Method != "initialize" && !ready {
				http.Error(w, "initialized notification required", 400)
				return
			}
			initialized := true
			res := response(raw, &initialized)
			if req.Method == "initialize" {
				w.Header().Set("Mcp-Session-Id", "test-session")
			}
			if res == nil {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(res)
		})
		server := &http.Server{Addr: *addr, ReadHeaderTimeout: 10 * time.Second}
		if err := server.ListenAndServe(); err != nil {
			panic(err)
		}
		return
	}
	initialized := false
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		res := response(scanner.Bytes(), &initialized)
		if res != nil {
			_ = json.NewEncoder(os.Stdout).Encode(res)
		}
	}
}
