package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Services of nested work.
const (
	// WorkflowService marks a step that runs the workflow named by its workflow field as a
	// child run under the same trace.
	WorkflowService = "workflow"
	// TeamService marks a step in which a lead agent and other members work together by
	// sending each other messages until the lead finishes the team.
	TeamService = "team"
)

// TeamWorkflowName is the registered Temporal workflow type of the team loop.
const TeamWorkflowName = "Team"

// TeamTurnActivity is the agent service's activity that runs one turn of a team member.
const TeamTurnActivity = "TeamTurn"

// Activity names of the orchestrator's own activities for teams and nested workflows.
const (
	OpenTeamActivity        = "OpenTeam"
	TeamStateActivity       = "TeamState"
	TeamTaskActivity        = "TeamTask"
	PostTeamMessageActivity = "PostTeamMessage"
	WriteTeamOutputActivity = "WriteTeamOutput"
	ResolveWorkflowActivity = "ResolveWorkflow"
)

const (
	// DefaultMaxTurns is the turn budget of a team step that sets none.
	DefaultMaxTurns = 40
	// MaxDepth is the deepest nesting of workflow steps and workflow team members a run may reach.
	MaxDepth = 8
	// DefaultMaxRejections is the number of rejections a gate with on_reject sends back when
	// it sets no max_rejections; a rejection beyond it fails the run.
	DefaultMaxRejections = 5
	// WorkflowSender is the reserved member name under which the orchestrator posts the task
	// and gate rejections to the lead.
	WorkflowSender = "workflow"
	// defaultTurnTimeout bounds one team turn unless the step sets a timeout.
	defaultTurnTimeout = 15 * time.Minute
	// maxNudges is the number of reminders a team run posts to a lead that neither acts nor finishes.
	maxNudges = 3
)

// NudgeText is posted to the lead when nobody is working and the team is not finished.
const NudgeText = "No member is working and you have not finished. Check the workspace files and your channels, " +
	"then either send instructions with send_message or end the work with finish_team(team, member, files, summary)."

// Member kinds.
const (
	AgentMember    = "agent"
	WorkflowMember = "workflow"
)

// TeamMember is one member of a team as stored in team.json.
type TeamMember struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Ref         string `json:"ref"`
	Description string `json:"description"`
}

// OneWay is the optional third entry of a channel in a team step: only the first member sends.
const OneWay = "one-way"

// TeamChannel is a stored chat between exactly two members. From names the only member that
// may send on a one-way channel; it is empty when both may send.
type TeamChannel struct {
	ID      string   `json:"id"`
	From    string   `json:"from,omitempty"`
	Members []string `json:"members"`
}

// sends reports whether the member may send on the channel.
func (c TeamChannel) sends(member string) bool {
	return c.From == "" || c.From == member
}

// TeamManifest is stored as <team>/team.json.
type TeamManifest struct {
	Lead    string       `json:"lead"`
	Members []TeamMember `json:"members"`
	TraceID string       `json:"trace_id"`
	StepID  string       `json:"step_id"`
	// Run marks the run of the team step that last opened the team; Round counts those runs.
	Run      string        `json:"run"`
	Channels []TeamChannel `json:"channels"`
	Round    int           `json:"round"`
}

// TeamMessage is one message of a queue; Seq counts from 1 per queue.
type TeamMessage struct {
	Text string `json:"text"`
	Sent string `json:"sent"`
	Seq  int    `json:"seq"`
}

// TeamQueue is stored as <team>/channels/<channel-id>/<sender>.json: the messages one member
// of a channel sent to the other. A channel is its two queues; each queue is written only on
// behalf of its sender, the orchestrator writing those of "workflow" and of workflow members.
// A queue that does not exist yet is empty.
type TeamQueue struct {
	Channel  string        `json:"channel"`
	From     string        `json:"from"`
	To       string        `json:"to"`
	Messages []TeamMessage `json:"messages"`
}

// sentLayout is RFC 3339 in UTC with a fixed number of fractional digits, so the sent times
// of the two queues of a channel also order as text.
const sentLayout = "2006-01-02T15:04:05.000000Z07:00"

// queueID names the queue of a sender in a channel.
func queueID(team, channel, sender string) string {
	return team + "/channels/" + channel + "/" + sender + ".json"
}

// other returns the member on the other end of a channel.
func (c TeamChannel) other(member string) string {
	for _, name := range c.Members {
		if name != member {
			return name
		}
	}
	return member
}

// teamMemberFile is <team>/members/<name>.json. The agent service owns the files of agent
// members; the orchestrator writes only those of workflow members.
type teamMemberFile struct {
	Member   string            `json:"member"`
	Agent    string            `json:"agent"`
	Read     map[string]int    `json:"read"`
	Messages []json.RawMessage `json:"messages"`
}

// TeamFinished is stored as <team>/finished.json by the finish_team tool.
type TeamFinished struct {
	Summary string   `json:"summary"`
	Files   []string `json:"files"`
}

