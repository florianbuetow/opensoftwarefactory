package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Connection is the content of mcpconfig.json: how the gateway reaches one MCP server.
type Connection struct {
	Transport string            `json:"transport"`
	URL       string            `json:"url,omitempty"`
	Command   string            `json:"command,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Args      []string          `json:"args,omitempty"`
}

// Config is one MCP service: its connection and the two lists that decide which tools run.
type Config struct {
	MCPConfig Connection `json:"mcpconfig"`
	Allowlist []string   `json:"allowlist"`
	Denylist  []string   `json:"denylist"`
}

var serviceName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)

func validateName(name string) error {
	if !serviceName.MatchString(name) {
		return fmt.Errorf("invalid service name")
	}
	return nil
}

func (c *Config) validate() error {
	switch c.MCPConfig.Transport {
	case "streamable-http":
		u, err := url.Parse(c.MCPConfig.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Fragment != "" {
			return fmt.Errorf("streamable-http requires an HTTP(S) url without a fragment")
		}
		if c.MCPConfig.Command != "" || len(c.MCPConfig.Args) > 0 || len(c.MCPConfig.Env) > 0 {
			return fmt.Errorf("HTTP configuration cannot contain stdio settings")
		}
	case "stdio":
		if strings.TrimSpace(c.MCPConfig.Command) == "" || strings.ContainsRune(c.MCPConfig.Command, 0) {
			return fmt.Errorf("stdio requires a command")
		}
		if c.MCPConfig.URL != "" {
			return fmt.Errorf("stdio configuration cannot contain a url")
		}
		for _, arg := range c.MCPConfig.Args {
			if strings.ContainsRune(arg, 0) {
				return fmt.Errorf("arguments cannot contain NUL")
			}
		}
		for k, v := range c.MCPConfig.Env {
			if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
				return fmt.Errorf("invalid environment entry")
			}
		}
	default:
		return fmt.Errorf("transport must be streamable-http or stdio")
	}
	for _, list := range [][]string{c.Allowlist, c.Denylist} {
		for _, name := range list {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("tool names must not be empty")
			}
		}
	}
	return nil
}

func (c *Config) allowed(name string) bool {
	for _, n := range c.Denylist {
		if n == name {
			return false
		}
	}
	for _, n := range c.Allowlist {
		if n == name {
			return true
		}
	}
	return false
}

func (c *Config) normalize() {
	if c.Allowlist == nil {
		c.Allowlist = []string{}
	}
	if c.Denylist == nil {
		c.Denylist = []string{}
	}
}

func decodeConfig(data []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	c.normalize()
	err := c.validate()
	return c, err
}
