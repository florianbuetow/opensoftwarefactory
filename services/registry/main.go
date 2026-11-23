// Command registry is a thin, versioned catalog in front of the blobstore.
// One deployment serves agent definitions, another serves policies; only the
// prefix and the file names differ:
//
//	GET    /{prefix}                    every entry with its versions
//	GET    /{prefix}/{name}             the versions of one entry, newest last
//	GET    /{prefix}/{name}/{version}   one version's files ("latest" is accepted)
//	POST   /{prefix}/{name}             store the posted files as the next version
//	DELETE /{prefix}/{name}             delete the entry with every version
//	DELETE /{prefix}/{name}/{version}   delete that one version
//
// A name is a path of up to four folder segments, such as "translation/en-2-de":
// folders group entries. Versions are never overwritten: every change is a new
// version stored as <prefix>/<name>/v<N>/<file> in the blobstore. On startup, the
// versions found in the seed directory are stored if they do not exist yet.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.yaml.in/yaml/v3"
)

const maxBodyBytes = 16 << 20
const retiredSkillVersionPrefix = ".retired-"

// segmentPattern matches one segment of a name; a name is up to maxDepth segments.
var segmentPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const maxDepth = 4

// validName reports whether name is a path of valid segments that cannot be
// mistaken for a version.
func validName(name string) bool {
	parts := strings.Split(name, "/")
	if len(parts) > maxDepth {
		return false
	}
	for _, p := range parts {
		if !segmentPattern.MatchString(p) || p == "latest" {
			return false
		}
		if _, ok := versionNumber(p); ok {
			return false
		}
	}
	return true
}

const badName = "name must be up to four lowercase segments of letters, digits, and dashes, separated by /"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	prefix := flag.String("prefix", "agents", "blobstore prefix and URL path of the entries, such as agents or policies")
	files := flag.String("files", "systemprompt.md,agent.json", "comma-separated files every version must contain")
	openFiles := flag.Bool("open-files", false, "allow additional text files below each version")
	seed := flag.String("seed", "", "directory with <name>/<version>/<files> to store when missing")
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()
	if !segmentPattern.MatchString(*prefix) {
		return fmt.Errorf("prefix %q must be lowercase letters, digits, and dashes", *prefix)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	shutdownTelemetry, err := initTelemetry(context.Background(), env("OTEL_SERVICE_NAME", "registry"))
	if err != nil {
		return fmt.Errorf("initializing telemetry: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(ctx); err != nil {
			logger.Warn("telemetry shutdown failed", "error", err)
		}
	}()
	tracedHTTP := tracedHTTPClient()
	reg := &registry{
		prefix:      *prefix,
		files:       strings.Split(*files, ","),
		openFiles:   *openFiles,
		blobs:       blobClient{base: strings.TrimRight(env("BLOBSTORE_URL", "http://blobstore:8080"), "/"), client: tracedHTTP},
		policiesURL: strings.TrimRight(env("POLICY_REGISTRY_URL", "http://policy-registry:8080"), "/"),
		skillsURL:   strings.TrimRight(env("SKILL_REGISTRY_URL", "http://skill-registry:8080"), "/"),
		http:        tracedHTTP,
		logger:      logger,
	}
	if *seed != "" {
		if err := reg.seedWithRetry(context.Background(), *seed, 2*time.Minute); err != nil {
			return fmt.Errorf("seeding %s: %w", *prefix, err)
		}
	}
	server := &http.Server{Addr: *addr, Handler: otelhttp.NewHandler(reg.routes(), "registry"), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() {
		logger.Info("registry listening", "address", server.Addr, "prefix", *prefix)
		result <- server.ListenAndServe()
	}()
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(drain)
	}
	return nil
}

func tracedHTTPClient() *http.Client {
	return &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
}

type registry struct {
	prefix      string
	files       []string
	openFiles   bool
	blobs       blobClient
	policiesURL string
	skillsURL   string
	http        *http.Client
	logger      *slog.Logger
	mu          sync.RWMutex // serializes version allocation and publication
}

