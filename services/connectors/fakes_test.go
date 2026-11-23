package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testNamespace = "opensoftwarefactory-devpods"
	// fakeStartCommit is the commit the fake git-helper reports for every clone.
	fakeStartCommit = "5ta27c0"
)

// fakeKube is a Kubernetes API server that knows pods, secrets and network policies of one
// namespace. A created pod runs at once and has an address.
type fakeKube struct {
	t        *testing.T
	pods     map[string]map[string]any
	secrets  map[string]map[string]any
	policies map[string]map[string]any
	srv      *httptest.Server
	denied   []string // requests outside the namespace or without the token
	mu       sync.Mutex
}

func newFakeKube(t *testing.T) *fakeKube {
	k := &fakeKube{t: t, pods: map[string]map[string]any{}, secrets: map[string]map[string]any{}, policies: map[string]map[string]any{}}
	k.srv = httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *fakeKube) serve(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var kind, name string
	core, apis := "/api/v1/namespaces/"+testNamespace+"/", "/apis/networking.k8s.io/v1/namespaces/"+testNamespace+"/"
	switch {
	case strings.HasPrefix(r.URL.Path, core):
		kind, name, _ = strings.Cut(strings.TrimPrefix(r.URL.Path, core), "/")
	case strings.HasPrefix(r.URL.Path, apis):
		kind, name, _ = strings.Cut(strings.TrimPrefix(r.URL.Path, apis), "/")
	}
	store := map[string]map[string]map[string]any{"pods": k.pods, "secrets": k.secrets, "networkpolicies": k.policies}[kind]
	if store == nil || r.Header.Get("Authorization") != "Bearer test-token" {
		k.denied = append(k.denied, r.Method+" "+r.URL.Path)
		w.WriteHeader(403)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "forbidden"})
		return
	}
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	missing := func() {
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": kind + " \"" + name + "\" not found"})
	}
	switch {
	case r.Method == http.MethodPost && name == "":
		metadata, _ := body["metadata"].(map[string]any)
		created, named := metadata["name"].(string)
		if !named {
			k.t.Errorf("%s %s: the object has no metadata.name: %v", r.Method, r.URL.Path, body)
			w.WriteHeader(422)
			return
		}
		if _, exists := store[created]; exists {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "already exists"})
			return
		}
		if kind == "pods" {
			body["status"] = map[string]any{"phase": "Running", "podIP": fmt.Sprintf("10.9.0.%d", len(store)+1)}
		}
		store[created] = body
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(body)
	case r.Method == http.MethodPut && name != "":
		if _, exists := store[name]; !exists {
			missing()
			return
		}
		store[name] = body
		_ = json.NewEncoder(w).Encode(body)
	case r.Method == http.MethodGet && name == "":
		items := []map[string]any{}
		want := r.URL.Query().Get("labelSelector")
		for _, object := range store {
			labels, _ := object["metadata"].(map[string]any)["labels"].(map[string]any)
			key, value, _ := strings.Cut(want, "=")
			if want == "" || labels[key] == value {
				items = append(items, object)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodGet:
		object, exists := store[name]
		if !exists {
			missing()
			return
		}
		_ = json.NewEncoder(w).Encode(object)
	case r.Method == http.MethodDelete && name != "":
		if _, exists := store[name]; !exists {
			missing()
			return
		}
		delete(store, name)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "Success"})
	default:
		w.WriteHeader(405)
	}
}

func (k *fakeKube) names(store map[string]map[string]any) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	names := []string{}
	for name := range store {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (k *fakeKube) secret(name, key string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	secret, ok := k.secrets[name]
	if !ok {
		return ""
	}
	value, _ := secret["data"].(map[string]any)[key].(string)
	decoded, _ := base64.StdEncoding.DecodeString(value)
	return string(decoded)
}

// tokens returns the value of a key in every token secret of a dev pod.
func (k *fakeKube) tokens(key string) map[string]bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	found := map[string]bool{}
	for name, secret := range k.secrets {
		if !strings.HasPrefix(name, "devpod-") {
			continue
		}
		value, _ := secret["data"].(map[string]any)[key].(string)
		decoded, _ := base64.StdEncoding.DecodeString(value)
		found[string(decoded)] = true
	}
	return found
}

