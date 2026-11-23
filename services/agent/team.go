package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/temporal"
)

// TeamTurnRequest is the input of the TeamTurn activity: one turn of one member of a team.
type TeamTurnRequest struct {
	TraceID string `json:"trace_id"`
	StepID  string `json:"step_id"`
	Team    string `json:"team"`
	Member  string `json:"member"`
	Agent   string `json:"agent"`
	// DevPod is the run's dev pod; empty when the run has none.
	DevPod                    string            `json:"dev_pod,omitempty"`
	Skills                    map[string]string `json:"skills,omitempty"`
	SkillScriptTimeoutSeconds int               `json:"skill_script_timeout_seconds,omitempty"`
}

// TeamTurnResult reports how many model calls the turn made; 0 when nothing was unread.
type TeamTurnResult struct {
	ModelCalls int `json:"model_calls"`
	// Blocked is the reason the member gave with report_blocked in this turn; else empty.
	Blocked string `json:"blocked,omitempty"`
}

// teamFile is <team>/team.json, written by the orchestrator.
type teamFile struct {
	Lead    string `json:"lead"`
	Members []struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Ref         string `json:"ref"`
		Description string `json:"description"`
	} `json:"members"`
	Channels []teamChannel `json:"channels"`
}

// teamChannel is a chat between two members. From names the only member that may send on a
// one-way channel; it is empty when both may send.
type teamChannel struct {
	ID      string   `json:"id"`
	Members []string `json:"members"`
	From    string   `json:"from,omitempty"`
}

// sends reports whether the member may send on the channel.
func (c teamChannel) sends(member string) bool {
	return c.From == "" || c.From == member
}

// teamQueue is <team>/channels/<channel-id>/<sender>.json: the messages one member of a
// channel sent to the other. A queue that does not exist yet is empty.
type teamQueue struct {
	Messages []teamMessage `json:"messages"`
}

type teamMessage struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
	Sent string `json:"sent"`
}