// TeamOutputFile names one result file of a team and the blob that holds it.
type TeamOutputFile struct {
	Name     string `json:"name"`
	Artifact string `json:"artifact"`
}

// TeamOutput is the output artifact of a team step.
type TeamOutput struct {
	Summary string           `json:"summary"`
	Team    string           `json:"team"`
	Files   []TeamOutputFile `json:"files"`
}

// PendingTeam describes a team step that is running. Team is the blob prefix of the team.
// Pass is the pass of a team that runs after a merge or batch.
type PendingTeam struct {
	Step string `json:"step"`
	Team string `json:"team"`
	Pass int    `json:"pass,omitempty"`
}

// TeamRejection is the gate rejection a team receives when it runs again.
type TeamRejection struct {
	Gate      string `json:"gate"`
	DecidedBy string `json:"decided_by"`
	Reason    string `json:"reason"`
	Round     int    `json:"round"`
}

// TeamInput starts the team loop of one team step.
type TeamInput struct {
	TraceID  string `json:"trace_id"`
	Workflow string `json:"workflow"`
	Step     Step   `json:"step"`
	Prefix   string `json:"prefix"`
	Artifact string `json:"artifact"`
	Team     string `json:"team"`
	// Run changes whenever the team step runs again, so OpenTeam can tell that from a retry.
	Run       string         `json:"run,omitempty"`
	Rejection *TeamRejection `json:"rejection,omitempty"`
	// DevPod is the run's dev pod, nil when the team works without one.
	DevPod               *DevPod      `json:"dev_pod,omitempty"`
	Intake               *IntakeInput `json:"intake,omitempty"`
	Inputs               []Item       `json:"inputs"`
	Depth                int          `json:"depth,omitempty"`
	ResolveAgentSettings bool         `json:"resolve_agent_settings,omitempty"`
}

// TeamTurnRequest is the input of the agent service's TeamTurn activity. DevPod is the ID of
// the run's dev pod, empty when the run has none.
type TeamTurnRequest struct {
	TraceID                   string            `json:"trace_id"`
	StepID                    string            `json:"step_id"`
	Team                      string            `json:"team"`
	Member                    string            `json:"member"`
	Agent                     string            `json:"agent"`
	Skills                    map[string]string `json:"skills,omitempty"`
	DevPod                    string            `json:"dev_pod,omitempty"`
	SkillScriptTimeoutSeconds int               `json:"skill_script_timeout_seconds,omitempty"`
}

// turnOutcome is what the team loop reads from a finished turn: the reason when the member
// ended its work with report_blocked.
type turnOutcome struct {
	Blocked string `json:"blocked,omitempty"`
}

// TeamPath names the blob prefix of a team step: "teams/", the run prefix without its
// leading "runs/", and the step ID.
func TeamPath(prefix, step string) string {
	return "teams/" + strings.TrimPrefix(prefix, "runs/") + "/" + step
}

// MemberName is the name of a team member: the last path segment of an agent reference
// without its version, or the workflow name without its version.
func MemberName(ref string) string {
	name := ref
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	return name
}

// ChannelID joins the two member names in alphabetical order with "--".
func ChannelID(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "--" + b
}

// LeadChannelID is the channel in which the orchestrator posts the task and gate rejections
// to the lead.
func LeadChannelID(lead string) string {
	return lead + "--" + WorkflowSender
}

// TeamMembers returns the members of a team step: the lead agent first, then the other
// agents, then the workflows. Descriptions are filled in when the team is opened.
func (s *Step) TeamMembers() []TeamMember {
	members := []TeamMember{{Name: MemberName(s.Agent), Kind: AgentMember, Ref: s.Agent}}
	for _, agent := range s.Agents {
		members = append(members, TeamMember{Name: MemberName(agent), Kind: AgentMember, Ref: agent})
	}
	for _, name := range s.Workflows {
		members = append(members, TeamMember{Name: MemberName(name), Kind: WorkflowMember, Ref: name})
	}
	return members
}

// TeamChannels returns the channels between the members of a team step. With no channels
// configured the lead has a channel to every other member.
func (s *Step) TeamChannels() []TeamChannel {
	var pairs [][]string
	if len(s.Channels) == 0 {
		members := s.TeamMembers()
		for _, m := range members[1:] {
			pairs = append(pairs, []string{members[0].Name, m.Name})
		}
	} else {
		pairs = s.Channels
	}
	channels := make([]TeamChannel, 0, len(pairs))
	for _, pair := range pairs {
		if len(pair) < 2 {
			continue
		}
		a, b := pair[0], pair[1]
		if b < a {
			a, b = b, a
		}
		channel := TeamChannel{ID: ChannelID(a, b), Members: []string{a, b}}
		if len(pair) > 2 && pair[2] == OneWay {
			channel.From = pair[0]
		}
		channels = append(channels, channel)
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].ID < channels[j].ID })
	return channels
}