// fakePod plays both servers of every dev pod: the tool server and git-helper.
type fakePod struct {
	t         *testing.T
	kube      *fakeKube
	bodies    map[string]map[string]any
	exitCodes map[string]int // exit code per command
	srv       *httptest.Server
	calls     []string         // every call, in order
	ops       []map[string]any // the arguments of every tool server operation
	commits   []commit
	mu        sync.Mutex
	failClone bool
	failPush  bool
}

func newFakePod(t *testing.T, kube *fakeKube) *fakePod {
	p := &fakePod{
		t: t, kube: kube, bodies: map[string]map[string]any{}, exitCodes: map[string]int{},
		commits: []commit{{Hash: "c0ffee1", Subject: "Add payment date", Author: "course/builder@v1"}},
	}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePod) serve(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Pod-Token")
	work, helper := p.kube.tokens("work-token")[token], p.kube.tokens("helper-token")[token]
	p.mu.Lock()
	defer p.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	answer := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if r.URL.Path == "/healthz" {
		answer(map[string]bool{"ok": true})
		return
	}
	// The tool server takes the work token only, git-helper the helper token only.
	workPath := r.URL.Path == opPath || r.URL.Path == skillScriptPath
	if (workPath && !work) || (!workPath && !helper) {
		p.calls = append(p.calls, "denied:"+r.URL.Path)
		w.WriteHeader(403)
		return
	}
	p.bodies[r.URL.Path] = body
	switch r.URL.Path {
	case opPath, skillScriptPath:
		name, _ := body["op"].(string)
		args, _ := body["args"].(map[string]any)
		p.ops = append(p.ops, args)
		switch name {
		case toolRunCommand:
			command, _ := args["command"].(string)
			p.calls = append(p.calls, "run:"+command)
			answer(map[string]any{"ok": true, "result": map[string]any{"exit_code": p.exitCodes[command], "stdout": "ran " + command, "stderr": ""}})
		case toolRunSkillScript:
			p.calls = append(p.calls, "run_skill_script:"+fmt.Sprint(args["script"]))
			timedOut := args["script"] == "scripts/timeout.sh"
			cut := args["script"] == "scripts/cut.sh"
			answer(map[string]any{"ok": true, "result": map[string]any{
				"exit_code": 0, "stdout": "skill output",
				"stderr": "skill error", "stdout_cut": cut, "stderr_cut": false, "timed_out": timedOut,
			}})
		case "read_file":
			p.calls = append(p.calls, "read_file:"+fmt.Sprint(args["path"]))
			if args["path"] == "missing.txt" {
				answer(map[string]any{"ok": false, "error": "missing.txt does not exist"})
				return
			}
			answer(map[string]any{"ok": true, "result": map[string]any{"path": args["path"], "content": "hello\n", "start_line": 1, "end_line": 1, "total_lines": 1}})
		case "list_files":
			p.calls = append(p.calls, "list_files:"+fmt.Sprint(args["path"]))
			answer(map[string]any{"ok": true, "result": map[string]any{
				"path": args["path"], "truncated": false,
				"entries": []map[string]any{{"name": "README.md", "type": "file", "size": 12}, {"name": "src", "type": "dir", "size": 0}},
			}})
		default:
			p.calls = append(p.calls, name+":"+fmt.Sprint(args["path"]))
			answer(map[string]any{"ok": true, "result": "done"})
		}
	case "/clone":
		p.calls = append(p.calls, "clone")
		if p.failClone {
			answer(map[string]any{"ok": false, "output": "fatal: repository not found"})
			return
		}
		answer(map[string]any{"ok": true, "start_commit": fakeStartCommit, "output": "Cloning into '/workspace'..."})
	case "/protect":
		p.calls = append(p.calls, "protect")
		answer(map[string]any{"ok": true})
	case "/push":
		p.calls = append(p.calls, "push:"+fmt.Sprint(body["branch"]))
		if p.failPush {
			answer(map[string]any{"ok": false, "output": "remote: Permission denied"})
			return
		}
		answer(map[string]any{"ok": true, "output": "To http://git-host:8080/claims-build.git\n * [new branch]"})
	case "/patch":
		p.calls = append(p.calls, "patch:"+fmt.Sprint(body["base"]))
		patch := ""
		if len(p.commits) > 0 {
			patch = "From c0ffee1 Mon Sep 17 00:00:00 2001\nSubject: [PATCH] Add payment date\n"
		}
		answer(map[string]any{"ok": true, "patch": patch, "commits": p.commits})
	case "/git":
		args := []string{}
		list, isList := body["args"].([]any)
		if !isList {
			p.t.Errorf("/git: args is a %T, not a list", body["args"])
		}
		for _, arg := range list {
			text, isText := arg.(string)
			if !isText {
				p.t.Errorf("/git: the argument %v is a %T, not a string", arg, arg)
			}
			args = append(args, text)
		}
		p.calls = append(p.calls, "git:"+strings.Join(args, " "))
		answer(map[string]any{"ok": true, "output": "output of git " + strings.Join(args, " ")})
	default:
		w.WriteHeader(404)
	}
}