// Entry summarizes one named entry and its versions.
type Entry struct {
	Name     string   `json:"name"`
	Versions []string `json:"versions"`
	Latest   string   `json:"latest"`
}

// Version describes one stored version.
type Version struct {
	Version string    `json:"version"`
	Created time.Time `json:"created"`
	Creator string    `json:"creator,omitempty"`
}

// Content is one version with its files. JSON files are returned parsed.
type Content struct {
	Name    string                     `json:"name"`
	Version string                     `json:"version"`
	Created time.Time                  `json:"created"`
	Creator string                     `json:"creator,omitempty"`
	Files   map[string]json.RawMessage `json:"files"`
}

func (r *registry) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /"+r.prefix, r.listEntries)
	mux.HandleFunc("GET /"+r.prefix+"/{path...}", r.getPath)
	mux.HandleFunc("POST /"+r.prefix+"/{name...}", r.createVersion)
	mux.HandleFunc("DELETE /"+r.prefix+"/{name...}", r.deleteEntry)
	return mux
}

// getPath serves /{name} (the versions) and /{name}/{version} (one version):
// a last segment that is a version or "latest" names a version.
func (r *registry) getPath(w http.ResponseWriter, req *http.Request) {
	if r.openFiles {
		r.mu.RLock()
		defer r.mu.RUnlock()
	}
	path := strings.Trim(req.PathValue("path"), "/")
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		last := path[i+1:]
		if _, ok := versionNumber(last); ok || last == "latest" {
			r.getVersion(w, req, path[:i], last)
			return
		}
	}
	r.listVersions(w, req, path)
}

