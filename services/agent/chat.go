package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/temporal"
)

// sessionChatRequest is the body of POST /chat: one user message for one tab of a session.
type sessionChatRequest struct {
	TraceID   string `json:"trace_id"`
	StepID    string `json:"step_id"`
	Session   string `json:"session"`
	Tab       string `json:"tab"`
	Agent     string `json:"agent"`
	Message   string `json:"message"`
	CreatedBy string `json:"created_by"`
	// DevPod is the run's dev pod; empty when the run has none.
	DevPod                    string            `json:"dev_pod,omitempty"`
	Skills                    map[string]string `json:"skills,omitempty"`
	SkillScriptTimeoutSeconds int               `json:"skill_script_timeout_seconds,omitempty"`
}

// chatLog is the stored history of one tab, <session>/chats/<tab>.json. Messages is the
// exact OpenAI-format list sent to the model and only grows.
type chatLog struct {
	Session     string            `json:"session"`
	Tab         string            `json:"tab"`
	Agent       string            `json:"agent"`
	Model       string            `json:"model,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	TraceID     string            `json:"trace_id"`
	StepID      string            `json:"step_id"`
	Created     time.Time         `json:"created"`
	Updated     time.Time         `json:"updated"`
	Messages    []json.RawMessage `json:"messages"`
	// Events holds one entry per model call and per tool call and only grows. A log written
	// before events were recorded has none.
	Events []TurnEvent `json:"events,omitempty"`
}

// TurnEvent is one model call or tool call of a session chat or a team member. Its fields
// are the measurement fields of a RunEvent, with the same names.
type TurnEvent struct {
	Kind                  string    `json:"kind"` // "model_call" or "tool_call"
	Name                  string    `json:"name,omitempty"`
	Status                string    `json:"status"` // "completed" or "failed"
	Started               time.Time `json:"started"`
	Finished              time.Time `json:"finished"`
	Model                 string    `json:"model,omitempty"`
	Turn                  int       `json:"turn,omitempty"` // number of the model call within one message or team turn
	Usage                 *usage    `json:"usage,omitempty"`
	EstimatedInputTokens  *int64    `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens *int64    `json:"estimated_output_tokens,omitempty"`
	EstimationMethod      string    `json:"estimation_method,omitempty"`
	Error                 string    `json:"error,omitempty"`
	Warning               string    `json:"warning,omitempty"`
	MeasurementID         string    `json:"measurement_id,omitempty"`
}

// record stores a finished model or tool call.
func (l *chatLog) record(event TurnEvent, status string, err error) {
	event.Finished = time.Now().UTC()
	event.Status = status
	if err != nil {
		event.Error = err.Error()
	}
	l.Events = append(l.Events, event)
}

// setModelSettings notes the model and the model settings the agent runs with.
func (l *chatLog) setModelSettings(def Definition) {
	l.Model, l.Temperature = def.Models[0], def.Temperature
}

func (l *chatLog) add(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	l.Messages = append(l.Messages, data)
	return nil
}

func (l *chatLog) modelMessages() []any {
	messages := make([]any, len(l.Messages))
	for i, m := range l.Messages {
		messages[i] = m
	}
	return messages
}

// chatError carries the HTTP status of a failed chat request.
type chatError struct {
	status int
	text   string
}

func (e chatError) Error() string { return e.text }

const (
	maxChatBodyBytes = 4 << 20
	// chatTurnTimeout bounds one message, including every model and tool call of its turn.
	chatTurnTimeout = 10 * time.Minute
	chatSaveTimeout = 10 * time.Second
)

var chatTabPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// chatLocks allows one message at a time per chat log within this process.
var chatLocks = struct {
	mu    sync.Mutex
	items map[string]*chatLock
}{items: map[string]*chatLock{}}

type chatLock struct {
	mu      sync.Mutex
	waiters int
}

func lockChat(id string) (unlock func()) {
	chatLocks.mu.Lock()
	l := chatLocks.items[id]
	if l == nil {
		l = &chatLock{}
		chatLocks.items[id] = l
	}
	l.waiters++
	chatLocks.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		chatLocks.mu.Lock()
		if l.waiters--; l.waiters == 0 {
			delete(chatLocks.items, id)
		}
		chatLocks.mu.Unlock()
	}
}