func (p *fakePod) called() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.calls...)
}

func (p *fakePod) count(prefix string) int {
	n := 0
	for _, call := range p.called() {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// fakeBlobs is the blob store: objects by ID, and a listing of a folder's own objects.
type fakeBlobs struct {
	objects map[string][]byte
	srv     *httptest.Server
	mu      sync.Mutex
}

func newFakeBlobs(t *testing.T) *fakeBlobs {
	b := &fakeBlobs{objects: map[string][]byte{}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if prefix, ok := strings.CutPrefix(r.URL.Path, "/list/"); ok {
			objects := []map[string]string{}
			for id := range b.objects {
				if filepath.Dir(id) == prefix {
					objects = append(objects, map[string]string{"id": id})
				}
			}
			if len(objects) == 0 {
				http.Error(w, "prefix not found", 404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"prefix": prefix, "objects": objects})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		switch r.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			b.objects[id] = data
			w.WriteHeader(204)
		case http.MethodGet:
			data, ok := b.objects[id]
			if !ok {
				http.Error(w, "object not found", 404)
				return
			}
			_, _ = w.Write(data)
		case http.MethodDelete:
			delete(b.objects, id)
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBlobs) get(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.objects[id])
}

func (b *fakeBlobs) all() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var all bytes.Buffer
	for id, data := range b.objects {
		all.WriteString(id + "\n")
		all.Write(data)
	}
	return all.String()
}

// world is a connector service with everything around it faked.
type world struct {
	t       *testing.T
	s       *service
	handler http.Handler
	kube    *fakeKube
	pod     *fakePod
	blobs   *fakeBlobs
	clock   time.Time
	clockMu sync.Mutex
}

// agentCommands is what the fake agent registry knows.
var agentCommands = map[string][]string{
	"/agents/course/builder/v1": {"./verify.sh", "python3 -m unittest *", "git status*", "git diff*", "git log*", "git add *", "git commit *", "cat a | wc -l"},
	"/agents/course/reader/v2":  nil,
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, kube: newFakeKube(t), blobs: newFakeBlobs(t), clock: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
	w.pod = newFakePod(t, w.kube)
	registry := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		commands, ok := agentCommands[r.URL.Path]
		if !ok {
			http.Error(rw, "not found", 404)
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"files": map[string]any{"agent.json": map[string]any{"commands": commands}}})
	}))
	t.Cleanup(registry.Close)
	w.s = w.start(registry.URL)
	w.handler = w.s.routes()
	return w
}

