package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The export of an evaluation set is one zip file:
//
//	README.md                 what the files are
//	set.json                  the evaluation set as stored
//	runs/<nn>-<workflow>-<trace>/
//	    manifest.json         every file of the run with its role, in the order written
//	    input.*               the input the run was started with
//	    run.json              who started the run, when, with which workflow
//	    steps/<step>/...      per step: its output, the agent's records, prompt evidence, gate rounds
//	    steps/<step>/pass-<n>/... the same per pass, for a step after a merge or batch node
//	    sessions/<step>/...   interactive sessions: chats, workspace files, completion
//	    teams/<step>/...      teams: members, channels, member histories, workspace files
//
// Objects in the store have no file extension; the export adds .json or .md by content.

const exportReadme = `# Evaluation set export

This archive holds one evaluation set and everything stored for each of its runs.

- ` + "`set.json`" + `: the set as stored: its name, description and a snapshot of every run.
- ` + "`runs/<nn>-<workflow>-<trace>/`" + `: one folder per run, in the order of the set.

## Inside a run folder

Start with ` + "`manifest.json`" + `. It names the input, the final output, and lists every file
with its role, the step it belongs to, who wrote it and when.

| Path | What it is |
| --- | --- |
| ` + "`input.*`" + ` | The input the run was started with. |
| ` + "`run.json`" + ` | Run record: workflow, who started it, when. |
| ` + "`metadata.json`" + ` | What produced the run: workflow name and version, nested workflow versions, every agent with its model and settings, start, end, and how the run ended technically. |
| ` + "`steps/<step>/output.*`" + ` | What the step handed to the steps after it. For an agent step this is the agent's answer; for a gate, the decision record; for a team, its summary and result files; for an interactive session, the file the person chose. |
| ` + "`steps/<step>/agent-record.json`" + ` | Record of the agent's last attempt: every message to and from the model, tool calls, token counts and timing. Not an output. |
| ` + "`steps/<step>/attempt-<n>.agent-record.json`" + ` | The same record for attempt n. With one attempt it equals agent-record.json. |
| ` + "`steps/<step>/evidence/attempt-<n>/`" + ` | The system prompt of that attempt: as stored (source) and as sent after references were resolved (effective). |
| ` + "`steps/<step>/decision-round-<n>.json`" + ` | An earlier decision of a gate, such as a rejection that went back to a team. |
| ` + "`steps/<step>/pass-<n>/`" + ` | A step after a merge or batch node runs once for every message the node forwards. Each of these runs is a pass with its own folder, holding ` + "`output.*`" + `, ` + "`agent-record.json`" + ` and ` + "`attempt-<k>.agent-record.json`" + ` as described above. |
| ` + "`sessions/<step>/chats/`" + ` | The chat between the person and each agent of an interactive step. |
| ` + "`sessions/<step>/files/`" + ` | The session's workspace files; ` + "`input-*`" + ` are the step's inputs. |
| ` + "`sessions/<step>/completed.json`" + ` | Which file the person chose as the step's output, and who completed it. |
| ` + "`teams/<step>/team.json`" + ` | The team's members and channels. |
| ` + "`teams/<step>/channels/<channel>/<sender>.json`" + ` | The messages one member sent on one channel. |
| ` + "`teams/<step>/members/<member>.json`" + ` | A member's own history with the model and what it has read. |
| ` + "`teams/<step>/files/`" + ` | The team's workspace files. |
| ` + "`teams/<step>/finished.json`" + ` | The lead's final summary and the result files it named. |
| ` + "`other/`" + ` | Anything else stored under the run. |

A run whose source was removed has only ` + "`manifest.json`" + ` with the saved snapshot.
`

type exportFile struct {
	Path        string `json:"path"`
	Role        string `json:"role"`
	Description string `json:"description"`
	Step        string `json:"step,omitempty"`
	Object      string `json:"object"`
	Creator     string `json:"creator,omitempty"`
	Created     string `json:"created,omitempty"`
	Size        int    `json:"size"`
}

type exportManifest struct {
	*RunSnapshot
	Input       string       `json:"input,omitempty"`
	FinalOutput string       `json:"final_output,omitempty"`
	Note        string       `json:"note,omitempty"`
	Files       []exportFile `json:"files"`
}

// The roles of an exported file that more than one rule assigns.
const (
	roleInput        = "input"
	roleStepOutput   = "step output"
	roleAgentRecord  = "agent record"
	roleGateDecision = "gate decision"
	roleTeamResult   = "team result"
)

// inputObject is the name of a run's input, in the store and in the export.
const inputObject = "input"

