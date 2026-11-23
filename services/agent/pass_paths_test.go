package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A step after a merge or batch runs once per pass. Its artifact, session and team paths
// end in "<step>.pass-<n>".

func TestProcessWorksWithAPassArtifact(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, respond(finalResponse))
	req := request()
	req.OutputArtifact = "runs/t1/plan.pass-2"
	res, err := f.svc.Process(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(res)
	if string(data) != `{"artifact":"runs/t1/plan.pass-2"}` {
		t.Fatalf("result = %s", data)
	}
	for _, id := range []string{
		"runs/t1/plan.pass-2", "runs/t1/plan.pass-2.run.json", "runs/t1/plan.pass-2.attempt-1.run.json",
		"runs/t1/_evidence/plan.pass-2/attempt-1/systemprompt.effective.md",
	} {
		if _, ok := f.blobs[id]; !ok {
			t.Errorf("object %q was not written", id)
		}
	}
}

func TestChatWorksWithAPassSession(t *testing.T) {
	f := newFakes(t, chatAgentJSON, respond(finalResponse))
	body := chatBody()
	body["session"] = "user-sessions/t1/talk.pass-2"
	code, out := postChat(t, f, body)
	if code != http.StatusOK || out["chat"] != "user-sessions/t1/talk.pass-2/chats/main.json" {
		t.Fatalf("chat = %d %v", code, out)
	}
	var log chatLog
	if err := json.Unmarshal(f.blobs["user-sessions/t1/talk.pass-2/chats/main.json"], &log); err != nil {
		t.Fatal(err)
	}
	var system map[string]string
	_ = json.Unmarshal(log.Messages[0], &system)
	if log.Session != "user-sessions/t1/talk.pass-2" || log.StepID != "talk" || log.TraceID != "t1" ||
		!strings.Contains(system["content"], "`user-sessions/t1/talk.pass-2/files/`") {
		t.Fatalf("log = %+v, system = %q", log, system["content"])
	}
}

func TestTeamTurnWorksWithAPassTeam(t *testing.T) {
	f := teamFakes(t, chatAgentJSON, respond(finalResponse))
	const passTeam = "teams/t1/build.pass-2"
	// The same team, stored under the path of the second pass.
	for id, data := range f.blobs {
		if strings.HasPrefix(id, teamPath+"/") {
			f.blobs[passTeam+strings.TrimPrefix(id, teamPath)] = data
			delete(f.blobs, id)
		}
	}
	req := leadTurn()
	req.Team = passTeam
	res, err := f.svc.TeamTurn(context.Background(), req)
	if err != nil || res.ModelCalls != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	log, messages := storedMember(t, f, passTeam+"/members/lead.json")
	if log.Member != "lead" || len(messages) < 3 || !strings.Contains(messages[0]["content"].(string), "in the team `"+passTeam+"`") {
		t.Fatalf("log = %+v, messages = %v", log, messages)
	}
}
