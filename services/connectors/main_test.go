package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	builder = "course/builder@v1"
	secret  = "ghp_the-secret-token"
)

func TestRepositoriesAreStoredAndTheTokenIsNeverReturned(t *testing.T) {
	w := newWorld(t)
	status, out := w.call("PUT", "/api/git/repositories/claims", map[string]any{
		"kind": "github", "url": "https://github.com/acme/claims.git", "credential": "read-write", "token": secret,
	})
	if status != 200 {
		t.Fatalf("status %d: %v", status, out)
	}
	want := map[string]any{
		"name": "claims", "kind": "github", "url": "https://github.com/acme/claims.git", "default_branch": "main",
		"setup_command": "", "idle_minutes": 30.0, "approve_publish": true, "credential": "read-write",
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("stored repository = %v, want %v", out, want)
	}
	if got := w.kube.secret("git-claims", "token"); got != secret {
		t.Fatalf("token in the Secret = %q", got)
	}
	_, listed := w.raw("GET", "/api/git/repositories", nil)
	_, single := w.raw("GET", "/api/git/repositories/claims", nil)
	for _, text := range []string{string(listed), string(single), w.blobs.all()} {
		if strings.Contains(text, secret) || strings.Contains(text, `"token"`) {
			t.Fatalf("the token leaked: %s", text)
		}
	}
	if repos := w.list("/api/git/repositories"); len(repos) != 1 || repos[0]["name"] != "claims" {
		t.Fatalf("listed %v", repos)
	}
	if !strings.Contains(w.blobs.get("connectors/git/claims.json"), `"credential": "read-write"`) {
		t.Fatalf("blob = %s", w.blobs.get("connectors/git/claims.json"))
	}

	// An edit without a token keeps the stored one.
	if status, out = w.call("PUT", "/api/git/repositories/claims", map[string]any{"kind": "github", "url": "https://github.com/acme/claims", "credential": "read", "idle_minutes": 5, "approve_publish": false}); status != 200 {
		t.Fatalf("edit: status %d: %v", status, out)
	}
	if out["idle_minutes"] != 5.0 || out["approve_publish"] != false || w.kube.secret("git-claims", "token") != secret {
		t.Fatalf("edit = %v, token %q", out, w.kube.secret("git-claims", "token"))
	}
	// Without a credential the token is removed.
	if status, _ = w.call("PUT", "/api/git/repositories/claims", map[string]any{"kind": "github", "url": "https://github.com/acme/claims", "credential": "none"}); status != 200 {
		t.Fatalf("status %d", status)
	}
	if len(w.kube.names(w.kube.secrets)) != 0 {
		t.Fatalf("secrets left: %v", w.kube.names(w.kube.secrets))
	}
	// A credential needs a token.
	if status, out = w.call("PUT", "/api/git/repositories/claims", map[string]any{"url": "https://github.com/acme/claims", "credential": "read"}); status != 400 {
		t.Fatalf("credential without token: status %d: %v", status, out)
	}
	if status, _ = w.call("DELETE", "/api/git/repositories/claims", nil); status != 204 {
		t.Fatalf("delete: status %d", status)
	}
	if status, _ = w.call("GET", "/api/git/repositories/claims", nil); status != 404 {
		t.Fatalf("after delete: status %d", status)
	}
	if len(w.kube.denied) != 0 {
		t.Fatalf("requests outside the dev pod namespace: %v", w.kube.denied)
	}
}

func TestRepositoryDefaultsAndValidation(t *testing.T) {
	w := newWorld(t)
	status, out := w.call("PUT", "/api/git/repositories/claims-build", map[string]any{"url": "http://git-host:8080/claims-build.git"})
	want := map[string]any{
		"name": "claims-build", "kind": "plain", "url": "http://git-host:8080/claims-build.git", "default_branch": "main",
		"setup_command": "", "idle_minutes": 30.0, "approve_publish": false, "credential": "none",
	}
	if status != 200 || !reflect.DeepEqual(out, want) {
		t.Fatalf("status %d: %v", status, out)
	}
	for name, body := range map[string]map[string]any{
		"Bad_Name": {"url": "http://git-host:8080/x.git"},
		"no-url":   {},
		"kind":     {"url": "http://git-host:8080/x.git", "kind": "gitlab"},
		"cred":     {"url": "http://git-host:8080/x.git", "credential": "admin"},
		"idle":     {"url": "http://git-host:8080/x.git", "idle_minutes": 0},
		"github":   {"url": "https://github.com/only-owner", "kind": "github"},
	} {
		if status, out := w.call("PUT", "/api/git/repositories/"+name, body); status != 400 || out["error"] == "" {
			t.Fatalf("%s: status %d: %v", name, status, out)
		}
	}
}

