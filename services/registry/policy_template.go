package main

import (
	"fmt"
	"regexp"
	"strings"
)

type policyToken struct {
	id         string
	start, end int
}

var policySegment = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$|^[a-z0-9]$`)

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
			return nil, fmt.Errorf("systemprompt.md has malformed policy reference")
		}
		end := start + len(open) + closeRelative + 2
		id := text[start+len(open) : end-2]
		if !validPolicyID(id) {
			return nil, fmt.Errorf("systemprompt.md has malformed policy reference %q", text[start:end])
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
		if !policySegment.MatchString(part) || part == latestVersion {
			return false
		}
		if _, ok := versionNumber(part); ok {
			return false
		}
	}
	return true
}