// FinalSteps returns the IDs of the steps no other step depends on, in definition order.
func FinalSteps(d *Definition) []string {
	needed := map[string]bool{}
	for i := range d.Steps {
		for _, dep := range d.Steps[i].DependsOn {
			needed[dep] = true
		}
	}
	var final []string
	for i := range d.Steps {
		if id := d.Steps[i].ID; !needed[id] {
			final = append(final, id)
		}
	}
	return final
}

// dependents returns the step and every step that depends on it, directly or through others.
func dependents(d *Definition, step string) map[string]bool {
	set := map[string]bool{step: true}
	for changed := true; changed; {
		changed = false
		for i := range d.Steps {
			s := &d.Steps[i]
			if set[s.ID] {
				continue
			}
			for _, dep := range s.DependsOn {
				if set[dep] {
					set[s.ID], changed = true, true
					break
				}
			}
		}
	}
	return set
}

// nestedRun describes a workflow that runs as a child of a workflow step or for a team member.
// Name is the workflow reference, such as build@v2. Inputs is what its steps without
// dependencies receive.
type nestedRun struct {
	TraceID    string
	Name       string
	Prefix     string
	Run        string
	WorkflowID string
	// DevPod is the dev pod the nested run works in, nil when it has none.
	DevPod               *DevPod
	Intake               *IntakeInput
	Inputs               []Item
	Depth                int
	ResolveAgentSettings bool
}

// NestedResult is the result of a nested workflow: the artifacts of its steps and the
// items of its single final step, which are the output of the nesting step. Blocked holds
// the reason when an agent of the nested run ended its work with report_blocked.
type NestedResult struct {
	Artifacts map[string]string `json:"artifacts"`
	Blocked   string            `json:"blocked,omitempty"`
	Output    []Item            `json:"output"`
}

// startNested resolves the referenced workflow version against the stored configuration and
// runs it as a child workflow under the same trace. The child's steps store their artifacts under the
// run's prefix.
func startNested(ctx, ownCtx workflow.Context, run *nestedRun) workflow.Future {
	future, setter := workflow.NewFuture(ctx)
	workflow.Go(ctx, func(ctx workflow.Context) {
		if run.Depth > MaxDepth {
			setter.Set(nil, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("workflow %q is nested deeper than %d levels", run.Name, MaxDepth), "NestingTooDeep", nil,
			))
			return
		}
		var resolved ResolveWorkflowResult
		if err := workflow.ExecuteActivity(ownCtx, ResolveWorkflowActivity, ResolveWorkflowRequest{Name: run.Name}).Get(ctx, &resolved); err != nil {
			setter.Set(nil, err)
			return
		}
		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID: run.WorkflowID,
			TaskQueue:  TaskQueue,
			// A step that runs again waits until its cancelled earlier run has closed.
			WaitForCancellation: true,
			Memo:                map[string]any{"workflow": resolved.Definition.Name, "workflow_version": DefinitionVersion(&resolved.Definition), "trace_id": run.TraceID, "artifact_prefix": run.Prefix},
		})
		var child Result
		if err := workflow.ExecuteChildWorkflow(childCtx, WorkflowName, Input{
			TraceID:              run.TraceID,
			Definition:           resolved.Definition,
			Inputs:               run.Inputs,
			Prefix:               run.Prefix,
			Depth:                run.Depth,
			Run:                  run.Run,
			DevPod:               run.DevPod,
			Intake:               run.Intake,
			ResolveAgentSettings: run.ResolveAgentSettings,
		}).Get(ctx, &child); err != nil {
			setter.Set(nil, err)
			return
		}
		if child.Blocked != "" {
			setter.Set(NestedResult{Artifacts: child.Artifacts, Blocked: child.Blocked}, nil)
			return
		}
		if len(child.Final) == 0 {
			setter.Set(nil, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("final step %q of workflow %q produced no artifact", resolved.Final, run.Name), "NoOutput", nil,
			))
			return
		}
		setter.Set(NestedResult{Artifacts: child.Artifacts, Output: child.Final}, nil)
	})
	return future
}