func TestCreatingADevPodClonesSetsUpProtectsAndLimitsTheNetwork(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", map[string]any{"setup_command": "pip install -r requirements.txt", "credential": "read-write", "token": secret})
	status, out := w.call("POST", "/api/devpods", map[string]any{
		"repository": "claims-build", "source_branch": "main", "work_branch": "agent/payment-date-1234abcd", "run": "run-1",
		"protected": []string{"tests/**", "verify.sh"},
		"rules":     []map[string]any{{"tool": "write_file", "path": "src/panel/**", "matches": "rejection ?reason", "message": "Blocked by CL-7."}},
	})
	if status != 200 || out["reused"] != false || out["start_commit"] != "5ta27c0" || out["work_branch"] != "agent/payment-date-1234abcd" {
		t.Fatalf("status %d: %v", status, out)
	}
	id := out["id"].(string)
	if got, want := w.pod.called(), []string{"clone", "run:pip install -r requirements.txt", "protect"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls into the pod = %v, want %v", got, want)
	}
	clone := w.pod.bodies["/clone"]
	if clone["url"] != "http://git-host:8080/claims-build.git" || clone["source_branch"] != "main" || clone["work_branch"] != "agent/payment-date-1234abcd" || clone["author_email"] != authorEmail {
		t.Fatalf("clone = %v", clone)
	}
	if got := fmt.Sprint(w.pod.bodies["/protect"]["paths"]); got != "[tests/** verify.sh]" {
		t.Fatalf("protect = %s", got)
	}

	// The pod is what the plan says.
	var pod struct {
		Metadata struct{ Labels map[string]string }
		Spec     struct {
			AutomountServiceAccountToken *bool
			DNSConfig                    struct{ Searches []string }
			Volumes                      []map[string]any
			Containers                   []struct {
				Name, Image, ImagePullPolicy string
				Command                      []string
				Env                          []map[string]any
				SecurityContext              struct {
					RunAsUser    int
					Capabilities struct{ Add, Drop []string }
				}
				Resources    struct{ Limits map[string]string }
				VolumeMounts []struct{ Name, MountPath string }
			}
		}
	}
	data, _ := json.Marshal(w.kube.pods["devpod-"+id])
	if err := json.Unmarshal(data, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Metadata.Labels["app"] != "devpod" || pod.Metadata.Labels["devpod-id"] != id {
		t.Fatalf("labels = %v", pod.Metadata.Labels)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("the pod would get a service account token")
	}
	if !reflect.DeepEqual(pod.Spec.DNSConfig.Searches, []string{"opensoftwarefactory.svc.cluster.local"}) {
		t.Fatalf("dns searches = %v", pod.Spec.DNSConfig.Searches)
	}
	work, helper := pod.Spec.Containers[0], pod.Spec.Containers[1]
	if work.Name != "work" || work.Image != "opensoftwarefactory-devpod:local" || work.ImagePullPolicy != "Never" || strings.Join(work.Command, " ") != "python /opt/devpod/toolserver.py" ||
		work.SecurityContext.RunAsUser != 1000 || work.Resources.Limits["cpu"] != "1" || work.Resources.Limits["memory"] != "1Gi" {
		t.Fatalf("work = %+v", work)
	}
	if len(work.VolumeMounts) != 1 || work.VolumeMounts[0].MountPath != "/workspace" {
		t.Fatalf("work must mount the workspace only, and never the credential: %+v", work.VolumeMounts)
	}
	if helper.Name != "git-helper" || strings.Join(helper.Command, " ") != "python /opt/devpod/githelper.py" || helper.SecurityContext.RunAsUser != 0 ||
		!reflect.DeepEqual(helper.SecurityContext.Capabilities.Drop, []string{"ALL"}) || strings.Join(helper.SecurityContext.Capabilities.Add, " ") != "CHOWN FOWNER DAC_OVERRIDE SETUID SETGID" || len(helper.VolumeMounts) != 2 || helper.VolumeMounts[1].MountPath != "/credential" {
		t.Fatalf("git-helper = %+v", helper)
	}
	if !strings.Contains(string(data), `"secretName":"git-claims-build"`) {
		t.Fatalf("the credential is not mounted from the repository's Secret: %s", data)
	}
	// Each server has a token of its own, kept in a Secret and out of the pod spec and the blob store.
	workToken, helperToken := w.kube.secret("devpod-"+id, "work-token"), w.kube.secret("devpod-"+id, "helper-token")
	if len(workToken) < 32 || len(helperToken) < 32 || workToken == helperToken {
		t.Fatalf("tokens %q %q", workToken, helperToken)
	}
	for _, text := range []string{string(data), w.blobs.all()} {
		if strings.Contains(text, workToken) || strings.Contains(text, helperToken) || strings.Contains(text, secret) {
			t.Fatalf("a token leaked: %s", text)
		}
	}
	if w.pod.count("denied") != 0 {
		t.Fatalf("a call used the wrong token: %v", w.pod.called())
	}

	// The network policy: in from the connector service, out to DNS and the repository's host.
	policy, _ := json.Marshal(w.kube.policies["devpod-"+id])
	mustContain(t, string(policy), `"podSelector":{"matchLabels":{"devpod-id":"`+id+`"}}`, `"policyTypes":["Ingress","Egress"]`,
		`"app.kubernetes.io/name":"connectors"`, `{"port":53,"protocol":"UDP"}`, `"app.kubernetes.io/name":"git-host"`,
		`"kubernetes.io/metadata.name":"opensoftwarefactory"`)

	// The record survives in the blob store, and the pod is listed.
	record := w.blobs.get("connectors/devpods/" + id + ".json")
	mustContain(t, record, `"state": "ready"`, `"start_commit": "5ta27c0"`, `"tests/**"`, `"Blocked by CL-7."`, `"run-1"`)
	pods := w.list("/api/devpods")
	if len(pods) != 1 || pods[0]["id"] != id || pods[0]["repository"] != "claims-build" || pods[0]["phase"] != "Running" ||
		fmt.Sprint(pods[0]["runs"]) != "[run-1]" || pods[0]["expires"] != "2026-11-17T09:30:00Z" || pods[0]["last_activity"] != "2026-11-17T09:00:00Z" {
		t.Fatalf("listed %v", pods)
	}
	if status, detail := w.call("GET", "/api/devpods/"+id, nil); status != 200 || detail["start_commit"] != "5ta27c0" || detail["state"] != "ready" {
		t.Fatalf("detail: status %d: %v", status, detail)
	}
	if len(w.kube.denied) != 0 {
		t.Fatalf("requests outside the dev pod namespace: %v", w.kube.denied)
	}
}

func TestNetworkPolicyOfAnOutsideHostUsesItsAddresses(t *testing.T) {
	w := newWorld(t)
	policy, _ := json.Marshal(w.s.networkPolicy("abc", repository{Name: "claims", URL: "https://github.com/acme/claims.git"}))
	mustContain(t, string(policy), `"ipBlock":{"cidr":"140.82.121.3/32"}`, `{"port":443,"protocol":"TCP"}`)
	if strings.Contains(string(policy), "0.0.0.0/0") {
		t.Fatalf("the policy opens the internet: %s", policy)
	}
}

func TestAWorkBranchThatIsTheSourceOrDefaultBranchIsRefused(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", map[string]any{"default_branch": "main"})
	for _, body := range []map[string]any{
		{"repository": "claims-build", "source_branch": "main", "work_branch": "main", "run": "r1"},
		{"repository": "claims-build", "source_branch": "develop", "work_branch": "develop", "run": "r1"},
		{"repository": "claims-build", "source_branch": "develop", "work_branch": "main", "run": "r1"},
		{"repository": "claims-build", "work_branch": "main", "run": "r1"},
	} {
		status, out := w.call("POST", "/api/devpods", body)
		if status != 409 || !strings.Contains(out["error"].(string), "work branch") {
			t.Fatalf("%v: status %d: %v", body, status, out)
		}
	}
	if status, _ := w.call("POST", "/api/devpods", map[string]any{"repository": "unknown", "work_branch": "agent/x", "run": "r1"}); status != 404 {
		t.Fatalf("unknown repository: status %d", status)
	}
	if status, _ := w.call("POST", "/api/devpods", map[string]any{"repository": "claims-build", "work_branch": "agent/x"}); status != 400 {
		t.Fatalf("missing run: status %d", status)
	}
	if status, out := w.call("POST", "/api/devpods", map[string]any{
		"repository": "claims-build", "work_branch": "agent/x", "run": "r1",
		"rules": []map[string]any{{"tool": "write_file", "matches": "(", "message": "m"}},
	}); status != 400 {
		t.Fatalf("broken rule: status %d: %v", status, out)
	}
	if len(w.kube.names(w.kube.pods)) != 0 || w.pod.count("") != 0 {
		t.Fatal("a refused request started a pod")
	}
}

func TestAFailedCreationLeavesNothingBehind(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	w.pod.failClone = true
	status, out := w.call("POST", "/api/devpods", map[string]any{"repository": "claims-build", "work_branch": "agent/x", "run": "r1"})
	if status != 502 || !strings.Contains(out["error"].(string), "repository not found") {
		t.Fatalf("status %d: %v", status, out)
	}
	if len(w.kube.names(w.kube.pods))+len(w.kube.names(w.kube.secrets))+len(w.kube.names(w.kube.policies)) != 0 || len(w.list("/api/devpods")) != 0 {
		t.Fatalf("left behind: pods %v secrets %v", w.kube.names(w.kube.pods), w.kube.names(w.kube.secrets))
	}
	if strings.Contains(w.blobs.all(), "connectors/devpods/") {
		t.Fatal("the record of the failed pod is still stored")
	}

	// A setup command that fails also fails the creation, with its output.
	w.pod.failClone = false
	w.repository("claims-build", map[string]any{"setup_command": "make deps"})
	w.pod.exitCodes["make deps"] = 2
	status, out = w.call("POST", "/api/devpods", map[string]any{"repository": "claims-build", "work_branch": "agent/x", "run": "r1"})
	if status != 502 || !strings.Contains(out["error"].(string), "exit code 2") || len(w.kube.names(w.kube.pods)) != 0 {
		t.Fatalf("status %d: %v", status, out)
	}
}

func TestANamedPodIsReusedAndAnUnnamedOneIsNot(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	w.repository("claims-review", nil)
	first := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1", "name": "shared", "protected": []string{"verify.sh"}})
	status, out := w.call("POST", "/api/devpods", map[string]any{
		"repository": "claims-build", "work_branch": "agent/b", "run": "run-2", "name": "shared",
		"protected": []string{"verify.sh", "tests/**"}, "rules": []map[string]any{{"tool": "run_command", "matches": "push", "message": "No pushing."}},
	})
	if status != 200 || out["id"] != first || out["reused"] != true || out["work_branch"] != "agent/a" || out["start_commit"] != "5ta27c0" {
		t.Fatalf("reuse: status %d: %v", status, out)
	}
	if w.pod.count("clone") != 1 || w.pod.count("protect") != 2 {
		t.Fatalf("calls %v", w.pod.called())
	}
	p, _ := w.s.pod(first)
	if !reflect.DeepEqual(p.Users, []string{"run-1", "run-2"}) || !reflect.DeepEqual(p.Protected, []string{"verify.sh", "tests/**"}) || len(p.Rules) != 1 || len(p.Holds) != 2 {
		t.Fatalf("pod after reuse = %+v", p)
	}
	// Both runs may use it now.
	if ok, _, msg := w.tool(first, "run-2", builder, "read_file", map[string]any{"path": "a.txt"}); !ok {
		t.Fatal(msg)
	}
	// The same name in another repository, and no name at all, give new pods.
	other := w.devpod(map[string]any{"repository": "claims-review", "work_branch": "agent/a", "run": "run-3", "name": "shared"})
	unnamed1 := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-4"})
	unnamed2 := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-4"})
	if other == first || unnamed1 == first || unnamed1 == unnamed2 || len(w.kube.names(w.kube.pods)) != 4 {
		t.Fatalf("pods %s %s %s %s", first, other, unnamed1, unnamed2)
	}
	// A named pod whose pod is gone is replaced.
	w.kube.mu.Lock()
	delete(w.kube.pods, "devpod-"+first)
	w.kube.mu.Unlock()
	status, out = w.call("POST", "/api/devpods", map[string]any{"repository": "claims-build", "work_branch": "agent/c", "run": "run-5", "name": "shared"})
	if status != 200 || out["reused"] != false || out["id"] == first {
		t.Fatalf("replace: status %d: %v", status, out)
	}
	if _, ok := w.s.pod(first); ok {
		t.Fatal("the record of the vanished pod is still there")
	}
}

func TestTwoRequestsForTheSameNameBuildOnePod(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	ids := make([]string, 4)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, out := w.call("POST", "/api/devpods", map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": fmt.Sprintf("run-%d", i), "name": "shared"})
			ids[i], _ = out["id"].(string)
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("ids %v", ids)
		}
	}
	if len(w.kube.names(w.kube.pods)) != 1 {
		t.Fatalf("pods %v", w.kube.names(w.kube.pods))
	}
}

