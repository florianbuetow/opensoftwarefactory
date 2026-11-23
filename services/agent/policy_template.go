package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type policyToken struct {
	id         string
	start, end int
}

type requiredPolicyError struct{ text string }

func (e requiredPolicyError) Error() string { return e.text }

type resolvedPolicy struct{ ID, Version, Content string }

var (
	policySegment        = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$|^[a-z0-9]$`)
	policyVersionSegment = regexp.MustCompile(`^v[1-9]\d*$`)
)

func parsePolicyTokens(text string) ([]policyToken, error) {
	const open = "{{policy::"
	var tokens []policyToken
	for offset := 0; ; {
		relative := strings.Index(text[offset:], open)
		if relative < 0 {
			return tokens, nil
		}
		start := offset + relative
		closeRelative := strings.Index(text[start+len(open):], "}}")
		if closeRelative < 0 {
			return nil, requiredPolicyError{text: "compiling agent: systemprompt.md has malformed policy reference"}
		}
		end := start + len(open) + closeRelative + 2
		id := text[start+len(open) : end-2]
		if !validPolicyID(id) {
			return nil, requiredPolicyError{text: fmt.Sprintf("compiling agent: systemprompt.md has malformed policy reference %q", text[start:end])}
		}
		tokens = append(tokens, policyToken{start: start, end: end, id: id})
		offset = end
	}
}

func validPolicyID(id string) bool {
	parts := strings.Split(id, "/")
	if len(parts) == 0 || len(parts) > 4 {
		return false
	}
	for _, part := range parts {
		if !policySegment.MatchString(part) || part == "latest" || policyVersionSegment.MatchString(part) {
			return false
		}
	}
	return true
}

func (s *Service) compilePrompt(ctx context.Context, prompt string) (string, []resolvedPolicy, error) {
	tokens, err := parsePolicyTokens(prompt)
	if err != nil || len(tokens) == 0 {
		return prompt, nil, err
	}
	policies := map[string]resolvedPolicy{}
	resolved := make([]resolvedPolicy, 0, len(tokens))
	for _, token := range tokens {
		if _, ok := policies[token.id]; ok {
			continue
		}
		parts := strings.Split(token.id, "/")
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		endpoint := strings.TrimRight(s.PolicyRegistryURL, "/") + "/policies/" + strings.Join(parts, "/") + "/latest"
		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, endpoint, http.NoBody)
		if err != nil {
			cancel()
			return "", nil, fmt.Errorf("compiling agent: required policy %q: %w", token.id, err)
		}
		setCorrelationHeaders(req)
		resp, err := s.HTTP.Do(req)
		if err != nil {
			cancel()
			return "", nil, fmt.Errorf("compiling agent: required policy %q: policy service: %w", token.id, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			return "", nil, fmt.Errorf("compiling agent: required policy %q: policy service: %w", token.id, readErr)
		}
		if resp.StatusCode == http.StatusNotFound {
			return "", nil, requiredPolicyError{text: fmt.Sprintf("compiling agent: required policy %q does not exist", token.id)}
		}
		if resp.StatusCode/100 != 2 {
			return "", nil, fmt.Errorf("compiling agent: required policy %q: policy service returned %s", token.id, resp.Status)
		}
		var entry struct {
			Files struct {
				Policy *string `json:"policy.md"`
			} `json:"files"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &entry); err != nil || entry.Version == "" || entry.Files.Policy == nil {
			return "", nil, fmt.Errorf("compiling agent: required policy %q: invalid policy service response", token.id)
		}
		policy := resolvedPolicy{ID: token.id, Content: *entry.Files.Policy, Version: entry.Version}
		policies[token.id] = policy
		resolved = append(resolved, policy)
	}
	var out strings.Builder
	last := 0
	for _, token := range tokens {
		out.WriteString(prompt[last:token.start])
		out.WriteString(policies[token.id].Content)
		last = token.end
	}
	out.WriteString(prompt[last:])
	return out.String(), resolved, nil
}
