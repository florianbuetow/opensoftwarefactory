package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
)

const (
	usageOutboxPrefix  = "gateway-measurements"
	usagePendingPrefix = "gateway-measurements-pending"
	usageReplayLimit   = 100
)

type usageContext struct {
	ID, RunID, ExecutionID, ParentExecutionID, Agent, AgentRef string
	AgentVersion                                               *int
	TraceID, SpanID, Vendor, Model, RequestedModel, Effort     string
	ActivityID, ActivityAttempt, Turn                          string
	DurationMS                                                 *int64
	OccurredAt                                                 time.Time
	Modes                                                      map[string]string
	RequestSettings                                            map[string]string
}

type usageMeasurement struct {
	SchemaVersion       int               `json:"schema_version"`
	ID                  string            `json:"id"`
	Kind                string            `json:"kind"`
	RunID               string            `json:"run_id"`
	ExecutionID         string            `json:"execution_id"`
	ParentExecutionID   string            `json:"parent_execution_id,omitempty"`
	Agent               string            `json:"agent,omitempty"`
	AgentVersion        *int              `json:"agent_version,omitempty"`
	TraceID             string            `json:"trace_id,omitempty"`
	SpanID              string            `json:"span_id,omitempty"`
	OccurredAt          time.Time         `json:"occurred_at"`
	Vendor              string            `json:"vendor"`
	Model               string            `json:"model"`
	RequestedModel      string            `json:"requested_model"`
	Effort              string            `json:"effort,omitempty"`
	Modes               map[string]string `json:"modes"`
	InputTokens         *int64            `json:"input_tokens,omitempty"`
	OutputTokens        *int64            `json:"output_tokens,omitempty"`
	CachedInputTokens   *int64            `json:"cached_input_tokens,omitempty"`
	UncachedInputTokens *int64            `json:"uncached_input_tokens,omitempty"`
	ReasoningTokens     *int64            `json:"reasoning_tokens,omitempty"`
	DurationMS          *int64            `json:"duration_ms,omitempty"`
	ArtifactID          string            `json:"artifact_id"`
	Status              string            `json:"status"`
	Missing             []string          `json:"missing"`
	RawUsage            json.RawMessage   `json:"raw_usage,omitempty"`
}

type usageOutbox struct {
	Measurement usageMeasurement `json:"measurement"`
	Attribution usageAttribution `json:"attribution"`
	Phase       string           `json:"phase"`
}

type usagePending struct {
	ArtifactID string `json:"artifact_id"`
}

type usageAttribution struct {
	ActivityID      string            `json:"activity_id,omitempty"`
	ActivityAttempt string            `json:"activity_attempt,omitempty"`
	Turn            string            `json:"turn,omitempty"`
	AgentRef        string            `json:"agent_ref,omitempty"`
	RequestSettings map[string]string `json:"request_settings,omitempty"`
}