func TestProtectedPathsAndRulesBlockWrites(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{
		"repository": "claims-build", "work_branch": "agent/a", "run": "run-1",
		"protected": []string{"tests/**", "verify.sh"},
		"rules": []map[string]any{
			{"tool": "write_file", "path": "src/panel/**", "matches": "rejection ?reason", "message": "Blocked by CL-7: the panel must not show a rejection reason. Remove the text and write the file again."},
			{"tool": "edit_file", "path": "src/panel/**", "matches": "rejection ?reason", "message": "Blocked by CL-7 (edit)."},
			{"tool": "write_file", "path": "generated/**", "message": "Generated files are built, not written."},
		},
	})
	before := len(w.pod.called())
	refused := []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"write_file", map[string]any{"path": "tests/test_panel.py", "content": "x"}, []string{"Refused: write_file", `"tests/test_panel.py"`, `"tests/**"`, "protected", "report_blocked"}},
		{"edit_file", map[string]any{"path": "/workspace/verify.sh", "old_text": "a", "new_text": "b"}, []string{"Refused: edit_file", `"verify.sh"`, "protected"}},
		{"write_file", map[string]any{"path": "src/../tests/deep/x.py", "content": "x"}, []string{"protected"}},
		{"write_file", map[string]any{"path": "src/panel/view.py", "content": "show the rejection reason"}, []string{"Refused: write_file", `"src/panel/view.py"`, "Blocked by CL-7", "Remove the text"}},
		{"edit_file", map[string]any{"path": "src/panel/view.py", "old_text": "a", "new_text": "rejectionreason"}, []string{"Blocked by CL-7 (edit)."}},
		{"write_file", map[string]any{"path": "generated/a/b.json", "content": "{}"}, []string{"Generated files are built"}},
		{"write_file", map[string]any{"content": "x"}, []string{"needs a path"}},
	}
	for _, c := range refused {
		ok, _, msg := w.tool(id, "run-1", builder, c.tool, c.args)
		if ok {
			t.Fatalf("%s %v was not refused", c.tool, c.args)
		}
		mustContain(t, msg, c.want...)
	}
	if got := len(w.pod.called()); got != before {
		t.Fatalf("a refused call reached the pod: %v", w.pod.called()[before:])
	}
	allowed := []struct {
		tool string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": "src/panel/view.py", "content": "show the payment date"}},
		{"write_file", map[string]any{"path": "src/other.py", "content": "rejection reason"}},
		{"edit_file", map[string]any{"path": "src/panel/view.py", "old_text": "rejection reason", "new_text": "payment date"}},
		{"write_file", map[string]any{"path": "verify.sh.bak", "content": "x"}},
		{"list_files", map[string]any{"path": "tests"}},
		{"read_file", map[string]any{"path": "tests/test_panel.py", "start_line": 1, "end_line": 5}},
		{"search", map[string]any{"pattern": "rejection reason"}},
	}
	for _, c := range allowed {
		if ok, _, msg := w.tool(id, "run-1", builder, c.tool, c.args); !ok {
			t.Fatalf("%s %v was refused: %s", c.tool, c.args, msg)
		}
	}
	if got := len(w.pod.called()); got != before+len(allowed) {
		t.Fatalf("calls into the pod: %v", w.pod.called()[before:])
	}
	// The arguments reach the tool server unchanged.
	if last := w.pod.ops[len(w.pod.ops)-2]; last["path"] != "tests/test_panel.py" || last["start_line"] != 1.0 || last["end_line"] != 5.0 {
		t.Fatalf("read_file arguments = %v", last)
	}
	// An error of the tool server is a refusal with its message.
	if ok, _, msg := w.tool(id, "run-1", builder, "read_file", map[string]any{"path": "missing.txt"}); ok || msg != "missing.txt does not exist" {
		t.Fatalf("ok %v: %s", ok, msg)
	}
}

