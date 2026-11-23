package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type connectionUpstream struct {
	initialize string
	tools      string
}

func (u connectionUpstream) RoundTrip(_ context.Context, body []byte, _ http.Header) (*http.Response, error) {
	if bytes.Contains(body, []byte(`"method":"notifications/initialized"`)) {
		return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	result := u.tools
	if bytes.Contains(body, []byte(`"method":"initialize"`)) {
		result = u.initialize
	}
	responseBody := `{"jsonrpc":"2.0","id":"connection-test","result":` + result + `}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}, nil
}

func (connectionUpstream) Close() error { return nil }
func (connectionUpstream) Delete(context.Context, http.Header) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestConnectionValidatesToolsCapabilityAndList(t *testing.T) {
	validInitialize := `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}}}`
	cases := []struct {
		name       string
		initialize string
		tools      string
		status     int
	}{
		{"valid empty list", validInitialize, `{"tools":[]}`, http.StatusOK},
		{"missing tools capability", `{"protocolVersion":"2025-11-25","capabilities":{}}`, `{"tools":[]}`, http.StatusBadGateway},
		{"wrong negotiated version", `{"protocolVersion":"2026-07-28","capabilities":{"tools":{}}}`, `{"tools":[]}`, http.StatusBadGateway},
		{"missing tool list", validInitialize, `{}`, http.StatusBadGateway},
		{"unnamed tool", validInitialize, `{"tools":[{"description":"missing name"}]}`, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(blobStore{}, slog.New(slog.DiscardHandler))
			g.entries["sample"] = &entry{
				config:   Config{MCPConfig: Connection{Transport: "streamable-http", URL: "http://example.invalid"}},
				upstream: connectionUpstream{initialize: tc.initialize, tools: tc.tools},
			}
			req := httptest.NewRequest(http.MethodPost, "/api/services/sample/test-connection", http.NoBody)
			res := httptest.NewRecorder()
			g.routes().ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", res.Code, tc.status, res.Body.String())
			}
		})
	}
}

func TestLoadReportsInvalidStoredServiceName(t *testing.T) {
	name := strings.Repeat("a", 129)
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/list/mcp-gateway" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"directories":["`+name+`"]}`)
	}))
	defer blob.Close()
	g := newGateway(blobStore{base: blob.URL, client: blob.Client()}, slog.New(slog.DiscardHandler))
	if err := g.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.invalid[name], "invalid service name") {
		t.Fatalf("invalid service not reported: %v", g.invalid)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/services", http.NoBody)
	res := httptest.NewRecorder()
	g.routes().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !bytes.Contains(res.Body.Bytes(), []byte(name)) {
		t.Fatalf("list response = %d %s", res.Code, res.Body.String())
	}
}

func TestToolsListAllowsLegacyResult(t *testing.T) {
	req := rpcRequest{Method: "tools/list", ID: []byte(`1`)}
	message := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	if _, err := filterResponse(message, req, &Config{}); err != nil {
		t.Fatal(err)
	}
}
