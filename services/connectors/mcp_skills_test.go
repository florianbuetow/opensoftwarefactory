package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestRegisterSkillsCreatesOnlyTheSkillToolsService(t *testing.T) {
	var saved map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/services/skills" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&saved); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer gateway.Close()
	s := &service{mcpGateway: gateway.URL, http: http.DefaultClient, cfg: config{mcpURL: "http://tool-gateway:8080/mcp"}}
	created, err := s.registerSkills(context.Background())
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if !reflect.DeepEqual(saved["allowlist"], []any{"read_skill", "run_skill_script"}) {
		t.Fatalf("allowlist=%v", saved["allowlist"])
	}
	if !reflect.DeepEqual(saved["denylist"], []any{}) {
		t.Fatalf("denylist=%v", saved["denylist"])
	}
}