// Team runs the loop of one team step: it opens the team, then starts a turn for every
// member with unread messages until the lead has finished the team and no turn is running.
// Agent members take their turn in the agent service; a workflow member's turn runs the
// named workflow as a child run with the unread text as input and posts its output as reply.
func Team(ctx workflow.Context, in *TeamInput) (StepResult, error) {
	ownCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
	turnTimeout := defaultTurnTimeout
	if in.Step.Timeout != "" {
		d, err := in.Step.StartToClose()
		if err != nil {
			return StepResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidDefinition", nil)
		}
		turnTimeout = d
	}
	turnCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           AgentService,
		StartToCloseTimeout: turnTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	maxTurns := in.Step.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	members := map[string]TeamMember{}
	for _, m := range in.Step.TeamMembers() {
		members[m.Name] = m
	}
	lead := MemberName(in.Step.Agent)

	open := OpenTeamRequest{TraceID: in.TraceID, Team: in.Team, Step: in.Step, Inputs: in.Inputs, Run: in.Run, Rejection: in.Rejection}
	var opened OpenTeamResult
	if err := workflow.ExecuteActivity(ownCtx, OpenTeamActivity, open).Get(ctx, &opened); err != nil {
		return StepResult{}, err
	}

	selector := workflow.NewSelector(ctx)
	running := map[string]bool{}
	agentSettings := map[string]AgentExecutionSettings{}
	runs := map[string]int{}
	turns := 0
	nudges := 0
	var failure error
	blocked := ""
	pod := ""
	if in.DevPod != nil {
		pod = in.DevPod.ID
	}
	for {
		var state TeamStateResult
		if err := workflow.ExecuteActivity(ownCtx, TeamStateActivity, TeamStateRequest{Team: in.Team}).Get(ctx, &state); err != nil {
			return StepResult{}, err
		}
		if state.Finished {
			if len(running) == 0 {
				var out StepResult
				write := WriteTeamOutputRequest{TraceID: in.TraceID, Step: in.Step.ID, Team: in.Team, Artifact: in.Artifact}
				err := workflow.ExecuteActivity(ownCtx, WriteTeamOutputActivity, write).Get(ctx, &out)
				return out, err
			}
		} else {
			for _, name := range state.Unread {
				member, known := members[name]
				if !known || running[name] {
					continue
				}
				if turns >= maxTurns {
					return StepResult{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
						"team %q used its budget of %d turns without the lead agent %q finishing the team",
						in.Step.ID, maxTurns, lead,
					), "TeamTurnBudget", nil)
				}
				turns++
				running[name] = true
				var turn workflow.Future
				if member.Kind == WorkflowMember {
					runs[name]++
					turn = startMemberRun(ctx, ownCtx, in, member, opened.Round, runs[name])
				} else {
					settings, loaded := agentSettings[member.Ref]
					if !loaded && in.ResolveAgentSettings {
						if err := workflow.ExecuteActivity(ownCtx, ResolveAgentSettingsActivity, ResolveAgentSettingsRequest{Agent: member.Ref}).Get(ctx, &settings); err != nil {
							return StepResult{}, fmt.Errorf("team member %q failed to load agent %q: %w", name, member.Ref, err)
						}
						agentSettings[member.Ref] = settings
					}
					turn = workflow.ExecuteActivity(turnCtx, TeamTurnActivity, TeamTurnRequest{
						TraceID: in.TraceID, StepID: in.Step.ID, Team: in.Team, Member: name, Agent: member.Ref, DevPod: pod,
						Skills: settings.Skills, SkillScriptTimeoutSeconds: settings.SkillScriptTimeoutSeconds,
					})
				}
				selector.AddFuture(turn, func(f workflow.Future) {
					delete(running, name)
					var out turnOutcome
					if err := f.Get(ctx, &out); err != nil && failure == nil {
						failure = fmt.Errorf("turn of team member %q failed: %w", name, err)
					}
					if out.Blocked != "" && blocked == "" {
						blocked = out.Blocked
					}
				})
			}
			if len(running) == 0 {
				// Nobody is working and the lead has not finished: remind the lead a few times
				// before giving up, since a model sometimes ends a turn without acting.
				if nudges < maxNudges {
					nudges++
					nudge := PostTeamMessageRequest{TraceID: in.TraceID, Team: in.Team, Channel: LeadChannelID(lead), From: WorkflowSender, Text: NudgeText, Repeat: true}
					if err := workflow.ExecuteActivity(ownCtx, PostTeamMessageActivity, nudge).Get(ctx, nil); err != nil {
						return StepResult{}, err
					}
					continue
				}
				return StepResult{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
					"team %q stalled: no member has unread messages and the lead agent %q has not finished the team",
					in.Step.ID, lead,
				), "TeamStalled", nil)
			}
		}
		selector.Select(ctx)
		if failure != nil {
			return StepResult{}, failure
		}
		// A member that reported itself blocked ends the team; the other turns are abandoned.
		if blocked != "" {
			return StepResult{Blocked: blocked}, nil
		}
	}
}

