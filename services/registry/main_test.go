package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBlobstore implements the parts of the blobstore API the registry uses.
type fakeBlobstore struct {
	mu   sync.Mutex
	data map[string][]byte
	meta map[string]objectMeta
}

func newFakeBlobstore() *fakeBlobstore {
	return &fakeBlobstore{data: map[string][]byte{}, meta: map[string]objectMeta{}}
}

func (f *fakeBlobstore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/blobs/"):
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		body, _ := io.ReadAll(r.Body)
		f.data[id] = body
		f.meta[id] = objectMeta{ID: id, Size: int64(len(body)), Created: time.Now().UTC(), Creator: r.Header.Get("X-Creator")}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/blobs/"):
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		if _, ok := f.data[id]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.data, id)
		delete(f.meta, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/blobs/"):
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		data, ok := f.data[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Creator", f.meta[id].Creator)
		w.Header().Set("X-Created", f.meta[id].Created.Format(time.RFC3339Nano))
		_, _ = w.Write(data)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/list/"):
		prefix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/list/"), "/")
		dirs, objects := map[string]bool{}, []objectMeta{}
		found := prefix == ""
		for id, m := range f.meta {
			rest := id
			if prefix != "" {
				if !strings.HasPrefix(id, prefix+"/") {
					continue
				}
				rest = strings.TrimPrefix(id, prefix+"/")
			}
			found = true
			if i := strings.Index(rest, "/"); i >= 0 {
				dirs[rest[:i]] = true
			} else {
				objects = append(objects, m)
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		l := listing{Prefix: prefix, Directories: []string{}, Objects: objects}
		for d := range dirs {
			l.Directories = append(l.Directories, d)
		}
		sort.Strings(l.Directories)
		sort.Slice(l.Objects, func(i, j int) bool { return l.Objects[i].ID < l.Objects[j].ID })
		_ = json.NewEncoder(w).Encode(l)
	default:
		http.NotFound(w, r)
	}
}

func newTestRegistry(t *testing.T, seed string) (*httptest.Server, *fakeBlobstore) {
	t.Helper()
	blobs := newFakeBlobstore()
	blobSrv := httptest.NewServer(blobs)
	t.Cleanup(blobSrv.Close)
	reg := &registry{prefix: "agents", files: []string{"systemprompt.md", "agent.json"}, blobs: blobClient{base: blobSrv.URL, client: http.DefaultClient}, logger: slog.New(slog.DiscardHandler)}
	if seed != "" {
		if err := reg.seedWithRetry(t.Context(), seed, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(reg.routes())
	t.Cleanup(srv.Close)
	return srv, blobs
}

func newTestRegistryWithPolicies(t *testing.T, policies http.Handler) (*httptest.Server, *fakeBlobstore) {
	t.Helper()
	blobs := newFakeBlobstore()
	blobSrv := httptest.NewServer(blobs)
	policySrv := httptest.NewServer(policies)
	t.Cleanup(blobSrv.Close)
	t.Cleanup(policySrv.Close)
	reg := &registry{
		prefix: "agents", files: []string{"systemprompt.md", "agent.json"},
		blobs:       blobClient{base: blobSrv.URL, client: http.DefaultClient},
		policiesURL: policySrv.URL, http: policySrv.Client(), logger: slog.New(slog.DiscardHandler),
	}
	srv := httptest.NewServer(reg.routes())
	t.Cleanup(srv.Close)
	return srv, blobs
}

func call(t *testing.T, method, url, body string, headers map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func TestCreateListAndReadVersions(t *testing.T) {
	srv, blobs := newTestRegistry(t, "")
	if code, body := call(t, http.MethodGet, srv.URL+"/agents", "", nil); code != http.StatusOK || body != "{\"entries\":[]}\n" {
		t.Fatalf("empty registry %d %s", code, body)
	}
	first := `{"files":{"systemprompt.md":"# Intake\nBe brief.","agent.json":{"unpack_inputs":true,"models":["m"],"tools":[]}}}`
	code, body := call(t, http.MethodPost, srv.URL+"/agents/intake", first, map[string]string{"X-Creator": "flo"})
	if code != http.StatusCreated || !strings.Contains(body, `"version":"v1"`) {
		t.Fatalf("create %d %s", code, body)
	}
	if string(blobs.data["agents/intake/v1/systemprompt.md"]) != "# Intake\nBe brief." || blobs.meta["agents/intake/v1/systemprompt.md"].Creator != "flo" {
		t.Fatalf("stored %q %+v", blobs.data["agents/intake/v1/systemprompt.md"], blobs.meta["agents/intake/v1/systemprompt.md"])
	}
	if !strings.Contains(string(blobs.data["agents/intake/v1/agent.json"]), "\"models\": [") {
		t.Fatalf("json not indented: %q", blobs.data["agents/intake/v1/agent.json"])
	}
	second := `{"files":{"systemprompt.md":"# Intake v2","agent.json":{"unpack_inputs":true,"models":["m2"]}}}`
	if code, body = call(t, http.MethodPost, srv.URL+"/agents/intake", second, nil); code != http.StatusCreated || !strings.Contains(body, `"version":"v2"`) {
		t.Fatalf("second version %d %s", code, body)
	}
	if _, ok := blobs.data["agents/intake/v1/systemprompt.md"]; !ok {
		t.Fatal("v1 was removed")
	}

	code, body = call(t, http.MethodGet, srv.URL+"/agents", "", nil)
	var entries struct{ Entries []Entry }
	_ = json.Unmarshal([]byte(body), &entries)
	if code != http.StatusOK || len(entries.Entries) != 1 || entries.Entries[0].Latest != "v2" || strings.Join(entries.Entries[0].Versions, ",") != "v1,v2" {
		t.Fatalf("entries %d %s", code, body)
	}
	code, body = call(t, http.MethodGet, srv.URL+"/agents/intake", "", nil)
	if code != http.StatusOK || !strings.Contains(body, `"creator":"flo"`) || !strings.Contains(body, `"version":"v2"`) {
		t.Fatalf("versions %d %s", code, body)
	}
	code, body = call(t, http.MethodGet, srv.URL+"/agents/intake/latest", "", nil)
	var c Content
	_ = json.Unmarshal([]byte(body), &c)
	var md string
	_ = json.Unmarshal(c.Files["systemprompt.md"], &md)
	if code != http.StatusOK || c.Version != "v2" || md != "# Intake v2" || !strings.Contains(string(c.Files["agent.json"]), `"m2"`) || c.Creator != "unknown" {
		t.Fatalf("latest %d %s", code, body)
	}
	if code, _ = call(t, http.MethodGet, srv.URL+"/agents/intake/v1", "", nil); code != http.StatusOK {
		t.Fatalf("v1 %d", code)
	}
	if code, _ = call(t, http.MethodGet, srv.URL+"/agents/intake/v9", "", nil); code != http.StatusNotFound {
		t.Fatalf("missing version %d", code)
	}
	if code, _ = call(t, http.MethodGet, srv.URL+"/agents/nobody", "", nil); code != http.StatusNotFound {
		t.Fatalf("missing entry %d", code)
	}
}

func TestAgentPolicyReferencesAreValidatedBeforeVersionWrite(t *testing.T) {
	var requests []string
	policies := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/policies/security/review/latest":
			writeJSON(w, http.StatusOK, map[string]any{"version": "v3", "files": map[string]string{"policy.md": "Never expose secrets."}})
		case "/policies/unavailable/latest":
			http.Error(w, "down", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	})
	srv, blobs := newTestRegistryWithPolicies(t, policies)
	body := func(prompt string) string {
		data, _ := json.Marshal(map[string]any{"files": map[string]any{"systemprompt.md": prompt, "agent.json": map[string]any{"models": []string{"m"}, "unpack_inputs": true}}})
		return string(data)
	}
	stored := "Before\n{{policy::security/review}}\n{{policy::security/review}}\nAfter"
	code, response := call(t, http.MethodPost, srv.URL+"/agents/test/policy", body(stored), nil)
	if code != http.StatusCreated || len(requests) != 1 || requests[0] != "/policies/security/review/latest" {
		t.Fatalf("valid policy create = %d %s requests=%v", code, response, requests)
	}
	if got := string(blobs.data["agents/test/policy/v1/systemprompt.md"]); got != stored {
		t.Fatalf("stored expanded prompt %q", got)
	}
	writes := len(blobs.data)
	for _, test := range []struct {
		prompt string
		status int
		want   string
	}{
		{"{{policy::missing}}", http.StatusUnprocessableEntity, `required policy \"missing\" does not exist`},
		{"{{policy::unavailable}}", http.StatusBadGateway, "cannot validate required policies"},
		{"{{policy::Bad ID}}", http.StatusUnprocessableEntity, "malformed policy reference"},
		{"{{policy::missing", http.StatusUnprocessableEntity, "malformed policy reference"},
	} {
		code, response = call(t, http.MethodPost, srv.URL+"/agents/test/rejected", body(test.prompt), nil)
		if code != test.status || !strings.Contains(response, test.want) {
			t.Errorf("prompt %q = %d %s, want %d containing %q", test.prompt, code, response, test.status, test.want)
		}
		if len(blobs.data) != writes {
			t.Fatalf("rejected prompt wrote a version: %v", blobs.data)
		}
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	srv, _ := newTestRegistry(t, "")
	cases := map[string]string{
		"missing file":  `{"files":{"systemprompt.md":"x"}}`,
		"json not obj":  `{"files":{"systemprompt.md":"x","agent.json":[1]}}`,
		"md not string": `{"files":{"systemprompt.md":{"a":1},"agent.json":{}}}`,
		"extra file":    `{"files":{"systemprompt.md":"x","agent.json":{},"other.txt":"y"}}`,
	}
	for name, body := range cases {
		if code, _ := call(t, http.MethodPost, srv.URL+"/agents/intake", body, nil); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := call(t, http.MethodPost, srv.URL+"/agents/Bad_Name", `{"files":{}}`, nil); code != http.StatusBadRequest {
		t.Errorf("bad name accepted: %d", code)
	}
	if code, _ := call(t, http.MethodPost, srv.URL+"/agents/intake", `{"files":{},"extra":1}`, nil); code != http.StatusBadRequest {
		t.Errorf("unknown field accepted: %d", code)
	}
	// "one" is not a version, so intake/one names an entry in folder intake, which does not exist.
	if code, _ := call(t, http.MethodGet, srv.URL+"/agents/intake/one", "", nil); code != http.StatusNotFound {
		t.Errorf("unknown nested entry: %d", code)
	}
}

func TestCreateRequiresUnpackInputs(t *testing.T) {
	srv, blobs := newTestRegistry(t, "")
	post := func(agentJSON string) (int, string) {
		return call(t, http.MethodPost, srv.URL+"/agents/intake", `{"files":{"systemprompt.md":"x","agent.json":`+agentJSON+`}}`, nil)
	}
	for name, test := range map[string]struct{ agentJSON, want string }{
		"missing": {`{"models":["m"]}`, "agent.json: unpack_inputs is required and must be true or false"},
		"string":  {`{"models":["m"],"unpack_inputs":"true"}`, `agent.json: unpack_inputs must be true or false, got \"true\"`},
		"number":  {`{"models":["m"],"unpack_inputs":1}`, "agent.json: unpack_inputs must be true or false, got 1"},
		"null":    {`{"models":["m"],"unpack_inputs":null}`, "agent.json: unpack_inputs must be true or false, got null"},
	} {
		if code, body := post(test.agentJSON); code != http.StatusUnprocessableEntity || !strings.Contains(body, test.want) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if len(blobs.data) != 0 {
		t.Fatalf("a refused version was stored: %v", blobs.data)
	}
	for i, agentJSON := range []string{`{"models":["m"],"unpack_inputs":true}`, `{"models":["m"],"unpack_inputs":false}`} {
		if code, body := post(agentJSON); code != http.StatusCreated {
			t.Fatalf("%s: %d %s", agentJSON, code, body)
		}
		want := []string{`"unpack_inputs": true`, `"unpack_inputs": false`}[i]
		if stored := string(blobs.data["agents/intake/v"+strconv.Itoa(i+1)+"/agent.json"]); !strings.Contains(stored, want) {
			t.Fatalf("the setting was not stored as given: %q", stored)
		}
	}
}

func TestCreateValidatesModelSettings(t *testing.T) {
	srv, blobs := newTestRegistry(t, "")
	post := func(agentJSON string) (int, string) {
		return call(t, http.MethodPost, srv.URL+"/agents/intake", `{"files":{"systemprompt.md":"x","agent.json":`+agentJSON+`}}`, nil)
	}
	for name, test := range map[string]struct{ agentJSON, want string }{
		"temperature string":   {`{"unpack_inputs":true,"models":["m"],"temperature":"0.2"}`, "temperature must be a number from 0 to 2"},
		"temperature bool":     {`{"unpack_inputs":true,"models":["m"],"temperature":true}`, "temperature must be a number from 0 to 2"},
		"temperature null":     {`{"unpack_inputs":true,"models":["m"],"temperature":null}`, "temperature must be a number from 0 to 2"},
		"temperature negative": {`{"unpack_inputs":true,"models":["m"],"temperature":-0.1}`, "temperature must be a number from 0 to 2"},
		"temperature too high": {`{"unpack_inputs":true,"models":["m"],"temperature":2.01}`, "temperature must be a number from 0 to 2"},
	} {
		if code, body := post(test.agentJSON); code != http.StatusUnprocessableEntity || !strings.Contains(body, test.want) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if len(blobs.data) != 0 {
		t.Fatalf("a refused version was stored: %v", blobs.data)
	}
	if code, body := post(`{"unpack_inputs":true,"models":["m"],"temperature":0.2}`); code != http.StatusCreated {
		t.Fatalf("valid settings: %d %s", code, body)
	}
	if stored := string(blobs.data["agents/intake/v1/agent.json"]); !strings.Contains(stored, `"temperature": 0.2`) {
		t.Fatalf("the temperature was not stored as given: %q", stored)
	}
	for name, agentJSON := range map[string]string{
		"lower bound": `{"unpack_inputs":true,"models":["m"],"temperature":0}`,
		"upper bound": `{"unpack_inputs":true,"models":["m"],"temperature":2}`,
		"neither":     `{"unpack_inputs":true,"models":["m"]}`,
	} {
		if code, body := post(agentJSON); code != http.StatusCreated {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}

func TestCreateValidatesCommands(t *testing.T) {
	srv, blobs := newTestRegistry(t, "")
	post := func(agentJSON string) (int, string) {
		return call(t, http.MethodPost, srv.URL+"/agents/intake", `{"files":{"systemprompt.md":"x","agent.json":`+agentJSON+`}}`, nil)
	}
	for name, commands := range map[string]string{
		"string":       `"git status"`,
		"null":         `null`,
		"object":       `{"a":"git status"}`,
		"number item":  `["git status", 3]`,
		"null item":    `["git status", null]`,
		"empty item":   `["git status", ""]`,
		"blank item":   `["  "]`,
		"nested lists": `[["git status"]]`,
	} {
		code, body := post(`{"unpack_inputs":true,"models":["m"],"commands":` + commands + `}`)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "agent.json: commands must be a list of non-empty strings, got") {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if len(blobs.data) != 0 {
		t.Fatalf("a refused version was stored: %v", blobs.data)
	}
	if code, body := post(`{"unpack_inputs":true,"models":["m"],"commands":["./verify.sh","git commit *"]}`); code != http.StatusCreated {
		t.Fatalf("valid commands: %d %s", code, body)
	}
	if stored := string(blobs.data["agents/intake/v1/agent.json"]); !strings.Contains(stored, `"git commit *"`) {
		t.Fatalf("the commands were not stored as given: %q", stored)
	}
	for name, agentJSON := range map[string]string{
		"empty list": `{"unpack_inputs":true,"models":["m"],"commands":[]}`,
		"missing":    `{"unpack_inputs":true,"models":["m"]}`,
	} {
		if code, body := post(agentJSON); code != http.StatusCreated {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}

func TestSeedStoresMissingVersionsOnly(t *testing.T) {
	seed := t.TempDir()
	for _, v := range []string{"v1", "v2"} {
		dir := filepath.Join(seed, "intake", v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, "systemprompt.md"), []byte("prompt "+v), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "agent.json"), []byte(`{"models":["m"]}`), 0o644)
	}
	srv, blobs := newTestRegistry(t, seed)
	if string(blobs.data["agents/intake/v2/systemprompt.md"]) != "prompt v2" || blobs.meta["agents/intake/v1/systemprompt.md"].Creator != "seed" {
		t.Fatalf("seed not stored: %v", blobs.data)
	}
	if code, body := call(t, http.MethodGet, srv.URL+"/agents", "", nil); code != http.StatusOK || !strings.Contains(body, `"latest":"v2"`) {
		t.Fatalf("entries %d %s", code, body)
	}
}

func TestVersionNumber(t *testing.T) {
	for v, want := range map[string]int{"v1": 1, "v12": 12} {
		if n, ok := versionNumber(v); !ok || n != want {
			t.Errorf("%s: %d %v", v, n, ok)
		}
	}
	for _, v := range []string{"1", "v0", "v01", "va", "latest", ""} {
		if _, ok := versionNumber(v); ok {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestDeleteEntryRemovesEveryVersion(t *testing.T) {
	srv, blobs := newTestRegistry(t, "")
	body := `{"files":{"systemprompt.md":"x","agent.json":{"a":1,"unpack_inputs":true}}}`
	for i := 0; i < 2; i++ {
		if code, _ := call(t, http.MethodPost, srv.URL+"/agents/demo", body, nil); code != http.StatusCreated {
			t.Fatalf("create %d", code)
		}
	}
	if code, _ := call(t, http.MethodDelete, srv.URL+"/agents/demo", "", nil); code != http.StatusNoContent {
		t.Fatalf("delete %d", code)
	}
	if len(blobs.data) != 0 {
		t.Fatalf("objects left: %v", blobs.data)
	}
	if code, _ := call(t, http.MethodGet, srv.URL+"/agents/demo", "", nil); code != http.StatusNotFound {
		t.Fatalf("after delete %d", code)
	}
	if code, _ := call(t, http.MethodDelete, srv.URL+"/agents/demo", "", nil); code != http.StatusNotFound {
		t.Fatalf("delete missing %d", code)
	}
}

func TestDeleteVersionRemovesOnlyThatVersion(t *testing.T) {
	srv, blobs := newTestRegistry(t, "")
	body := `{"files":{"systemprompt.md":"x","agent.json":{"a":1,"unpack_inputs":true}}}`
	for _, name := range []string{"intake/triage", "intake/triage", "intake/intake"} {
		if code, _ := call(t, http.MethodPost, srv.URL+"/agents/"+name, body, nil); code != http.StatusCreated {
			t.Fatalf("create %s: %d", name, code)
		}
	}
	if code, _ := call(t, http.MethodDelete, srv.URL+"/agents/intake/triage/v1", "", nil); code != http.StatusNoContent {
		t.Fatalf("delete v1: %d", code)
	}
	if _, ok := blobs.data["agents/intake/triage/v1/systemprompt.md"]; ok {
		t.Fatal("v1 is still stored")
	}
	if _, ok := blobs.data["agents/intake/triage/v2/systemprompt.md"]; !ok {
		t.Fatal("v2 was deleted with v1")
	}
	if _, ok := blobs.data["agents/intake/intake/v1/systemprompt.md"]; !ok {
		t.Fatal("another entry of the group was deleted")
	}
	if code, _ := call(t, http.MethodDelete, srv.URL+"/agents/intake/triage/v9", "", nil); code != http.StatusNotFound {
		t.Fatalf("delete missing version: %d", code)
	}
	if code, _ := call(t, http.MethodDelete, srv.URL+"/agents/intake/triage/v2", "", nil); code != http.StatusNoContent {
		t.Fatalf("delete last version: %d", code)
	}
	if code, _ := call(t, http.MethodGet, srv.URL+"/agents/intake/triage", "", nil); code != http.StatusNotFound {
		t.Fatalf("entry without versions: %d", code)
	}
}

func TestFoldersGroupEntries(t *testing.T) {
	srv, blobs := newTestRegistry(t, "")
	body := `{"files":{"systemprompt.md":"x","agent.json":{"a":1,"unpack_inputs":true}}}`
	if code, b := call(t, http.MethodPost, srv.URL+"/agents/translation/en-2-de", body, nil); code != http.StatusCreated || !strings.Contains(b, `"name":"translation/en-2-de"`) {
		t.Fatalf("create in folder %d %s", code, b)
	}
	if _, ok := blobs.data["agents/translation/en-2-de/v1/systemprompt.md"]; !ok {
		t.Fatal("not stored under the folder")
	}
	if code, _ := call(t, http.MethodPost, srv.URL+"/agents/solo", body, nil); code != http.StatusCreated {
		t.Fatalf("create top level %d", code)
	}
	code, b := call(t, http.MethodGet, srv.URL+"/agents", "", nil)
	if code != http.StatusOK || !strings.Contains(b, `"name":"solo"`) || !strings.Contains(b, `"name":"translation/en-2-de"`) || strings.Contains(b, `"name":"translation"`) {
		t.Fatalf("list %d %s", code, b)
	}
	if code, b := call(t, http.MethodGet, srv.URL+"/agents/translation/en-2-de/latest", "", nil); code != http.StatusOK || !strings.Contains(b, `"version":"v1"`) {
		t.Fatalf("get latest %d %s", code, b)
	}
	if code, b := call(t, http.MethodGet, srv.URL+"/agents/translation/en-2-de", "", nil); code != http.StatusOK || !strings.Contains(b, `"versions"`) {
		t.Fatalf("versions %d %s", code, b)
	}
	// A folder is not an entry, and an entry cannot become a folder.
	if code, _ := call(t, http.MethodPost, srv.URL+"/agents/translation", body, nil); code != http.StatusConflict {
		t.Fatalf("entry over folder %d", code)
	}
	if code, _ := call(t, http.MethodPost, srv.URL+"/agents/solo/child", body, nil); code != http.StatusConflict {
		t.Fatalf("folder under entry %d", code)
	}
	for _, bad := range []string{"/agents/a/v2/b", "/agents/Upper", "/agents/a/b/c/d/e"} {
		if code, _ := call(t, http.MethodPost, srv.URL+bad, body, nil); code != http.StatusBadRequest {
			t.Errorf("POST %s: %d, want 400", bad, code)
		}
	}
	if code, _ := call(t, http.MethodDelete, srv.URL+"/agents/translation/en-2-de", "", nil); code != http.StatusNoContent {
		t.Fatalf("delete in folder %d", code)
	}
}
