package main

import (
	"net/http"
	"reflect"
	"testing"
)

const describedVersions = `workflows:
  - name: flow
    version: 1
    description: first description
    created: "2026-01-02T03:04:05Z"
    created_by: first-author
    steps:
      - {id: first, service: svc}
  - name: flow
    version: 2
    description: second description
    created: "2026-02-03T04:05:06Z"
    created_by: second-author
    steps:
      - {id: first, service: svc}
      - {id: second, service: svc, depends_on: [first]}
`

func TestPatchWorkflowDescriptionChangesOnlySelectedVersion(t *testing.T) {
	srv, _, _, workflows := serverWith(t, describedVersions, nil)
	before, err := storedConfig(workflows)
	if err != nil {
		t.Fatal(err)
	}

	var updated workflowView
	if code := send(t, http.MethodPatch, srv.URL+"/api/workflows/flow/versions/1/description", "editor",
		`{"description":"clearer first description"}`, &updated); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if updated.Version != 1 || updated.Description != "clearer first description" || !reflect.DeepEqual(updated.Versions, []int{1, 2}) {
		t.Fatalf("updated view: %+v", updated)
	}

	after, err := storedConfig(workflows)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Workflows) != len(before.Workflows) {
		t.Fatalf("version count changed from %d to %d", len(before.Workflows), len(after.Workflows))
	}
	wantV1, _ := before.find("flow", 1)
	gotV1, _ := after.find("flow", 1)
	wantV1.Description = "clearer first description"
	if !reflect.DeepEqual(gotV1, wantV1) {
		t.Fatalf("v1 changed beyond its description:\n got: %+v\nwant: %+v", gotV1, wantV1)
	}
	wantV2, _ := before.find("flow", 2)
	gotV2, _ := after.find("flow", 2)
	if !reflect.DeepEqual(gotV2, wantV2) {
		t.Fatalf("v2 changed:\n got: %+v\nwant: %+v", gotV2, wantV2)
	}

	var cleared workflowView
	if code := send(t, http.MethodPatch, srv.URL+"/api/workflows/flow/versions/1/description", "editor",
		`{"description":""}`, &cleared); code != http.StatusOK || cleared.Description != "" || cleared.Version != 1 {
		t.Fatalf("clear description: status %d, view %+v", code, cleared)
	}
}

func TestPatchWorkflowDescriptionRejectsInvalidRequestsWithoutWriting(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{name: "missing description", path: "/api/workflows/flow/versions/1/description", body: `{}`},
		{name: "null description", path: "/api/workflows/flow/versions/1/description", body: `{"description":null}`},
		{name: "line break", path: "/api/workflows/flow/versions/1/description", body: `{"description":"first\nsecond"}`},
		{name: "carriage return", path: "/api/workflows/flow/versions/1/description", body: `{"description":"first\rsecond"}`},
		{name: "unicode line separator", path: "/api/workflows/flow/versions/1/description", body: `{"description":"first\u2028second"}`},
		{name: "next line", path: "/api/workflows/flow/versions/1/description", body: `{"description":"first\u0085second"}`},
		{name: "vertical tab", path: "/api/workflows/flow/versions/1/description", body: `{"description":"first\u000bsecond"}`},
		{name: "form feed", path: "/api/workflows/flow/versions/1/description", body: `{"description":"first\fsecond"}`},
		{name: "unknown field", path: "/api/workflows/flow/versions/1/description", body: `{"description":"x","steps":[]}`},
		{name: "trailing JSON", path: "/api/workflows/flow/versions/1/description", body: `{"description":"x"}{}`},
		{name: "malformed JSON", path: "/api/workflows/flow/versions/1/description", body: `{"description":`},
		{name: "zero version", path: "/api/workflows/flow/versions/0/description", body: `{"description":"x"}`},
		{name: "invalid version", path: "/api/workflows/flow/versions/v1/description", body: `{"description":"x"}`},
		{name: "unknown version", path: "/api/workflows/flow/versions/3/description", body: `{"description":"x"}`},
		{name: "unknown workflow", path: "/api/workflows/missing/versions/1/description", body: `{"description":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _, workflows := serverWith(t, describedVersions, nil)
			before, err := workflowObjects(workflows)
			if err != nil {
				t.Fatal(err)
			}
			code, _ := call(t, http.MethodPatch, srv.URL+tc.path, tc.body)
			if code != http.StatusBadRequest && code != http.StatusNotFound {
				t.Fatalf("status %d", code)
			}
			after, err := workflowObjects(workflows)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatal("rejected request changed the configuration")
			}
		})
	}
}

func TestNewWorkflowVersionRejectsDescriptionLineBreaks(t *testing.T) {
	srv, _, _, workflows := serverWith(t, describedVersions, nil)
	before, err := workflowObjects(workflows)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := call(t, http.MethodPost, srv.URL+"/api/workflows/flow/versions", `{"description":"first\nsecond","steps":[{"id":"first","service":"svc"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status %d", code)
	}
	after, err := workflowObjects(workflows)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid description changed stored versions: %v", err)
	}
}
