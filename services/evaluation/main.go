package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const setPrefix = "evaluation-sets"

// runStatusUnknown is the status of a run whose state could not be read.
const runStatusUnknown = "Unknown"

// RunSnapshot is what an evaluation set keeps of a run: its identity, timing,
// status and agents as they were when the run was added.
type RunSnapshot struct {
	TraceID            string    `json:"trace_id"`
	Workflow           string    `json:"workflow"`
	Started            time.Time `json:"started"`
	Finished           time.Time `json:"finished,omitempty"`
	Status             string    `json:"status"`
	InputArtifact      string    `json:"input_artifact,omitempty"`
	DistributedTraceID string    `json:"distributed_trace_id,omitempty"`
	IndexError         string    `json:"index_error,omitempty"`
	Agents             []string  `json:"agents"`
	DurationMS         int64     `json:"duration_ms,omitempty"`
	DurationKnown      bool      `json:"duration_known"`
	SourcePresent      bool      `json:"source_present"`
	AgentsComplete     bool      `json:"agents_complete"`
	HistoryUnavailable bool      `json:"history_unavailable,omitempty"`
}

// EvaluationSet is a named selection of run snapshots. Every update raises its revision.
type EvaluationSet struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Description   string        `json:"description,omitempty"`
	Created       time.Time     `json:"created"`
	Updated       time.Time     `json:"updated"`
	CreatedBy     string        `json:"created_by,omitempty"`
	Runs          []RunSnapshot `json:"runs"`
	SchemaVersion int           `json:"schema_version"`
	Revision      int           `json:"revision"`
}

type setInput struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	ExpectedRevision *int     `json:"expected_revision,omitempty"`
	RunIDs           []string `json:"run_ids"`
}

// setSummary is a set as the list shows it: without its runs, with their count
// and the number of runs whose source is gone.
type setSummary struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Description     string    `json:"description,omitempty"`
	Created         time.Time `json:"created"`
	Updated         time.Time `json:"updated"`
	CreatedBy       string    `json:"created_by,omitempty"`
	RunCount        int       `json:"run_count"`
	MissingRunCount int       `json:"missing_run_count"`
}

