package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type usage struct {
	PromptTokens                                            *int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens                                        *int64 `json:"completion_tokens,omitempty"`
	TotalTokens                                             *int64 `json:"total_tokens,omitempty"`
	CachedInputTokens                                       *int64 `json:"cached_input_tokens,omitempty"`
	ReasoningTokens                                         *int64 `json:"reasoning_tokens,omitempty"`
	Raw                                                     string `json:"-"`
	promptIncomplete, completionIncomplete, totalIncomplete bool
	cachedIncomplete, reasoningIncomplete                   bool
}

func (u *usage) UnmarshalJSON(data []byte) error {
	type wire usage
	var decoded struct {
		InputTokens   *int64 `json:"input_tokens"`
		OutputTokens  *int64 `json:"output_tokens"`
		PromptDetails struct {
			Cached *int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails struct {
			Reasoning *int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		wire
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = usage(decoded.wire)
	if u.PromptTokens == nil {
		u.PromptTokens = decoded.InputTokens
	}
	if u.CompletionTokens == nil {
		u.CompletionTokens = decoded.OutputTokens
	}
	if u.CachedInputTokens == nil {
		u.CachedInputTokens = decoded.PromptDetails.Cached
	}
	if u.ReasoningTokens == nil {
		u.ReasoningTokens = decoded.CompletionDetails.Reasoning
	}
	u.Raw = string(data)
	return nil
}

func (u usage) MarshalJSON() ([]byte, error) {
	if u.Raw != "" {
		return []byte(u.Raw), nil
	}
	type wire usage
	return json.Marshal(wire(u))
}

type chatRequest struct {
	Model           string            `json:"model"`
	Messages        []any             `json:"messages"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	ResponseFormat  map[string]string `json:"response_format,omitempty"`
	Tools           []any             `json:"tools,omitempty"`
	// MaxTokens limits one answer, so a model that does not stop cannot hold a step for minutes.
	MaxTokens int  `json:"max_tokens,omitempty"`
	Stream    bool `json:"stream"`
}

// completion is one parsed model response. Message is the assistant message
// exactly as returned, so it can be appended to the conversation unchanged.
type completion struct {
	Message       json.RawMessage
	Content       string
	Usage         *usage
	MeasurementID string
	ToolCalls     []toolCall
}

func chat(ctx context.Context, client *http.Client, baseURL string, req *chatRequest) (c completion, err error) {
	data, err := json.Marshal(req)
	if err != nil {
		return completion{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return completion{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	setCorrelationHeaders(hreq)
	resp, err := client.Do(hreq)
	if err != nil {
		return completion{}, fmt.Errorf("llm gateway: %w", err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return completion{}, fmt.Errorf("llm gateway: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			return completion{}, categorizedError{"llm_gateway", fmt.Sprintf("llm gateway: %s: %s", resp.Status, e.Error.Message)}
		}
		return completion{}, categorizedError{"llm_gateway", fmt.Sprintf("llm gateway: %s", resp.Status)}
	}
	var out struct {
		Usage   *usage `json:"usage"`
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	err = json.Unmarshal(body, &out)
	if err != nil {
		return completion{}, fmt.Errorf("llm gateway: invalid response: %w", err)
	}
	if len(out.Choices) == 0 || len(out.Choices[0].Message) == 0 {
		return completion{}, fmt.Errorf("llm gateway: response has no message")
	}
	var msg struct {
		Content   *string    `json:"content"`
		ToolCalls []toolCall `json:"tool_calls"`
	}
	err = json.Unmarshal(out.Choices[0].Message, &msg)
	if err != nil {
		return completion{}, fmt.Errorf("llm gateway: invalid message: %w", err)
	}
	c = completion{Message: out.Choices[0].Message, ToolCalls: msg.ToolCalls, Usage: out.Usage, MeasurementID: resp.Header.Get("X-Usage-Measurement-Id")}
	if msg.Content != nil {
		c.Content = *msg.Content
	}
	return c, nil
}