func newAttemptID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	s := hex.EncodeToString(raw[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

func usageContextFromRequest(r *http.Request, span trace.Span, id string, at time.Time, selected route, requestedModel, effort string, baseModes, requestSettings map[string]string) usageContext {
	agentRef := r.Header.Get("X-Agent-Ref")
	agent, versionText := agentRef, ""
	if name, v, ok := strings.Cut(agent, "@"); ok {
		agent, versionText = name, v
	}
	var version *int
	if strings.HasPrefix(versionText, "v") {
		if parsed, err := strconv.Atoi(strings.TrimPrefix(versionText, "v")); err == nil && parsed > 0 {
			version = &parsed
		}
	}
	modes := make(map[string]string, len(baseModes))
	for key, value := range baseModes {
		modes[key] = value
	}
	sc := span.SpanContext()
	c := usageContext{
		ID: id, RunID: r.Header.Get("X-Trace-Id"), ExecutionID: r.Header.Get("X-Execution-Id"), ParentExecutionID: r.Header.Get("X-Parent-Execution-Id"), Agent: agent, AgentRef: agentRef, AgentVersion: version,
		Vendor: selected.provider.Name, Model: selected.model,
		RequestedModel: requestedModel, Effort: effort, OccurredAt: at, Modes: modes,
		ActivityID: r.Header.Get("X-Activity-Id"), ActivityAttempt: r.Header.Get("X-Activity-Attempt"), Turn: r.Header.Get("X-Turn"), RequestSettings: requestSettings,
	}
	if sc.IsValid() {
		c.TraceID = sc.TraceID().String()
		c.SpanID = sc.SpanID().String()
	}
	return c
}

// usageObserver sees a copy of bytes while the original response is forwarded.
// Native SSE retains at most one bounded frame and the last non-null usage object.
type usageObserver struct {
	streaming  bool
	pending    []byte
	raw        json.RawMessage
	overflow   bool
	discarding bool
}

func newUsageObserver(streaming bool) *usageObserver { return &usageObserver{streaming: streaming} }

func nextSSEFrame(data []byte) (int, int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	if crlf >= 0 && (lf < 0 || crlf < lf) {
		return crlf, 4
	}
	if lf >= 0 {
		return lf, 2
	}
	return -1, 0
}

func (o *usageObserver) Write(p []byte) (int, error) {
	originalLen := len(p)
	if o.streaming && o.discarding {
		combined := append(o.pending, p...)
		end, separator := nextSSEFrame(combined)
		if end < 0 {
			o.pending = append(o.pending[:0], combined[max(0, len(combined)-1):]...)
			return originalLen, nil
		}
		o.pending, o.discarding = nil, false
		p = combined[end+separator:]
	}
	if o.overflow {
		return originalLen, nil
	}
	if len(o.pending)+len(p) > 4<<20 {
		if o.streaming {
			probe := append(append([]byte(nil), o.pending[max(0, len(o.pending)-3):]...), p...)
			if end, separator := nextSSEFrame(probe); end >= 0 {
				o.pending = append(o.pending[:0], probe[end+separator:]...)
				p = nil
			} else {
				o.pending = append(o.pending[:0], probe[max(0, len(probe)-3):]...)
				o.discarding = true
				return originalLen, nil
			}
		} else {
			o.pending, o.overflow = nil, true
			return originalLen, nil
		}
	}
	o.pending = append(o.pending, p...)
	if !o.streaming {
		return len(p), nil
	}
	for {
		end, separator := nextSSEFrame(o.pending)
		if end < 0 {
			break
		}
		frame := o.pending[:end]
		o.pending = o.pending[end+separator:]
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(data, []byte("[DONE]")) {
				continue
			}
			var object map[string]json.RawMessage
			if json.Unmarshal(data, &object) == nil && len(object["usage"]) > 0 && string(object["usage"]) != "null" {
				o.raw = append(o.raw[:0], object["usage"]...)
			}
		}
	}
	return originalLen, nil
}

func (o *usageObserver) Usage() json.RawMessage {
	if o.streaming {
		return append(json.RawMessage(nil), o.raw...)
	}
	var object map[string]json.RawMessage
	if !o.overflow && json.Unmarshal(o.pending, &object) == nil && len(object["usage"]) > 0 && string(object["usage"]) != "null" {
		return append(json.RawMessage(nil), object["usage"]...)
	}
	return nil
}

func normalizeUsage(c usageContext, raw json.RawMessage, complete bool) usageMeasurement {
	m := usageMeasurement{
		SchemaVersion: 1, ID: c.ID, Kind: "usage", RunID: c.RunID, ExecutionID: c.ExecutionID, ParentExecutionID: c.ParentExecutionID,
		Agent: c.Agent, AgentVersion: c.AgentVersion, TraceID: c.TraceID, SpanID: c.SpanID, OccurredAt: c.OccurredAt,
		Vendor: c.Vendor, Model: c.Model, RequestedModel: c.RequestedModel, Effort: c.Effort, Modes: c.Modes,
		ArtifactID: usageOutboxPrefix + "/" + c.ID + ".json", Status: "reported", Missing: []string{}, RawUsage: raw,
	}
	m.DurationMS = c.DurationMS
	if c.RunID == "" {
		m.Missing = append(m.Missing, "run_id/request attribution omitted")
	}
	if c.ExecutionID == "" {
		m.Missing = append(m.Missing, "execution_id/request attribution omitted")
	}
	if c.Effort == "" {
		m.Missing = append(m.Missing, "effort/request omitted")
	}
	if c.AgentRef != "" && c.AgentVersion == nil {
		m.Missing = append(m.Missing, "agent_version/agent reference is not vN")
	}
	if c.TraceID == "" {
		m.Missing = append(m.Missing, "trace_id/trace context unavailable")
	}
	if c.SpanID == "" {
		m.Missing = append(m.Missing, "span_id/trace context unavailable")
	}
	if len(raw) == 0 {
		m.Status = "missing"
		m.Missing = append(m.Missing, "raw_usage/provider omitted usage", "input_tokens/provider usage omitted", "output_tokens/provider usage omitted", "cached_input_tokens/provider usage omitted", "uncached_input_tokens/provider usage omitted", "reasoning_tokens/provider usage omitted")
		if !complete {
			m.Missing = append(m.Missing, "response/interrupted before terminal usage")
		}
		return m
	}
	var u struct {
		Prompt        *int64 `json:"prompt_tokens"`
		Completion    *int64 `json:"completion_tokens"`
		Input         *int64 `json:"input_tokens"`
		Output        *int64 `json:"output_tokens"`
		Cached        *int64 `json:"cached_input_tokens"`
		Uncached      *int64 `json:"uncached_input_tokens"`
		Reasoning     *int64 `json:"reasoning_tokens"`
		PromptDetails struct {
			Cached *int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails struct {
			Reasoning *int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if json.Unmarshal(raw, &u) != nil {
		m.Status, m.Missing = "invalid", []string{"raw_usage/invalid provider usage object"}
		return m
	}
	m.InputTokens, m.OutputTokens = u.Prompt, u.Completion
	if m.InputTokens == nil {
		m.InputTokens = u.Input
	}
	if m.OutputTokens == nil {
		m.OutputTokens = u.Output
	}
	m.CachedInputTokens = u.Cached
	if m.CachedInputTokens == nil {
		m.CachedInputTokens = u.PromptDetails.Cached
	}
	m.ReasoningTokens = u.Reasoning
	if m.ReasoningTokens == nil {
		m.ReasoningTokens = u.CompletionDetails.Reasoning
	}
	m.UncachedInputTokens = u.Uncached
	if m.UncachedInputTokens == nil && m.InputTokens != nil && m.CachedInputTokens != nil && *m.InputTokens >= *m.CachedInputTokens {
		value := *m.InputTokens - *m.CachedInputTokens
		m.UncachedInputTokens = &value
	}
	if m.InputTokens == nil {
		m.Missing = append(m.Missing, "input_tokens/provider usage omitted")
	}
	if m.OutputTokens == nil {
		m.Missing = append(m.Missing, "output_tokens/provider usage omitted")
	}
	if m.CachedInputTokens == nil {
		m.Missing = append(m.Missing, "cached_input_tokens/provider usage omitted")
	}
	if m.UncachedInputTokens == nil {
		m.Missing = append(m.Missing, "uncached_input_tokens/provider usage omitted")
	}
	if m.ReasoningTokens == nil {
		m.Missing = append(m.Missing, "reasoning_tokens/provider usage omitted")
	}
	if !complete {
		m.Missing = append(m.Missing, "response/interrupted")
	}
	return m
}

func (g *Gateway) beginUsageAttempt(ctx context.Context, c usageContext) error {
	if g.blobstoreURL == "" && g.evaluationURL == "" {
		return nil
	}
	if g.blobstoreURL == "" || g.evaluationURL == "" {
		return errors.New("usage evidence destinations are not configured")
	}
	outbox := usageOutbox{Phase: "active", Measurement: normalizeUsage(c, nil, false), Attribution: usageAttribution{
		ActivityID: c.ActivityID, ActivityAttempt: c.ActivityAttempt, Turn: c.Turn, AgentRef: c.AgentRef, RequestSettings: c.RequestSettings,
	}}
	if err := g.storeOutbox(ctx, outbox, true); err != nil {
		return err
	}
	return g.storePending(ctx, usagePending{ArtifactID: outbox.Measurement.ArtifactID}, true)
}

func (g *Gateway) completeUsage(ctx context.Context, c usageContext, raw json.RawMessage, complete bool) {
	if g.blobstoreURL == "" && g.evaluationURL == "" {
		return
	}
	duration := max(int64(0), time.Since(c.OccurredAt).Milliseconds())
	c.DurationMS = &duration
	outbox := usageOutbox{Phase: "final", Measurement: normalizeUsage(c, raw, complete), Attribution: usageAttribution{
		ActivityID: c.ActivityID, ActivityAttempt: c.ActivityAttempt, Turn: c.Turn, AgentRef: c.AgentRef, RequestSettings: c.RequestSettings,
	}}
	deliveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var err error
	for range 3 {
		if err = g.storeOutbox(deliveryCtx, outbox, false); err == nil {
			break
		}
	}
	if err != nil {
		g.log.Error("usage evidence persistence failed", "attempt_id", c.ID, "error", err)
		return
	}
	if err := g.deliverOutbox(deliveryCtx, &outbox); err != nil {
		g.log.Error("usage measurement delivery failed", "attempt_id", c.ID, "error", err)
		return
	}
	if err := g.deletePending(deliveryCtx, c.ID); err != nil {
		g.log.Error("usage pending marker removal failed", "attempt_id", c.ID, "error", err)
	}
}

func (g *Gateway) storeOutbox(ctx context.Context, outbox usageOutbox, createOnly bool) error {
	data, err := json.Marshal(outbox)
	if err != nil {
		return err
	}
	endpoint := g.blobstoreURL + "/blobs/" + outbox.Measurement.ArtifactID
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if createOnly {
		req.Header.Set("If-None-Match", "*")
	}
	resp, err := g.evidenceClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("outbox PUT: %s", resp.Status)
	}
	return nil
}

func (g *Gateway) storePending(ctx context.Context, pending usagePending, createOnly bool) error {
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	id := strings.TrimSuffix(strings.TrimPrefix(pending.ArtifactID, usageOutboxPrefix+"/"), ".json")
	endpoint := g.blobstoreURL + "/blobs/" + usagePendingPrefix + "/" + id + ".json"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if createOnly {
		req.Header.Set("If-None-Match", "*")
	}
	resp, err := g.evidenceClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("pending PUT: %s", resp.Status)
	}
	return nil
}

func (g *Gateway) deletePending(ctx context.Context, id string) error {
	endpoint := g.blobstoreURL + "/blobs/" + usagePendingPrefix + "/" + id + ".json"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := g.evidenceClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("pending DELETE: %s", resp.Status)
	}
	return nil
}

func (g *Gateway) deliverOutbox(ctx context.Context, outbox *usageOutbox) error {
	data, err := json.Marshal(outbox.Measurement)
	if err != nil {
		return err
	}
	endpoint := g.evaluationURL + "/api/measurements/" + url.PathEscape(outbox.Measurement.ID)
	for attempt := 0; attempt < 3; attempt++ {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(data))
		if reqErr != nil {
			return reqErr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-None-Match", "*")
		resp, doErr := g.evidenceClient.Do(req)
		if doErr != nil {
			err = doErr
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			err = readErr
			continue
		}
		switch {
		case resp.StatusCode/100 == 2:
			err = nil
		case resp.StatusCode == http.StatusPreconditionFailed:
			err = g.compareExistingMeasurement(ctx, endpoint, data)
		default:
			err = fmt.Errorf("evaluation PUT: %s: %s", resp.Status, bytes.TrimSpace(body))
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	return nil
}

func (g *Gateway) compareExistingMeasurement(ctx context.Context, endpoint string, expected []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := g.evidenceClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("evaluation conflict GET: %s", resp.Status)
	}
	var want, got map[string]any
	if json.Unmarshal(expected, &want) != nil || json.Unmarshal(body, &got) != nil {
		return fmt.Errorf("evaluation conflict response is invalid JSON")
	}
	revision, _ := got["revision"].(string)
	delete(got, "revision")
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if !bytes.Equal(wantJSON, gotJSON) {
		if got["status"] == "missing" && want["status"] == "reported" && sameAttemptIdentity(got, want) && revision != "" {
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(expected))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", `"`+revision+`"`)
			resp, err := g.evidenceClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			if resp.StatusCode/100 == 2 {
				return nil
			}
			return fmt.Errorf("measurement recovery CAS: %s", resp.Status)
		}
		return fmt.Errorf("measurement identity %s has different content", want["id"])
	}
	return nil
}

func sameAttemptIdentity(a, b map[string]any) bool {
	for _, key := range []string{"schema_version", "id", "kind", "run_id", "execution_id", "parent_execution_id", "agent", "agent_version", "trace_id", "span_id", "occurred_at", "vendor", "model", "requested_model", "effort", "modes", "artifact_id"} {
		if !reflect.DeepEqual(a[key], b[key]) {
			return false
		}
	}
	return true
}

func (g *Gateway) replayUsageOutbox(ctx context.Context) error {
	if g.blobstoreURL == "" || g.evaluationURL == "" {
		return nil
	}
	count, err := g.pendingUsageCount(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for range (count + usageReplayLimit - 1) / usageReplayLimit {
		if err := g.replayPendingUsage(ctx, true); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (g *Gateway) pendingUsageCount(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.blobstoreURL+"/list/"+usagePendingPrefix, nil)
	if err != nil {
		return 0, err
	}
	resp, err := g.evidenceClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("pending usage list: %s", resp.Status)
	}
	var listing struct {
		Objects []json.RawMessage `json:"objects"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&listing); err != nil {
		return 0, err
	}
	return len(listing.Objects), nil
}

func (g *Gateway) replayPendingUsage(ctx context.Context, recoverActive bool) error {
	if g.blobstoreURL == "" || g.evaluationURL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.blobstoreURL+"/list/"+usagePendingPrefix, nil)
	if err != nil {
		return err
	}
	resp, err := g.evidenceClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pending usage list: %s", resp.Status)
	}
	var listing struct {
		Objects []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&listing); err != nil {
		return err
	}
	if len(listing.Objects) == 0 {
		return nil
	}
	g.replayMu.Lock()
	start := g.replayOffset % len(listing.Objects)
	count := min(usageReplayLimit, len(listing.Objects))
	g.replayOffset = (start + count) % len(listing.Objects)
	g.replayMu.Unlock()
	var replayErrors []error
	for i := 0; i < count; i++ {
		object := listing.Objects[(start+i)%len(listing.Objects)]
		var pending usagePending
		if err := g.getBlobJSON(ctx, object.ID, &pending); err != nil {
			replayErrors = append(replayErrors, fmt.Errorf("reading pending %s: %w", object.ID, err))
			continue
		}
		if !strings.HasPrefix(pending.ArtifactID, usageOutboxPrefix+"/") {
			replayErrors = append(replayErrors, fmt.Errorf("pending %s has invalid artifact", object.ID))
			continue
		}
		var outbox usageOutbox
		if err := g.getBlobJSON(ctx, pending.ArtifactID, &outbox); err != nil {
			replayErrors = append(replayErrors, fmt.Errorf("reading outbox %s: %w", pending.ArtifactID, err))
			continue
		}
		if outbox.Phase == "active" {
			if !recoverActive {
				continue
			}
			outbox.Phase = "final"
			outbox.Measurement.DurationMS = nil
			outbox.Measurement.Status = "missing"
			outbox.Measurement.RawUsage = nil
			outbox.Measurement.Missing = append(outbox.Measurement.Missing, "response/process interrupted before final usage")
			if err := g.storeOutbox(ctx, outbox, false); err != nil {
				replayErrors = append(replayErrors, err)
				continue
			}
		} else if outbox.Phase != "" && outbox.Phase != "final" {
			replayErrors = append(replayErrors, fmt.Errorf("outbox %s has invalid phase", pending.ArtifactID))
			continue
		}
		if err := g.deliverOutbox(ctx, &outbox); err != nil {
			replayErrors = append(replayErrors, fmt.Errorf("delivering %s: %w", outbox.Measurement.ID, err))
			continue
		}
		if err := g.deletePending(ctx, outbox.Measurement.ID); err != nil {
			replayErrors = append(replayErrors, err)
		}
	}
	return errors.Join(replayErrors...)
}

func (g *Gateway) getBlobJSON(ctx context.Context, id string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.blobstoreURL+"/blobs/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := g.evidenceClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", id, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(target)
}

func (g *Gateway) replayUsageOutboxLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			replayCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if err := g.replayPendingUsage(replayCtx, false); err != nil {
				g.log.Warn("usage outbox replay incomplete", "error", err)
			}
			cancel()
		}
	}
}
