package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

import "opensoftwarefactory/orchestrator/pipeline"

// isVersion7 tells whether id is a version 7 UUID.
func isVersion7(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.Version() == 7
}

// noRandom is a random source that always fails.
type noRandom struct{}

func (noRandom) Read([]byte) (int, error) { return 0, errors.New("no random source") }

func TestNewRunIDIsVersion7AndNew(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id, err := newRunID()
		if err != nil {
			t.Fatal(err)
		}
		if !isVersion7(id) {
			t.Fatalf("run id %q is not a version 7 UUID", id)
		}
		if seen[id] {
			t.Fatalf("run id %q was generated twice", id)
		}
		seen[id] = true
	}
}

func TestNewRunIDFailsWithoutRandomSource(t *testing.T) {
	uuid.SetRand(noRandom{})
	defer uuid.SetRand(nil)
	if id, err := newRunID(); err == nil {
		t.Fatalf("run id %q generated without a random source", id)
	}
}

func TestManualStartsGetDistinctVersion7RunIDs(t *testing.T) {
	srv, runner, _, _ := newTestServer(t)
	ids := map[string]bool{}
	for range 2 {
		code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs", "same input")
		if code != http.StatusAccepted {
			t.Fatalf("start = %d %s", code, body)
		}
		var started struct {
			TraceID string `json:"trace_id"`
		}
		if err := json.Unmarshal([]byte(body), &started); err != nil {
			t.Fatal(err)
		}
		if !isVersion7(started.TraceID) {
			t.Fatalf("run id %q is not a version 7 UUID", started.TraceID)
		}
		ids[started.TraceID] = true
	}
	if len(ids) != 2 || len(runner.started) != 2 || runner.started[0].TraceID == runner.started[1].TraceID {
		t.Fatalf("two starts of the same input must be two runs: ids=%v started=%d", ids, len(runner.started))
	}
}

func TestManualStartFailsWithoutRandomSource(t *testing.T) {
	srv, runner, store, _ := newTestServer(t)
	uuid.SetRand(noRandom{})
	defer uuid.SetRand(nil)
	code, body := call(t, http.MethodPost, srv.URL+"/api/workflows/sample/runs", "hello")
	if code != http.StatusInternalServerError || len(runner.started) != 0 || len(store.data) != 0 {
		t.Fatalf("start without a run id = %d %s, started=%d, stored=%d", code, body, len(runner.started), len(store.data))
	}
}

// intakeTemporalFake stands in for Temporal on intake starts. It knows the run IDs it
// started, refuses to start one twice and answers it as reused, and forgets a removed run,
// as Temporal does. With receipts it writes the receipt the run's first activity writes.
type intakeTemporalFake struct {
	*fakeRunner
	store    *memStore
	known    map[string]string // run ID -> execution ID
	startErr error
	started  []string // the run ID of every execution started, in order
	receipts bool
}

func (f *intakeTemporalFake) StartIntake(ctx context.Context, in *pipeline.Input, _ string) (executionID string, reused bool, err error) {
	if f.startErr != nil {
		return "", false, f.startErr
	}
	if known, ok := f.known[in.TraceID]; ok {
		return known, true, nil
	}
	f.started = append(f.started, in.TraceID)
	executionID = fmt.Sprintf("execution-%d", len(f.started))
	f.known[in.TraceID] = executionID
	if f.receipts {
		receipt, _ := json.Marshal(pipeline.IntakeReceipt{
			SchemaVersion: 1, BindingID: in.Intake.BindingID, BindingRevision: in.Intake.BindingRevision, EventID: in.Intake.EventID,
			TraceID: in.TraceID, ExecutionID: executionID, Workflow: in.Definition.Name, AcceptanceHash: in.Intake.AcceptanceHash, Phase: "initialized",
		})
		if putErr := f.store.Put(ctx, "intake-receipts/"+in.TraceID+".json", receipt, pipeline.Meta{}); putErr != nil {
			return "", false, putErr
		}
	}
	return executionID, false, nil
}

func (f *intakeTemporalFake) Remove(ctx context.Context, traceID string) error {
	delete(f.known, traceID)
	return f.fakeRunner.Remove(ctx, traceID)
}