func TestOnlyAllowedCommandsRun(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{
		"repository": "claims-build", "work_branch": "agent/a", "run": "run-1",
		"rules": []map[string]any{{"tool": "run_command", "matches": `git commit .*--no-verify`, "message": "Blocked by CL-9: commits must run the hooks. Commit without --no-verify."}},
	})
	run := func(agent, command string) (bool, any, string) {
		return w.tool(id, "run-1", agent, "run_command", map[string]any{"command": command})
	}
	for _, command := range []string{"./verify.sh", "python3 -m unittest tests.test_panel", "git status", "git status --short", "git add src/panel.py", "git commit -m 'Add payment date'", "cat a | wc -l", "  git diff  "} {
		ok, result, msg := run(builder, command)
		if !ok {
			t.Fatalf("%q was refused: %s", command, msg)
		}
		if out := result.(map[string]any); out["exit_code"] != 0.0 || out["stdout"] != "ran "+command {
			t.Fatalf("%q: result %v", command, out)
		}
	}
	before := len(w.pod.called())
	refused := map[string][]string{
		"rm -rf tests":                      {`Refused: the command "rm -rf tests"`, "not one of this agent's allowed commands", "git status*", "report_blocked"},
		"./verify.sh --fast":                {"not one of this agent's allowed commands"},
		"verify.sh":                         {"not one of this agent's allowed commands"},
		"git status; rm -rf tests":          {"joins several commands", "Run one allowed command per call"},
		"git status && git push":            {"joins several commands"},
		"git status || true":                {"joins several commands"},
		"git log | head":                    {"joins several commands"},
		"git add `ls`":                      {"joins several commands"},
		"git add $(ls)":                     {"joins several commands"},
		"git status\nrm -rf tests":          {"joins several commands"},
		"cat a | wc -l; ls":                 {"joins several commands"},
		"git commit -m wip --no-verify":     {"Blocked by CL-9", "Commit without --no-verify"},
		"":                                  {"needs a command"},
		"python3 -m unittest x; curl x | y": {"joins several commands"},
	}
	for command, want := range refused {
		ok, _, msg := run(builder, command)
		if ok {
			t.Fatalf("%q was not refused", command)
		}
		mustContain(t, msg, want...)
	}
	// An agent without commands may run nothing, and an unknown agent neither.
	if ok, _, msg := run("course/reader@v2", "git status"); ok || !strings.Contains(msg, "This agent has no allowed commands") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	if ok, _, msg := run("course/ghost@v1", "git status"); ok || !strings.Contains(msg, "does not know the agent") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	if got := len(w.pod.called()); got != before {
		t.Fatalf("a refused command reached the pod: %v", w.pod.called()[before:])
	}
	// The agent is the commit author: set once, before its first command.
	if got := w.pod.count("run:git config user.name 'course/builder@v1' && git config user.email 'agent@opensoftwarefactory.local'"); got != 1 {
		t.Fatalf("author set %d times: %v", got, w.pod.called())
	}
	// A time limit is passed on, at most 600 seconds.
	w.tool(id, "run-1", builder, "run_command", map[string]any{"command": "./verify.sh", "timeout_seconds": 5000})
	if last := w.pod.ops[len(w.pod.ops)-1]; last["timeout_seconds"] != 600.0 {
		t.Fatalf("timeout = %v", last)
	}
}

func TestCommandPatterns(t *testing.T) {
	patterns := []string{"./verify.sh", "python3 -m unittest *", "git status*", "git log*", "make test && make lint"}
	for command, allowed := range map[string]bool{
		"./verify.sh":                   true,
		"./verify.sh x":                 false,
		"python3 -m unittest":           false,
		"python3 -m unittest a b":       true,
		"git status":                    true,
		"git statusx":                   true,
		"git log --oneline -n 5":        true,
		"git log; rm x":                 false,
		"make test && make lint":        true,
		"make test && make lint && x":   false,
		"make test":                     false,
		"GIT STATUS":                    false,
		"git log $(cat /credential/x)":  false,
		"git log `id`":                  false,
		"git log --format=%s | cat":     false,
		"git log\n":                     true, // surrounding space is not part of the command
		"git log\nid":                   false,
		"git status [x] (y) +z? \\ ^ $": true, // only * is special in a pattern
	} {
		if err := commandAllowed(patterns, command); (err == nil) != allowed {
			t.Errorf("%q: allowed = %v, want %v (%v)", command, err == nil, allowed, err)
		}
	}
	if err := commandAllowed(nil, "ls"); err == nil || !strings.Contains(err.Error(), "no allowed commands") {
		t.Errorf("no patterns: %v", err)
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"tests/**", "tests/a.py", true},
		{"tests/**", "tests/unit/deep/a.py", true},
		{"tests/**", "src/tests/a.py", false},
		{"tests/**", "tests.py", false},
		{"verify.sh", "verify.sh", true},
		{"verify.sh", "scripts/verify.sh", false},
		{"**/verify.sh", "scripts/verify.sh", true},
		{"**/verify.sh", "verify.sh", true},
		{"docs", "docs/guide/a.md", true},
		{"docs/", "docs/a.md", true},
		{"src/*.py", "src/a.py", true},
		{"src/*.py", "src/sub/a.py", false},
		{"src/panel/**", "src/panel/view.py", true},
		{"src/panel/**", "src/panelx/view.py", false},
		{"*.lock", "uv.lock", true},
		{"a?c", "abc", true},
		{"a?c", "a/c", false},
		{"./verify.sh", "verify.sh", true},
		{"a.b", "aXb", false},
	} {
		if got := globMatch(c.pattern, c.path); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
	if got := workspacePath("/workspace/src/../tests/./a.py"); got != "tests/a.py" {
		t.Errorf("workspacePath = %q", got)
	}
	if got := workspacePath("../../etc/passwd"); got != "etc/passwd" {
		t.Errorf("workspacePath = %q", got)
	}
}

func TestOnlyTheRunsOfAPodMayUseIt(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	other := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/b", "run": "run-2"})
	before := len(w.pod.called())
	for _, run := range []string{"run-2", ""} {
		ok, _, msg := w.tool(id, run, builder, "read_file", map[string]any{"path": "a.txt"})
		if ok || !strings.Contains(msg, "does not use this dev pod") {
			t.Fatalf("run %q: ok %v: %s", run, ok, msg)
		}
	}
	if ok, _, msg := w.tool(other, "run-1", builder, "run_command", map[string]any{"command": "git status"}); ok {
		t.Fatalf("run-1 used the pod of run-2: %s", msg)
	}
	if ok, _, msg := w.tool("0000000000", "run-1", builder, "read_file", map[string]any{"path": "a.txt"}); ok || !strings.Contains(msg, "does not exist any more") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	if ok, _, msg := w.tool(id, "run-1", builder, "delete_everything", nil); ok || !strings.Contains(msg, "is not a dev pod tool") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	if len(w.pod.called()) != before {
		t.Fatalf("a refused call reached a pod: %v", w.pod.called()[before:])
	}
}

func TestReportBlockedIsRecorded(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	if ok, _, msg := w.tool(id, "run-1", builder, "report_blocked", map[string]any{"reason": " "}); ok || !strings.Contains(msg, "needs a reason") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	ok, result, msg := w.tool(id, "run-1", builder, "report_blocked", map[string]any{"reason": "The test service is down."})
	if !ok || !strings.HasPrefix(result.(string), "Recorded.") {
		t.Fatalf("ok %v: %v %s", ok, result, msg)
	}
	_, detail := w.call("GET", "/api/devpods/"+id, nil)
	blocked := detail["blocked"].([]any)[0].(map[string]any)
	if blocked["run"] != "run-1" || blocked["agent"] != builder || blocked["reason"] != "The test service is down." {
		t.Fatalf("blocked = %v", blocked)
	}
	mustContain(t, w.blobs.get("connectors/devpods/"+id+".json"), "The test service is down.")
}

