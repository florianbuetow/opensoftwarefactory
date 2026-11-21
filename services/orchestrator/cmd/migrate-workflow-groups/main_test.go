package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"opensoftwarefactory/orchestrator/pipeline"
)

const legacy = `workflows:
  - name: ECC-plan
    steps: [{id: a, service: svc}]
  - name: ex02-research
    steps: [{id: a, service: svc}]
  - name: EGM-intake
    steps: [{id: a, service: svc}]
  - name: EGM-intake
    version: 2
    steps: [{id: a, service: svc}]
  - name: workflow-1
    steps:
      - id: team
        service: team
        agent: demo/lead@v1
        workflows: [EGM-intake@v1]
        channels: [[lead, EGM-intake]]
        member_positions: {EGM-intake: {x: 1, y: 2}}
      - {id: nested, service: workflow, workflow: ECC-plan, depends_on: [team]}
  - name: kept/already-grouped
    steps: [{id: a, service: svc}]
`

func TestMigrateGroupsNamesAndRewritesReferences(t *testing.T) {
	got, err := migrate([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, w := range got {
		names = append(names, pipeline.WorkflowRef(w.Name, w.Version))
	}
	want := "ecc/ECC-plan@v1 examples/ex02-research@v1 egm/EGM-intake@v1 egm/EGM-intake@v2 default/workflow-1@v1 kept/already-grouped@v1"
	if strings.Join(names, " ") != want {
		t.Fatalf("names %s", strings.Join(names, " "))
	}
	team, nested := got[4].Steps[0], got[4].Steps[1]
	if team.Workflows[0] != "egm/EGM-intake@v1" || nested.Workflow != "ecc/ECC-plan@v1" {
		t.Errorf("references %q %q", team.Workflows[0], nested.Workflow)
	}
	// Members keep the last segment of their name, so channels and positions stay.
	if _, ok := team.MemberPositions["EGM-intake"]; team.Channels[0][1] != "EGM-intake" || !ok {
		t.Errorf("team %+v", team)
	}
}

// blobstore is a minimal /blobs server.
type blobstore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (b *blobstore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := strings.TrimPrefix(r.URL.Path, "/blobs/")
	switch r.Method {
	case http.MethodGet:
		data, ok := b.data[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	case http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		b.data[id] = data
	}
}

func TestStoreAllKeepsStoredVersions(t *testing.T) {
	blobs := &blobstore{data: map[string][]byte{"workflows/egm/EGM-intake/v1/workflow.json": []byte("edited")}}
	srv := httptest.NewServer(blobs)
	defer srv.Close()
	workflows, err := migrate([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	store := pipeline.HTTPStore{BaseURL: srv.URL + "/blobs", Client: srv.Client()}
	var out bytes.Buffer
	if err = storeAll(context.Background(), store, workflows, &out); err != nil {
		t.Fatal(err)
	}
	if string(blobs.data["workflows/egm/EGM-intake/v1/workflow.json"]) != "edited" {
		t.Error("a stored version was overwritten")
	}
	if len(blobs.data) != len(workflows) || !strings.Contains(out.String(), "kept    workflows/egm/EGM-intake/v1/workflow.json") {
		t.Errorf("stored %d objects:\n%s", len(blobs.data), out.String())
	}
	if !bytes.Contains(blobs.data["workflows/default/workflow-1/v1/workflow.json"], []byte(`"workflow": "ecc/ECC-plan@v1"`)) {
		t.Errorf("stored %s", blobs.data["workflows/default/workflow-1/v1/workflow.json"])
	}
}
