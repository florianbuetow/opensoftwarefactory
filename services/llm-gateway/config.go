package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Auth struct {
	Type     string `yaml:"type"`
	Token    string `yaml:"token"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}
type Provider struct {
	Name           string            `yaml:"name"`
	BaseURL        string            `yaml:"base_url"`
	Auth           Auth              `yaml:"auth"`
	Streaming      bool              `yaml:"streaming"`
	ModelDiscovery string            `yaml:"model_discovery"`
	Models         map[string]string `yaml:"models"`
}
type Config struct {
	Providers     []Provider      `yaml:"providers"`
	LogLevel      string          `yaml:"log_level"`
	Telemetry     TelemetryConfig `yaml:"telemetry"`
	BlobstoreURL  string          `yaml:"blobstore_url"`
	EvaluationURL string          `yaml:"evaluation_url"`
}
type route struct {
	provider *Provider
	model    string
}

func loadConfig(path string) (Config, error) {
	c := Config{LogLevel: "DEBUG"}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid YAML configuration")
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("configuration must contain one YAML document")
	}
	_, err = routes(&c)
	return c, err
}

func routes(c *Config) (map[string]route, error) {
	out := make(map[string]route)
	names := make(map[string]bool)
	for name, endpoint := range map[string]string{"blobstore_url": c.BlobstoreURL, "evaluation_url": c.EvaluationURL} {
		if endpoint == "" {
			continue
		}
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%s must be an HTTP(S) URL without credentials, query or fragment", name)
		}
	}
	if len(c.Providers) == 0 {
		return nil, fmt.Errorf("at least one provider is required")
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Name == "" || names[p.Name] {
			return nil, fmt.Errorf("provider names must be nonempty and unique")
		}
		names[p.Name] = true
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("provider base_url must be an HTTP(S) URL without credentials, query or fragment")
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		switch p.Auth.Type {
		case "none":
		case "bearer":
			if p.Auth.Token == "" || strings.ContainsAny(p.Auth.Token, "\r\n") {
				return nil, fmt.Errorf("bearer token is missing or invalid")
			}
		case "basic":
			if p.Auth.Username == "" || strings.Contains(p.Auth.Username, ":") {
				return nil, fmt.Errorf("basic username is missing or invalid")
			}
		default:
			return nil, fmt.Errorf("auth.type must be none, bearer or basic")
		}
		if p.ModelDiscovery != "" && p.ModelDiscovery != "lm-studio" {
			return nil, fmt.Errorf("model_discovery must be empty or lm-studio")
		}
		if len(p.Models) == 0 {
			return nil, fmt.Errorf("each provider requires model mappings")
		}
		for alias, model := range p.Models {
			if alias == "" || model == "" {
				return nil, fmt.Errorf("model names cannot be empty")
			}
			if _, ok := out[alias]; ok {
				return nil, fmt.Errorf("model aliases must be unique across providers")
			}
			out[alias] = route{p, model}
		}
	}
	return out, nil
}
