package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type outputContract struct {
	Format   string          `json:"format"`
	Mappings []outputMapping `json:"mappings"`
}

type outputMapping struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func validateOutputContract(agentJSON []byte) error {
	var agent map[string]json.RawMessage
	if err := json.Unmarshal(agentJSON, &agent); err != nil {
		return fmt.Errorf("file %q must be a JSON object", "agent.json")
	}
	raw, exists := agent["output_contract"]
	if !exists {
		return nil
	}
	var schemaVersion int
	if err := json.Unmarshal(agent["schema_version"], &schemaVersion); err != nil || schemaVersion != 1 {
		return fmt.Errorf("agent.json: output_contract requires schema_version 1")
	}
	var contract outputContract
	if err := json.Unmarshal(raw, &contract); err != nil || string(raw) == "null" {
		return fmt.Errorf("agent.json: output_contract must be an object")
	}
	if contract.Format != "json" {
		return fmt.Errorf("agent.json: output_contract.format must be %q", "json")
	}
	if contract.Mappings == nil {
		return fmt.Errorf("agent.json: output_contract.mappings must be a list")
	}
	names := map[string]bool{}
	for i, mapping := range contract.Mappings {
		if !validJSONPointer(mapping.Path) {
			return fmt.Errorf("agent.json: output_contract.mappings[%d].path must be an RFC 6901 JSON pointer", i)
		}
		if strings.TrimSpace(mapping.Name) == "" {
			return fmt.Errorf("agent.json: output_contract.mappings[%d].name must be non-empty", i)
		}
		if names[mapping.Name] {
			return fmt.Errorf("agent.json: output_contract mapping name %q is duplicated", mapping.Name)
		}
		names[mapping.Name] = true
		if mapping.Type != "number" && mapping.Type != "string" {
			return fmt.Errorf("agent.json: output_contract.mappings[%d].type must be number or string", i)
		}
	}
	return nil
}

func validJSONPointer(pointer string) bool {
	if pointer == "" {
		return true
	}
	if !strings.HasPrefix(pointer, "/") {
		return false
	}
	for i := 0; i < len(pointer); i++ {
		if pointer[i] == '~' {
			if i+1 >= len(pointer) || (pointer[i+1] != '0' && pointer[i+1] != '1') {
				return false
			}
			i++
		}
	}
	return true
}