type service struct {
	http                 *http.Client
	log                  *slog.Logger
	blobs, orchestrator  string
	openobserve          string
	openobserveUser      string
	openobservePassword  string
	projectionStream     string
	setMu, measurementMu sync.Mutex
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (err error) {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	shutdown, err := initTelemetry(context.Background())
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()
	client := &http.Client{Timeout: 15 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}
	openObserveUser, openObservePassword, err := openObserveCredentials(env("OPENOBSERVE_CONFIG", "/openobserve/config/openobserve.env"))
	if err != nil {
		log.Warn("OpenObserve projection and evidence are unavailable", "error", err)
		openObserveUser, openObservePassword = "", ""
	}
	lock, err := acquireWriterLock(env("EVALUATION_LOCK_PATH", "/state/writer.lock"))
	if err != nil {
		return err
	}
	defer func() { err = closeAfter(err, lock) }()
	projectionStreamName := env("EVALUATION_PROJECTION_STREAM", projectionStream)
	if !validProjectionStream(projectionStreamName) {
		return fmt.Errorf("EVALUATION_PROJECTION_STREAM must match ^[a-z][a-z0-9_]{0,95}$")
	}
	s := &service{
		blobs:           strings.TrimRight(env("BLOBSTORE_URL", "http://blobstore:8080"), "/"),
		orchestrator:    strings.TrimRight(env("ORCHESTRATOR_URL", "http://orchestrator:8080"), "/"),
		openobserve:     strings.TrimRight(env("OPENOBSERVE_URL", "http://openobserve:5080"), "/"),
		openobserveUser: openObserveUser, openobservePassword: openObservePassword,
		projectionStream: projectionStreamName,
		http:             client, log: log,
	}
	server := &http.Server{Addr: env("EVALUATION_ADDR", ":8080"), Handler: otelhttp.NewHandler(s.routes(), "evaluation"), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.projectorLoop(ctx)
	done := make(chan error, 1)
	go func() { log.Info("evaluation listening", "address", server.Addr); done <- server.ListenAndServe() }()
	select {
	case err = <-done:
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

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (s *service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("GET /api/sets", s.listSets)
	mux.HandleFunc("POST /api/sets", s.createSet)
	mux.HandleFunc("GET /api/sets/{id}", s.getSet)
	mux.HandleFunc("PUT /api/sets/{id}", s.updateSet)
	mux.HandleFunc("GET /api/sets/{id}/export", s.exportSet)
	s.measurementRoutes(mux)
	return mux
}

func (s *service) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err := optionalTime(q.Get("started_from"))
	if err != nil {
		s.fail(w, 400, fmt.Errorf("started_from must be RFC3339"))
		return
	}
	to, err := optionalTime(q.Get("started_to"))
	if err != nil {
		s.fail(w, 400, fmt.Errorf("started_to must be RFC3339"))
		return
	}
	minDuration, err := optionalInt(q.Get("duration_min_ms"))
	if err != nil {
		s.fail(w, 400, fmt.Errorf("duration_min_ms must be a non-negative integer"))
		return
	}
	maxDuration, err := optionalInt(q.Get("duration_max_ms"))
	if err != nil {
		s.fail(w, 400, fmt.Errorf("duration_max_ms must be a non-negative integer"))
		return
	}
	if minDuration != nil && maxDuration != nil && *minDuration > *maxDuration {
		s.fail(w, 400, fmt.Errorf("duration range is invalid"))
		return
	}
	runs, err := s.catalog(r.Context())
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	filtered := make([]RunSnapshot, 0, len(runs))
	incomplete := 0
	historyUnavailable := 0
	historyOmitted := 0
	for i := range runs {
		run := &runs[i]
		matches, uncertain := matchesBaseFilters(run, q.Get("workflow"), q.Get("agent"), from, to)
		if !matches {
			if uncertain {
				incomplete++
			}
			continue
		}
		if run.IndexError != "" {
			incomplete++
		}
		if run.HistoryUnavailable {
			historyUnavailable++
			if q.Get("status") != "" || minDuration != nil || maxDuration != nil {
				historyOmitted++
				continue
			}
		}
		if q.Get("status") != "" && !strings.EqualFold(run.Status, q.Get("status")) {
			continue
		}
		if (minDuration != nil || maxDuration != nil) && !run.DurationKnown {
			if run.IndexError == "" {
				incomplete++
			}
			continue
		}
		if minDuration != nil && run.DurationMS < *minDuration || maxDuration != nil && run.DurationMS > *maxDuration {
			continue
		}
		filtered = append(filtered, *run)
	}
	if cursor := q.Get("cursor"); cursor != "" {
		ct, id, err := decodeCursor(cursor)
		if err != nil {
			s.fail(w, 400, fmt.Errorf("invalid cursor"))
			return
		}
		n := filtered[:0]
		for i := range filtered {
			if filtered[i].Started.Before(ct) || filtered[i].Started.Equal(ct) && filtered[i].TraceID < id {
				n = append(n, filtered[i])
			}
		}
		filtered = n
	}
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		v, e := strconv.Atoi(raw)
		if e != nil || v < 1 || v > 200 {
			s.fail(w, 400, fmt.Errorf("limit must be between 1 and 200"))
			return
		}
		limit = v
	}
	next := ""
	if len(filtered) > limit {
		last := filtered[limit-1]
		next = encodeCursor(last.Started, last.TraceID)
		filtered = filtered[:limit]
	}
	writeJSON(w, 200, map[string]any{"runs": filtered, "next_cursor": next, "incomplete_runs": incomplete, "history_unavailable": historyUnavailable, "history_unavailable_omitted": historyOmitted})
}

func matchesBaseFilters(run *RunSnapshot, workflow, agent string, from, to *time.Time) (matches, uncertain bool) {
	if workflow != "" {
		if run.Workflow == "" && run.IndexError != "" {
			return false, true
		}
		if run.Workflow != workflow {
			return false, false
		}
	}
	if agent != "" {
		if !run.AgentsComplete {
			return false, true
		}
		if !matchesAgent(run.Agents, agent) {
			return false, false
		}
	}
	if from != nil || to != nil {
		if run.Started.IsZero() {
			return false, true
		}
		if from != nil && run.Started.Before(*from) || to != nil && run.Started.After(*to) {
			return false, false
		}
	}
	return true, false
}

func (s *service) listSets(w http.ResponseWriter, r *http.Request) {
	sets, err := s.loadSets(r.Context())
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	present, err := s.runIDs(r.Context())
	if err != nil {
		s.fail(w, http.StatusBadGateway, fmt.Errorf("verifying evaluation-set source runs: %w", err))
		return
	}
	out := make([]setSummary, 0, len(sets))
	for i := range sets {
		set := &sets[i]
		missing := 0
		for j := range set.Runs {
			if !present[set.Runs[j].TraceID] {
				missing++
			}
		}
		out = append(out, setSummary{ID: set.ID, Name: set.Name, Description: set.Description, Created: set.Created, Updated: set.Updated, CreatedBy: set.CreatedBy, RunCount: len(set.Runs), MissingRunCount: missing})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	writeJSON(w, 200, map[string]any{"sets": out})
}

func (s *service) getSet(w http.ResponseWriter, r *http.Request) {
	set, err := s.loadSet(r.Context(), r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, err)
		return
	}
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	if err := s.refreshPresence(r.Context(), &set); err != nil {
		s.fail(w, http.StatusBadGateway, fmt.Errorf("verifying source runs: %w", err))
		return
	}
	writeJSON(w, 200, set)
}

func (s *service) createSet(w http.ResponseWriter, r *http.Request) {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	in, ok := decodeSetInput(w, r)
	if !ok {
		return
	}
	sets, err := s.loadSets(r.Context())
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	if duplicateName(sets, in.Name, "") {
		s.fail(w, 409, fmt.Errorf("an evaluation set named %q already exists", in.Name))
		return
	}
	runs, err := s.snapshots(r.Context(), in.RunIDs, nil)
	if err != nil {
		s.fail(w, 422, err)
		return
	}
	now := time.Now().UTC()
	set := EvaluationSet{SchemaVersion: 1, ID: uuid.NewString(), Name: strings.TrimSpace(in.Name), Description: strings.TrimSpace(in.Description), Created: now, Updated: now, CreatedBy: strings.TrimSpace(r.Header.Get("X-Creator")), Revision: 1, Runs: runs}
	if err = s.saveSet(r.Context(), &set); err != nil {
		s.fail(w, 502, err)
		return
	}
	writeJSON(w, 201, set)
}

func (s *service) updateSet(w http.ResponseWriter, r *http.Request) {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	id := r.PathValue("id")
	if uuid.Validate(id) != nil {
		s.fail(w, 400, fmt.Errorf("invalid set id"))
		return
	}
	in, ok := decodeSetInput(w, r)
	if !ok {
		return
	}
	current, err := s.loadSet(r.Context(), id)
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, err)
		return
	}
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	if in.ExpectedRevision != nil && *in.ExpectedRevision != current.Revision {
		s.fail(w, http.StatusConflict, fmt.Errorf("evaluation set changed; reload revision %d", current.Revision))
		return
	}
	sets, err := s.loadSets(r.Context())
	if err != nil {
		s.fail(w, 502, err)
		return
	}
	if duplicateName(sets, in.Name, id) {
		s.fail(w, 409, fmt.Errorf("an evaluation set named %q already exists", in.Name))
		return
	}
	existing := map[string]RunSnapshot{}
	for i := range current.Runs {
		existing[current.Runs[i].TraceID] = current.Runs[i]
	}
	runs, err := s.snapshots(r.Context(), in.RunIDs, existing)
	if err != nil {
		s.fail(w, 422, err)
		return
	}
	current.Name, current.Description, current.Runs, current.Updated = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description), runs, time.Now().UTC()
	current.Revision++
	if err = s.saveSet(r.Context(), &current); err != nil {
		s.fail(w, 502, err)
		return
	}
	writeJSON(w, 200, current)
}