var (
	attemptRecord = regexp.MustCompile(`^(.+)\.attempt-(\d+)\.run\.json$`)
	gateRound     = regexp.MustCompile(`^(.+)\.(\d+)$`)
	stepPass      = regexp.MustCompile(`^(.+)\.pass-(\d+)$`)
	evidencePath  = regexp.MustCompile(`^_evidence/(.+)/(attempt-\d+)/([^/]+)$`)
	sessionPart   = regexp.MustCompile(`^(.+?)/(chats/.+|files/.+|completed\.json)$`)
	teamPart      = regexp.MustCompile(`^(.+?)/(team\.json|finished\.json|channels/.+|members/.+|files/.+)$`)
	unsafeName    = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// classify maps an object of a run to its path in the run's folder, its role, a sentence
// about it, and the step it belongs to. The path has no extension yet where the object has none.
func classify(trace, id string) (file, role, description, step string) {
	if rest, ok := strings.CutPrefix(id, "runs/"+trace+"/"); ok {
		switch {
		case rest == inputObject:
			return inputObject, roleInput, "The input the run was started with.", ""
		case rest == "run.json":
			return "run.json", "run record", "Workflow, who started the run, and when.", ""
		case rest == "metadata.json":
			return "metadata.json", "run metadata", "What produced the run: workflow name and version, nested workflow versions, every agent with its model and settings, start, end, and how the run ended technically.", ""
		}
		if m := evidencePath.FindStringSubmatch(rest); m != nil {
			dir, step, _ := stepFolder(m[1])
			return dir + "/evidence/" + m[2] + "/" + m[3], "prompt evidence",
				"The system prompt of this attempt: source is as stored, effective is as sent to the model.", step
		}
		if m := attemptRecord.FindStringSubmatch(rest); m != nil {
			dir, step, pass := stepFolder(m[1])
			return dir + "/attempt-" + m[2] + ".agent-record.json", roleAgentRecord,
				"Record of attempt " + m[2] + pass + ": every message, tool call, token count and timing. Not an output.", step
		}
		if s, ok := strings.CutSuffix(rest, ".run.json"); ok {
			dir, step, pass := stepFolder(s)
			return dir + "/agent-record.json", roleAgentRecord,
				"Record of the agent's last attempt" + pass + ": every message, tool call, token count and timing. Not an output.", step
		}
		if m := gateRound.FindStringSubmatch(rest); m != nil {
			dir, step, pass := stepFolder(m[1])
			return dir + "/decision-round-" + m[2], roleGateDecision,
				"The gate's decision in round " + m[2] + pass + ", before its last one.", step
		}
		dir, step, pass := stepFolder(rest)
		return dir + "/output", roleStepOutput, "What the step handed to the steps after it" + pass + ".", step
	}
	if rest, ok := strings.CutPrefix(id, "user-sessions/"+trace+"/"); ok {
		step, part := rest, ""
		if m := sessionPart.FindStringSubmatch(rest); m != nil {
			step, part = m[1], m[2]
		}
		switch {
		case strings.HasPrefix(part, "chats/"):
			return "sessions/" + rest, "session chat", "The chat between the person and this agent.", step
		case strings.HasPrefix(part, "files/input-"):
			return "sessions/" + rest, "session input", "An input of the interactive step, placed in the session's files.", step
		case strings.HasPrefix(part, "files/"):
			return "sessions/" + rest, "session file", "A file written in the session's workspace.", step
		case part == "completed.json":
			return "sessions/" + rest, "session completion", "Which file the person chose as the step's output, and who completed it.", step
		}
		return "sessions/" + rest, "session data", "Stored for the interactive session.", step
	}
	if rest, ok := strings.CutPrefix(id, "teams/"+trace+"/"); ok {
		step, part := rest, ""
		if m := teamPart.FindStringSubmatch(rest); m != nil {
			step, part = m[1], m[2]
		}
		switch {
		case part == "team.json":
			return "teams/" + rest, "team manifest", "The team's members and channels.", step
		case part == "finished.json":
			return "teams/" + rest, roleTeamResult, "The lead's final summary and the result files it named.", step
		case strings.HasPrefix(part, "channels/"):
			return "teams/" + rest, "channel queue", "The messages one member sent on one channel.", step
		case strings.HasPrefix(part, "members/"):
			return "teams/" + rest, "member history", "A member's own history with the model and what it has read.", step
		case strings.HasPrefix(part, "files/input-"):
			return "teams/" + rest, "team input", "An input of the team step, placed in the team's files.", step
		case strings.HasPrefix(part, "files/"):
			return "teams/" + rest, "team file", "A file in the team's workspace.", step
		}
		return "teams/" + rest, "team data", "Stored for the team.", step
	}
	return "other/" + id, "other", "Stored under this run.", ""
}

// stepFolder returns the export folder of a step, the step without its pass, and a phrase
// that names the pass. "compare.pass-2" is pass 2 of the step compare, a step after a merge
// or batch node: its files go to steps/compare/pass-2.
func stepFolder(step string) (dir, plain, pass string) {
	if m := stepPass.FindStringSubmatch(step); m != nil {
		return "steps/" + m[1] + "/pass-" + m[2], m[1], " in pass " + m[2]
	}
	return "steps/" + step, step, ""
}

// refine names what a step output is when its content tells.
func refine(role, description string, data []byte) (refinedRole, refinedDescription string) {
	if role != roleStepOutput {
		return role, description
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(data, &doc) != nil {
		return role, description
	}
	if _, ok := doc["decision"]; ok {
		return roleGateDecision, "The step's output: the gate's last decision, with the question, the inputs it was shown, and who decided."
	}
	_, team := doc["team"]
	_, files := doc["files"]
	if team && files {
		return roleTeamResult, "The step's output: the team's summary and the result files it named."
	}
	return role, description
}

// withExtension adds .json or .md to a name without an extension, by content.
func withExtension(name string, data []byte) string {
	if ext := path.Ext(name); ext != "" && len(ext) <= 6 && !strings.ContainsAny(ext[1:], "0123456789") {
		return name
	}
	trimmed := strings.TrimSpace(string(data))
	if (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid(data) {
		return name + ".json"
	}
	return name + ".md"
}

func safeName(s string) string {
	s = strings.Trim(unsafeName.ReplaceAllString(s, "-"), "-.")
	if s == "" {
		return "set"
	}
	return s
}

func (s *service) getBytes(ctx context.Context, url string) (data []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode == 404 {
		return nil, errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// exportSet streams the evaluation set and the stored objects of its runs as a zip file.
func (s *service) exportSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	set, err := s.loadSet(ctx, r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, err)
		return
	}
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	if err := s.refreshPresence(ctx, &set); err != nil {
		s.fail(w, http.StatusBadGateway, fmt.Errorf("verifying source runs: %w", err))
		return
	}
	name := safeName(set.Name)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, name))
	zw := zip.NewWriter(w)
	// The archive's directory is written on Close. If that fails the download
	// is already under way, so the failure can only be logged.
	defer func() {
		if closeErr := zw.Close(); closeErr != nil {
			s.log.Error("closing the evaluation export archive failed", "set", set.ID, "error", closeErr)
		}
	}()
	add := func(file string, data []byte, modified time.Time) error {
		if modified.IsZero() {
			modified = set.Updated
		}
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name + "/" + file, Method: zip.Deflate, Modified: modified})
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	}
	// Once the download has begun, a failure can only be reported inside the archive.
	abort := func(err error) {
		s.log.Warn("evaluation export failed", "set", set.ID, "error", err)
		_ = add("EXPORT-FAILED.txt", []byte("The export stopped early: "+err.Error()+"\n"), time.Time{})
	}
	if err := add("README.md", []byte(exportReadme), time.Time{}); err != nil {
		return
	}
	stored, _ := json.MarshalIndent(set, "", "  ")
	if err := add("set.json", stored, time.Time{}); err != nil {
		return
	}
	for i := range set.Runs {
		run := &set.Runs[i]
		short := run.TraceID
		if len(short) > 8 {
			short = short[:8]
		}
		dir := fmt.Sprintf("runs/%02d-%s-%s/", i+1, safeName(run.Workflow), safeName(short))
		manifest := exportManifest{RunSnapshot: run, Files: []exportFile{}}
		if !run.SourcePresent {
			manifest.Note = "The source run was removed; only this snapshot remains."
		} else if err := s.exportRun(ctx, run.TraceID, &manifest, func(file string, data []byte, modified time.Time) error {
			return add(dir+file, data, modified)
		}); err != nil {
			abort(fmt.Errorf("run %s: %w", run.TraceID, err))
			return
		}
		data, _ := json.MarshalIndent(manifest, "", "  ")
		if err := add(dir+"manifest.json", data, time.Time{}); err != nil {
			return
		}
	}
}