// routes is the HTTP API served next to the Temporal worker.
func (s *Service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /chat", s.handleChat)
	return mux
}

func writeChatJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Service) handleChat(w http.ResponseWriter, r *http.Request) {
	var req sessionChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatBodyBytes)).Decode(&req); err != nil {
		writeChatJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	reply, chatID, count, err := s.Chat(r.Context(), req)
	if err != nil {
		status := http.StatusBadGateway
		var ce chatError
		var app *temporal.ApplicationError
		if errors.As(err, &ce) {
			status = ce.status
		} else if errors.As(err, &app) && app.NonRetryable() {
			writeChatJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": app.Message(), "type": app.Type()})
			return
		}
		writeChatJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeChatJSON(w, http.StatusOK, map[string]any{"reply": reply, "chat": chatID, "messages": count})
}

// validateChat checks the request and returns the pinned agent reference and the chat log ID.
func validateChat(req sessionChatRequest) (name, version, chatID string, err error) {
	bad := func(format string, args ...any) (string, string, string, error) {
		return "", "", "", chatError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
	}
	if !segmentPattern.MatchString(req.TraceID) {
		return bad("trace_id is missing or invalid")
	}
	if !validID(req.StepID) {
		return bad("step_id is missing or invalid")
	}
	prefix := "user-sessions/" + req.TraceID + "/"
	if !strings.HasPrefix(req.Session, prefix) || !validID(req.Session) {
		return bad("session must be a path starting with %q", prefix)
	}
	if !chatTabPattern.MatchString(req.Tab) {
		return bad("tab must match %s", chatTabPattern)
	}
	if name, version, err = parseRef(req.Agent); err != nil {
		return bad("%s", err.Error())
	}
	if strings.TrimSpace(req.Message) == "" {
		return bad("message is empty")
	}
	chatID = req.Session + "/chats/" + req.Tab + ".json"
	if !validID(chatID) {
		return bad("session is too long")
	}
	return name, version, chatID, nil
}

// sessionNote is appended to the system prompt of a new tab.
func sessionNote(session string) string {
	return "\n\n## Interactive session\n" +
		"You are chatting with a person in the interactive session `" + session + "`. " +
		"The session files live under `" + session + "/files/`. " +
		"If you have the tools list_session_files, read_session_file, search_session_files and write_session_file, " +
		"use them to work with these files and always pass `session` = `" + session + "`."
}

// Chat runs one user message of one tab: it loads the tab's history, runs the agent's
// model and tool loop, stores the grown history and returns the reply.
func (s *Service) Chat(ctx context.Context, req sessionChatRequest) (reply, chatID string, count int, retErr error) {
	name, version, chatID, err := validateChat(req)
	if err != nil {
		return "", "", 0, err
	}
	ref := name + "@" + version
	ctx, span := otel.Tracer("opensoftwarefactory/agent").Start(ctx, "agent.chat",
		trace.WithAttributes(attribute.String("factory.run_id", req.TraceID), attribute.String("factory.step", req.StepID), attribute.String("factory.agent", ref)))
	defer span.End()
	ctx = withCorrelation(ctx, StepRequest{TraceID: req.TraceID, StepID: req.StepID, Agent: ref, OutputArtifact: chatID}, ref, 1)
	ctx = withDevPod(ctx, req.DevPod, ref, req.TraceID)
	ctx = withSkills(ctx, req.Skills, req.SkillScriptTimeoutSeconds)
	ctx, cancel := context.WithTimeout(ctx, chatTurnTimeout)
	defer cancel()
	log := s.Logger.With("run_id", req.TraceID, "step", req.StepID, "agent", ref, "chat", chatID, "created_by", req.CreatedBy)
	started := time.Now()
	defer func() {
		if retErr != nil {
			span.RecordError(retErr)
			span.SetStatus(codes.Error, errorCategory(retErr))
			log.Error("chat message failed", "error", retErr, "messages", count)
			return
		}
		span.SetStatus(codes.Ok, "completed")
		log.Info("chat message completed", "messages", count, "processing_time_ms", time.Since(started).Milliseconds())
	}()

	defer lockChat(chatID)()

	history := chatLog{Session: req.Session, Tab: req.Tab, Agent: ref, TraceID: req.TraceID, StepID: req.StepID}
	data, err := s.Store.Get(ctx, chatID)
	var missing notFoundError
	switch {
	case errors.As(err, &missing):
	case err != nil:
		return "", chatID, 0, fmt.Errorf("reading chat log %q: %w", chatID, err)
	default:
		if err := json.Unmarshal(data, &history); err != nil {
			return "", chatID, 0, fmt.Errorf("reading chat log %q: %w", chatID, err)
		}
		if history.Agent != ref {
			return "", chatID, len(history.Messages), chatError{http.StatusConflict, fmt.Sprintf("tab %q already belongs to agent %s", req.Tab, history.Agent)}
		}
	}

	prompt, def, err := s.loadAgent(ctx, name, version)
	if err != nil {
		var app *temporal.ApplicationError
		if errors.As(err, &app) && app.Type() == "AgentNotFound" {
			return "", chatID, len(history.Messages), chatError{http.StatusNotFound, err.Error()}
		}
		if errors.As(err, &app) && app.Type() == "InvalidAgent" {
			return "", chatID, len(history.Messages), chatError{http.StatusUnprocessableEntity, app.Message()}
		}
		return "", chatID, len(history.Messages), err
	}
	history.setModelSettings(def)
	now := time.Now().UTC()
	if len(history.Messages) == 0 {
		if prompt, _, err = s.compilePrompt(ctx, prompt); err != nil {
			return "", chatID, 0, err
		}
		skillNote, _, err := s.loadSkillNote(ctx, req.Skills)
		if err != nil {
			return "", chatID, 0, err
		}
		prompt += skillNote
		history.Created = now
		note := sessionNote(req.Session)
		if strings.TrimSpace(req.DevPod) != "" {
			note += devPodNote(def.Commands)
		}
		if err := history.add(map[string]string{"role": "system", "content": prompt + note}); err != nil {
			return "", chatID, 0, err
		}
	}
	if err := history.add(map[string]string{"role": "user", "content": req.Message}); err != nil {
		return "", chatID, len(history.Messages), err
	}
	// From here on the history is stored whatever happens, so a failed turn keeps its messages.
	defer func() {
		history.Updated = time.Now().UTC()
		count = len(history.Messages)
		if err := s.saveChat(ctx, chatID, history, Meta{Creator: "agent:" + ref, TraceID: req.TraceID}); err != nil {
			log.Error("persisting chat log failed", "error", err)
			if retErr == nil {
				reply, retErr = "", err
			}
		}
	}()

	reply, err = s.chatTurn(ctx, ref, def, &history, log)
	return reply, chatID, len(history.Messages), err
}

func (s *Service) saveChat(ctx context.Context, chatID string, history chatLog, meta Meta) error {
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	// A canceled or timed-out turn is still stored.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatSaveTimeout)
	defer cancel()
	if err := s.Store.Put(ctx, chatID, data, meta); err != nil {
		return fmt.Errorf("writing chat log %q: %w", chatID, err)
	}
	return nil
}

// chatTurn runs the model and tool loop of Process for one message and appends every
// assistant and tool message to the history.
func (s *Service) chatTurn(ctx context.Context, ref string, def Definition, history *chatLog, log *slog.Logger) (string, error) {
	// One model call, plus max_retries more (each tool round needs one).
	maxCalls := 1 + max(def.MaxRetries, 0)
	var tools []any
	routes := map[string]toolRoute{}
	if selected := skillTools(ctx, def.Tools); len(selected) > 0 {
		eventCtx, event, eventTrace := eventSpan(ctx, RunEvent{Kind: "tool_discovery", Started: time.Now().UTC()}, "agent.tool_discovery")
		var err error
		if tools, routes, err = s.discoverTools(eventCtx, selected, log); err != nil {
			finishEventSpan(event, eventTrace, "failed", categorizedError{"mcp_gateway", err.Error()})
			return "", err
		}
		finishEventSpan(event, eventTrace, "completed", nil)
		defer closeToolRoutes(routes, log)
	}
	for calls := 1; calls <= maxCalls; calls++ {
		eventCtx, event, eventTrace := eventSpan(ctx, RunEvent{Kind: "model_call", Started: time.Now().UTC()}, "agent.model_call",
			attribute.String("gen_ai.request.model", def.Models[0]), attribute.Int("factory.turn", calls))
		modelRequest := def.chatRequest(history.modelMessages(), tools)
		inputEstimate := estimateJSONTokens(modelRequest)
		modelEvent := TurnEvent{Kind: "model_call", Name: "chat completion", Model: def.Models[0], Turn: calls, Started: event.Started,
			EstimatedInputTokens: &inputEstimate, EstimationMethod: tokenEstimationMethod}
		c, err := chat(withModelTurn(eventCtx, calls), s.HTTP, s.LLMURL, modelRequest)
		if err != nil {
			finishEventSpan(event, eventTrace, "failed", err)
			history.record(modelEvent, "failed", err)
			return "", err
		}
		finishEventSpan(event, eventTrace, "completed", nil)
		outputEstimate := estimateTokens(string(c.Message))
		modelEvent.Usage, modelEvent.EstimatedOutputTokens = c.Usage, &outputEstimate
		modelEvent.MeasurementID = c.MeasurementID
		history.record(modelEvent, "completed", nil)
		history.Messages = append(history.Messages, c.Message)
		if len(c.ToolCalls) == 0 {
			return c.Content, nil
		}
		for i, tc := range c.ToolCalls {
			service := strings.SplitN(tc.Function.Name, "__", 2)[0]
			eventCtx, event, eventTrace := eventSpan(ctx, RunEvent{Kind: "tool_call", Started: time.Now().UTC()}, "agent.tool_call",
				attribute.String("gen_ai.tool.name", tc.Function.Name), attribute.String("factory.mcp_service", service), attribute.Int("factory.turn", calls))
			toolInputEstimate := estimateJSONTokens(map[string]string{"name": tc.Function.Name, "arguments": tc.Function.Arguments})
			toolEvent := TurnEvent{Kind: "tool_call", Name: tc.Function.Name, Turn: calls, Started: event.Started,
				EstimatedInputTokens: &toolInputEstimate, EstimationMethod: tokenEstimationMethod}
			text, toolFailed, err := s.runTurnTool(eventCtx, routes, tc)
			if err != nil {
				finishEventSpan(event, eventTrace, "failed", categorizedError{"mcp_gateway", err.Error()})
				history.record(toolEvent, "failed", err)
				// Every tool call needs an answer, or the stored history could not be sent again.
				for _, open := range c.ToolCalls[i:] {
					_ = history.add(map[string]string{"role": "tool", "tool_call_id": open.ID, "content": "Error: the tool call did not complete: " + err.Error()})
				}
				return "", err
			}
			toolOutputEstimate := estimateTokens(text)
			toolEvent.EstimatedOutputTokens = &toolOutputEstimate
			toolEvent.Warning = skillWarning(routes[tc.Function.Name], text)
			if toolFailed {
				finishEventSpan(event, eventTrace, "failed", categorizedError{"tool", text})
				history.record(toolEvent, "failed", errors.New(text))
			} else {
				finishEventSpan(event, eventTrace, "completed", nil)
				history.record(toolEvent, "completed", nil)
			}
			log.Info("tool call completed", "tool", tc.Function.Name, "turn", calls, "failed", toolFailed)
			if err := history.add(map[string]string{"role": "tool", "tool_call_id": tc.ID, "content": text}); err != nil {
				return "", err
			}
			// A successful report_blocked ends the turn at once: the reason is the answer.
			if reason, ok := blockedReason(ctx); ok {
				// Every tool call needs an answer, or the stored history could not be sent again.
				for _, open := range c.ToolCalls[i+1:] {
					_ = history.add(map[string]string{"role": "tool", "tool_call_id": open.ID, "content": "Not run: the turn ended because you reported that you are blocked."})
				}
				return reason, nil
			}
		}
		if err := teamTurnStopped(ctx); err != nil {
			return "", err
		}
	}
	return "", turnBudgetError{fmt.Sprintf("agent %s gave no final answer within %d model calls (max_retries=%d)", ref, maxCalls, def.MaxRetries)}
}