var errNotFound = errors.New("evaluation set not found")

func decodeSetInput(w http.ResponseWriter, r *http.Request) (setInput, bool) {
	var in setInput
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || strings.TrimSpace(in.Name) == "" || len([]rune(strings.TrimSpace(in.Name))) > 120 || len(in.RunIDs) > 1000 {
		http.Error(w, "invalid evaluation set", 400)
		return in, false
	}
	seen := map[string]bool{}
	for _, id := range in.RunIDs {
		if uuid.Validate(id) != nil || seen[id] {
			http.Error(w, "run_ids must contain unique UUIDs", 400)
			return in, false
		}
		seen[id] = true
	}
	return in, true
}

func duplicateName(sets []EvaluationSet, name, except string) bool {
	name = strings.TrimSpace(name)
	for i := range sets {
		if sets[i].ID != except && strings.EqualFold(sets[i].Name, name) {
			return true
		}
	}
	return false
}

func (s *service) snapshots(ctx context.Context, ids []string, existing map[string]RunSnapshot) ([]RunSnapshot, error) {
	catalog, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]RunSnapshot{}
	for i := range catalog {
		byID[catalog[i].TraceID] = catalog[i]
	}
	out := make([]RunSnapshot, 0, len(ids))
	for _, id := range ids {
		if old, ok := existing[id]; ok {
			_, old.SourcePresent = byID[id]
			out = append(out, old)
			continue
		}
		run, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("run %s does not exist", id)
		}
		out = append(out, run)
	}
	return out, nil
}

