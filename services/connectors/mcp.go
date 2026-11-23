package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

var (
	factoryTools         = []string{"read_trace", "read_trace_artifact"}
	previousFactoryTools = []string{"read_trace", "get_artifact", "list_artifacts"}
	factoryDeniedTools   = []string{"write_file", "edit_file", "delete_file", "run_command", "push_branch", "create_pull_request"}
	skillTools           = []string{"read_skill", "run_skill_script"}
)

// registerUntilDone makes sure the MCP gateway knows the service devpod, and keeps trying
// until the gateway answers.
func (s *service) registerUntilDone(ctx context.Context) {
	for {
		created, err := s.register(ctx)
		if err == nil {
			if created {
				s.log.Info("registered the MCP service devpod", "url", s.cfg.mcpURL)
			}
			return
		}
		s.log.Warn("registering the MCP service devpod failed; trying again", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.registerRetry):
		}
	}
}

// register creates the entry devpod in the MCP gateway when it is missing. An entry that
// exists is left alone, so a person's changes to it stay. The entry points at the tool
// gateway, which serves the dev pod tools and forwards them to this service.
func (s *service) register(ctx context.Context) (created bool, err error) {
	endpoint := s.mcpGateway + "/api/services/devpod"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return false, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	err = resp.Body.Close()
	if err != nil {
		return false, err
	}
	if resp.StatusCode == http.StatusOK {
		return false, nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return false, fmt.Errorf("the MCP gateway answered %s", resp.Status)
	}
	body, err := json.Marshal(map[string]any{
		"mcpconfig": map[string]any{"transport": "streamable-http", "url": s.cfg.mcpURL},
		"allowlist": devpodTools,
		"denylist":  []string{},
	})
	if err != nil {
		return false, err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = s.http.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return false, fmt.Errorf("the MCP gateway refused the service devpod: %s %s", resp.Status, data)
	}
	return true, nil
}

// registerSkillsUntilDone independently ensures the skill tools have their own MCP service.
func (s *service) registerSkillsUntilDone(ctx context.Context) {
	for {
		created, err := s.registerSkills(ctx)
		if err == nil {
			if created {
				s.log.Info("registered the MCP service skills", "url", s.cfg.mcpURL)
			}
			return
		}
		s.log.Warn("registering the MCP service skills failed; trying again", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.registerRetry):
		}
	}
}

func (s *service) registerSkills(ctx context.Context) (created bool, err error) {
	endpoint := s.mcpGateway + "/api/services/skills"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return false, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	err = resp.Body.Close()
	if err != nil {
		return false, err
	}
	if resp.StatusCode == http.StatusOK {
		return false, nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return false, fmt.Errorf("the MCP gateway answered %s", resp.Status)
	}
	body, err := json.Marshal(map[string]any{
		"mcpconfig": map[string]any{"transport": "streamable-http", "url": s.cfg.mcpURL},
		"allowlist": skillTools,
		"denylist":  []string{},
	})
	if err != nil {
		return false, err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = s.http.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return false, fmt.Errorf("the MCP gateway refused the service skills: %s %s", resp.Status, data)
	}
	return true, nil
}

// registerFactoryUntilDone independently ensures the read-only factory evidence service is
// present. Existing operator configuration is never replaced.
func (s *service) registerFactoryUntilDone(ctx context.Context) {
	for {
		created, err := s.registerFactory(ctx)
		if err == nil {
			if created {
				s.log.Info("registered the MCP service factory", "url", s.cfg.mcpURL)
			}
			return
		}
		s.log.Warn("registering the MCP service factory failed; trying again", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.registerRetry):
		}
	}
}

func (s *service) registerFactory(ctx context.Context) (bool, error) {
	endpoint := s.mcpGateway + "/api/services/factory"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return false, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return false, err
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	err = resp.Body.Close()
	if err != nil {
		return false, err
	}
	if resp.StatusCode == http.StatusOK {
		var existing struct {
			MCPConfig struct {
				Transport string `json:"transport"`
				URL       string `json:"url"`
			} `json:"mcpconfig"`
			Allowlist []string `json:"allowlist"`
			Denylist  []string `json:"denylist"`
		}
		if json.Unmarshal(data, &existing) == nil && existing.MCPConfig.Transport == "streamable-http" && existing.MCPConfig.URL == s.cfg.mcpURL && slices.Equal(existing.Allowlist, previousFactoryTools) && slices.Equal(existing.Denylist, factoryDeniedTools) {
			return s.putFactoryRegistration(ctx, endpoint)
		}
		return false, nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return false, fmt.Errorf("the MCP gateway answered %s", resp.Status)
	}
	return s.putFactoryRegistration(ctx, endpoint)
}

func (s *service) putFactoryRegistration(ctx context.Context, endpoint string) (created bool, err error) {
	body, err := json.Marshal(map[string]any{"mcpconfig": map[string]any{"transport": "streamable-http", "url": s.cfg.mcpURL}, "allowlist": factoryTools, "denylist": factoryDeniedTools})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return false, fmt.Errorf("the MCP gateway refused the service factory: %s %s", resp.Status, data)
	}
	return true, nil
}