func TestIdlePodsAreDeletedUnlessARunHoldsThem(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", map[string]any{"idle_minutes": 30})
	id := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	ctx := context.Background()
	alive := func() bool { return len(w.kube.names(w.kube.pods)) == 1 }

	// Held by the run that created it: it stays however long nobody uses it.
	w.advance(3 * time.Hour)
	w.s.sweep(ctx)
	if !alive() {
		t.Fatal("a held pod was deleted")
	}
	// A second run holds it too; releasing one of them is not enough.
	if status, out := w.call("POST", "/api/devpods/"+id+"/hold", map[string]any{"run": "run-2"}); status != 200 || fmt.Sprint(out["runs"]) != "[run-1 run-2]" {
		t.Fatalf("hold: status %d: %v", status, out)
	}
	if status, out := w.call("POST", "/api/devpods/"+id+"/release", map[string]any{"run": "run-1"}); status != 200 || fmt.Sprint(out["runs"]) != "[run-2]" {
		t.Fatalf("release: status %d: %v", status, out)
	}
	w.s.sweep(ctx)
	if !alive() {
		t.Fatal("a pod held by a second run was deleted")
	}
	// An agent uses it, then the last run lets go: the idle time counts from the agent's call.
	if ok, _, msg := w.tool(id, "run-2", builder, "read_file", map[string]any{"path": "a.txt"}); !ok {
		t.Fatal(msg)
	}
	w.call("POST", "/api/devpods/"+id+"/release", map[string]any{"run": "run-2"})
	w.advance(29 * time.Minute)
	w.s.sweep(ctx)
	if !alive() {
		t.Fatal("the pod was deleted before its idle time")
	}
	// A command step and a person looking are not agent activity.
	w.call("POST", "/api/devpods/"+id+"/exec", map[string]any{"command": "./verify.sh"})
	w.call("GET", "/api/devpods/"+id+"/files", nil)
	w.advance(2 * time.Minute)
	w.s.sweep(ctx)
	if alive() || len(w.kube.names(w.kube.secrets)) != 0 || len(w.kube.names(w.kube.policies)) != 0 {
		t.Fatalf("after the idle time: pods %v secrets %v policies %v", w.kube.names(w.kube.pods), w.kube.names(w.kube.secrets), w.kube.names(w.kube.policies))
	}
	if len(w.list("/api/devpods")) != 0 || strings.Contains(w.blobs.all(), "connectors/devpods/") {
		t.Fatal("the record of the deleted pod is still there")
	}
	// Releasing a pod that is gone is not an error.
	if status, _ := w.call("POST", "/api/devpods/"+id+"/release", map[string]any{"run": "run-2"}); status != 204 {
		t.Fatalf("release of a deleted pod: status %d", status)
	}
	if status, _ := w.call("POST", "/api/devpods/"+id+"/hold", map[string]any{"run": "run-2"}); status != 404 {
		t.Fatalf("hold of a deleted pod: status %d", status)
	}
}