func (s *service) refreshPresence(ctx context.Context, set *EvaluationSet) error {
	ids, err := s.runIDs(ctx)
	if err != nil {
		return err
	}
	for i := range set.Runs {
		set.Runs[i].SourcePresent = ids[set.Runs[i].TraceID]
	}
	return nil
}

func (s *service) catalog(ctx context.Context) ([]RunSnapshot, error) {
	ids, err := s.runIDs(ctx)
	if err != nil {
		return nil, err
	}
	sem := make(chan struct{}, 8)
	ch := make(chan RunSnapshot, len(ids))
	for id := range ids {
		go func(id string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			ch <- s.readRun(ctx, id)
		}(id)
	}
	runs := make([]RunSnapshot, 0, len(ids))
	for range ids {
		runs = append(runs, <-ch)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Started.Equal(runs[j].Started) {
			return runs[i].TraceID > runs[j].TraceID
		}
		return runs[i].Started.After(runs[j].Started)
	})
	return runs, nil
}

// readRun builds the snapshot of one run. It does not fail: what it cannot
// read is named in the snapshot's IndexError, so the run is still listed.
func (s *service) readRun(ctx context.Context, id string) RunSnapshot {
	var rec struct {
		Started            time.Time `json:"started"`
		TraceID            string    `json:"trace_id"`
		DistributedTraceID string    `json:"distributed_trace_id"`
		Workflow           string    `json:"workflow"`
		InputArtifact      string    `json:"input_artifact"`
	}
	if err := s.getJSON(ctx, s.blobs+"/blobs/runs/"+id+"/run.json", &rec); err != nil {
		s.log.Warn("run record is unreadable", "run", id, "error", err)
		return RunSnapshot{TraceID: id, Agents: []string{}, Status: runStatusUnknown, SourcePresent: true, IndexError: "Cannot read run record"}
	}
	var status struct {
		Started       time.Time `json:"started"`
		Finished      time.Time `json:"finished"`
		Status        string    `json:"status"`
		DurationMS    int64     `json:"duration_ms"`
		DurationKnown bool      `json:"duration_known"`
	}
	run := RunSnapshot{TraceID: id, Workflow: rec.Workflow, Agents: []string{}, Started: rec.Started, InputArtifact: rec.InputArtifact, DistributedTraceID: rec.DistributedTraceID, SourcePresent: true}
	if err := s.getJSON(ctx, s.orchestrator+"/api/runs/"+id, &status); errors.Is(err, errNotFound) {
		run.Status, run.HistoryUnavailable = runStatusUnknown, true
	} else if err != nil {
		run.Status, run.IndexError = runStatusUnknown, "Cannot read run status"
	} else {
		if !status.Started.IsZero() {
			run.Started = status.Started
		}
		run.Status, run.Finished, run.DurationMS, run.DurationKnown = status.Status, status.Finished, status.DurationMS, status.DurationKnown
	}
	agents, complete, err := s.runAgents(ctx, id)
	run.Agents, run.AgentsComplete = agents, complete
	if err != nil {
		if run.IndexError != "" {
			run.IndexError += "; "
		}
		run.IndexError += "Cannot read agent records"
	}
	return run
}