// startMemberRun takes the unread messages of one channel of a workflow member, runs the
// member's workflow with their text as input, and posts the output as the member's reply.
func startMemberRun(ctx, ownCtx workflow.Context, in *TeamInput, member TeamMember, round, n int) workflow.Future {
	future, setter := workflow.NewFuture(ctx)
	workflow.Go(ctx, func(ctx workflow.Context) {
		// The team step's own artifact is the object <prefix>/<step>, so the runs of its
		// workflow members live beside it under <prefix>/<step>.<member>.r<round>.<n>; the round
		// keeps the runs of a later round from replacing those of an earlier one.
		prefix := fmt.Sprintf("%s.%s.r%d.%d", in.Artifact, member.Name, round, n)
		var task TeamTaskResult
		take := TeamTaskRequest{TraceID: in.TraceID, Team: in.Team, Member: member.Name, Artifact: ArtifactID(prefix, InputSource)}
		if err := workflow.ExecuteActivity(ownCtx, TeamTaskActivity, take).Get(ctx, &task); err != nil {
			setter.Set(nil, err)
			return
		}
		if task.Channel == "" {
			setter.Set(turnOutcome{}, nil)
			return
		}
		var nested NestedResult
		if err := startNested(ctx, ownCtx, &nestedRun{
			TraceID: in.TraceID, Name: member.Ref, Prefix: prefix, Depth: in.Depth + 1,
			Inputs:               []Item{{Source: InputSource, Artifact: task.Artifact, Round: 1}},
			WorkflowID:           fmt.Sprintf("%s/%s.r%d.%d", workflow.GetInfo(ctx).WorkflowExecution.ID, member.Name, round, n),
			Run:                  fmt.Sprintf("%s/r%d.%d", in.Run, round, n),
			DevPod:               in.DevPod,
			Intake:               in.Intake,
			ResolveAgentSettings: in.ResolveAgentSettings,
		}).Get(ctx, &nested); err != nil {
			setter.Set(nil, err)
			return
		}
		if nested.Blocked != "" {
			setter.Set(turnOutcome{Blocked: nested.Blocked}, nil)
			return
		}
		reply := PostTeamMessageRequest{
			TraceID: in.TraceID, Team: in.Team, Channel: task.Channel, From: member.Name,
			Member: member.Name, Ref: member.Ref, Read: task.Seq,
		}
		for _, item := range nested.Output {
			reply.Artifacts = append(reply.Artifacts, item.Artifact)
		}
		setter.Set(turnOutcome{}, workflow.ExecuteActivity(ownCtx, PostTeamMessageActivity, reply).Get(ctx, nil))
	})
	return future
}

// teamCreator is recorded as the creator of the team blobs the orchestrator writes.
const teamCreator = "orchestrator"

// ResolveWorkflowRequest names a configured workflow by reference, such as build@v2. A bare
// name means the newest version.
type ResolveWorkflowRequest struct {
	Name string `json:"name"`
}

// ResolveWorkflowResult is the configured workflow and its single final step.
type ResolveWorkflowResult struct {
	Final      string     `json:"final"`
	Definition Definition `json:"definition"`
}

// ResolveWorkflow returns the referenced workflow version for a workflow step or a workflow
// team member. The workflow needs exactly one final step, whose artifact is the output.
func (a *Activities) ResolveWorkflow(_ context.Context, req ResolveWorkflowRequest) (ResolveWorkflowResult, error) {
	definitions, err := a.LoadDefinitions()
	if err != nil {
		return ResolveWorkflowResult{}, err
	}
	d, ok := FindDefinition(definitions, req.Name)
	if !ok {
		return ResolveWorkflowResult{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("workflow %q is not a configured workflow", req.Name), "UnknownWorkflow", nil,
		)
	}
	final := FinalSteps(&d)
	if len(final) != 1 {
		return ResolveWorkflowResult{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
			"nested workflow %q needs exactly one final step, but has %d (%s)", d.Name, len(final), strings.Join(final, ", "),
		),
			"InvalidNestedWorkflow", nil)
	}
	return ResolveWorkflowResult{Definition: d, Final: final[0]}, nil
}

// OpenTeamRequest names the team of a team step, the artifacts it starts with, and, when
// the team runs again, the gate rejection it has to incorporate.
type OpenTeamRequest struct {
	TraceID   string         `json:"trace_id"`
	Team      string         `json:"team"`
	Step      Step           `json:"step"`
	Run       string         `json:"run,omitempty"`
	Rejection *TeamRejection `json:"rejection,omitempty"`
	Inputs    []Item         `json:"inputs"`
}

// OpenTeamResult tells which run of the team this is: 1 for the first, growing with every
// time the team step runs again.
type OpenTeamResult struct {
	Round int `json:"round"`
}