func (r *registry) listEntries(w http.ResponseWriter, req *http.Request) {
	if r.openFiles {
		r.mu.RLock()
		defer r.mu.RUnlock()
	}
	names, err := r.names(req.Context())
	if err != nil {
		r.fail(w, err)
		return
	}
	entries := []Entry{}
	for _, name := range names {
		versions, err := r.versions(req.Context(), name)
		if err != nil {
			r.fail(w, err)
			return
		}
		if len(versions) == 0 {
			continue
		}
		e := Entry{Name: name, Versions: []string{}, Latest: versions[len(versions)-1].Version}
		for _, v := range versions {
			e.Versions = append(e.Versions, v.Version)
		}
		entries = append(entries, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// deleteEntry removes an entry with every version it has, or, when the path ends in a
// version (/{name}/v<N>), that one version alone.
func (r *registry) deleteEntry(w http.ResponseWriter, req *http.Request) {
	if r.openFiles {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	name := strings.Trim(req.PathValue("name"), "/")
	only := ""
	if i := strings.LastIndexByte(name, '/'); i > 0 {
		if _, ok := versionNumber(name[i+1:]); ok {
			name, only = name[:i], name[i+1:]
		}
	}
	if !validName(name) {
		writeError(w, http.StatusBadRequest, badName)
		return
	}
	versions, err := r.versions(req.Context(), name)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(versions) == 0 {
		writeError(w, http.StatusNotFound, "unknown "+r.prefix)
		return
	}
	if only != "" {
		var kept []Version
		for _, v := range versions {
			if v.Version == only {
				kept = append(kept, v)
			}
		}
		if len(kept) == 0 {
			writeError(w, http.StatusNotFound, "unknown version")
			return
		}
		versions = kept
	}
	for _, v := range versions {
		files, err := r.versionFiles(req.Context(), name, v.Version)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if r.openFiles {
			// Reserve the version number before the first file is removed.
			if err := r.blobs.put(req.Context(), r.retiredSkillVersionID(name, v.Version), nil, "skill-registry"); err != nil {
				r.fail(w, err)
				return
			}
		}
		if err := r.deleteFiles(req.Context(), name, v.Version, files); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *registry) retiredSkillVersionID(name, version string) string {
	return r.prefix + "/" + name + "/" + retiredSkillVersionPrefix + version
}

func (r *registry) deleteFiles(ctx context.Context, name, version string, files []string) error {
	if r.openFiles {
		// Unpublish first so an interrupted delete does not expose a partial version.
		marker := r.files[0]
		files = append([]string{marker}, slices.DeleteFunc(files, func(file string) bool { return file == marker })...)
	}
	for _, file := range files {
		if err := r.blobs.delete(ctx, r.prefix+"/"+name+"/"+version+"/"+file); err != nil && !errors.Is(err, errNotFound) {
			return err
		}
	}
	return nil
}

func (r *registry) listVersions(w http.ResponseWriter, req *http.Request, name string) {
	if !validName(name) {
		writeError(w, http.StatusBadRequest, badName)
		return
	}
	versions, err := r.versions(req.Context(), name)
	if err != nil {
		r.fail(w, err)
		return
	}
	if len(versions) == 0 {
		writeError(w, http.StatusNotFound, "unknown "+r.prefix+" entry")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "versions": versions})
}

func (r *registry) getVersion(w http.ResponseWriter, req *http.Request, name, version string) {
	if !validName(name) {
		writeError(w, http.StatusBadRequest, badName)
		return
	}
	if version == "latest" {
		versions, err := r.versions(req.Context(), name)
		if err != nil {
			r.fail(w, err)
			return
		}
		if len(versions) == 0 {
			writeError(w, http.StatusNotFound, "unknown "+r.prefix+" entry")
			return
		}
		version = versions[len(versions)-1].Version
	} else if _, ok := versionNumber(version); !ok {
		writeError(w, http.StatusBadRequest, "version must look like v1")
		return
	}
	content, err := r.read(req.Context(), name, version)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "unknown version")
		return
	}
	if err != nil {
		r.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, content)
}

func (r *registry) createVersion(w http.ResponseWriter, req *http.Request) {
	name := strings.Trim(req.PathValue("name"), "/")
	if !validName(name) {
		writeError(w, http.StatusBadRequest, badName)
		return
	}
	var body struct {
		Files map[string]json.RawMessage `json:"files"`
	}
	reader := io.Reader(req.Body)
	if !r.openFiles {
		reader = http.MaxBytesReader(w, req.Body, maxBodyBytes)
	}
	d := json.NewDecoder(reader)
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	files, err := r.encodeFiles(body.Files)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if r.openFiles {
		if err := validateSkillFiles(name, files); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	if r.prefix == "agents" {
		if err := validateModelSettings(files["agent.json"]); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := validateUnpackInputs(files["agent.json"]); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := validateOutputContract(files["agent.json"]); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := r.validateSkillReferences(req.Context(), files["agent.json"]); err != nil {
			var statusErr *skillValidationError
			if errors.As(err, &statusErr) {
				writeError(w, statusErr.status, statusErr.Error())
			} else {
				writeError(w, http.StatusBadGateway, "cannot validate skills")
			}
			return
		}
		if err := r.validatePolicyReferences(req.Context(), string(files["systemprompt.md"])); err != nil {
			var statusErr *policyValidationError
			if errors.As(err, &statusErr) {
				writeError(w, statusErr.status, statusErr.Error())
			} else {
				writeError(w, http.StatusBadGateway, "cannot validate required policies")
			}
			return
		}
	}
	creator := strings.TrimSpace(req.Header.Get("X-Creator"))
	if creator == "" {
		creator = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	versions, err := r.versions(req.Context(), name)
	if err != nil {
		r.fail(w, err)
		return
	}
	if len(versions) == 0 {
		if conflict, err := r.folderConflict(req.Context(), name); err != nil {
			r.fail(w, err)
			return
		} else if conflict != "" {
			writeError(w, http.StatusConflict, conflict)
			return
		}
	}
	var last int
	if len(versions) > 0 {
		last, _ = versionNumber(versions[len(versions)-1].Version)
	}
	if r.openFiles {
		l, err := r.blobs.list(req.Context(), r.prefix+"/"+name)
		if err != nil && !errors.Is(err, errNotFound) {
			r.fail(w, err)
			return
		}
		for _, object := range l.Objects {
			version, retired := strings.CutPrefix(filepath.Base(object.ID), retiredSkillVersionPrefix)
			if n, valid := versionNumber(version); retired && valid {
				last = max(last, n)
			}
		}
	}
	if last == math.MaxInt {
		writeError(w, http.StatusConflict, "no version numbers remain for this entry")
		return
	}
	version := "v" + strconv.Itoa(last+1)
	if err = r.write(req.Context(), name, version, files, creator); err != nil {
		r.fail(w, err)
		return
	}
	r.logger.Info("version stored", "prefix", r.prefix, "name", name, "version", version, "creator", creator)
	writeJSON(w, http.StatusCreated, map[string]string{"name": name, "version": version})
}

// validateModelSettings checks the optional settings of an agent.json:
// "temperature" is a number from 0 to 2, and "commands" is a list of non-empty strings.
func validateModelSettings(agentJSON []byte) error {
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(agentJSON, &settings); err != nil {
		return fmt.Errorf("file %q must be a JSON object", "agent.json")
	}
	if raw, ok := settings["temperature"]; ok {
		var temperature *float64
		if err := json.Unmarshal(raw, &temperature); err != nil || temperature == nil || *temperature < 0 || *temperature > 2 {
			return fmt.Errorf("agent.json: temperature must be a number from 0 to 2, got %s", raw)
		}
	}
	if raw, ok := settings["commands"]; ok {
		var commands []string
		if err := json.Unmarshal(raw, &commands); err != nil || commands == nil {
			return fmt.Errorf("agent.json: commands must be a list of non-empty strings, got %s", raw)
		}
		for _, command := range commands {
			if strings.TrimSpace(command) == "" {
				return fmt.Errorf("agent.json: commands must be a list of non-empty strings, got %s", raw)
			}
		}
	}
	return nil
}

// validateUnpackInputs checks the required setting of an agent.json:
// "unpack_inputs" is true or false.
func validateUnpackInputs(agentJSON []byte) error {
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(agentJSON, &settings); err != nil {
		return fmt.Errorf("file %q must be a JSON object", "agent.json")
	}
	raw, ok := settings["unpack_inputs"]
	if !ok {
		return fmt.Errorf("agent.json: unpack_inputs is required and must be true or false")
	}
	var unpack *bool
	if err := json.Unmarshal(raw, &unpack); err != nil || unpack == nil {
		return fmt.Errorf("agent.json: unpack_inputs must be true or false, got %s", raw)
	}
	return nil
}

type policyValidationError struct {
	status int
	text   string
}

func (e *policyValidationError) Error() string { return e.text }

type skillValidationError struct {
	status int
	text   string
}

func (e *skillValidationError) Error() string { return e.text }

func (r *registry) validateSkillReferences(ctx context.Context, agentJSON []byte) error {
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(agentJSON, &settings); err != nil {
		return &skillValidationError{status: http.StatusUnprocessableEntity, text: `file "agent.json" must be a JSON object`}
	}
	if raw, ok := settings["skill_script_timeout_seconds"]; ok {
		var timeout int
		if err := json.Unmarshal(raw, &timeout); err != nil || timeout <= 0 {
			return &skillValidationError{status: http.StatusUnprocessableEntity, text: fmt.Sprintf("agent.json: skill_script_timeout_seconds must be a positive integer, got %s", raw)}
		}
	}
	raw, ok := settings["skills"]
	if !ok {
		return nil
	}
	var references []string
	if err := json.Unmarshal(raw, &references); err != nil || references == nil {
		return &skillValidationError{status: http.StatusUnprocessableEntity, text: fmt.Sprintf("agent.json: skills must be a list of pinned skill versions, got %s", raw)}
	}
	seen := map[string]bool{}
	for _, reference := range references {
		at := strings.LastIndexByte(reference, '@')
		if at <= 0 || at == len(reference)-1 {
			return &skillValidationError{status: http.StatusUnprocessableEntity, text: fmt.Sprintf("agent.json: skill %q must include a pinned version such as @v1", reference)}
		}
		name, version := reference[:at], reference[at+1:]
		if !validName(name) {
			return &skillValidationError{status: http.StatusUnprocessableEntity, text: fmt.Sprintf("agent.json: skill %q has an invalid name", reference)}
		}
		if _, valid := versionNumber(version); !valid {
			return &skillValidationError{status: http.StatusUnprocessableEntity, text: fmt.Sprintf("agent.json: skill %q must use a pinned version such as v1", reference)}
		}
		if seen[name] {
			return &skillValidationError{status: http.StatusUnprocessableEntity, text: fmt.Sprintf("agent.json: skill %q is listed more than once", name)}
		}
		seen[name] = true
		parts := strings.Split(name, "/")
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		endpoint := r.skillsURL + "/skills/" + strings.Join(parts, "/") + "/" + version
		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := r.http.Do(req)
		if err != nil {
			cancel()
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusNotFound {
			return &skillValidationError{status: http.StatusUnprocessableEntity, text: fmt.Sprintf("skill %q does not exist", reference)}
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("skill registry GET %s: %s", endpoint, resp.Status)
		}
	}
	return nil
}

func (r *registry) validatePolicyReferences(ctx context.Context, prompt string) error {
	tokens, err := parsePolicyTokens(prompt)
	if err != nil {
		return &policyValidationError{status: http.StatusUnprocessableEntity, text: err.Error()}
	}
	seen := map[string]bool{}
	for _, token := range tokens {
		if seen[token.id] {
			continue
		}
		seen[token.id] = true
		parts := strings.Split(token.id, "/")
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		endpoint := r.policiesURL + "/policies/" + strings.Join(parts, "/") + "/latest"
		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := r.http.Do(req)
		if err != nil {
			cancel()
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusNotFound {
			return &policyValidationError{status: http.StatusUnprocessableEntity, text: fmt.Sprintf("required policy %q does not exist", token.id)}
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("policy registry GET %s: %s", endpoint, resp.Status)
		}
	}
	return nil
}

// encodeFiles checks that every configured file is present and returns the
// bytes to store: JSON files are stored as given, other files as their text.
func (r *registry) encodeFiles(files map[string]json.RawMessage) (map[string][]byte, error) {
	out := map[string][]byte{}
	if r.openFiles {
		for _, file := range r.files {
			if _, ok := files[file]; !ok {
				return nil, fmt.Errorf("file %q is required", file)
			}
		}
		for file, raw := range files {
			if !utf8.Valid(raw) {
				return nil, fmt.Errorf("file %q must be UTF-8 text", file)
			}
			var text *string
			if err := json.Unmarshal(raw, &text); err != nil || text == nil {
				return nil, fmt.Errorf("file %q must be text", file)
			}
			out[file] = []byte(*text)
		}
		return out, nil
	}
	for _, f := range r.files {
		raw, ok := files[f]
		if !ok {
			return nil, fmt.Errorf("file %q is required", f)
		}
		if strings.HasSuffix(f, ".json") {
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
				return nil, fmt.Errorf("file %q must be a JSON object", f)
			}
			var buf bytes.Buffer
			if err := json.Indent(&buf, raw, "", "  "); err != nil {
				return nil, err
			}
			buf.WriteByte('\n')
			out[f] = buf.Bytes()
			continue
		}
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, fmt.Errorf("file %q must be a string", f)
		}
		out[f] = []byte(text)
	}
	for f := range files {
		if _, ok := out[f]; !ok {
			return nil, fmt.Errorf("file %q is not part of %s entries", f, r.prefix)
		}
	}
	return out, nil
}

var skillPathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var skillFrontMatterNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

func validateFilePath(path string) error {
	if path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return fmt.Errorf("file %q must be a relative path with no empty segment", path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." || !skillPathSegmentPattern.MatchString(segment) {
			return fmt.Errorf("file %q has invalid path segment %q; use only letters, digits, dots, dashes, and underscores", path, segment)
		}
	}
	return nil
}

func validateSkillFiles(entry string, files map[string][]byte) error {
	for path, content := range files {
		if err := validateFilePath(path); err != nil {
			return err
		}
		if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
			return fmt.Errorf("file %q must be UTF-8 text without NUL bytes", path)
		}
		for folder := filepath.Dir(path); folder != "."; folder = filepath.Dir(folder) {
			if _, exists := files[folder]; exists {
				return fmt.Errorf("file %q cannot be stored because %q is also a file", path, folder)
			}
		}
		if filepath.Base(path) != "SKILL.md" {
			continue
		}
		expected := strings.Split(entry, "/")
		name := expected[len(expected)-1]
		if dir := filepath.ToSlash(filepath.Dir(path)); dir != "." {
			parts := strings.Split(dir, "/")
			name = parts[len(parts)-1]
		}
		if err := validateSkillFrontMatter(path, name, string(content)); err != nil {
			return err
		}
	}
	return nil
}

func validateSkillFrontMatter(path, expectedName, content string) error {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return fmt.Errorf("file %q must start with YAML front matter", path)
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return fmt.Errorf("file %q has unclosed YAML front matter", path)
	}
	var frontMatter struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &frontMatter); err != nil {
		return fmt.Errorf("file %q has invalid YAML front matter: %w", path, err)
	}
	name := frontMatter.Name
	if !skillFrontMatterNamePattern.MatchString(name) || len(name) > 64 {
		return fmt.Errorf("file %q front matter name must be lowercase letters, digits, and dashes, at most 64 characters", path)
	}
	if name != expectedName {
		return fmt.Errorf("file %q front matter name %q must equal its folder name %q", path, name, expectedName)
	}
	description := strings.TrimSpace(frontMatter.Description)
	if description == "" || utf8.RuneCountInString(frontMatter.Description) > 1024 {
		return fmt.Errorf("file %q front matter description must be non-empty and at most 1024 characters", path)
	}
	return nil
}

