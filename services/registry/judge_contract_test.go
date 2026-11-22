package main

import (
	"strings"
	"testing"
)

func TestValidateOutputContract(t *testing.T) {
	valid := `{"schema_version":1,"output_contract":{"format":"json","mappings":[{"path":"/scores/~1quality","name":"quality","type":"number"},{"path":"/finding","name":"finding","type":"string"}]}}`
	if err := validateOutputContract([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	if err := validateOutputContract([]byte(`{"models":["m"]}`)); err != nil {
		t.Fatalf("legacy definition rejected: %v", err)
	}
	tests := []struct{ body, want string }{
		{`{"output_contract":{"format":"json","mappings":[]}}`, "requires schema_version 1"},
		{`{"schema_version":1,"output_contract":null}`, "must be an object"},
		{`{"schema_version":1,"output_contract":{"format":"text","mappings":[]}}`, "format must be"},
		{`{"schema_version":1,"output_contract":{"format":"json"}}`, "mappings must be a list"},
		{`{"schema_version":1,"output_contract":{"format":"json","mappings":[{"path":"bad","name":"x","type":"number"}]}}`, "RFC 6901"},
		{`{"schema_version":1,"output_contract":{"format":"json","mappings":[{"path":"/~2","name":"x","type":"number"}]}}`, "RFC 6901"},
		{`{"schema_version":1,"output_contract":{"format":"json","mappings":[{"path":"/x","name":"x","type":"boolean"}]}}`, "number or string"},
	}
	for _, test := range tests {
		if err := validateOutputContract([]byte(test.body)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("validate %s = %v, want %q", test.body, err, test.want)
		}
	}
}