// OpenTeam writes the input files, the task message to the lead in the workflow's queue, and
// last team.json, whose presence marks the team as opened. When the team exists and the
// request carries another run marker than team.json, the team step runs again: OpenTeam
// refreshes the input files, posts the gate rejection or the changed inputs to the lead,
// and removes finished.json, so the team works again. With the same marker it is a retry
// and changes nothing.
func (a *Activities) OpenTeam(ctx context.Context, req *OpenTeamRequest) (OpenTeamResult, error) {
	meta := Meta{Creator: teamCreator, TraceID: req.TraceID}
	lead := MemberName(req.Step.Agent)
	leadChannel := TeamChannel{ID: LeadChannelID(lead), Members: []string{lead, WorkflowSender}}
	var manifest TeamManifest
	exists, err := a.getJSON(ctx, req.Team+"/team.json", &manifest)
	if err != nil {
		return OpenTeamResult{}, err
	}
	if exists && manifest.Run == req.Run {
		return OpenTeamResult{Round: max(manifest.Round, 1)}, nil
	}

	var inputs strings.Builder
	for i, item := range req.Inputs {
		data, readErr := a.Store.Get(ctx, item.Artifact)
		if readErr != nil {
			return OpenTeamResult{}, fmt.Errorf("reading artifact %q: %w", item.Artifact, readErr)
		}
		name := inputFile(req.Inputs, i, data)
		id := req.Team + "/files/" + name
		if err = a.Store.Put(ctx, id, data, meta); err != nil {
			return OpenTeamResult{}, fmt.Errorf("writing team file %q: %w", id, err)
		}
		fmt.Fprintf(&inputs, "\n--- workspace file %s ---\n%s\n", name, strings.TrimSpace(string(data)))
	}

	if exists {
		round := max(manifest.Round, 1) + 1
		var text string
		if req.Rejection != nil {
			// Markdown with blank lines between the parts, so the chat view keeps them apart.
			text = fmt.Sprintf("Review round %d: the review gate %q rejected the team's result.\n\n**Rejected by:** %s\n\n**Reason:**\n\n%s\n\n"+
				"Incorporate this with your team, then finish the team again.",
				req.Rejection.Round, titleCase(req.Rejection.Gate), req.Rejection.DecidedBy, strings.TrimSpace(req.Rejection.Reason))
		} else {
			text = fmt.Sprintf("Run %d: the inputs of your team changed because earlier work was revised after a review. "+
				"The current inputs follow and replace the workspace input files. Revise the result with your team, "+
				"then finish the team again.\n%s", round, inputs.String())
		}
		// The text names the round, so a retry is recognised as the queue's last message.
		err = a.postMessage(ctx, req.Team, leadChannel, WorkflowSender, text, meta, false, "")
		if err != nil {
			return OpenTeamResult{}, err
		}
		remover, ok := a.Store.(interface {
			Delete(ctx context.Context, id string) error
		})
		if !ok {
			return OpenTeamResult{}, temporal.NewNonRetryableApplicationError("the artifact store cannot delete objects", "StoreCannotDelete", nil)
		}
		if err = remover.Delete(ctx, req.Team+"/finished.json"); err != nil && !errors.Is(err, ErrNotFound) {
			return OpenTeamResult{}, fmt.Errorf("removing %s/finished.json: %w", req.Team, err)
		}
		manifest.Run, manifest.Round = req.Run, round
		return OpenTeamResult{Round: round}, a.putJSON(ctx, req.Team+"/team.json", manifest, meta)
	}

	task := "This is the task for your team. Work on it with your team and finish the team when the result is ready.\n" + inputs.String()
	var definitions []Definition
	if a.LoadDefinitions != nil {
		if definitions, err = a.LoadDefinitions(); err != nil {
			return OpenTeamResult{}, err
		}
	}
	members := req.Step.TeamMembers()
	for i, m := range members {
		switch {
		case m.Kind == WorkflowMember:
			d, _ := FindDefinition(definitions, m.Ref)
			members[i].Description = d.Description
		case a.DescribeAgent != nil:
			members[i].Description = a.DescribeAgent(ctx, m.Ref)
		}
	}
	channels := append(req.Step.TeamChannels(), leadChannel)
	// A retry finds the task as the queue's last message and does not post it again.
	err = a.postMessage(ctx, req.Team, leadChannel, WorkflowSender, task, meta, false, "")
	if err != nil {
		return OpenTeamResult{}, err
	}
	manifest = TeamManifest{Lead: lead, Members: members, Channels: channels, TraceID: req.TraceID, StepID: req.Step.ID, Run: req.Run, Round: 1}
	return OpenTeamResult{Round: 1}, a.putJSON(ctx, req.Team+"/team.json", manifest, meta)
}

// TeamStateRequest names a team.
type TeamStateRequest struct {
	Team string `json:"team"`
}

// TeamStateResult lists the members with unread messages and tells whether the lead has
// finished the team.
type TeamStateResult struct {
	Unread   []string `json:"unread"`
	Finished bool     `json:"finished"`
}

// TeamState reads the team's queues and read markers. A member has unread messages when
// the other side's queue of one of its channels holds a message with a seq above the member's
// read marker in members/<name>.json, which is 0 while that file is missing.
func (a *Activities) TeamState(ctx context.Context, req TeamStateRequest) (TeamStateResult, error) {
	res := TeamStateResult{Unread: []string{}}
	var manifest TeamManifest
	if _, err := a.getJSON(ctx, req.Team+"/team.json", &manifest); err != nil {
		return res, err
	}
	found, err := a.getJSON(ctx, req.Team+"/finished.json", &TeamFinished{})
	if err != nil {
		return res, err
	}
	res.Finished = found
	for _, m := range manifest.Members {
		var file teamMemberFile
		if _, err = a.getJSON(ctx, req.Team+"/members/"+m.Name+".json", &file); err != nil {
			return res, err
		}
		for _, c := range manifest.Channels {
			if !contains(c.Members, m.Name) {
				continue
			}
			var queue TeamQueue
			if _, err = a.getJSON(ctx, queueID(req.Team, c.ID, c.other(m.Name)), &queue); err != nil {
				return res, err
			}
			if len(unread(queue, file.Read[c.ID])) > 0 {
				res.Unread = append(res.Unread, m.Name)
				break
			}
		}
	}
	sort.Strings(res.Unread)
	return res, nil
}

