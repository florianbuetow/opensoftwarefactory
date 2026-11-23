package pipeline

import (
	"strings"
	"testing"
)

func teamWithWorkflowMembers(channels [][]string) *Definition {
	return &Definition{Name: "team-channels", Steps: []Step{{
		ID: "coordinate", Service: TeamService, Agent: "lead@v1",
		Agents:    []string{"helper@v1"},
		Workflows: []string{"review-flow@v2", "verify-flow@v3"},
		Channels:  channels,
	}}}
}

func TestValidateTeamRejectsChannelsBetweenWorkflowMembers(t *testing.T) {
	for name, channel := range map[string][]string{
		"two way":           {"review-flow", "verify-flow"},
		"one way forwards":  {"review-flow", "verify-flow", OneWay},
		"one way backwards": {"verify-flow", "review-flow", OneWay},
	} {
		t.Run(name, func(t *testing.T) {
			err := Validate(teamWithWorkflowMembers([][]string{channel}))
			if err == nil || !strings.Contains(err.Error(), `a channel cannot connect workflow members "review-flow" and "verify-flow"`) &&
				!strings.Contains(err.Error(), `a channel cannot connect workflow members "verify-flow" and "review-flow"`) {
				t.Fatalf("error %v", err)
			}
		})
	}
}

func TestValidateTeamAllowsChannelsWithAnAgentEndpoint(t *testing.T) {
	for name, channels := range map[string][][]string{
		"lead to workflows":         {{"lead", "review-flow"}, {"lead", "verify-flow"}},
		"agent to workflow":         {{"helper", "review-flow"}},
		"workflow to agent one way": {{"review-flow", "helper", OneWay}},
		"default lead channels":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(teamWithWorkflowMembers(channels)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