// teamMemberLog is <team>/members/<name>.json: the member's own history and how far it
// has read each of its channels. Only the agent service writes it.
type teamMemberLog struct {
	Member      string            `json:"member"`
	Agent       string            `json:"agent"`
	Model       string            `json:"model,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	Read        map[string]int    `json:"read"`
	Messages    []json.RawMessage `json:"messages"`
	// Events holds one entry per model call and per tool call of every turn and only grows.
	Events []TurnEvent `json:"events,omitempty"`
}

// turnBudgetError reports a turn that used all its model calls without a final answer.
type turnBudgetError struct{ text string }

func (e turnBudgetError) Error() string { return e.text }

// teamWorkflowMember is the reserved name of the orchestrator's side of the lead's task channel.
const teamWorkflowMember = "workflow"

func invalidTeamTurn(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), "InvalidTeamTurn", nil)
}

// otherSide returns the member on the other end of a channel.
func (c teamChannel) otherSide(member string) (string, bool) {
	if len(c.Members) != 2 {
		return "", false
	}
	switch member {
	case c.Members[0]:
		return c.Members[1], true
	case c.Members[1]:
		return c.Members[0], true
	}
	return "", false
}

// memberChannels lists the channels of a member. The lead always has its channel to the
// workflow, where the task and gate rejections arrive, also when team.json does not list it.
func (t teamFile) memberChannels(member string) []teamChannel {
	var out []teamChannel
	hasWorkflow := false
	for _, c := range t.Channels {
		if other, ok := c.otherSide(member); ok {
			out = append(out, c)
			hasWorkflow = hasWorkflow || other == teamWorkflowMember
		}
	}
	if member == t.Lead && !hasWorkflow {
		out = append(out, teamChannel{ID: member + "--" + teamWorkflowMember, Members: []string{member, teamWorkflowMember}})
	}
	return out
}

func (t teamFile) description(member string) string {
	if member == teamWorkflowMember {
		return "The workflow that runs this team. It sends the task and, later, review rejections that the team must work in."
	}
	for _, m := range t.Members {
		if m.Name == member {
			return strings.Join(strings.Fields(m.Description), " ")
		}
	}
	return ""
}

// teamNote is appended to the system prompt on a member's first turn. It is explicit
// because small local models run team members.
func teamNote(team string, t teamFile, member string, channels []teamChannel) string {
	isLead := member == t.Lead
	var b strings.Builder
	b.WriteString("\n\n## Team work\n")
	fmt.Fprintf(&b, "You are the team member `%s` in the team `%s`. ", member, team)
	if isLead {
		b.WriteString("You are the lead of this team.\n")
	} else {
		fmt.Fprintf(&b, "The lead of this team is `%s`.\n", t.Lead)
	}
	b.WriteString("Tool names may carry a prefix such as `factory__`. Use the tool whose name ends with the name given here.\n")

	b.WriteString("\n### Your channels\n")
	b.WriteString("A channel is a chat between you and exactly one other member. These are all your channels:\n")
	workflowChannel := ""
	for _, c := range channels {
		other, _ := c.otherSide(member)
		switch {
		case !c.sends(member):
			fmt.Fprintf(&b, "- channel `%s` <- receives from `%s` only; you cannot send on it.", c.ID, other)
		case c.From != "":
			fmt.Fprintf(&b, "- channel `%s` -> reaches `%s`; `%s` cannot answer on it.", c.ID, other, other)
		default:
			fmt.Fprintf(&b, "- channel `%s` -> reaches `%s`.", c.ID, other)
		}
		if d := t.description(other); d != "" {
			b.WriteString(" " + d)
		}
		b.WriteString("\n")
		if other == teamWorkflowMember {
			workflowChannel = c.ID
		}
	}
	b.WriteString("You can only talk to the members listed here.\n")

	b.WriteString("\n### Receiving messages\n")
	b.WriteString("Every incoming message is a user message that starts with the line `[Message from <name> on channel <channel id>]`. " +
		"The rest is the text that member sent you. Messages arrive on their own; do not wait or poll for them.\n")

	b.WriteString("\n### Sending messages\n")
	b.WriteString("To answer or instruct a member, call the tool send_message with `channel` = the channel id from the list above and `text` = your message. " +
		"`channel` selects who receives the message. ")
	fmt.Fprintf(&b, "The arguments `team` and `member` of the team tools are filled in automatically: `team` is always `%s` and `member` is always your own name `%s`, never the recipient. ", team, member)
	if isLead {
		b.WriteString("Text you write without a tool call is delivered to nobody. Only send_message delivers a message. ")
	} else {
		b.WriteString("If you end your turn without calling send_message, your final answer is sent as your reply to the member who wrote to you last on a channel you can send on. ")
	}
	b.WriteString("In one turn you can send only one message per channel, so put everything the other member needs into it.\n")
	if workflowChannel != "" {
		fmt.Fprintf(&b, "Do not send messages on channel `%s`; `workflow` does not answer.\n", workflowChannel)
	}
	b.WriteString("If you have the tools list_channels and read_messages, they show your channels and let you re-read a channel.\n")

	b.WriteString("\n### Shared files\n")
	fmt.Fprintf(&b, "All members share one set of files. Work with them using the tools list_session_files, read_session_file, "+
		"search_session_files and write_session_file. Their argument `session` is filled in automatically and is always `session` = `%s`. ", team)
	b.WriteString("Write code, tests and documents as files there, then tell the other member the file names in a message instead of pasting the content.\n")
	if isLead {
		b.WriteString("The inputs of the task are files there whose names start with `input-`.\n")
	}

	b.WriteString("\n### Ending your turn\n")
	b.WriteString("When you have done everything you can do right now, reply with one short sentence and no tool call. " +
		"That ends your turn.\n")
	b.WriteString("After you have sent your messages, end your turn. You are woken automatically when a reply arrives. Never send messages that only say you are waiting.\n")

	b.WriteString("\n### Finishing the team's work\n")
	if isLead {
		fmt.Fprintf(&b, "Only you can finish the team's work. When the work is complete, call the tool finish_team with "+
			"`files` = the list of the result file names and `summary` = a short summary of the result. "+
			"Every file you name must exist in the shared files. Until you call finish_team the team is not done.\n")
	} else {
		fmt.Fprintf(&b, "You cannot finish the team's work. Only the lead `%s` can. Report your result to the member who asked for it with send_message.\n", t.Lead)
	}
	return b.String()
}

// unreadTeamMessage is one message to deliver, with the channel it came on.
type unreadTeamMessage struct {
	channel string
	from    string
	order   int
	teamMessage
}

// sentBefore orders messages by their `sent` time; unparsable times compare as text.
func sentBefore(a, b string) (before, equal bool) {
	ta, errA := time.Parse(time.RFC3339Nano, a)
	tb, errB := time.Parse(time.RFC3339Nano, b)
	if errA == nil && errB == nil {
		return ta.Before(tb), ta.Equal(tb)
	}
	return a < b, a == b
}

func (s *Service) teamGetJSON(ctx context.Context, id string, v any) error {
	data, err := s.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("reading %q: %w", id, err)
	}
	return nil
}

// TeamTurn runs one turn of one team member: it appends the member's unread channel
// messages to the member's stored history, runs the model and tool loop, and stores the
// grown history together with the advanced read markers.
func (s *Service) TeamTurn(ctx context.Context, req TeamTurnRequest) (result TeamTurnResult, retErr error) {
	if !segmentPattern.MatchString(req.TraceID) {
		return result, invalidTeamTurn("trace_id is missing or invalid")
	}
	if !validID(req.StepID) {
		return result, invalidTeamTurn("step_id is missing or invalid")
	}
	if !strings.HasPrefix(req.Team, "teams/") || !validID(req.Team) {
		return result, invalidTeamTurn("team must be a path starting with %q", "teams/")
	}
	if !segmentPattern.MatchString(req.Member) || req.Member == teamWorkflowMember {
		return result, invalidTeamTurn("member is missing or invalid")
	}
	name, version, err := parseRef(req.Agent)
	if err != nil {
		return result, invalidTeamTurn("%s", err.Error())
	}
	ref := name + "@" + version
	memberID := req.Team + "/members/" + req.Member + ".json"
	if !validID(memberID) {
		return result, invalidTeamTurn("team is too long")
	}

	ctx, span := otel.Tracer("opensoftwarefactory/agent").Start(ctx, "agent.team_turn",
		trace.WithAttributes(attribute.String("factory.run_id", req.TraceID), attribute.String("factory.step", req.StepID),
			attribute.String("factory.agent", ref), attribute.String("factory.team", req.Team), attribute.String("factory.team_member", req.Member)))
	defer span.End()
	ctx = withCorrelation(ctx, StepRequest{TraceID: req.TraceID, StepID: req.StepID, Agent: ref, OutputArtifact: memberID}, ref, attemptOf(ctx))
	log := s.Logger.With("run_id", req.TraceID, "step", req.StepID, "agent", ref, "team", req.Team, "member", req.Member)
	started := time.Now()
	delivered := 0
	defer func() {
		if retErr != nil {
			span.RecordError(retErr)
			span.SetStatus(codes.Error, errorCategory(retErr))
			log.Error("team turn failed", "error", retErr, "delivered", delivered, "model_calls", result.ModelCalls)
			return
		}
		span.SetStatus(codes.Ok, "completed")
		log.Info("team turn completed", "delivered", delivered, "model_calls", result.ModelCalls, "processing_time_ms", time.Since(started).Milliseconds())
	}()

	defer lockChat(memberID)()

	var team teamFile
	if err := s.teamGetJSON(ctx, req.Team+"/team.json", &team); err != nil {
		return result, fmt.Errorf("reading team %q: %w", req.Team, err)
	}
	known := false
	for _, m := range team.Members {
		known = known || m.Name == req.Member
	}
	if !known {
		return result, invalidTeamTurn("member %q is not in team %q", req.Member, req.Team)
	}
	channels := team.memberChannels(req.Member)

	history := teamMemberLog{Member: req.Member, Agent: ref}
	var missing notFoundError
	switch err := s.teamGetJSON(ctx, memberID, &history); {
	case errors.As(err, &missing):
	case err != nil:
		return result, fmt.Errorf("reading member history %q: %w", memberID, err)
	case history.Agent != ref:
		return result, invalidTeamTurn("member %q of team %q already belongs to agent %s", req.Member, req.Team, history.Agent)
	}
	if history.Read == nil {
		history.Read = map[string]int{}
	}

	// Collect what the other side of each channel sent after the read marker: the marker is the
	// highest seq of the other side's queue this member has been given.
	var unread []unreadTeamMessage
	for i, c := range channels {
		other, _ := c.otherSide(req.Member)
		var queue teamQueue
		if err := s.teamGetJSON(ctx, req.Team+"/channels/"+c.ID+"/"+other+".json", &queue); errors.As(err, &missing) {
			continue
		} else if err != nil {
			return result, fmt.Errorf("reading channel %q: %w", c.ID, err)
		}
		for _, m := range queue.Messages {
			if m.Seq > history.Read[c.ID] {
				unread = append(unread, unreadTeamMessage{channel: c.ID, from: other, order: i, teamMessage: m})
			}
		}
	}
	if len(unread) == 0 {
		return result, nil
	}
	sort.SliceStable(unread, func(i, j int) bool {
		a, b := unread[i], unread[j]
		if before, equal := sentBefore(a.Sent, b.Sent); !equal {
			return before
		}
		if a.order != b.order {
			return a.order < b.order
		}
		return a.Seq < b.Seq
	})

	prompt, def, err := s.loadAgent(ctx, name, version)
	if err != nil {
		return result, err
	}
	history.Model, history.Temperature = def.Models[0], def.Temperature
	turn := chatLog{Messages: history.Messages, Events: history.Events}
	firstTurn := len(turn.Messages) == 0
	if firstTurn {
		if prompt, _, err = s.compilePrompt(ctx, prompt); err != nil {
			return result, err
		}
		skillNote, _, err := s.loadSkillNote(ctx, req.Skills)
		if err != nil {
			return result, err
		}
		prompt += skillNote
		if err := turn.add(map[string]string{"role": "system", "content": prompt + teamNote(req.Team, team, req.Member, channels)}); err != nil {
			return result, err
		}
	}
	for i, m := range unread {
		content := fmt.Sprintf("[Message from %s on channel %s]\n%s", m.from, m.channel, m.Text)
		if i == 0 && firstTurn && strings.TrimSpace(req.DevPod) != "" {
			content += devPodNote(def.Commands)
		}
		if err := turn.add(map[string]string{"role": "user", "content": content}); err != nil {
			return result, err
		}
		history.Read[m.channel] = max(history.Read[m.channel], m.Seq)
	}
	delivered = len(unread)
	before := len(turn.Messages)
	// From here on the history is stored whatever happens, so no message is delivered twice.
	defer func() {
		history.Messages, history.Events = turn.Messages, turn.Events
		result.ModelCalls = 0
		for _, raw := range turn.Messages[before:] {
			var m struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(raw, &m) == nil && m.Role != "tool" {
				result.ModelCalls++
			}
		}
		if err := s.saveTeamMember(ctx, memberID, history, Meta{Creator: "agent:" + ref, TraceID: req.TraceID}); err != nil {
			log.Error("persisting member history failed", "error", err)
			if retErr == nil {
				retErr = err
			}
		}
	}()

	// The markers are stored before the model runs, so the team tools report the delivered
	// messages as read during the turn.
	history.Messages = turn.Messages
	if err := s.saveTeamMember(ctx, memberID, history, Meta{Creator: "agent:" + ref, TraceID: req.TraceID}); err != nil {
		return result, err
	}

	turnCtx := withTeamTurnTools(withDevPod(ctx, req.DevPod, ref, req.TraceID), req.Team, req.Member)
	turnCtx = withSkills(turnCtx, req.Skills, req.SkillScriptTimeoutSeconds)
	reply, err := s.chatTurn(turnCtx, ref, def, &turn, log)
	if reason, ok := blockedReason(turnCtx); ok {
		result.Blocked = reason
	}
	var budget turnBudgetError
	var stopped turnStoppedError
	switch {
	case errors.As(err, &stopped):
		log.Warn("team turn ended by the harness", "detail", err.Error())
		reply = ""
	case errors.As(err, &budget):
		// The turn ends here; what the member sent so far is delivered and the team goes on.
		log.Warn("team turn used its model-call budget", "detail", err.Error())
		reply = lastAssistantText(turn.Messages[before:])
	case err != nil:
		return result, err
	}
	if req.Member != team.Lead {
		// A member's last answer is its reply.
		// Only a message that came on a channel the member may send on can be answered.
		var answerable []unreadTeamMessage
		for _, m := range unread {
			for _, c := range channels {
				if c.ID == m.channel && c.sends(req.Member) {
					answerable = append(answerable, m)
				}
			}
		}
		if err := s.sendFinalAnswer(ctx, req, def, answerable, turn.Messages[before:], reply, log); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) saveTeamMember(ctx context.Context, memberID string, history teamMemberLog, meta Meta) error {
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	// A canceled or timed-out turn is still stored.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatSaveTimeout)
	defer cancel()
	if err := s.Store.Put(ctx, memberID, data, meta); err != nil {
		return fmt.Errorf("writing member history %q: %w", memberID, err)
	}
	return nil
}

const teamSendTool = "send_message"

// turnMessage is the part of a stored message the harness inspects after a turn.
type turnMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls"`
	ToolCallID string     `json:"tool_call_id"`
}