// exportRun writes every stored object of one run and fills the run's manifest.
func (s *service) exportRun(ctx context.Context, trace string, manifest *exportManifest, add func(string, []byte, time.Time) error) error {
	var listing struct {
		Objects []struct {
			ID      string `json:"id"`
			Creator string `json:"creator"`
			Created string `json:"created"`
		} `json:"objects"`
	}
	if err := s.getJSON(ctx, s.blobs, "/trace/"+trace, &listing); err != nil {
		return fmt.Errorf("listing the run's objects: %w", err)
	}
	sort.SliceStable(listing.Objects, func(i, j int) bool { return listing.Objects[i].Created < listing.Objects[j].Created })
	used := map[string]bool{}
	for _, o := range listing.Objects {
		data, err := s.getBytes(ctx, s.blobs+"/blobs/"+o.ID)
		if errors.Is(err, errNotFound) {
			continue // removed since the listing
		}
		if err != nil {
			return err
		}
		file, role, description, step := classify(trace, o.ID)
		role, description = refine(role, description, data)
		file = withExtension(file, data)
		for n := 2; used[file]; n++ {
			file = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(file, path.Ext(file)), n, path.Ext(file))
		}
		used[file] = true
		modified, _ := time.Parse(time.RFC3339Nano, o.Created)
		if err := add(file, data, modified); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, exportFile{
			Path: file, Role: role, Description: description, Step: step,
			Object: o.ID, Creator: o.Creator, Created: o.Created, Size: len(data),
		})
		switch {
		case role == roleInput:
			manifest.Input = file
		case strings.HasPrefix(file, "steps/") && strings.HasPrefix(path.Base(file), "output.") && !strings.Contains(step, "/"):
			// The top-level step that wrote its output last ended the run.
			manifest.FinalOutput = file
		}
	}
	return nil
}
