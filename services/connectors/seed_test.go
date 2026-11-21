package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func repositoryFixture() []repository {
	return []repository{
		{Name: "alpha", Kind: "plain", URL: "http://git-host:8080/alpha.git", DefaultBranch: "main", IdleMinutes: 30, Credential: "none"},
		{Name: "beta", Kind: "plain", URL: "http://git-host:8080/beta.git", DefaultBranch: "main", IdleMinutes: 30, Credential: "none"},
		{Name: "gamma", Kind: "plain", URL: "http://git-host:8080/gamma.git", DefaultBranch: "main", IdleMinutes: 30, Credential: "none"},
	}
}

func TestRepositorySeedsAreAddedOnceAndNeverOverwritten(t *testing.T) {
	w := newWorld(t)
	// A person changed one of them before the service started.
	w.repository("beta", map[string]any{"url": "http://elsewhere:9000/beta.git", "idle_minutes": 5, "approve_publish": true})
	if err := w.s.seedRepositories(context.Background(), repositoryFixture()); err != nil {
		t.Fatal(err)
	}
	repos := w.list("/api/git/repositories")
	if len(repos) != 3 {
		t.Fatalf("repositories = %v", repos)
	}
	want := map[string]any{"name": "alpha", "kind": "plain", "url": "http://git-host:8080/alpha.git", "default_branch": "main",
		"setup_command": "", "idle_minutes": 30.0, "approve_publish": false, "credential": "none"}
	if !reflect.DeepEqual(repos[0], want) {
		t.Fatalf("alpha = %v", repos[0])
	}
	if repos[1]["url"] != "http://elsewhere:9000/beta.git" || repos[1]["idle_minutes"] != 5.0 || repos[1]["approve_publish"] != true {
		t.Fatalf("the existing entry was overwritten: %v", repos[1])
	}
	if repos[2]["name"] != "gamma" || repos[2]["url"] != "http://git-host:8080/gamma.git" {
		t.Fatalf("gamma = %v", repos[2])
	}
	// A second start changes nothing.
	before := map[string]string{}
	for _, repo := range repositoryFixture() {
		before[repo.Name] = w.blobs.get(repositoryObject(repo.Name))
	}
	if err := w.s.seedRepositories(context.Background(), repositoryFixture()); err != nil {
		t.Fatal(err)
	}
	for name, want := range before {
		if got := w.blobs.get(repositoryObject(name)); got != want {
			t.Fatalf("a second seeding changed %s: %s", name, got)
		}
	}
	// The blob store being down is an error, so the caller tries again.
	w.blobs.srv.Close()
	if err := w.s.seedRepositories(context.Background(), repositoryFixture()); err == nil {
		t.Fatal("seeding without a blob store reported success")
	}
}

func TestReadRepositorySeedsIsOptionalAndValidatesSyntheticFiles(t *testing.T) {
	if got, err := readRepositorySeeds(filepath.Join(t.TempDir(), "missing.json")); err != nil || got != nil {
		t.Fatalf("missing file: %#v, %v", got, err)
	}
	path := filepath.Join(t.TempDir(), "repositories.json")
	data := `{"repositories":[{"name":"demo","kind":"plain","url":"http://git-host:8080/demo.git","default_branch":"trunk","setup_command":"","idle_minutes":12,"approve_publish":false,"credential":"none"}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readRepositorySeeds(path)
	if err != nil || len(got) != 1 || got[0].Name != "demo" || got[0].DefaultBranch != "trunk" || got[0].IdleMinutes != 12 {
		t.Fatalf("seed fixture: %#v, %v", got, err)
	}
	if err = os.WriteFile(path, []byte(`{"repositories":[{"name":"bad","url":"not-a-url"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = readRepositorySeeds(path); err == nil {
		t.Fatal("invalid repository seed was accepted")
	}
}