func TestIdleTimeAndIntervalCanBeSecondsAndHoldsCanExpire(t *testing.T) {
	t.Setenv("DEVPOD_SWEEP_SECONDS", "0.02")
	t.Setenv("DEVPOD_IDLE_SECONDS", "0.05")
	t.Setenv("DEVPOD_HOLD_SECONDS", "90")
	cfg := configFromEnv()
	if cfg.sweep != 20*time.Millisecond || cfg.idleOverride != 50*time.Millisecond || cfg.holdTTL != 90*time.Second {
		t.Fatalf("config = %+v", cfg)
	}
	w := newWorld(t)
	w.s.cfg.sweep, w.s.cfg.idleOverride = cfg.sweep, cfg.idleOverride
	w.s.now = time.Now // the loop runs on real time here
	w.repository("claims-build", nil)
	held := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	loose := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/b", "run": "run-2"})
	w.call("POST", "/api/devpods/"+loose+"/release", map[string]any{"run": "run-2"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.s.sweepLoop(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(w.kube.names(w.kube.pods)) != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := w.kube.names(w.kube.pods); !reflect.DeepEqual(got, []string{"devpod-" + held}) {
		t.Fatalf("pods after the idle time = %v, want only the held one", got)
	}
	// With a hold time limit, a hold that is not renewed ends.
	cancel()
	<-done
	w.s.cfg.holdTTL = 30 * time.Millisecond
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go w.s.sweepLoop(ctx)
	for len(w.kube.names(w.kube.pods)) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := w.kube.names(w.kube.pods); len(got) != 0 {
		t.Fatalf("pods after the hold ended = %v", got)
	}
}

func TestSweepBringsRecordsAndClusterInLine(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	gone := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	kept := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/b", "run": "run-2"})
	w.kube.mu.Lock()
	delete(w.kube.pods, "devpod-"+gone)
	w.kube.pods["devpod-stray"] = map[string]any{"metadata": map[string]any{"name": "devpod-stray", "labels": map[string]any{"app": "devpod"}}}
	w.kube.pods["something-else"] = map[string]any{"metadata": map[string]any{"name": "something-else", "labels": map[string]any{"app": "other"}}}
	w.kube.mu.Unlock()
	w.s.sweep(context.Background())
	if got := w.kube.names(w.kube.pods); !reflect.DeepEqual(got, []string{"devpod-" + kept, "something-else"}) {
		t.Fatalf("pods = %v", got)
	}
	if pods := w.list("/api/devpods"); len(pods) != 1 || pods[0]["id"] != kept {
		t.Fatalf("listed %v", pods)
	}
}

func TestDestroyingAPod(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	if status, out := w.call("DELETE", "/api/git/repositories/claims-build", nil); status != 409 {
		t.Fatalf("deleting a repository with a pod: status %d: %v", status, out)
	}
	if status, _ := w.call("DELETE", "/api/devpods/"+id, nil); status != 204 {
		t.Fatalf("status %d", status)
	}
	if len(w.kube.names(w.kube.pods))+len(w.kube.names(w.kube.secrets))+len(w.kube.names(w.kube.policies)) != 0 || len(w.list("/api/devpods")) != 0 {
		t.Fatal("something of the pod is left")
	}
	if status, _ := w.call("DELETE", "/api/devpods/"+id, nil); status != 404 {
		t.Fatalf("second delete: status %d", status)
	}
	if status, _ := w.call("DELETE", "/api/git/repositories/claims-build", nil); status != 204 {
		t.Fatalf("deleting the repository: status %d", status)
	}
}

func TestExecRunsACommandWithoutRulesOrActivity(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{
		"repository": "claims-build", "work_branch": "agent/a", "run": "run-1",
		"rules": []map[string]any{{"tool": "run_command", "matches": "verify", "message": "blocked"}},
	})
	w.pod.exitCodes["./verify.sh && echo done"] = 3
	w.advance(10 * time.Minute)
	status, out := w.call("POST", "/api/devpods/"+id+"/exec", map[string]any{"command": "./verify.sh && echo done", "timeout_seconds": 300})
	if status != 200 || !reflect.DeepEqual(out, map[string]any{"exit_code": 3.0, "stdout": "ran ./verify.sh && echo done", "stderr": ""}) {
		t.Fatalf("status %d: %v", status, out)
	}
	if last := w.pod.ops[len(w.pod.ops)-1]; last["timeout_seconds"] != 300.0 {
		t.Fatalf("arguments = %v", last)
	}
	if p, _ := w.s.pod(id); !p.LastActivity.Equal(p.Created) {
		t.Fatalf("exec counted as agent activity: %v", p.LastActivity)
	}
	if status, _ = w.call("POST", "/api/devpods/"+id+"/exec", map[string]any{"command": " "}); status != 400 {
		t.Fatalf("empty command: status %d", status)
	}
	if status, _ = w.call("POST", "/api/devpods/nope/exec", map[string]any{"command": "ls"}); status != 404 {
		t.Fatalf("unknown pod: status %d", status)
	}
}

func TestReadOnlyViews(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil)
	id := w.devpod(map[string]any{"repository": "claims-build", "source_branch": "main", "work_branch": "agent/a", "run": "run-1"})
	if status, out := w.call("GET", "/api/devpods/"+id+"/files?path=src", nil); status != 200 || out["path"] != "src" || len(out["entries"].([]any)) != 2 || out["truncated"] != false {
		t.Fatalf("files: status %d: %v", status, out)
	}
	if status, out := w.call("GET", "/api/devpods/"+id+"/file?path=README.md", nil); status != 200 || out["content"] != "hello\n" || out["total_lines"] != 1.0 {
		t.Fatalf("file: status %d: %v", status, out)
	}
	if status, out := w.call("GET", "/api/devpods/"+id+"/file?path=missing.txt", nil); status != 422 || out["error"] != "missing.txt does not exist" {
		t.Fatalf("missing file: status %d: %v", status, out)
	}
	if status, out := w.call("GET", "/api/devpods/"+id+"/log", nil); status != 200 || !strings.HasPrefix(out["output"].(string), "output of git log ") {
		t.Fatalf("log: status %d: %v", status, out)
	}
	if status, out := w.call("GET", "/api/devpods/"+id+"/diff", nil); status != 200 || out["output"] != "output of git diff 5ta27c0" {
		t.Fatalf("diff: status %d: %v", status, out)
	}
	if p, _ := w.s.pod(id); !p.LastActivity.Equal(p.Created) {
		t.Fatal("looking at a pod counted as agent activity")
	}
}

func TestPlainRepositoryPushAndPullRequestRecord(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", nil) // plain, no credential, no approval: the demo Git host
	id := w.devpod(map[string]any{"repository": "claims-build", "source_branch": "main", "work_branch": "agent/payment-date", "run": "run-1"})
	git := func(name string, args map[string]any) (bool, any, string) {
		return w.tool(id, "run-1", builder, name, args)
	}

	if ok, _, msg := git("get_pull_request", nil); ok || !strings.Contains(msg, "no pull request yet") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	w.pod.commits = nil
	if ok, _, msg := git("create_pull_request", map[string]any{"title": "Payment date", "body": "text"}); ok || !strings.Contains(msg, "no commits yet") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	w.pod.commits = []commit{{Hash: "c0ffee1", Subject: "Add payment date", Author: builder}}
	if ok, _, msg := git("create_pull_request", map[string]any{"body": "text"}); ok || !strings.Contains(msg, "needs a title") {
		t.Fatalf("ok %v: %s", ok, msg)
	}

	// The record works without a push.
	ok, result, msg := git("create_pull_request", map[string]any{"title": "Payment date", "body": "Shows the payment date."})
	if !ok {
		t.Fatal(msg)
	}
	if out := result.(map[string]any); out["pull_request"] != "runs/run-1/pull-request.json" || out["patch"] != "runs/run-1/pull-request.patch" || out["commits"] != 1.0 || out["pushed"] != false {
		t.Fatalf("result = %v", out)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(w.blobs.get("runs/run-1/pull-request.json")), &record); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"repository": "claims-build", "source_branch": "main", "work_branch": "agent/payment-date", "title": "Payment date",
		"body": "Shows the payment date.", "patch": "runs/run-1/pull-request.patch", "pushed": false, "created": "2026-11-17T09:00:00Z", "created_by_agent": builder,
	} {
		if record[key] != want {
			t.Fatalf("record %s = %v, want %v", key, record[key], want)
		}
	}
	if got := fmt.Sprint(record["commits"]); got != "[map[author:course/builder@v1 hash:c0ffee1 subject:Add payment date]]" {
		t.Fatalf("commits = %s", got)
	}
	if patch := w.blobs.get("runs/run-1/pull-request.patch"); !strings.HasPrefix(patch, "From c0ffee1") {
		t.Fatalf("patch = %q", patch)
	}
	if base := w.pod.bodies["/patch"]["base"]; base != "5ta27c0" {
		t.Fatalf("the patch starts at %v, not at the commit the pod was created from", base)
	}

	// The push goes through git-helper and shows in the record.
	ok, result, msg = git("push_branch", nil)
	if !ok || result.(map[string]any)["pushed"] != true || w.pod.count("push:agent/payment-date") != 1 {
		t.Fatalf("ok %v: %v %s", ok, result, msg)
	}
	mustContain(t, w.blobs.get("runs/run-1/pull-request.json"), `"pushed": true`)

	if ok, _, msg = git("create_pull_request", map[string]any{"title": "Again", "body": ""}); ok || !strings.Contains(msg, "already has a pull request") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	if ok, _, msg = git("update_pull_request", map[string]any{}); ok || !strings.Contains(msg, "needs a new title") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	if ok, _, msg = git("update_pull_request", map[string]any{"body": "Shows the payment date on the panel."}); !ok {
		t.Fatal(msg)
	}
	if ok, _, msg = git("comment_on_pull_request", map[string]any{"body": "Checks pass."}); !ok {
		t.Fatal(msg)
	}
	mustContain(t, w.blobs.get("runs/run-1/pull-request.json"), `"title": "Payment date"`, `"body": "Shows the payment date on the panel."`, `"Checks pass."`, `"created": "2026-11-17T09:00:00Z"`)
	ok, result, msg = git("get_pull_request", nil)
	if out := result.(map[string]any); !ok || out["title"] != "Payment date" || out["body"] != "Shows the payment date on the panel." || out["pushed"] != true || len(out["comments"].([]any)) != 1 {
		t.Fatalf("ok %v: %v %s", ok, result, msg)
	}

	// A push the host refuses is an error that says so.
	w.pod.failPush = true
	if ok, _, msg = git("push_branch", nil); ok {
		t.Fatal("a refused push was reported as done")
	} else {
		mustContain(t, msg, "push_branch failed", "Permission denied", "no credential that may write", "create_pull_request")
	}
}

func TestAReadOnlyCredentialRefusesThePush(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", map[string]any{"credential": "read", "token": secret})
	id := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	ok, _, msg := w.tool(id, "run-1", builder, "push_branch", nil)
	if ok || w.pod.count("push") != 0 {
		t.Fatalf("ok %v, calls %v", ok, w.pod.called())
	}
	mustContain(t, msg, "Refused", `"claims-build"`, "may only read", "create_pull_request")
	// The patch record still works.
	if ok, _, msg = w.tool(id, "run-1", builder, "create_pull_request", map[string]any{"title": "T", "body": "B"}); !ok {
		t.Fatal(msg)
	}
	mustContain(t, w.blobs.get("runs/run-1/pull-request.json"), `"pushed": false`)
}

