package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"
	"time"
)

const (
	protocolVersion = "2025-11-25"
	maxResponse     = 16 << 20

	methodInitialize  = "initialize"
	methodInitialized = "notifications/initialized"
	methodCancelled   = "notifications/cancelled"
	methodToolsList   = "tools/list"
	methodToolsCall   = "tools/call"

	outcomeSuccess = "success"
	outcomeError   = "error"

	eventStream = "text/event-stream"
)

type rpcRequest struct {
	JSONRPC string                     `json:"jsonrpc"`
	Method  string                     `json:"method"`
	Params  map[string]json.RawMessage `json:"params"`
	ID      json.RawMessage            `json:"id"`
}

func protocolHeaders(initializing bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/event-stream")
	if !initializing {
		h.Set("MCP-Protocol-Version", protocolVersion)
	}
	return h
}

func initializeRequest(id string) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": methodInitialize, "params": map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "mcp-gateway", "version": "1"}}})
	return b
}

func initializedNotification() []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": methodInitialized})
	return b
}

func parseRequest(data []byte, h http.Header) (rpcRequest, string, error) {
	var r rpcRequest
	if err := json.Unmarshal(data, &r); err != nil {
		return r, "", fmt.Errorf("invalid JSON-RPC request")
	}
	if r.JSONRPC != "2.0" || r.Method == "" {
		return r, "", fmt.Errorf("a JSON-RPC 2.0 message is required")
	}
	if r.Method != methodInitialized && r.Method != methodCancelled {
		if len(r.ID) == 0 || string(r.ID) == "null" || !json.Valid(r.ID) {
			return r, "", fmt.Errorf("a JSON-RPC request id is required")
		}
		var id any
		if json.Unmarshal(r.ID, &id) != nil {
			return r, "", fmt.Errorf("invalid JSON-RPC request id")
		}
		switch id.(type) {
		case string, float64:
		default:
			return r, "", fmt.Errorf("id must be a string or number")
		}
	}
	if r.Method == methodInitialize {
		var version string
		if json.Unmarshal(r.Params["protocolVersion"], &version) != nil || version != protocolVersion {
			return r, "", fmt.Errorf("only MCP %s is supported", protocolVersion)
		}
		if header := h.Get("MCP-Protocol-Version"); header != "" && header != protocolVersion {
			return r, "", fmt.Errorf("MCP-Protocol-Version must be %s when supplied", protocolVersion)
		}
	} else if h.Get("MCP-Protocol-Version") != protocolVersion {
		return r, "", fmt.Errorf("MCP-Protocol-Version %s is required after initialize", protocolVersion)
	}
	name := ""
	if r.Method == methodToolsCall && (json.Unmarshal(r.Params["name"], &name) != nil || name == "") {
		return r, "", fmt.Errorf("tool name is required")
	}
	return r, name, nil
}

func rpcError(w http.ResponseWriter, status, code int, id json.RawMessage, err error) {
	if !json.Valid(id) {
		id = json.RawMessage("null")
	}
	writeJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": err.Error()}})
}

