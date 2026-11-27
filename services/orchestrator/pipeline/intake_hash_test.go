package pipeline

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The acceptance hashes below were recorded before the fields of Definition and Step were
// reordered. An accepted event stores its hash in the intake receipt and in the memo of its
// run, so the same event, binding, and pinned workflow version must hash to the same value
// with every later version of the orchestrator.
const (
	pinnedFullHash       = "b09ed59c89e02df36f9712c13e0e69eb6d5167c33d528ec365c84f0b30ce6671"
	pinnedMinimalHash    = "61e3b06a7efc0fe2148625760197241032bb9cfe336d15016f1b7ac1d35af600"
	pinnedEmptyStepsHash = "3ac3080ac8ef78135b916c17e3ad6dc79429dbf670f52368829f646ce47c3293"
	// Recorded when Definition.Groups was added, for the full definition with a group.
	pinnedFullWithGroupsHash = "625179992dd4a61affb94239e922b145461ee3eb0f9a95a8a565d36c34283149"
)

// fullDefinition sets every field of Definition, Step, Point, and Rule to a value that is
// not its zero value, so no omitempty hides a key. Its second step sets only the required
// fields, and the texts hold the characters encoding/json escapes.
func fullDefinition() Definition {
	return Definition{
		Name:        "group/score",
		Version:     7,
		Description: "Scores <a> & \"b\"   für Ω",
		Created:     "2026-01-02T03:04:05Z",
		CreatedBy:   "flo",
		Steps: []Step{
			{
				ID:              "every-field",
				Service:         "team",
				Activity:        "Process",
				Agent:           "demo/lead@v1",
				Agents:          []string{"demo/coder@v2", "demo/tester@v3"},
				Question:        "Ship <it>?",
				Workflow:        "inner@v2",
				Workflows:       []string{"docs@v1", "lib/sheets/sheet@v4"},
				Channels:        [][]string{{"lead", "coder"}, {"coder", "docs", "one-way"}},
				MaxTurns:        12,
				MemberPositions: map[string]Point{"lead": {X: 12.5, Y: 40}, "coder": {X: 1e-7, Y: 1e21}},
				OnReject:        "earlier",
				MaxRejections:   3,
				Repository:      "claims-build",
				SourceBranch:    "main",
				WorkBranch:      "agent/change-{run}",
				Name:            "payment-date",
				Protected:       []string{"tests/**", "verify.sh"},
				Rules:           []Rule{{Tool: "write_file", Path: "src/**", Matches: "a ?b", Message: "Blocked: fix it."}, {Tool: "run_command", Message: "no"}},
				Command:         "./verify.sh && echo done",
				AllowFailure:    true,
				DependsOn:       []string{"earlier", "other"},
				Timeout:         "10m",
				MaxAttempts:     4,
				Capacity:        16,
				Size:            5,
				InputTimeout:    300,
				Position:        &Point{X: 10, Y: 20.25},
			},
			{ID: "required-only", Service: "svc"},
		},
		Groups: []Group{{Label: "Triage <a> & \"b\"", Nodes: []string{"every-field", "required-only"}}},
	}
}

func hashInput() *IntakeInput {
	return &IntakeInput{
		BindingID: "quality/main", BindingRevision: "rev-3", EventID: "event-7",
		Event:   json.RawMessage(`{"run_id":"target","b":2,"a":[1,{"z":null,"y":"<&>"}]}`),
		Binding: json.RawMessage(`{"id":"quality/main","enabled":true}`),
	}
}

func TestIntakeAcceptanceHashIsPinned(t *testing.T) {
	full := fullDefinition()
	// The fields that existed when the hashes were first pinned: a definition without groups
	// must hash as it always did, so pinnedFullHash stays as recorded.
	before := full
	before.Groups = nil
	for name, tc := range map[string]struct {
		def  *Definition
		want string
	}{
		"every field before groups": {&before, pinnedFullHash},
		"every field":               {&full, pinnedFullWithGroupsHash},
		"name only":                 {&Definition{Name: "group/score", Version: 3}, pinnedMinimalHash},
		"empty steps":               {&Definition{Name: "group/score", Steps: []Step{}}, pinnedEmptyStepsHash},
	} {
		got, err := IntakeAcceptanceHash(tc.def, hashInput())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: hash %s, want %s", name, got, tc.want)
		}
	}
}

// TestHashKeysNameEveryFieldOfDefinitionAndStep fails when a field is added to Definition or
// Step without a place in the acceptance hash.
func TestHashKeysNameEveryFieldOfDefinitionAndStep(t *testing.T) {
	for name, tc := range map[string]struct {
		typ  reflect.Type
		keys []string
	}{
		"Definition": {reflect.TypeFor[Definition](), definitionHashKeys},
		"Step":       {reflect.TypeFor[Step](), stepHashKeys},
	} {
		listed := map[string]bool{}
		for _, key := range tc.keys {
			if listed[key] {
				t.Errorf("%s: the hash keys list %q twice", name, key)
			}
			listed[key] = true
		}
		for i := range tc.typ.NumField() {
			key, _, _ := strings.Cut(tc.typ.Field(i).Tag.Get("json"), ",")
			if !listed[key] {
				t.Errorf("%s.%s: the hash keys lack %q", name, tc.typ.Field(i).Name, key)
			}
			delete(listed, key)
		}
		for key := range listed {
			t.Errorf("%s: the hash keys list %q, which is not a field", name, key)
		}
	}
}

func TestOrderedObjectRefusesAKeyWithoutAPlace(t *testing.T) {
	members := map[string]json.RawMessage{"name": json.RawMessage(`"w"`), "added": json.RawMessage(`1`)}
	if _, err := orderedObject(members, definitionHashKeys); err == nil || !strings.Contains(err.Error(), "added") {
		t.Fatalf("a key without a place in the hash was accepted: %v", err)
	}
	delete(members, "added")
	members["version"] = json.RawMessage(`2`)
	got, err := orderedObject(members, definitionHashKeys)
	if err != nil || string(got) != `{"name":"w","version":2}` {
		t.Fatalf("ordered object %s, %v", got, err)
	}
}

// TestFullDefinitionSetsEveryField keeps the pinned definition complete: a field added to
// Definition, Step, Point, or Rule must be set in fullDefinition, or the pinned hash would
// not cover it.
func TestFullDefinitionSetsEveryField(t *testing.T) {
	def := fullDefinition()
	step := def.Steps[0]
	for name, value := range map[string]reflect.Value{
		"Definition": reflect.ValueOf(def),
		"Step":       reflect.ValueOf(step),
		"Point":      reflect.ValueOf(*step.Position),
		"Rule":       reflect.ValueOf(step.Rules[0]),
	} {
		for i := range value.NumField() {
			if value.Field(i).IsZero() {
				t.Errorf("fullDefinition leaves %s.%s at its zero value", name, value.Type().Field(i).Name)
			}
		}
	}
	if !strings.Contains(def.Description, "<") {
		t.Error("the description must hold a character encoding/json escapes")
	}
}
