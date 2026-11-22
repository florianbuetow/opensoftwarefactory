package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigDefaultsLogLevelAndRejectsUnknownFields(t *testing.T) {
	valid := `providers:
  - name: p
    base_url: https://example.com/v1
    auth:
      type: none
    streaming: true
    models:
      public: upstream
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "DEBUG" {
		t.Fatalf("default log level = %q", c.LogLevel)
	}

	if err := os.WriteFile(path, []byte(valid+"unknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("unknown configuration field was accepted")
	}
}

func TestMeasurementEndpointConfiguration(t *testing.T) {
	c := Config{BlobstoreURL: "http://blobstore:8080", EvaluationURL: "https://evaluation.example", Providers: []Provider{{Name: "p", BaseURL: "https://provider.example/v1", Auth: Auth{Type: "none"}, Models: map[string]string{"m": "m"}}}}
	if _, err := routes(&c); err != nil {
		t.Fatal(err)
	}
	c.EvaluationURL = "https://user:secret@example.com"
	if _, err := routes(&c); err == nil {
		t.Fatal("credentialed evaluation URL accepted")
	}
}

func TestInvalidProviderConfigurations(t *testing.T) {
	valid := Provider{
		Name: "one", BaseURL: "https://example.com/v1", Auth: Auth{Type: "none"},
		Models: map[string]string{"public": "upstream"},
	}
	tests := map[string]Config{
		"no providers": {},
		"duplicate provider names": {Providers: []Provider{
			valid, {Name: "one", BaseURL: "https://other.example/v1", Auth: Auth{Type: "none"}, Models: map[string]string{"other": "model"}},
		}},
		"credentials in URL": {Providers: []Provider{{Name: "one", BaseURL: "https://user:secret@example.com/v1", Auth: Auth{Type: "none"}, Models: valid.Models}}},
		"query in URL":       {Providers: []Provider{{Name: "one", BaseURL: "https://example.com/v1?secret=x", Auth: Auth{Type: "none"}, Models: valid.Models}}},
		"unsupported scheme": {Providers: []Provider{{Name: "one", BaseURL: "ftp://example.com/v1", Auth: Auth{Type: "none"}, Models: valid.Models}}},
		"missing bearer token": {Providers: []Provider{{
			Name: "one", BaseURL: valid.BaseURL, Auth: Auth{Type: "bearer"}, Models: valid.Models,
		}}},
		"header injection in bearer token": {Providers: []Provider{{
			Name: "one", BaseURL: valid.BaseURL, Auth: Auth{Type: "bearer", Token: "secret\r\nX-Bad: yes"}, Models: valid.Models,
		}}},
		"colon in basic username": {Providers: []Provider{{
			Name: "one", BaseURL: valid.BaseURL, Auth: Auth{Type: "basic", Username: "bad:name", Password: "secret"}, Models: valid.Models,
		}}},
		"unsupported auth": {Providers: []Provider{{
			Name: "one", BaseURL: valid.BaseURL, Auth: Auth{Type: "oauth"}, Models: valid.Models,
		}}},
		"unsupported model discovery": {Providers: []Provider{{
			Name: "one", BaseURL: valid.BaseURL, Auth: Auth{Type: "none"}, ModelDiscovery: "other", Models: valid.Models,
		}}},
		"no model mapping": {Providers: []Provider{{Name: "one", BaseURL: valid.BaseURL, Auth: Auth{Type: "none"}}}},
		"empty model alias": {Providers: []Provider{{
			Name: "one", BaseURL: valid.BaseURL, Auth: Auth{Type: "none"}, Models: map[string]string{"": "upstream"},
		}}},
		"duplicate model alias": {Providers: []Provider{
			valid, {Name: "two", BaseURL: "https://other.example/v1", Auth: Auth{Type: "none"}, Models: map[string]string{"public": "other"}},
		}},
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := routes(&config); err == nil || strings.TrimSpace(err.Error()) == "" {
				t.Fatalf("invalid configuration accepted: %+v", config)
			}
		})
	}
}
