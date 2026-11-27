package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// definitionHashKeys and stepHashKeys are the JSON keys of Definition and Step in the order
// in which the acceptance hash encodes them. The order is the one the hash was first
// computed with, and it never changes: an accepted event keeps its hash in the intake
// receipt and in the memo of its run, so the same event, binding, and pinned workflow
// version must hash to the same value forever. The order of the fields in the structs has
// no effect on the hash. The key of a new field is added at the end of its list.
var (
	definitionHashKeys = []string{"name", "version", "description", "created", "created_by", "steps", "groups"}
	stepHashKeys       = []string{
		"id", "service", "activity", "agent", "agents", "question", "workflow", "workflows", "channels", "max_turns",
		"member_positions", "on_reject", "max_rejections", "repository", "source_branch", "work_branch", "name",
		"protected", "rules", "command", "allow_failure", "depends_on", "timeout", "max_attempts", "capacity", "size",
		"input_timeout", "position",
	}
)

// stepsKey is the key of a definition's steps, which are encoded in the order of stepHashKeys.
const stepsKey = "steps"

// IntakeAcceptanceHash covers the complete immutable trigger and pinned workflow snapshot.
func IntakeAcceptanceHash(def *Definition, in *IntakeInput) (string, error) {
	var event, binding any
	if len(in.Event) == 0 || json.Unmarshal(in.Event, &event) != nil {
		return "", errors.New("event must be valid JSON")
	}
	if len(in.Binding) == 0 || json.Unmarshal(in.Binding, &binding) != nil {
		return "", errors.New("binding must be valid JSON")
	}
	definition, err := hashedDefinition(def)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		BindingID       string          `json:"binding_id"`
		BindingRevision string          `json:"binding_revision"`
		EventID         string          `json:"event_id"`
		Event           any             `json:"event"`
		Binding         any             `json:"binding"`
		Definition      json.RawMessage `json:"definition"`
	}{in.BindingID, in.BindingRevision, in.EventID, event, binding, definition})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// hashedDefinition encodes a definition as JSON with the keys of the definition and of its
// steps in the order of definitionHashKeys and stepHashKeys. Every value is the one
// encoding/json produces for the field.
func hashedDefinition(def *Definition) ([]byte, error) {
	members, err := jsonMembers(def)
	if err != nil {
		return nil, err
	}
	// A definition without a steps list encodes it as null, as encoding/json does.
	if def.Steps != nil {
		var steps bytes.Buffer
		steps.WriteByte('[')
		for i := range def.Steps {
			stepMembers, stepErr := jsonMembers(&def.Steps[i])
			if stepErr != nil {
				return nil, stepErr
			}
			step, stepErr := orderedObject(stepMembers, stepHashKeys)
			if stepErr != nil {
				return nil, fmt.Errorf("step %q: %w", def.Steps[i].ID, stepErr)
			}
			if i > 0 {
				steps.WriteByte(',')
			}
			steps.Write(step)
		}
		steps.WriteByte(']')
		members[stepsKey] = steps.Bytes()
	}
	return orderedObject(members, definitionHashKeys)
}

// jsonMembers encodes a struct as JSON and returns the members of the object, each value
// byte for byte as encoding/json wrote it.
func jsonMembers(v any) (map[string]json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var members map[string]json.RawMessage
	err = json.Unmarshal(data, &members)
	return members, err
}

// orderedObject writes the members of a JSON object in the order of keys. A member that is
// absent stays absent. A member whose key is not in keys is an error, so a new field can
// never change the hash unnoticed.
func orderedObject(members map[string]json.RawMessage, keys []string) ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	written := 0
	for _, key := range keys {
		value, ok := members[key]
		if !ok {
			continue
		}
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		if written > 0 {
			out.WriteByte(',')
		}
		out.Write(name)
		out.WriteByte(':')
		out.Write(value)
		written++
	}
	if written != len(members) {
		var unknown []string
		for key := range members {
			if !contains(keys, key) {
				unknown = append(unknown, key)
			}
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("the acceptance hash has no place for the key %s; add it at the end of its list of hash keys", strings.Join(unknown, ", "))
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
