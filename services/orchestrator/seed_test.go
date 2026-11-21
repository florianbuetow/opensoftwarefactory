package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSeedTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSeedWorkflowsPreservesExistingNamesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	seedDir := filepath.Join(dir, "seed")
	if err := os.Mkdir(seedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSeedTestFile(t, configPath, `workflows:
  - name: Flow
    version: 1
    description: original-v1
    steps: [{id: run, service: svc}]
  - name: Flow
    version: 2
    description: original-v2
    steps: [{id: run, service: svc}]
`)
	writeSeedTestFile(t, filepath.Join(seedDir, "b.yaml"), `workflows:
  - name: new-flow
    version: 2
    description: new-v2
    steps: [{id: run, service: svc}]
`)
	writeSeedTestFile(t, filepath.Join(seedDir, "a.json"), `{"workflows":[
  {"name":"flow","version":1,"description":"must-not-replace","steps":[{"id":"run","service":"svc"}]},
  {"name":"new-flow","version":1,"description":"new-v1","steps":[{"id":"run","service":"svc"}]}
]}`)
	writeSeedTestFile(t, filepath.Join(seedDir, "ignored.txt"), "not a workflow")

	if added, err := seedWorkflows(configPath, seedDir); err != nil || added != 2 {
		t.Fatalf("seeded %d versions: %v", added, err)
	}
	got, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Workflows) != 4 {
		t.Fatalf("workflows: %+v", got.Workflows)
	}
	if got.Workflows[0].Name != "Flow" || got.Workflows[0].Description != "original-v1" ||
		got.Workflows[1].Name != "Flow" || got.Workflows[1].Description != "original-v2" {
		t.Fatalf("existing versions changed: %+v", got.Workflows[:2])
	}
	if got.Workflows[2].Name != "new-flow" || got.Workflows[2].Version != 1 ||
		got.Workflows[3].Name != "new-flow" || got.Workflows[3].Version != 2 {
		t.Fatalf("seed order: %+v", got.Workflows[2:])
	}
	before, _ := os.ReadFile(configPath)
	if added, err := seedWorkflows(configPath, seedDir); err != nil || added != 0 {
		t.Fatalf("second seed added %d versions: %v", added, err)
	}
	after, _ := os.ReadFile(configPath)
	if string(after) != string(before) {
		t.Fatal("idempotent seed rewrote the configuration")
	}
}

func TestSeedWorkflowsPinsReferences(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	seedDir := filepath.Join(dir, "seed")
	if err := os.Mkdir(seedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSeedTestFile(t, configPath, "workflows: []\n")
	writeSeedTestFile(t, filepath.Join(seedDir, "nested.yaml"), `workflows:
  - name: leaf
    version: 1
    steps: [{id: run, service: svc}]
  - name: leaf
    version: 2
    steps: [{id: run, service: svc}]
  - name: root
    steps:
      - id: nested
        service: workflow
        workflow: leaf
`)
	if _, err := seedWorkflows(configPath, seedDir); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := got.find("root", 1)
	if !ok || root.Steps[0].Workflow != "leaf@v2" {
		t.Fatalf("root reference: %+v", root)
	}
}

func TestSeedWorkflowsRejectsInvalidSeedWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	seedDir := filepath.Join(dir, "seed")
	if err := os.Mkdir(seedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := "workflows:\n  - name: kept\n    steps: [{id: run, service: svc}]\n"
	writeSeedTestFile(t, configPath, original)
	writeSeedTestFile(t, filepath.Join(seedDir, "bad.yaml"), "workflows:\n  - name: broken\n    steps: []\n")
	if _, err := seedWorkflows(configPath, seedDir); err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Fatalf("error: %v", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatal("invalid seed changed the configuration")
	}
}

func TestSeedWorkflowsAllowsDisabledDirectory(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeSeedTestFile(t, configPath, "workflows: []\n")
	if added, err := seedWorkflows(configPath, ""); err != nil || added != 0 {
		t.Fatalf("disabled seed added %d: %v", added, err)
	}
}

func TestSeedWorkflowsRejectsMissingDirectoryWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	original := "workflows: []\n"
	writeSeedTestFile(t, configPath, original)
	if _, err := seedWorkflows(configPath, filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "workflow seed directory") {
		t.Fatalf("error: %v", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatal("missing seed directory changed the configuration")
	}
}