// names walks the folders and returns every entry name, such as "group/name".
// A directory that holds version directories is an entry; others are folders.
func (r *registry) names(ctx context.Context) ([]string, error) {
	names := []string{}
	var walk func(path string, depth int) error
	walk = func(path string, depth int) error {
		dir := r.prefix
		if path != "" {
			dir += "/" + path
		}
		l, err := r.blobs.list(ctx, dir)
		if errors.Is(err, errNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		entry := false
		for _, d := range l.Directories {
			if _, ok := versionNumber(d); ok {
				entry = true
			}
		}
		if entry && path != "" {
			names = append(names, path)
		}
		if depth == maxDepth {
			return nil
		}
		for _, d := range l.Directories {
			if _, ok := versionNumber(d); ok {
				continue
			}
			child := d
			if path != "" {
				child = path + "/" + d
			}
			if validName(child) {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// folderConflict explains why a new entry cannot be created at name: name is
// already a folder, or one of its parent folders is an entry.
func (r *registry) folderConflict(ctx context.Context, name string) (string, error) {
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		versions, err := r.versions(ctx, parent)
		if err != nil {
			return "", err
		}
		if len(versions) > 0 {
			return fmt.Sprintf("%s is an entry, not a folder", parent), nil
		}
	}
	l, err := r.blobs.list(ctx, r.prefix+"/"+name)
	if errors.Is(err, errNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, dir := range l.Directories {
		_, staged := versionNumber(dir)
		if !r.openFiles || !staged {
			return fmt.Sprintf("%s is a folder", name), nil
		}
	}
	return "", nil
}

// versions lists an entry's versions, oldest first, with the metadata of the
// first file stored in each version.
func (r *registry) versions(ctx context.Context, name string) ([]Version, error) {
	l, err := r.blobs.list(ctx, r.prefix+"/"+name)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Version
	for _, d := range l.Directories {
		if _, ok := versionNumber(d); !ok {
			continue
		}
		files, err := r.blobs.list(ctx, r.prefix+"/"+name+"/"+d)
		if r.openFiles && err != nil && !errors.Is(err, errNotFound) {
			return nil, err
		}
		if err != nil || len(files.Objects) == 0 {
			continue
		}
		for _, meta := range files.Objects {
			if !r.openFiles || strings.HasSuffix(meta.ID, "/"+r.files[0]) {
				out = append(out, Version{Version: d, Created: meta.Created, Creator: meta.Creator})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := versionNumber(out[i].Version)
		b, _ := versionNumber(out[j].Version)
		return a < b
	})
	return out, nil
}

func (r *registry) read(ctx context.Context, name, version string) (Content, error) {
	c := Content{Name: name, Version: version, Files: map[string]json.RawMessage{}}
	files, err := r.versionFiles(ctx, name, version)
	if err != nil {
		return Content{}, err
	}
	if r.openFiles && !slicesContains(files, r.files[0]) {
		return Content{}, errNotFound
	}
	for _, f := range files {
		data, meta, err := r.blobs.get(ctx, r.prefix+"/"+name+"/"+version+"/"+f)
		if err != nil {
			return Content{}, err
		}
		if (!r.openFiles && c.Created.IsZero()) || (r.openFiles && f == r.files[0]) {
			c.Created, c.Creator = meta.Created, meta.Creator
		}
		if !r.openFiles && strings.HasSuffix(f, ".json") && json.Valid(data) {
			c.Files[f] = json.RawMessage(data)
			continue
		}
		text, _ := json.Marshal(string(data))
		c.Files[f] = text
	}
	return c, nil
}

func (r *registry) write(ctx context.Context, name, version string, files map[string][]byte, creator string) error {
	paths := make([]string, 0, len(files))
	for f := range files {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	if r.openFiles {
		staged, err := r.versionFiles(ctx, name, version)
		if err != nil && !errors.Is(err, errNotFound) {
			return err
		}
		if slicesContains(staged, r.files[0]) {
			return fmt.Errorf("skill %s@%s is already stored", name, version)
		}
		if len(staged) != 0 {
			if err := r.deleteFiles(ctx, name, version, staged); err != nil {
				return err
			}
		}
		// Write the required root last so readers cannot see a partial version.
		marker := r.files[0]
		paths = append(slices.DeleteFunc(paths, func(path string) bool { return path == marker }), marker)
	}
	for _, f := range paths {
		if err := r.blobs.put(ctx, r.prefix+"/"+name+"/"+version+"/"+f, files[f], creator); err != nil {
			return err
		}
	}
	return nil
}

func (r *registry) versionFiles(ctx context.Context, name, version string) ([]string, error) {
	if !r.openFiles {
		return append([]string(nil), r.files...), nil
	}
	root := r.prefix + "/" + name + "/" + version
	var files []string
	var walk func(string, string) error
	walk = func(id, relative string) error {
		listing, err := r.blobs.list(ctx, id)
		if err != nil {
			return err
		}
		for _, object := range listing.Objects {
			file := strings.TrimPrefix(object.ID, id+"/")
			if relative != "" {
				file = relative + "/" + file
			}
			files = append(files, file)
		}
		for _, directory := range listing.Directories {
			child := directory
			if relative != "" {
				child = relative + "/" + directory
			}
			if err := walk(id+"/"+directory, child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// seedWithRetry stores the seed versions that do not exist yet, waiting for
// the blobstore to become reachable.
func (r *registry) seedWithRetry(ctx context.Context, dir string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := r.seed(ctx, dir)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		r.logger.Warn("seeding failed, retrying", "error", err)
		time.Sleep(2 * time.Second)
	}
}

// seed stores the versions found under dir that do not exist yet. A directory that
// holds version directories is an entry; other directories are folders.
func (r *registry) seed(ctx context.Context, dir string) error {
	return r.seedDir(ctx, dir, "")
}

func (r *registry) seedDir(ctx context.Context, dir, name string) error {
	children, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, c := range children {
		if !c.IsDir() {
			continue
		}
		if _, ok := versionNumber(c.Name()); ok {
			if name == "" {
				continue
			}
			if err := r.seedVersion(ctx, filepath.Join(dir, c.Name()), name, c.Name()); err != nil {
				return err
			}
			continue
		}
		child := c.Name()
		if name != "" {
			child = name + "/" + c.Name()
		}
		if validName(child) {
			if err := r.seedDir(ctx, filepath.Join(dir, c.Name()), child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *registry) seedVersion(ctx context.Context, dir, name, version string) error {
	if r.openFiles {
		if _, _, err := r.blobs.get(ctx, r.retiredSkillVersionID(name, version)); err == nil {
			return nil
		} else if !errors.Is(err, errNotFound) {
			return err
		}
	}
	id := r.prefix + "/" + name + "/" + version
	if listing, err := r.blobs.list(ctx, id); err == nil {
		if !r.openFiles {
			return nil // already stored
		}
		for _, object := range listing.Objects {
			if object.ID == id+"/"+r.files[0] {
				return nil
			}
		}
	} else if !errors.Is(err, errNotFound) {
		return err
	}
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !r.openFiles && !slicesContains(r.files, relative) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[relative] = data
		return nil
	})
	if err != nil {
		return fmt.Errorf("seed %s/%s: %w", name, version, err)
	}
	for _, required := range r.files {
		if _, ok := files[required]; !ok {
			return fmt.Errorf("seed %s/%s: file %q is required", name, version, required)
		}
	}
	if r.openFiles {
		if err := validateSkillFiles(name, files); err != nil {
			return fmt.Errorf("seed %s/%s: %w", name, version, err)
		}
	}
	if r.prefix == "agents" {
		if err := validateOutputContract(files["agent.json"]); err != nil {
			return fmt.Errorf("seed %s/%s: %w", name, version, err)
		}
	}
	if err := r.write(ctx, name, version, files, "seed"); err != nil {
		return err
	}
	r.logger.Info("seeded", "prefix", r.prefix, "name", name, "version", version)
	return nil
}

func slicesContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (r *registry) fail(w http.ResponseWriter, err error) {
	r.logger.Error("request failed", "error", err)
	writeError(w, http.StatusBadGateway, "blobstore request failed")
}

// versionNumber parses "v12" into 12.
func versionNumber(v string) (int, bool) {
	if !strings.HasPrefix(v, "v") {
		return 0, false
	}
	n, err := strconv.Atoi(v[1:])
	if err != nil || n < 1 || strings.HasPrefix(v[1:], "0") {
		return 0, false
	}
	return n, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// blobClient talks to the blobstore service.
type blobClient struct {
	base   string
	client *http.Client
}

var errNotFound = errors.New("not found")

// objectMeta mirrors the blobstore's metadata record.
type objectMeta struct {
	ID      string    `json:"id"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
	Creator string    `json:"creator,omitempty"`
	TraceID string    `json:"trace_id,omitempty"`
}

type listing struct {
	Prefix      string       `json:"prefix"`
	Directories []string     `json:"directories"`
	Objects     []objectMeta `json:"objects"`
}

func (b blobClient) list(ctx context.Context, prefix string) (listing, error) {
	var out listing
	err := b.do(ctx, http.MethodGet, "/list/"+prefix, nil, "", func(resp *http.Response) error {
		return json.NewDecoder(resp.Body).Decode(&out)
	})
	return out, err
}

func (b blobClient) get(ctx context.Context, id string) ([]byte, objectMeta, error) {
	var data []byte
	var meta objectMeta
	err := b.do(ctx, http.MethodGet, "/blobs/"+id, nil, "", func(resp *http.Response) error {
		var err error
		data, err = io.ReadAll(resp.Body)
		meta.Creator = resp.Header.Get("X-Creator")
		meta.TraceID = resp.Header.Get("X-Trace-Id")
		meta.Created, _ = time.Parse(time.RFC3339Nano, resp.Header.Get("X-Created"))
		return err
	})
	return data, meta, err
}

func (b blobClient) delete(ctx context.Context, id string) error {
	return b.do(ctx, http.MethodDelete, "/blobs/"+id, nil, "", func(*http.Response) error { return nil })
}

func (b blobClient) put(ctx context.Context, id string, data []byte, creator string) error {
	return b.do(ctx, http.MethodPut, "/blobs/"+id, data, creator, func(*http.Response) error { return nil })
}

func (b blobClient) do(ctx context.Context, method, path string, body []byte, creator string, read func(*http.Response) error) error {
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if creator != "" {
		req.Header.Set("X-Creator", creator)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return read(resp)
}