// TeamTaskRequest asks for the next task of a workflow member; its text is stored as Artifact.
type TeamTaskRequest struct {
	TraceID  string `json:"trace_id"`
	Team     string `json:"team"`
	Member   string `json:"member"`
	Artifact string `json:"artifact"`
}

// TeamTaskResult names the channel the task came from and the seq of its last message.
// Channel is empty when the member has no unread messages.
type TeamTaskResult struct {
	Channel  string `json:"channel"`
	Artifact string `json:"artifact"`
	Seq      int    `json:"seq"`
}

// TeamTask stores the unread messages of the first channel of a workflow member that has
// any as the input artifact of the member's next run.
func (a *Activities) TeamTask(ctx context.Context, req TeamTaskRequest) (TeamTaskResult, error) {
	var manifest TeamManifest
	if _, err := a.getJSON(ctx, req.Team+"/team.json", &manifest); err != nil {
		return TeamTaskResult{}, err
	}
	var file teamMemberFile
	if _, err := a.getJSON(ctx, req.Team+"/members/"+req.Member+".json", &file); err != nil {
		return TeamTaskResult{}, err
	}
	for _, c := range manifest.Channels {
		if !contains(c.Members, req.Member) {
			continue
		}
		var queue TeamQueue
		if _, err := a.getJSON(ctx, queueID(req.Team, c.ID, c.other(req.Member)), &queue); err != nil {
			return TeamTaskResult{}, err
		}
		messages := unread(queue, file.Read[c.ID])
		if len(messages) == 0 {
			continue
		}
		texts := make([]string, 0, len(messages))
		for _, m := range messages {
			texts = append(texts, m.Text)
		}
		data := []byte(strings.Join(texts, "\n\n"))
		if err := a.Store.Put(ctx, req.Artifact, data, Meta{Creator: teamCreator, TraceID: req.TraceID}); err != nil {
			return TeamTaskResult{}, fmt.Errorf("writing artifact %q: %w", req.Artifact, err)
		}
		return TeamTaskResult{Channel: c.ID, Seq: messages[len(messages)-1].Seq, Artifact: req.Artifact}, nil
	}
	return TeamTaskResult{}, nil
}

