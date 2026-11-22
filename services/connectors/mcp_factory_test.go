package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFactoryMCPRegistrationIsReadOnlyAndPreservesExisting(t *testing.T) {
	var saved map[string]any
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		status := http.StatusNotFound
		body := ""
		if r.Method == http.MethodPut {
			status = http.StatusNoContent
			data, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
		}
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	s := newService(config{mcpURL: "http://tool-gateway:8080/mcp"}, nil, client, nil)
	s.mcpGateway = "http://mcp-gateway"
	created, err := s.registerFactory(context.Background())
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if !reflect.DeepEqual(saved["allowlist"], []any{"read_trace", "read_trace_artifact"}) {
		t.Fatalf("allowlist=%v", saved["allowlist"])
	}
	denied := saved["denylist"].([]any)
	if len(denied) == 0 {
		t.Fatal("write denylist is empty")
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}

	calls = 0
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Status: "OK", Body: io.NopCloser(strings.NewReader(`{"operator":true}`)), Header: make(http.Header)}, nil
	})
	created, err = s.registerFactory(context.Background())
	if err != nil || created || calls != 1 {
		t.Fatalf("existing: created=%v calls=%d err=%v", created, calls, err)
	}

	calls = 0
	saved = nil
	old, _ := json.Marshal(map[string]any{"mcpconfig": map[string]any{"transport": "streamable-http", "url": "http://tool-gateway:8080/mcp"}, "allowlist": previousFactoryTools, "denylist": factoryDeniedTools})
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == http.MethodGet {
			return &http.Response{StatusCode: 200, Status: "OK", Body: io.NopCloser(strings.NewReader(string(old))), Header: make(http.Header)}, nil
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &saved)
		return &http.Response{StatusCode: 204, Status: "No Content", Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	created, err = s.registerFactory(context.Background())
	if err != nil || !created || calls != 2 {
		t.Fatalf("migration: changed=%v calls=%d err=%v", created, calls, err)
	}
	if !reflect.DeepEqual(saved["allowlist"], []any{"read_trace", "read_trace_artifact"}) {
		t.Fatalf("migrated allowlist=%v", saved["allowlist"])
	}
}