// start builds a service on the world's blob store and cluster, as a restart would.
func (w *world) start(registry string) *service {
	tokenFile := filepath.Join(w.t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		w.t.Fatal(err)
	}
	cfg := config{
		podNamespace: testNamespace, factoryNamespace: "opensoftwarefactory", image: "opensoftwarefactory-devpod:local", pullPolicy: "Never",
		sweep: time.Hour, startTimeout: 2 * time.Second, poll: 5 * time.Millisecond, mcpURL: "http://tool-gateway:8080/mcp", registerRetry: 5 * time.Millisecond,
	}
	kube := &kubeClient{base: w.kube.srv.URL, namespace: testNamespace, tokenFile: tokenFile, http: http.DefaultClient}
	s := newService(&cfg, slog.New(slog.DiscardHandler), http.DefaultClient, kube)
	s.blobs = blobClient{base: w.blobs.srv.URL, http: http.DefaultClient}
	s.registry = registry
	s.skillRegistry = registry
	s.podURL = func(string, int) string { return w.pod.srv.URL }
	s.resolve = func(string) ([]string, error) { return []string{"140.82.121.3"}, nil }
	s.now = func() time.Time { w.clockMu.Lock(); defer w.clockMu.Unlock(); return w.clock }
	if err := s.load(context.Background()); err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *world) advance(d time.Duration) {
	w.clockMu.Lock()
	w.clock = w.clock.Add(d)
	w.clockMu.Unlock()
}

// call sends a request to the service and returns the status and the decoded answer.
func (w *world) call(method, path string, body any) (status int, out map[string]any) {
	w.t.Helper()
	status, raw := w.raw(method, path, body)
	if len(bytes.TrimSpace(raw)) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &out); err != nil {
			w.t.Fatalf("%s %s: answer is not JSON: %s", method, path, raw)
		}
	}
	return status, out
}

func (w *world) raw(method, path string, body any) (status int, answer []byte) {
	w.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			w.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, httptest.NewRequest(method, path, reader))
	return rec.Code, rec.Body.Bytes()
}

func (w *world) list(path string) []map[string]any {
	w.t.Helper()
	status, raw := w.raw("GET", path, nil)
	if status != 200 {
		w.t.Fatalf("GET %s: status %d: %s", path, status, raw)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		w.t.Fatalf("GET %s: not a JSON array: %s", path, raw)
	}
	return out
}

// repository stores a repository; fields override the defaults of the demo repository.
func (w *world) repository(name string, fields map[string]any) {
	w.t.Helper()
	body := map[string]any{"kind": "plain", "url": "http://git-host:8080/" + name + ".git", "default_branch": "main"}
	for key, value := range fields {
		body[key] = value
	}
	if status, out := w.call("PUT", "/api/git/repositories/"+name, body); status != 200 {
		w.t.Fatalf("storing repository %s: status %d: %v", name, status, out)
	}
}

// devpod creates a dev pod and returns its ID.
func (w *world) devpod(body map[string]any) string {
	w.t.Helper()
	status, out := w.call("POST", "/api/devpods", body)
	if status != 200 {
		w.t.Fatalf("creating a dev pod: status %d: %v", status, out)
	}
	return as[string](w.t, out["id"])
}

// tool makes a tool call as the tool gateway does and returns ok, the result and the error.
func (w *world) tool(pod, run, agent, name string, args map[string]any) (ok bool, result any, message string) {
	w.t.Helper()
	status, out := w.call("POST", "/api/devpods/"+pod+"/tool", map[string]any{"tool": name, "args": args, "agent": agent, "run": run})
	if status != 200 {
		w.t.Fatalf("tool %s: status %d: %v", name, status, out)
	}
	message, _ = out["error"].(string)
	return as[bool](w.t, out["ok"]), out["result"], message
}

// as returns v as a T and fails the test when v is something else.
func as[T any](t *testing.T, v any) T {
	t.Helper()
	got, ok := v.(T)
	if !ok {
		t.Fatalf("%v is a %T, not a %T", v, v, got)
	}
	return got
}

func mustContain(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Fatalf("%q does not contain %q", text, part)
		}
	}
}