// PostTeamMessageRequest posts Text, or the content of Artifacts with a blank line between
// them, into a channel. With Member set it then advances that workflow member's read marker
// of the channel to Read.
type PostTeamMessageRequest struct {
	TraceID   string   `json:"trace_id"`
	Team      string   `json:"team"`
	Channel   string   `json:"channel"`
	From      string   `json:"from"`
	Text      string   `json:"text,omitempty"`
	Member    string   `json:"member,omitempty"`
	Ref       string   `json:"ref,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	Read      int      `json:"read,omitempty"`
	// Repeat posts the message even when it equals the channel's last one.
	Repeat bool `json:"repeat,omitempty"`
}

// PostTeamMessage appends a message to the queue of its sender in a channel of a team. The
// orchestrator writes only the queues of "workflow" and of workflow members; the queue of an
// agent member belongs to the tool-gateway.
func (a *Activities) PostTeamMessage(ctx context.Context, req *PostTeamMessageRequest) error {
	meta := Meta{Creator: teamCreator, TraceID: req.TraceID}
	text := req.Text
	if len(req.Artifacts) > 0 {
		parts := make([]string, 0, len(req.Artifacts))
		for _, id := range req.Artifacts {
			data, err := a.Store.Get(ctx, id)
			if err != nil {
				return fmt.Errorf("reading artifact %q: %w", id, err)
			}
			parts = append(parts, string(data))
		}
		text = strings.Join(parts, "\n\n")
	}
	var manifest TeamManifest
	if _, err := a.getJSON(ctx, req.Team+"/team.json", &manifest); err != nil {
		return err
	}
	var channel TeamChannel
	for _, c := range manifest.Channels {
		if c.ID == req.Channel && contains(c.Members, req.From) {
			channel = c
		}
	}
	owned := req.From == WorkflowSender
	for _, m := range manifest.Members {
		owned = owned || (m.Name == req.From && m.Kind == WorkflowMember)
	}
	if channel.ID == "" || !owned {
		return temporal.NewNonRetryableApplicationError(fmt.Sprintf(
			"the orchestrator does not write the queue of %q in channel %q of team %q", req.From, req.Channel, req.Team,
		), "InvalidTeamMessage", nil)
	}
	// A reply equal to an earlier one is new when it answers a later message; only a retry,
	// whose reply was sent after the message it answers, is not posted again.
	after := ""
	if req.Member != "" {
		var task TeamQueue
		if _, err := a.getJSON(ctx, queueID(req.Team, req.Channel, channel.other(req.From)), &task); err != nil {
			return err
		}
		for _, m := range task.Messages {
			if m.Seq == req.Read {
				after = m.Sent
			}
		}
	}
	// On a one-way channel towards a workflow member, the member's output is not sent back.
	if channel.sends(req.From) {
		if err := a.postMessage(ctx, req.Team, channel, req.From, text, meta, req.Repeat, after); err != nil {
			return err
		}
	}
	if req.Member == "" {
		return nil
	}
	id := req.Team + "/members/" + req.Member + ".json"
	file := teamMemberFile{}
	if _, err := a.getJSON(ctx, id, &file); err != nil {
		return err
	}
	file.Member, file.Agent = req.Member, req.Ref
	if file.Read == nil {
		file.Read = map[string]int{}
	}
	if file.Messages == nil {
		file.Messages = []json.RawMessage{}
	}
	if file.Read[req.Channel] < req.Read {
		file.Read[req.Channel] = req.Read
	}
	return a.putJSON(ctx, id, file, meta)
}

// WriteTeamOutputRequest names the finished team and the artifact of its step.
type WriteTeamOutputRequest struct {
	TraceID  string `json:"trace_id"`
	Step     string `json:"step"`
	Team     string `json:"team"`
	Artifact string `json:"artifact"`
}

// WriteTeamOutput stores the team step's output artifact: the lead's summary and the
// workspace files it named as the result.
func (a *Activities) WriteTeamOutput(ctx context.Context, req WriteTeamOutputRequest) (StepResult, error) {
	var finished TeamFinished
	found, err := a.getJSON(ctx, req.Team+"/finished.json", &finished)
	if err != nil {
		return StepResult{}, err
	}
	if !found {
		return StepResult{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("team %q has no finished.json", req.Team), "TeamNotFinished", nil,
		)
	}
	out := TeamOutput{Summary: finished.Summary, Team: req.Team, Files: []TeamOutputFile{}}
	for _, name := range finished.Files {
		out.Files = append(out.Files, TeamOutputFile{Name: name, Artifact: req.Team + "/files/" + name})
	}
	err = a.putJSON(ctx, req.Artifact, out, Meta{Creator: teamCreator, TraceID: req.TraceID})
	if err != nil {
		return StepResult{}, err
	}
	return StepResult{Artifact: req.Artifact}, nil
}

// postMessage appends a message to the sender's queue of a channel, creating the queue when
// it is missing. A message equal to the queue's last one, sent after the time in after, is
// not posted again, so a retried activity does not repeat it.
func (a *Activities) postMessage(ctx context.Context, team string, channel TeamChannel, from, text string, meta Meta, repeat bool, after string) error {
	id := queueID(team, channel.ID, from)
	var queue TeamQueue
	if _, err := a.getJSON(ctx, id, &queue); err != nil {
		return err
	}
	queue.Channel, queue.From, queue.To = channel.ID, from, channel.other(from)
	seq := 0
	if n := len(queue.Messages); n > 0 {
		last := queue.Messages[n-1]
		if !repeat && last.Text == text && last.Sent > after {
			return nil
		}
		seq = last.Seq
	}
	queue.Messages = append(queue.Messages, TeamMessage{Seq: seq + 1, Text: text, Sent: time.Now().UTC().Format(sentLayout)})
	return a.putJSON(ctx, id, queue, meta)
}

// getJSON reads a JSON blob into v and reports whether it exists.
func (a *Activities) getJSON(ctx context.Context, id string, v any) (bool, error) {
	data, err := a.Store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %q: %w", id, err)
	}
	if err = json.Unmarshal(data, v); err != nil {
		return true, temporal.NewNonRetryableApplicationError(fmt.Sprintf("%q is not valid JSON", id), "InvalidTeamBlob", err)
	}
	return true, nil
}

func (a *Activities) putJSON(ctx context.Context, id string, v any, meta Meta) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err = a.Store.Put(ctx, id, data, meta); err != nil {
		return fmt.Errorf("writing %q: %w", id, err)
	}
	return nil
}

// unread returns the messages of the other side's queue after the member's read marker.
func unread(queue TeamQueue, marker int) []TeamMessage {
	var out []TeamMessage
	for _, m := range queue.Messages {
		if m.Seq > marker {
			out = append(out, m)
		}
	}
	return out
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// titleCase shows a step ID as a title: dashes become spaces and every word starts with a
// capital ("api-review" becomes "Api Review", "API-review" becomes "API Review").
func titleCase(id string) string {
	words := strings.Split(id, "-")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}
