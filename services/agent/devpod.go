package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// devPodService is the MCP service whose tools work in the run's dev pod.
const devPodService = "devpod"

// blockedTool is the dev pod tool an agent calls to end its work as blocked.
const blockedTool = "report_blocked"

// noBlockedReason is the reason of a report_blocked call that gave none.
const noBlockedReason = "The agent reported that it is blocked and gave no reason."

// devPodIdentity names the arguments of every dev pod tool that the harness sets. The model
// neither sees nor chooses them.
var devPodIdentity = []string{"pod", "agent", "run"}

// devPodCalls is the harness's control over the dev pod tool calls of one turn.
type devPodCalls struct {
	pod, agent, run string
	reason          string
	blocked         bool
}

type devPodKey struct{}

// withDevPod notes the dev pod of the run (empty when it has none), the agent reference with
// its version and the run ID for the dev pod tool calls of a turn.
func withDevPod(ctx context.Context, pod, agent, run string) context.Context {
	return context.WithValue(ctx, devPodKey{}, &devPodCalls{pod: strings.TrimSpace(pod), agent: agent, run: run})
}

func devPodOf(ctx context.Context) *devPodCalls {
	d, _ := ctx.Value(devPodKey{}).(*devPodCalls)
	return d
}

// blockedReason returns the reason of a report_blocked call that succeeded in this turn.
func blockedReason(ctx context.Context) (string, bool) {
	if d := devPodOf(ctx); d != nil && d.blocked {
		return d.reason, true
	}
	return "", false
}

// trimDevPodSchema removes the arguments the harness sets from the parameter schema the model
// is shown. A schema it cannot read is returned unchanged.
func trimDevPodSchema(raw json.RawMessage) json.RawMessage {
	return hideToolArguments(raw, devPodIdentity)
}

func hideToolArguments(raw json.RawMessage, names []string) json.RawMessage {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return raw
	}
	hidden := map[string]bool{}
	for _, name := range names {
		hidden[name] = true
	}
	var props map[string]json.RawMessage
	if json.Unmarshal(schema["properties"], &props) == nil && props != nil {
		for name := range hidden {
			delete(props, name)
		}
		if data, err := json.Marshal(props); err == nil {
			schema["properties"] = data
		}
	}
	var required []string
	if _, ok := schema["required"]; ok && json.Unmarshal(schema["required"], &required) == nil {
		kept := []string{}
		for _, name := range required {
			if !hidden[name] {
				kept = append(kept, name)
			}
		}
		if len(kept) == 0 {
			delete(schema, "required")
		} else if data, err := json.Marshal(kept); err == nil {
			schema["required"] = data
		}
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return raw
	}
	return data
}

// devPodNote tells the agent how to work in the dev pod. It is short and explicit because
// small local models run the agents.
func devPodNote(commands []string) string {
	var b strings.Builder
	b.WriteString("\n\n## Dev pod\n")
	b.WriteString("You work in the project folder of a Git repository, on its own work branch.\n")
	b.WriteString("- Use the tools to list, read, search, write and edit files and to run commands. Tool names may carry a prefix such as `devpod__`.\n")
	b.WriteString("- All paths are relative to the project folder.\n")
	var allowed []string
	canAdd, canCommit := false, false
	for _, c := range commands {
		if c = strings.TrimSpace(c); c != "" {
			allowed = append(allowed, "`"+c+"`")
			canAdd = canAdd || c == "git add *" || c == "*"
			canCommit = canCommit || c == "git commit *" || c == "*"
		}
	}
	if len(allowed) == 0 {
		b.WriteString("- You are not allowed to run any command.\n")
	} else {
		fmt.Fprintf(&b, "- You can only run these commands (`*` stands for any text): %s. Every other command is refused.\n", strings.Join(allowed, ", "))
	}
	if canAdd && canCommit {
		b.WriteString("- Commit your work with the command tool: first `git add <files>`, then `git commit -m \"<what you changed>\"`.\n")
	}
	b.WriteString("- Never try to push or to switch branches.\n")
	b.WriteString("- If you cannot go on, call the tool report_blocked with the reason.")
	return b.String()
}
