package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const mcpProtocolVersion = "2025-11-25"

type mcpClient struct {
	url         string
	service     string
	http        *http.Client
	nextID      atomic.Uint64
	session     string
	initialized bool
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *mcpClient) connect(ctx context.Context) error {
	raw, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "agent-service", "version": "1"},
	})
	if err != nil {
		return err
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if result.ProtocolVersion != mcpProtocolVersion || result.Capabilities.Tools == nil {
		return fmt.Errorf("initialize: server does not support MCP %s tools", mcpProtocolVersion)
	}
	c.initialized = true
	if err := c.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		c.initialized = false
		return err
	}
	return nil
}

func (c *mcpClient) listTools(ctx context.Context) ([]mcpTool, error) {
	var tools []mcpTool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		tools = append(tools, page.Tools...)
		if page.NextCursor == "" {
			return tools, nil
		}
		cursor = page.NextCursor
	}
}

func (c *mcpClient) callTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", false, err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", false, fmt.Errorf("tools/call: %w", err)
	}
	var b strings.Builder
	for _, item := range res.Content {
		if item.Type == "text" {
			b.WriteString(item.Text)
		}
	}
	return b.String(), res.IsError, nil
}

func (c *mcpClient) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	id := json.RawMessage(fmt.Sprint(c.nextID.Add(1)))
	payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	resp, err := c.post(ctx, payload)
	if err != nil {
		if ctx.Err() != nil && method != "initialize" {
			c.cancel(id)
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("mcp gateway: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	skillCall := c.service == skillsService && method == "tools/call"
	var msg *rpcMessage
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "text/event-stream" {
		limit := 16 << 20
		if skillCall {
			limit = math.MaxInt
		}
		msg, err = nextEvent(resp.Body, id, limit)
	} else {
		msg = &rpcMessage{}
		var body io.Reader = resp.Body
		if !skillCall {
			body = io.LimitReader(body, 16<<20)
		}
		err = json.NewDecoder(body).Decode(msg)
	}
	if err != nil {
		if ctx.Err() != nil && method != "initialize" {
			c.cancel(id)
		}
		return nil, fmt.Errorf("mcp %s: %w", method, err)
	}
	if msg == nil {
		return nil, fmt.Errorf("mcp %s: no response", method)
	}
	if msg.Error != nil {
		return nil, fmt.Errorf("mcp %s: %s", method, msg.Error.Message)
	}
	return msg.Result, nil
}

func (c *mcpClient) post(ctx context.Context, payload any) (*http.Response, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	setCorrelationHeaders(req)
	if c.initialized {
		req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp gateway: %w", err)
	}
	if session := resp.Header.Get("Mcp-Session-Id"); session != "" {
		c.session = session
	}
	return resp, nil
}

func (c *mcpClient) notify(ctx context.Context, method string, params map[string]any) error {
	resp, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("mcp gateway: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *mcpClient) cancel(id json.RawMessage) {
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	_ = c.notify(ctx, "notifications/cancelled", map[string]any{"requestId": id})
}

// close releases an upstream session when the server created one. A missing
// session and a session already forgotten by the server both require no action.
func (c *mcpClient) close() error {
	if c.session == "" {
		return nil
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	req.Header.Set("Mcp-Session-Id", c.session)
	setCorrelationHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode/100 == 2 {
		c.session = ""
		return nil
	}
	return fmt.Errorf("mcp gateway: DELETE session: %s", resp.Status)
}

func nextEvent(r io.Reader, id json.RawMessage, limit int) (*rpcMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), limit)
	var data []string
	flush := func() (*rpcMessage, error) {
		if len(data) == 0 {
			return nil, nil
		}
		var m rpcMessage
		err := json.Unmarshal([]byte(strings.Join(data, "\n")), &m)
		data = nil
		if err != nil {
			return nil, err
		}
		if bytes.Equal(m.ID, id) {
			return &m, nil
		}
		return nil, nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if msg, err := flush(); err != nil || msg != nil {
				return msg, err
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if msg, err := flush(); err != nil || msg != nil {
		return msg, err
	}
	return nil, errors.New("no matching message in event stream")
}