func (s *service) runAgents(ctx context.Context, id string) (agents []string, complete bool, err error) {
	var listing struct {
		Objects []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	if s.getJSON(ctx, s.blobs+"/trace/"+id, &listing) != nil {
		return []string{}, false, fmt.Errorf("trace listing unavailable")
	}
	seen := map[string]bool{}
	for _, o := range listing.Objects {
		if !strings.HasSuffix(o.ID, ".run.json") || strings.Contains(o.ID, ".attempt-") || strings.HasSuffix(o.ID, "/run.json") {
			continue
		}
		var rec struct {
			Agent string `json:"agent"`
		}
		err = s.getJSON(ctx, s.blobs+"/blobs/"+o.ID, &rec)
		if err != nil {
			return nil, false, err
		}
		if rec.Agent != "" {
			seen[rec.Agent] = true
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out, true, nil
}

func (s *service) runIDs(ctx context.Context) (map[string]bool, error) {
	var list struct {
		Directories []string `json:"directories"`
	}
	if err := s.getJSON(ctx, s.blobs+"/list/runs", &list); err != nil {
		if errors.Is(err, errNotFound) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	out := map[string]bool{}
	for _, id := range list.Directories {
		if uuid.Validate(id) == nil {
			out[id] = true
		}
	}
	return out, nil
}

func (s *service) loadSets(ctx context.Context) ([]EvaluationSet, error) {
	var list struct {
		Objects []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	if err := s.getJSON(ctx, s.blobs+"/list/"+setPrefix, &list); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	sets := make([]EvaluationSet, 0, len(list.Objects))
	for _, o := range list.Objects {
		var set EvaluationSet
		if err := s.getJSON(ctx, s.blobs+"/blobs/"+o.ID, &set); err != nil {
			return nil, fmt.Errorf("reading %s: %w", o.ID, err)
		}
		sets = append(sets, set)
	}
	return sets, nil
}

func (s *service) loadSet(ctx context.Context, id string) (EvaluationSet, error) {
	if uuid.Validate(id) != nil {
		return EvaluationSet{}, errNotFound
	}
	var set EvaluationSet
	err := s.getJSON(ctx, s.blobs+"/blobs/"+setPrefix+"/"+id+".json", &set)
	if errors.Is(err, errNotFound) {
		return set, errNotFound
	}
	return set, err
}

func (s *service) saveSet(ctx context.Context, set *EvaluationSet) (err error) {
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.blobs+"/blobs/"+setPrefix+"/"+set.ID+".json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Creator", set.CreatedBy)
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("saving evaluation set: %s", resp.Status)
	}
	return nil
}

func (s *service) getJSON(ctx context.Context, url string, out any) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode == 404 {
		return errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

func (s *service) fail(w http.ResponseWriter, status int, err error) {
	s.log.Warn("evaluation request failed", "status", status, "error", err)
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func optionalTime(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, err
	}
	t = t.UTC()
	return &t, nil
}

func optionalInt(v string) (*int64, error) {
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("invalid integer")
	}
	return &n, nil
}

func matchesAgent(agents []string, want string) bool {
	for _, a := range agents {
		if a == want || !strings.Contains(want, "@") && strings.SplitN(a, "@", 2)[0] == want {
			return true
		}
	}
	return false
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.Format(time.RFC3339Nano) + "\n" + id))
}

func decodeCursor(v string) (time.Time, string, error) {
	data, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(data), "\n", 2)
	if len(parts) != 2 || uuid.Validate(parts[1]) != nil {
		return time.Time{}, "", fmt.Errorf("invalid")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return t, parts[1], err
}