func TestPublishingWaitsForApproval(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", map[string]any{"approve_publish": true})
	id := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/a", "run": "run-1"})
	git := func(name string, args map[string]any) (bool, any, string) {
		return w.tool(id, "run-1", builder, name, args)
	}

	for range 2 { // asking twice leaves one request
		if ok, result, msg := git("push_branch", nil); !ok || result != awaitingApproval {
			t.Fatalf("ok %v: %v %s", ok, result, msg)
		}
	}
	git("create_pull_request", map[string]any{"title": "First title", "body": "B"})
	if ok, result, _ := git("create_pull_request", map[string]any{"title": "Payment date", "body": "Shows the payment date."}); !ok || result != "Recorded. A person must approve before this is published." {
		t.Fatalf("ok %v: %v", ok, result)
	}
	if w.pod.count("push")+w.pod.count("patch") != 0 || strings.Contains(w.blobs.all(), "pull-request") {
		t.Fatalf("something was published before the approval: %v", w.pod.called())
	}
	requests := w.list("/api/devpods/" + id + "/publish-requests?run=run-1")
	if len(requests) != 2 || requests[0]["n"] != 1.0 || requests[0]["kind"] != "push" || requests[0]["state"] != "pending" ||
		requests[1]["n"] != 2.0 || requests[1]["kind"] != "pull_request" || requests[1]["title"] != "Payment date" || requests[1]["body"] != "Shows the payment date." || requests[1]["result"] != "" {
		t.Fatalf("requests = %v", requests)
	}
	if other := w.list("/api/devpods/" + id + "/publish-requests?run=run-9"); len(other) != 0 {
		t.Fatalf("requests of another run = %v", other)
	}

	// Approving the push pushes.
	status, out := w.call("POST", "/api/devpods/"+id+"/publish-requests/1/decision", map[string]any{"decision": "approve", "comment": "ok", "decided_by": "flo"})
	if status != 200 || out["state"] != "done" || !strings.Contains(out["result"].(string), "Pushed the branch agent/a") || out["decided_by"] != "flo" || w.pod.count("push:agent/a") != 1 {
		t.Fatalf("status %d: %v, calls %v", status, out, w.pod.called())
	}
	if status, out = w.call("POST", "/api/devpods/"+id+"/publish-requests/1/decision", map[string]any{"decision": "approve"}); status != 409 || w.pod.count("push") != 1 {
		t.Fatalf("deciding twice: status %d: %v", status, out)
	}
	// Approving the pull request stores the record, for the run and agent that asked.
	status, out = w.call("POST", "/api/devpods/"+id+"/publish-requests/2/decision", map[string]any{"decision": "approve", "decided_by": "flo"})
	if status != 200 || out["state"] != "done" || out["result"] != "Pull request recorded: runs/run-1/pull-request.json" {
		t.Fatalf("status %d: %v", status, out)
	}
	mustContain(t, w.blobs.get("runs/run-1/pull-request.json"), `"title": "Payment date"`, `"pushed": true`, `"created_by_agent": "course/builder@v1"`)

	// A rejected request publishes nothing.
	other := w.devpod(map[string]any{"repository": "claims-build", "work_branch": "agent/b", "run": "run-2"})
	w.tool(other, "run-2", builder, "push_branch", nil)
	status, out = w.call("POST", "/api/devpods/"+other+"/publish-requests/1/decision", map[string]any{"decision": "reject", "comment": "not yet", "decided_by": "flo"})
	if status != 200 || out["state"] != "rejected" || out["comment"] != "not yet" || w.pod.count("push:agent/b") != 0 {
		t.Fatalf("status %d: %v", status, out)
	}
	if status, _ = w.call("POST", "/api/devpods/"+other+"/publish-requests/1/decision", map[string]any{"decision": "maybe"}); status != 400 {
		t.Fatalf("status %d", status)
	}
	if status, _ = w.call("POST", "/api/devpods/"+other+"/publish-requests/7/decision", map[string]any{"decision": "approve"}); status != 404 {
		t.Fatalf("status %d", status)
	}

	// A publish that fails stays approved, says why, and can be decided again.
	w.tool(other, "run-2", builder, "push_branch", nil)
	w.pod.failPush = true
	status, out = w.call("POST", "/api/devpods/"+other+"/publish-requests/2/decision", map[string]any{"decision": "approve"})
	if status != 502 || !strings.Contains(out["error"].(string), "Permission denied") {
		t.Fatalf("status %d: %v", status, out)
	}
	if req := w.list("/api/devpods/" + other + "/publish-requests")[1]; req["state"] != "approved" || !strings.HasPrefix(req["result"].(string), "Failed: ") {
		t.Fatalf("request = %v", req)
	}
	w.pod.failPush = false
	if status, out = w.call("POST", "/api/devpods/"+other+"/publish-requests/2/decision", map[string]any{"decision": "approve"}); status != 200 || out["state"] != "done" {
		t.Fatalf("status %d: %v", status, out)
	}
}

// fakeGitHub is the part of the GitHub REST API the connector service calls.
type fakeGitHub struct {
	mu    sync.Mutex
	calls []string
	pull  map[string]any
	srv   *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		data, _ := io.ReadAll(r.Body)
		g.calls = append(g.calls, r.Method+" "+r.URL.Path+" "+string(data))
		if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("Accept") != "application/vnd.github+json" {
			w.WriteHeader(401)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Bad credentials"})
			return
		}
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		switch {
		case r.Method == "POST" && r.URL.Path == "/repos/acme/claims/pulls":
			if body["head"] == "agent/taken" {
				w.WriteHeader(422)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "A pull request already exists for acme:agent/taken."})
				return
			}
			g.pull = map[string]any{"number": 7, "html_url": "https://github.com/acme/claims/pull/7", "state": "open", "title": body["title"], "body": body["body"], "merged": false}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(g.pull)
		case r.Method == "PATCH" && r.URL.Path == "/repos/acme/claims/pulls/7":
			g.pull["title"], g.pull["body"] = body["title"], body["body"]
			_ = json.NewEncoder(w).Encode(g.pull)
		case r.Method == "GET" && r.URL.Path == "/repos/acme/claims/pulls/7":
			_ = json.NewEncoder(w).Encode(g.pull)
		case r.Method == "POST" && r.URL.Path == "/repos/acme/claims/issues/7/comments":
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func TestGitHubPullRequests(t *testing.T) {
	w := newWorld(t)
	hub := newFakeGitHub(t)
	w.s.githubAPI = hub.srv.URL
	w.repository("claims", map[string]any{"kind": "github", "url": "https://github.com/acme/claims.git", "credential": "read-write", "token": secret, "approve_publish": false})
	id := w.devpod(map[string]any{"repository": "claims", "source_branch": "main", "work_branch": "agent/payment-date", "run": "run-1"})
	git := func(name string, args map[string]any) (bool, any, string) {
		return w.tool(id, "run-1", builder, name, args)
	}

	if ok, _, msg := git("create_pull_request", map[string]any{"title": "Payment date", "body": "B"}); ok || !strings.Contains(msg, "Call push_branch first") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	if ok, _, msg := git("push_branch", nil); !ok {
		t.Fatal(msg)
	}
	ok, result, msg := git("create_pull_request", map[string]any{"title": "Payment date", "body": "Shows the payment date."})
	if !ok {
		t.Fatal(msg)
	}
	if out := result.(map[string]any); out["url"] != "https://github.com/acme/claims/pull/7" || out["number"] != 7.0 || out["pull_request"] != "runs/run-1/pull-request.json" || out["patch"] != nil {
		t.Fatalf("result = %v", out)
	}
	if want := `POST /repos/acme/claims/pulls {"base":"main","body":"Shows the payment date.","head":"agent/payment-date","title":"Payment date"}`; hub.calls[0] != want {
		t.Fatalf("call = %s\nwant   %s", hub.calls[0], want)
	}
	// The record names the pull request; a patch is not stored for GitHub.
	mustContain(t, w.blobs.get("runs/run-1/pull-request.json"), `"url": "https://github.com/acme/claims/pull/7"`, `"number": 7`, `"pushed": true`, `"kind": "github"`)
	if strings.Contains(w.blobs.all(), "pull-request.patch") {
		t.Fatal("a patch was stored for a GitHub repository")
	}
	if ok, _, msg = git("update_pull_request", map[string]any{"title": "Show the payment date"}); !ok {
		t.Fatal(msg)
	}
	if ok, _, msg = git("comment_on_pull_request", map[string]any{"body": "Checks pass."}); !ok {
		t.Fatal(msg)
	}
	ok, result, msg = git("get_pull_request", nil)
	if out := result.(map[string]any); !ok || out["state"] != "open" || out["title"] != "Show the payment date" || out["body"] != "Shows the payment date." || out["merged"] != false || out["number"] != 7.0 {
		t.Fatalf("ok %v: %v %s", ok, result, msg)
	}
	want := []string{
		`PATCH /repos/acme/claims/pulls/7 {"body":"Shows the payment date.","title":"Show the payment date"}`,
		`POST /repos/acme/claims/issues/7/comments {"body":"Checks pass."}`,
		`GET /repos/acme/claims/pulls/7 `,
	}
	if !reflect.DeepEqual(hub.calls[1:], want) {
		t.Fatalf("calls = %q", hub.calls[1:])
	}

	// GitHub's refusal reaches the agent with GitHub's words.
	taken := w.devpod(map[string]any{"repository": "claims", "work_branch": "agent/taken", "run": "run-2"})
	w.tool(taken, "run-2", builder, "push_branch", nil)
	if ok, _, msg = w.tool(taken, "run-2", builder, "create_pull_request", map[string]any{"title": "T", "body": "B"}); ok {
		t.Fatal("GitHub's refusal was reported as done")
	} else {
		mustContain(t, msg, "create_pull_request failed", "422", "A pull request already exists")
	}
}