func isSendTool(alias string) bool {
	return alias == teamSendTool || strings.HasSuffix(alias, "__"+teamSendTool)
}

// lastAssistantText returns the last non-empty text the model wrote in a turn.
func lastAssistantText(messages []json.RawMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		var m turnMessage
		if json.Unmarshal(messages[i], &m) == nil && m.Role == "assistant" && m.Content != nil && strings.TrimSpace(*m.Content) != "" {
			return *m.Content
		}
	}
	return ""
}

// sentMessage reports whether a send_message call of the turn succeeded.
func sentMessage(messages []json.RawMessage) bool {
	sends := map[string]bool{}
	for _, raw := range messages {
		var m turnMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		for _, tc := range m.ToolCalls {
			if isSendTool(tc.Function.Name) {
				sends[tc.ID] = true
			}
		}
		if m.Role == "tool" && sends[m.ToolCallID] && (m.Content == nil || !strings.HasPrefix(*m.Content, "Error:")) {
			return true
		}
	}
	return false
}

// sendFinalAnswer sends a member's final answer as its reply when the member did not send
// a message itself: on the channel of the most recently delivered message, through the
// send_message tool of the gateway, as a tool call of the model would.
func (s *Service) sendFinalAnswer(ctx context.Context, req TeamTurnRequest, def Definition, delivered []unreadTeamMessage, turn []json.RawMessage, answer string, log *slog.Logger) error {
	if strings.TrimSpace(answer) == "" || sentMessage(turn) {
		return nil
	}
	channel := ""
	for i := len(delivered) - 1; i >= 0 && channel == ""; i-- {
		if delivered[i].from != teamWorkflowMember {
			channel = delivered[i].channel
		}
	}
	if channel == "" {
		return nil
	}
	toolRef := ""
	for _, ref := range def.Tools {
		if tool, err := parseConfiguredTool(ref); err == nil && tool.name == teamSendTool {
			toolRef = ref
			break
		}
	}
	if toolRef == "" {
		return fmt.Errorf("member %q ended its turn without sending a message, and its final answer cannot be sent as its reply: agent %s has no tool %s", req.Member, req.Agent, teamSendTool)
	}
	eventCtx, event, eventTrace := eventSpan(ctx, RunEvent{Kind: "tool_call", Started: time.Now().UTC()}, "agent.tool_call",
		attribute.String("gen_ai.tool.name", toolRef), attribute.Bool("factory.team_auto_reply", true))
	fail := func(err error) error {
		finishEventSpan(event, eventTrace, "failed", categorizedError{"mcp_gateway", err.Error()})
		return err
	}
	_, routes, err := s.discoverTools(eventCtx, []string{toolRef}, log)
	if err != nil {
		return fail(err)
	}
	defer closeToolRoutes(routes, log)
	if len(routes) == 0 {
		return fail(fmt.Errorf("member %q ended its turn without sending a message, and its final answer cannot be sent as its reply: tool %s is not available", req.Member, toolRef))
	}
	args, err := json.Marshal(map[string]string{"team": req.Team, "member": req.Member, "channel": channel, "text": answer})
	if err != nil {
		return fail(err)
	}
	for _, route := range routes {
		text, isError, err := route.client.callTool(eventCtx, route.name, args)
		if err != nil {
			return fail(fmt.Errorf("sending the final answer of member %q as its reply: %w", req.Member, err))
		}
		if isError {
			return fail(fmt.Errorf("sending the final answer of member %q as its reply: %s", req.Member, text))
		}
	}
	finishEventSpan(event, eventTrace, "completed", nil)
	log.Info("final answer sent as the member's reply", "channel", channel)
	return nil
}