func newIntakeTestServer(t *testing.T, receipts bool) (*httptest.Server, *intakeTemporalFake, *memStore) {
	t.Helper()
	store := newMemStore()
	runner := &intakeTemporalFake{fakeRunner: &fakeRunner{}, store: store, known: map[string]string{}, receipts: receipts}
	s := &server{
		workflows: storeWorkflows(t, "workflows:\n  - name: group/score\n    steps:\n      - id: s\n        service: svc\n"),
		blobs:     store, runner: runner, logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return srv, runner, store
}

// intakeBody is the start request of one event on the binding b1.
func intakeBody(eventID, payload string) string {
	return `{"binding_id":"b1","event_id":"` + eventID + `","workflow":"group/score","workflow_version":1,"binding_revision":"r1","event":` + payload + `,"binding":{"enabled":true}}`
}

func startIntake(t *testing.T, srv *httptest.Server, body string) (status int, result intakeRunResult) {
	t.Helper()
	status, answer := call(t, http.MethodPost, srv.URL+"/api/intake/runs", body)
	if status == http.StatusAccepted {
		if err := json.Unmarshal([]byte(answer), &result); err != nil {
			t.Fatal(err)
		}
	}
	return status, result
}

// claims counts the stored intake claims.
func claims(store *memStore) int {
	n := 0
	for id := range store.data {
		if strings.HasPrefix(id, intakeClaimPrefix+"/") {
			n++
		}
	}
	return n
}

func TestIntakeStartsGetDistinctVersion7RunIDs(t *testing.T) {
	srv, runner, store := newIntakeTestServer(t, true)
	_, first := startIntake(t, srv, intakeBody("e1", `{"a":1}`))
	_, second := startIntake(t, srv, intakeBody("e2", `{"a":1}`))
	if !isVersion7(first.TraceID) || !isVersion7(second.TraceID) || first.TraceID == second.TraceID {
		t.Fatalf("two events must start two runs with version 7 ids: %q %q", first.TraceID, second.TraceID)
	}
	if first.Reused || second.Reused || len(runner.started) != 2 || claims(store) != 2 {
		t.Fatalf("reused=%v/%v started=%d claims=%d", first.Reused, second.Reused, len(runner.started), claims(store))
	}
}

func TestSameIntakeEventNamesTheClaimedRun(t *testing.T) {
	for name, receipts := range map[string]bool{"run left its receipt": true, "run has no receipt yet": false} {
		t.Run(name, func(t *testing.T) {
			srv, runner, store := newIntakeTestServer(t, receipts)
			status, first := startIntake(t, srv, intakeBody("e1", `{"a":1}`))
			if status != http.StatusAccepted || first.Reused || first.ExecutionID != "execution-1" {
				t.Fatalf("first = %d %+v", status, first)
			}
			status, again := startIntake(t, srv, intakeBody("e1", `{"a":1}`))
			if status != http.StatusAccepted || !again.Reused || again.TraceID != first.TraceID || again.ExecutionID != first.ExecutionID {
				t.Fatalf("again = %d %+v, first %+v", status, again, first)
			}
			if len(runner.started) != 1 || claims(store) != 1 {
				t.Fatalf("one event must start one run: started=%d claims=%d", len(runner.started), claims(store))
			}
		})
	}
}

func TestIntakeEventWithAnotherPayloadIsAConflict(t *testing.T) {
	srv, runner, store := newIntakeTestServer(t, true)
	if status, _ := startIntake(t, srv, intakeBody("e1", `{"a":1}`)); status != http.StatusAccepted {
		t.Fatalf("first = %d", status)
	}
	status, answer := call(t, http.MethodPost, srv.URL+"/api/intake/runs", intakeBody("e1", `{"a":2}`))
	if status != http.StatusPreconditionFailed || !strings.Contains(answer, errIntakeConflict.Error()) {
		t.Fatalf("another payload = %d %s", status, answer)
	}
	if len(runner.started) != 1 || claims(store) != 1 {
		t.Fatalf("a conflict must start nothing: started=%d claims=%d", len(runner.started), claims(store))
	}
}

func TestIntakeEventAfterItsRunWasRemovedStartsNothing(t *testing.T) {
	srv, runner, store := newIntakeTestServer(t, true)
	_, first := startIntake(t, srv, intakeBody("e1", `{"a":1}`))
	if code, body := call(t, http.MethodDelete, srv.URL+"/api/runs/"+first.TraceID, ""); code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", code, body)
	}
	if _, known := runner.known[first.TraceID]; known {
		t.Fatal("the removed run is still known to the runner")
	}
	status, again := startIntake(t, srv, intakeBody("e1", `{"a":1}`))
	if status != http.StatusAccepted || !again.Reused || again.TraceID != first.TraceID || again.ExecutionID != first.ExecutionID {
		t.Fatalf("after removal = %d %+v, first %+v", status, again, first)
	}
	if len(runner.started) != 1 || claims(store) != 1 {
		t.Fatalf("a removed run's event must start no run and claim no second id: started=%v claims=%d", runner.started, claims(store))
	}
}