func TestGitHubWithApprovalPushesAndOpensThePullRequest(t *testing.T) {
	w := newWorld(t)
	hub := newFakeGitHub(t)
	w.s.githubAPI = hub.srv.URL
	w.repository("claims", map[string]any{"kind": "github", "url": "https://github.com/acme/claims", "credential": "read-write", "token": secret})
	id := w.devpod(map[string]any{"repository": "claims", "work_branch": "agent/payment-date", "run": "run-1"})
	if ok, result, msg := w.tool(id, "run-1", builder, "create_pull_request", map[string]any{"title": "Payment date", "body": "B"}); !ok || result != awaitingApproval {
		t.Fatalf("ok %v: %v %s", ok, result, msg)
	}
	if len(hub.calls) != 0 || w.pod.count("push") != 0 {
		t.Fatal("GitHub was called before the approval")
	}
	status, out := w.call("POST", "/api/devpods/"+id+"/publish-requests/1/decision", map[string]any{"decision": "approved", "decided_by": "flo"})
	if status != 200 || out["state"] != "done" || out["result"] != "Pull request created: https://github.com/acme/claims/pull/7" || w.pod.count("push:agent/payment-date") != 1 {
		t.Fatalf("status %d: %v", status, out)
	}

	// Without a credential that may write, nothing can go to GitHub.
	w.repository("readonly", map[string]any{"kind": "github", "url": "https://github.com/acme/claims", "credential": "read", "token": secret})
	ro := w.devpod(map[string]any{"repository": "readonly", "work_branch": "agent/x", "run": "run-3"})
	for _, tool := range []string{"push_branch", "create_pull_request"} {
		if ok, _, msg := w.tool(ro, "run-3", builder, tool, map[string]any{"title": "T", "body": "B"}); ok || !strings.Contains(msg, "may only read") || !strings.Contains(msg, "report_blocked") {
			t.Fatalf("%s: ok %v: %s", tool, ok, msg)
		}
	}
}

func TestTheMCPServiceIsRegisteredWhenMissing(t *testing.T) {
	w := newWorld(t)
	var mu sync.Mutex
	var stored []byte
	requests, failures := []string{}, 2
	gateway := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case failures > 0:
			failures--
			rw.WriteHeader(503)
		case r.Method == "GET" && stored == nil:
			rw.WriteHeader(404)
		case r.Method == "GET":
			_, _ = rw.Write(stored)
		case r.Method == "PUT":
			stored, _ = io.ReadAll(r.Body)
			_, _ = rw.Write(stored)
		}
	}))
	defer gateway.Close()
	w.s.mcpGateway = gateway.URL
	w.s.registerUntilDone(context.Background())
	var config struct {
		MCPConfig struct{ Transport, URL string }
		Allowlist []string
		Denylist  []string
	}
	if err := json.Unmarshal(stored, &config); err != nil {
		t.Fatalf("stored %q: %v", stored, err)
	}
	wantTools := []string{
		"list_files", "read_file", "write_file", "edit_file", "search", "run_command", "report_blocked",
		"push_branch", "create_pull_request", "update_pull_request", "get_pull_request", "comment_on_pull_request",
	}
	if config.MCPConfig.Transport != "streamable-http" || config.MCPConfig.URL != "http://tool-gateway:8080/mcp" || !reflect.DeepEqual(config.Allowlist, wantTools) || config.Denylist == nil || len(config.Denylist) != 0 {
		t.Fatalf("config = %+v", config)
	}
	if want := []string{"GET /api/services/devpod", "GET /api/services/devpod", "GET /api/services/devpod", "PUT /api/services/devpod"}; !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %v", requests)
	}
	// An entry that exists is left as it is.
	stored = []byte(`{"mcpconfig":{"transport":"streamable-http","url":"http://elsewhere/mcp"},"allowlist":["read_file"],"denylist":[]}`)
	if created, err := w.s.register(context.Background()); err != nil || created || !strings.Contains(string(stored), "elsewhere") || len(requests) != 5 {
		t.Fatalf("created %v, err %v, requests %v", created, err, requests)
	}
}

func TestARestartLosesNothing(t *testing.T) {
	w := newWorld(t)
	w.repository("claims-build", map[string]any{"approve_publish": true})
	id := w.devpod(map[string]any{
		"repository": "claims-build", "work_branch": "agent/a", "run": "run-1", "name": "shared",
		"protected": []string{"verify.sh"}, "rules": []map[string]any{{"tool": "run_command", "matches": "push", "message": "No pushing."}},
	})
	w.tool(id, "run-1", builder, "push_branch", nil)
	w.tool(id, "run-1", builder, "report_blocked", map[string]any{"reason": "Waiting."})
	w.advance(7 * time.Minute)
	w.tool(id, "run-1", builder, "read_file", map[string]any{"path": "a.txt"})
	before, _ := w.s.pod(id)

	registry := w.s.registry
	w.s = w.start(registry)
	w.handler = w.s.routes()
	after, ok := w.s.pod(id)
	if !ok || !reflect.DeepEqual(before, after) {
		t.Fatalf("after the restart:\n%+v\nbefore:\n%+v", after, before)
	}
	if after.LastActivity.Sub(after.Created) != 7*time.Minute || len(after.PublishRequests) != 1 || len(after.Blocked) != 1 || len(after.Holds) != 1 {
		t.Fatalf("record = %+v", after)
	}
	// The pod still works, with its rules, and is still found by its name.
	if ok, _, msg := w.tool(id, "run-1", builder, "write_file", map[string]any{"path": "verify.sh", "content": "x"}); ok || !strings.Contains(msg, "protected") {
		t.Fatalf("ok %v: %s", ok, msg)
	}
	if ok, _, msg := w.tool(id, "run-1", builder, "run_command", map[string]any{"command": "git status"}); !ok {
		t.Fatal(msg)
	}
	if status, out := w.call("POST", "/api/devpods", map[string]any{"repository": "claims-build", "work_branch": "agent/b", "run": "run-2", "name": "shared"}); status != 200 || out["id"] != id || out["reused"] != true {
		t.Fatalf("status %d: %v", status, out)
	}
	if w.pod.count("denied") != 0 {
		t.Fatalf("a call used the wrong token: %v", w.pod.called())
	}
}

func TestTheAPIWaitsUntilThePodsAreRead(t *testing.T) {
	w := newWorld(t)
	w.s.mu.Lock()
	w.s.loaded = false
	w.s.mu.Unlock()
	if status, _ := w.call("GET", "/api/devpods", nil); status != 503 {
		t.Fatalf("status %d", status)
	}
	if status, _ := w.call("GET", "/healthz", nil); status != 200 {
		t.Fatalf("healthz: status %d", status)
	}
}
