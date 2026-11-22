package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// IntakeAcceptanceHash covers the complete immutable trigger and pinned workflow snapshot.
func IntakeAcceptanceHash(def Definition, in IntakeInput) (string, error) {
	var event, binding any
	if len(in.Event) == 0 || json.Unmarshal(in.Event, &event) != nil {
		return "", errors.New("event must be valid JSON")
	}
	if len(in.Binding) == 0 || json.Unmarshal(in.Binding, &binding) != nil {
		return "", errors.New("binding must be valid JSON")
	}
	payload, err := json.Marshal(struct {
		BindingID       string     `json:"binding_id"`
		BindingRevision string     `json:"binding_revision"`
		EventID         string     `json:"event_id"`
		Event           any        `json:"event"`
		Binding         any        `json:"binding"`
		Definition      Definition `json:"definition"`
	}{in.BindingID, in.BindingRevision, in.EventID, event, binding, def})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
