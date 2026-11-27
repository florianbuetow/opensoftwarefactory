package pipeline

import (
	"encoding/json"
	"strings"
	"testing"
)

// A workflow may mark parts of its graph with labelled groups. They are optional, name
// only steps of the workflow, and change nothing about a workflow that has none.
func TestValidateGroups(t *testing.T) {
	base := func(groups ...Group) *Definition {
		return &Definition{Name: "w", Steps: []Step{work("a"), work("b", "a"), work("c", "b")}, Groups: groups}
	}
	for name, d := range map[string]*Definition{
		"none":        base(),
		"one":         base(Group{Label: "Input triage", Nodes: []string{"a"}}),
		"two":         base(Group{Label: "Input triage", Nodes: []string{"a", "b"}}, Group{Label: "Agentic review", Nodes: []string{"c"}}),
		"overlapping": base(Group{Label: "Specification refinement", Nodes: []string{"a", "b"}}, Group{Label: "Review", Nodes: []string{"b", "c"}}),
		"same label":  base(Group{Label: "Review", Nodes: []string{"a"}}, Group{Label: "Review", Nodes: []string{"c"}}),
	} {
		if err := Validate(d); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		want  string
		group Group
	}{
		"no label":      {"needs a label", Group{Nodes: []string{"a"}}},
		"padded label":  {"needs a label", Group{Label: " Review", Nodes: []string{"a"}}},
		"two lines":     {"needs a label", Group{Label: "Input\ntriage", Nodes: []string{"a"}}},
		"long label":    {"at most 80 characters", Group{Label: strings.Repeat("x", MaxGroupLabel+1), Nodes: []string{"a"}}},
		"no nodes":      {`group "Review" needs at least one node`, Group{Label: "Review"}},
		"unknown node":  {`names the unknown node "z"`, Group{Label: "Review", Nodes: []string{"a", "z"}}},
		"repeated node": {`names the node "a" twice`, Group{Label: "Review", Nodes: []string{"a", "a"}}},
	} {
		if err := Validate(base(tc.group)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}

// The field is an addition: a workflow without groups is stored and hashed exactly as
// before, and one with groups keeps them through JSON.
func TestGroupsAreAnOptionalAddition(t *testing.T) {
	plain := Definition{Name: "w", Steps: []Step{work("a")}}
	stored, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "groups") {
		t.Fatalf("a workflow without groups stores the key: %s", stored)
	}
	hashed, err := hashedDefinition(&plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hashed), "groups") {
		t.Fatalf("a workflow without groups hashes the key: %s", hashed)
	}
	marked := plain
	marked.Groups = []Group{{Label: "Input triage", Nodes: []string{"a"}}}
	stored, err = json.Marshal(marked)
	if err != nil {
		t.Fatal(err)
	}
	var back Definition
	if err := json.Unmarshal(stored, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Groups) != 1 || back.Groups[0].Label != "Input triage" || len(back.Groups[0].Nodes) != 1 || back.Groups[0].Nodes[0] != "a" {
		t.Fatalf("groups after a round trip: %+v", back.Groups)
	}
	if _, err := hashedDefinition(&marked); err != nil {
		t.Fatalf("a workflow with groups cannot be hashed: %v", err)
	}
}