func (g *gateway) proxy(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	service := r.PathValue("name")
	method, tool, outcome, detail := "invalid", "", outcomeError, ""
	status := http.StatusOK
	defer func() {
		attrs := []any{"service", service, "method", method, "outcome", outcome, "status", status, "duration_ms", time.Since(started).Milliseconds()}
		if tool != "" {
			attrs = append(attrs, "tool", tool)
		}
		attrs = append(attrs, correlationLogAttrs(r.Header)...)
		if detail != "" {
			attrs = append(attrs, "error", detail)
		}
		if outcome == outcomeSuccess {
			g.log.Info("MCP request completed", attrs...)
		} else {
			g.log.Warn("MCP request completed", attrs...)
		}
	}()
	fail := func(httpStatus, code int, id json.RawMessage, err error) {
		status, detail = httpStatus, err.Error()
		rpcError(w, httpStatus, code, id, err)
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		fail(400, -32600, nil, err)
		return
	}
	req, name, err := parseRequest(data, r.Header)
	method, tool = req.Method, name
	if err != nil {
		fail(400, -32600, req.ID, err)
		return
	}
	e := g.get(service)
	if e == nil {
		fail(404, -32602, req.ID, errMissing)
		return
	}
	switch req.Method {
	case methodInitialize, methodInitialized, methodCancelled, methodToolsList:
	case methodToolsCall:
		if !e.config.allowed(name) {
			fail(403, -32602, req.ID, fmt.Errorf("tool %q is denied", name))
			return
		}
	default:
		fail(400, -32601, req.ID, fmt.Errorf("unsupported method %q", req.Method))
		return
	}
	ctx := r.Context()
	skillCall := service == "skills" && req.Method == methodToolsCall
	// The connector owns the skill script deadline and returns the retained output on timeout.
	if !skillCall || tool != "run_skill_script" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	headers := protocolHeaders(req.Method == methodInitialize)
	copyCorrelationHeaders(headers, r.Header)
	if session := r.Header.Get("Mcp-Session-Id"); session != "" {
		headers.Set("Mcp-Session-Id", session)
	}
	resp, err := e.upstream.RoundTrip(ctx, data, headers)
	if err != nil {
		fail(502, -32603, req.ID, err)
		return
	}
	defer g.closeResponse(resp, service)
	if session := resp.Header.Get("Mcp-Session-Id"); session != "" {
		w.Header().Set("Mcp-Session-Id", session)
	}
	if resp.StatusCode == http.StatusAccepted {
		status, outcome = http.StatusAccepted, outcomeSuccess
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if resp.StatusCode/100 != 2 {
		status, detail = resp.StatusCode, fmt.Sprintf("upstream HTTP %d", resp.StatusCode)
		copyErrorResponse(w, resp)
		return
	}
	typ, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	responseLimit := maxResponse
	if skillCall {
		responseLimit = math.MaxInt
	}
	if typ == eventStream {
		streamOutcome, streamDetail := outcomeSuccess, ""
		w.Header().Set("Content-Type", eventStream)
		w.Header().Set("Cache-Control", "no-cache")
		err = streamEvents(resp.Body, responseLimit, func(lines []string) error {
			filtered, filterErr := filterSSE(lines, req, &e.config)
			if filterErr != nil || filtered == nil {
				return filterErr
			}
			data := []string{}
			for _, line := range filtered {
				if strings.HasPrefix(line, "data:") {
					data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				}
			}
			if len(data) > 0 {
				if eventOutcome, eventDetail := responseOutcome([]byte(strings.Join(data, "\n")), req.Method); eventOutcome == outcomeError {
					streamOutcome, streamDetail = eventOutcome, eventDetail
				}
			}
			for _, line := range filtered {
				if _, writeErr := fmt.Fprintln(w, line); writeErr != nil {
					return writeErr
				}
			}
			_, _ = fmt.Fprintln(w)
			return http.NewResponseController(w).Flush()
		})
		if err != nil {
			status, detail = 502, err.Error()
			g.log.Warn("upstream stream interrupted", "service", r.PathValue("name"), "error", err)
			panic(http.ErrAbortHandler)
		}
		outcome, detail = streamOutcome, streamDetail
		return
	}
	if typ != "application/json" {
		fail(502, -32603, req.ID, fmt.Errorf("unsupported upstream content type"))
		return
	}
	var body []byte
	if skillCall {
		body, err = io.ReadAll(resp.Body)
	} else {
		body, err = readBounded(resp.Body)
	}
	if err == nil {
		body, err = filterResponse(body, req, &e.config)
	}
	if err != nil || body == nil {
		if err == nil {
			err = fmt.Errorf("upstream returned no response")
		}
		fail(502, -32603, req.ID, err)
		return
	}
	outcome, detail = responseOutcome(body, req.Method)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func copyCorrelationHeaders(dst, src http.Header) {
	for _, name := range []string{"Traceparent", "Tracestate", "X-Trace-Id", "X-Step-Id", "X-Agent-Ref"} {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}

func correlationLogAttrs(headers http.Header) []any {
	attrs := []any{}
	for _, item := range []struct{ header, key string }{{"X-Trace-Id", "run_id"}, {"X-Step-Id", "step"}, {"X-Agent-Ref", "agent"}} {
		if value := headers.Get(item.header); value != "" {
			attrs = append(attrs, item.key, value)
		}
	}
	parts := strings.Split(headers.Get("Traceparent"), "-")
	if len(parts) == 4 && len(parts[1]) == 32 && len(parts[2]) == 16 {
		attrs = append(attrs, "trace_id", parts[1], "span_id", parts[2])
	}
	return attrs
}

// responseOutcome classifies JSON-RPC and MCP tool errors without logging response bodies.
func responseOutcome(body []byte, method string) (outcome, detail string) {
	var msg struct {
		Error *struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &msg) != nil {
		return outcomeError, "invalid JSON-RPC response"
	}
	if msg.Error != nil {
		// The upstream message is response content and may contain tool output or secrets.
		// Keep the machine-readable code for diagnosis without copying the body into logs.
		return outcomeError, fmt.Sprintf("JSON-RPC error %d", msg.Error.Code)
	}
	if method == methodToolsCall && msg.Result.IsError {
		return outcomeError, "tool returned isError"
	}
	return outcomeSuccess, ""
}

func (g *gateway) closeSession(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	service, outcome, detail, status := r.PathValue("name"), outcomeError, "", http.StatusNoContent
	defer func() {
		attrs := []any{"service", service, "method", "session/close", "outcome", outcome, "status", status, "duration_ms", time.Since(started).Milliseconds()}
		attrs = append(attrs, correlationLogAttrs(r.Header)...)
		if detail != "" {
			attrs = append(attrs, "error", detail)
		}
		if outcome == outcomeSuccess {
			g.log.Info("MCP request completed", attrs...)
		} else {
			g.log.Warn("MCP request completed", attrs...)
		}
	}()
	e := g.get(service)
	if e == nil {
		status, detail = 404, errMissing.Error()
		apiError(w, 404, errMissing)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	headers := r.Header.Clone()
	headers.Set("MCP-Protocol-Version", protocolVersion)
	resp, err := e.upstream.Delete(ctx, headers)
	if err != nil {
		status, detail = 502, err.Error()
		apiError(w, 502, err)
		return
	}
	defer g.closeResponse(resp, service)
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusMethodNotAllowed {
		status, detail = resp.StatusCode, fmt.Sprintf("upstream HTTP %d", resp.StatusCode)
		copyErrorResponse(w, resp)
		return
	}
	status, outcome = resp.StatusCode, outcomeSuccess
	w.WriteHeader(resp.StatusCode)
}

func copyErrorResponse(w http.ResponseWriter, resp *http.Response) {
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if retry := resp.Header.Get("Retry-After"); retry != "" {
		w.Header().Set("Retry-After", retry)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxResponse))
}

func filterSSE(lines []string, req rpcRequest, c *Config) ([]string, error) {
	var dataLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(dataLines) == 0 {
		return lines, nil
	}
	filtered, err := filterResponse([]byte(strings.Join(dataLines, "\n")), req, c)
	if err != nil || filtered == nil {
		return nil, err
	}
	out := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		if !strings.HasPrefix(line, "data:") {
			out = append(out, line)
		}
	}
	return append(out, "data: "+string(filtered)), nil
}

func filterResponse(data []byte, req rpcRequest, c *Config) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(data, &msg); err != nil || msg == nil {
		return nil, fmt.Errorf("invalid upstream JSON-RPC response")
	}
	var version string
	if json.Unmarshal(msg["jsonrpc"], &version) != nil || version != "2.0" {
		return nil, fmt.Errorf("invalid upstream JSON-RPC version")
	}
	if method, ok := msg["method"]; ok {
		var m string
		_ = json.Unmarshal(method, &m)
		if m == "notifications/progress" && len(msg["id"]) == 0 {
			return data, nil
		}
		return nil, nil
	}
	if !sameID(msg["id"], req.ID) {
		return nil, fmt.Errorf("upstream response id mismatch")
	}
	if len(msg["error"]) > 0 {
		return data, nil
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(msg["result"], &result) != nil || result == nil {
		return nil, fmt.Errorf("upstream response missing result")
	}
	switch req.Method {
	case methodInitialize:
		if err := validateInitialize(result); err != nil {
			return nil, err
		}
		result["capabilities"], _ = json.Marshal(map[string]any{"tools": map[string]any{}})
	case methodToolsList:
		tools, err := toolList(result)
		if err != nil {
			return nil, err
		}
		kept := make([]json.RawMessage, 0, len(tools))
		for _, tool := range tools {
			var item struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(tool, &item)
			if c.allowed(item.Name) {
				kept = append(kept, tool)
			}
		}
		result["tools"], _ = json.Marshal(kept)
	}
	msg["result"], _ = json.Marshal(result)
	return json.Marshal(msg)
}

func toolList(result map[string]json.RawMessage) ([]json.RawMessage, error) {
	var tools []json.RawMessage
	if json.Unmarshal(result["tools"], &tools) != nil || tools == nil {
		return nil, fmt.Errorf("upstream tool list is invalid")
	}
	for _, tool := range tools {
		var item struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(tool, &item) != nil || item.Name == "" {
			return nil, fmt.Errorf("upstream tool is invalid")
		}
	}
	return tools, nil
}

func validateInitialize(result map[string]json.RawMessage) error {
	var version string
	if json.Unmarshal(result["protocolVersion"], &version) != nil || version != protocolVersion {
		return fmt.Errorf("upstream does not negotiate MCP %s", protocolVersion)
	}
	var capabilities map[string]json.RawMessage
	if json.Unmarshal(result["capabilities"], &capabilities) != nil || capabilities == nil {
		return fmt.Errorf("upstream initialization has invalid capabilities")
	}
	tools, ok := capabilities["tools"]
	if !ok || !json.Valid(tools) {
		return fmt.Errorf("upstream does not advertise tools capability")
	}
	var toolCapabilities map[string]json.RawMessage
	if json.Unmarshal(tools, &toolCapabilities) != nil || toolCapabilities == nil {
		return fmt.Errorf("upstream tools capability is invalid")
	}
	return nil
}

func sameID(a, b json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b)) }

func readBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxResponse+1))
	if len(b) > maxResponse {
		return nil, fmt.Errorf("upstream response exceeds limit")
	}
	return b, err
}

func streamEvents(r io.Reader, limit int, handle func([]string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), limit)
	lines := []string{}
	size := 0
	for scanner.Scan() {
		line := scanner.Text()
		size += len(line)
		if size > limit {
			return fmt.Errorf("SSE event exceeds limit")
		}
		if line == "" {
			if err := handle(lines); err != nil {
				return err
			}
			lines, size = nil, 0
		} else {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(lines) > 0 {
		return handle(lines)
	}
	return nil
}

func readRPCResponse(resp *http.Response, id json.RawMessage) (map[string]json.RawMessage, error) {
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("upstream HTTP %d", resp.StatusCode)
	}
	var result map[string]json.RawMessage
	consume := func(data []byte) error {
		var msg struct {
			ID     json.RawMessage            `json:"id"`
			Result map[string]json.RawMessage `json:"result"`
			Error  json.RawMessage            `json:"error"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			return err
		}
		if !sameID(msg.ID, id) {
			return nil
		}
		if len(msg.Error) > 0 {
			return fmt.Errorf("upstream MCP error: %s", msg.Error)
		}
		result = msg.Result
		return nil
	}
	typ, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var err error
	if typ == eventStream {
		done := fmt.Errorf("response complete")
		err = streamEvents(resp.Body, maxResponse, func(lines []string) error {
			var parts []string
			for _, line := range lines {
				if strings.HasPrefix(line, "data:") {
					parts = append(parts, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				}
			}
			if len(parts) == 0 {
				return nil
			}
			if consumeErr := consume([]byte(strings.Join(parts, "\n"))); consumeErr != nil {
				return consumeErr
			}
			if result != nil {
				return done
			}
			return nil
		})
		if err == done {
			err = nil
		}
	} else {
		var body []byte
		body, err = readBounded(resp.Body)
		if err == nil {
			err = consume(body)
		}
	}
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("upstream did not return a result")
	}
	return result, nil
}