// turnStoppedError reports a turn the harness ended because the member kept sending
// messages on a channel it had already used in the turn.
type turnStoppedError struct{ text string }

func (e turnStoppedError) Error() string { return e.text }

// teamRefusedSendsLimit is the number of refused send_message calls that ends a turn.
const teamRefusedSendsLimit = 2

// teamTurnTools is the harness's control over the tool calls of one team turn.
type teamTurnTools struct {
	team, member string
	sent         map[string]bool // channels the member sent a message on in this turn
	refused      int
}

type teamTurnToolsKey struct{}

func withTeamTurnTools(ctx context.Context, team, member string) context.Context {
	return context.WithValue(ctx, teamTurnToolsKey{}, &teamTurnTools{team: team, member: member, sent: map[string]bool{}})
}

// teamTurnStopped returns the error that ends a team turn early; nil outside a team turn.
func teamTurnStopped(ctx context.Context) error {
	if t, ok := ctx.Value(teamTurnToolsKey{}).(*teamTurnTools); ok && t.refused >= teamRefusedSendsLimit {
		return turnStoppedError{fmt.Sprintf("member %q sent %d more messages on channels it had already used in this turn", t.member, t.refused)}
	}
	return nil
}

// runTurnTool is runTool for a turn. In a team turn the harness, which knows who is
// running, sets the identity arguments of the team and session file tools whatever the
// model passed, and forwards at most one send_message per channel.
func (s *Service) runTurnTool(ctx context.Context, routes map[string]toolRoute, tc toolCall) (string, bool, error) {
	t, ok := ctx.Value(teamTurnToolsKey{}).(*teamTurnTools)
	route, known := routes[tc.Function.Name]
	if !ok || !known {
		return s.runTool(ctx, routes, tc)
	}
	var identity map[string]string
	switch route.name {
	case "list_channels", teamSendTool, "read_messages", "finish_team":
		identity = map[string]string{"team": t.team, "member": t.member}
	case "list_session_files", "read_session_file", "search_session_files", "write_session_file":
		identity = map[string]string{"session": t.team}
	default:
		return s.runTool(ctx, routes, tc)
	}
	args := map[string]any{}
	if strings.TrimSpace(tc.Function.Arguments) != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil || args == nil {
			return s.runTool(ctx, routes, tc) // reports the invalid arguments to the model
		}
	}
	for k, v := range identity {
		args[k] = v
	}
	channel := ""
	if route.name == teamSendTool {
		channel, _ = args["channel"].(string)
		if t.sent[channel] {
			t.refused++
			return fmt.Sprintf("Error: you already sent a message on channel %s in this turn. End your turn now by answering without a tool call; you are woken when a reply arrives.", channel), true, nil
		}
	}
	data, err := json.Marshal(args)
	if err != nil {
		return "", false, err
	}
	// The stored tool call stays as the model wrote it; only the forwarded copy changes.
	tc.Function.Arguments = string(data)
	text, failed, err := s.runTool(ctx, routes, tc)
	if route.name == teamSendTool && err == nil && !failed {
		t.sent[channel] = true
	}
	return text, failed, err
}