func TestIntakeClaimWithoutARunIsStartedUnderTheClaimedID(t *testing.T) {
	srv, runner, store := newIntakeTestServer(t, true)
	// The claim is written and the start fails, as when the orchestrator stops in between.
	runner.startErr = errors.New("temporal unavailable")
	if status, _ := startIntake(t, srv, intakeBody("e1", `{"a":1}`)); status != http.StatusServiceUnavailable {
		t.Fatalf("failed start = %d", status)
	}
	if len(runner.started) != 0 || claims(store) != 1 {
		t.Fatalf("started=%d claims=%d", len(runner.started), claims(store))
	}
	var claim intakeClaim
	if err := json.Unmarshal(store.data[intakeClaimID("b1", "e1")], &claim); err != nil || !isVersion7(claim.RunID) {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
	runner.startErr = nil
	status, retried := startIntake(t, srv, intakeBody("e1", `{"a":1}`))
	if status != http.StatusAccepted || retried.Reused || retried.TraceID != claim.RunID {
		t.Fatalf("retry = %d %+v, claimed %s", status, retried, claim.RunID)
	}
	if !reflect.DeepEqual(runner.started, []string{claim.RunID}) || claims(store) != 1 {
		t.Fatalf("the retry must start the claimed run once: started=%v claims=%d", runner.started, claims(store))
	}
}

func TestIntakeNeedsACreateOnlyStore(t *testing.T) {
	// A store without a create-only write cannot hold a claim: the start is refused.
	plain := struct{ pipeline.Store }{newMemStore()}
	s := &server{
		workflows: storeWorkflows(t, "workflows:\n  - name: group/score\n    steps:\n      - id: s\n        service: svc\n"),
		blobs:     plain, runner: &intakeTemporalFake{fakeRunner: &fakeRunner{}, known: map[string]string{}}, logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/intake/runs", strings.NewReader(intakeBody("e1", `{"a":1}`))))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a create-only store = %d %s", rec.Code, rec.Body.String())
	}
}

func TestInvalidIntakeClaimFailsTheStart(t *testing.T) {
	srv, runner, store := newIntakeTestServer(t, true)
	store.data[intakeClaimID("b1", "e1")] = []byte(`{"schema_version":1,"run_id":"not-a-uuid","binding_id":"b1","event_id":"e1","acceptance_hash":"x"}`)
	if status, _ := startIntake(t, srv, intakeBody("e1", `{"a":1}`)); status != http.StatusServiceUnavailable || len(runner.started) != 0 {
		t.Fatalf("invalid claim = %d, started=%d", status, len(runner.started))
	}
}

// TestRunIDsHaveOneSource fails when a file of the orchestrator other than runid.go
// generates a UUID. The one exception is the ID of a workflow event, which is derived from
// its execution and type so that the same event is never published twice.
func TestRunIDsHaveOneSource(t *testing.T) {
	generator := regexp.MustCompile(`uuid\.(New[A-Za-z0-9]*)\(`)
	found := map[string][]string{}
	// The package's own directory, read through a file system rooted there.
	sources := os.DirFS(".")
	err := fs.WalkDir(sources, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, readErr := fs.ReadFile(sources, path)
		if readErr != nil {
			return readErr
		}
		for _, match := range generator.FindAllStringSubmatch(string(source), -1) {
			found[path] = append(found[path], match[1])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"runid.go": {"NewV7"}, "events.go": {"NewSHA1"}}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("UUIDs are generated in %v, want only %v: a run id comes from newRunID", found, want)
	}
}
