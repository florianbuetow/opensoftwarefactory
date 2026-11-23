package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSeedTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedDirectory returns a new, empty seed directory.
func seedDirectory(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "seed")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSeedWorkflowsPreservesExistingNamesAndIsIdempotent(t *testing.T) {
	seedDir := seedDirectory(t)
	store := storeWorkflows(t, `workflows:
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

	if added, err := seedWorkflows(context.Background(), store, seedDir); err != nil || added != 2 {
		t.Fatalf("seeded %d versions: %v", added, err)
	}
	got := store.config
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
	if stored, err := storedConfig(store); err != nil || len(stored.Workflows) != 4 {
		t.Fatalf("stored %d versions: %v", len(stored.Workflows), err)
	}
	before, _ := workflowObjects(store)
	if added, err := seedWorkflows(context.Background(), store, seedDir); err != nil || added != 0 {
		t.Fatalf("second seed added %d versions: %v", added, err)
	}
	after, _ := workflowObjects(store)
	if !bytes.Equal(after, before) {
		t.Fatal("idempotent seed rewrote the stored workflows")
	}
}

func TestSeedWorkflowsPinsReferences(t *testing.T) {
	seedDir := seedDirectory(t)
	store := storeWorkflows(t, "workflows: []\n")
	writeSeedTestFile(t, filepath.Join(seedDir, "nested.yaml"), `workflows:
  - name: lib/leaf
    version: 1
    steps: [{id: run, service: svc}]
  - name: lib/leaf
    version: 2
    steps: [{id: run, service: svc}]
  - name: root
    steps:
      - id: nested
        service: workflow
        workflow: lib/leaf
`)
	if _, err := seedWorkflows(context.Background(), store, seedDir); err != nil {
		t.Fatal(err)
	}
	got, err := storedConfig(store)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := got.find("root", 1)
	if !ok || root.Steps[0].Workflow != "lib/leaf@v2" {
		t.Fatalf("root reference: %+v", root)
	}
}

func TestSeedWorkflowsRejectsInvalidSeedWithoutWriting(t *testing.T) {
	seedDir := seedDirectory(t)
	store := storeWorkflows(t, "workflows:\n  - name: kept\n    steps: [{id: run, service: svc}]\n")
	before, _ := workflowObjects(store)
	writeSeedTestFile(t, filepath.Join(seedDir, "bad.yaml"), "workflows:\n  - name: broken\n    steps: []\n")
	if _, err := seedWorkflows(context.Background(), store, seedDir); err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Fatalf("error: %v", err)
	}
	if after, _ := workflowObjects(store); !bytes.Equal(after, before) {
		t.Fatal("invalid seed changed the stored workflows")
	}
}

func TestSeedWorkflowsAllowsDisabledDirectory(t *testing.T) {
	store := storeWorkflows(t, "workflows: []\n")
	if added, err := seedWorkflows(context.Background(), store, ""); err != nil || added != 0 {
		t.Fatalf("disabled seed added %d: %v", added, err)
	}
}

func TestSeedWorkflowsRejectsMissingDirectoryWithoutWriting(t *testing.T) {
	store := storeWorkflows(t, "workflows: []\n")
	if _, err := seedWorkflows(context.Background(), store, filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "workflow seed directory") {
		t.Fatalf("error: %v", err)
	}
	if after, _ := workflowObjects(store); len(after) != 0 {
		t.Fatal("missing seed directory changed the stored workflows")
	}
}
