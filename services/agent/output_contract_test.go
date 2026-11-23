package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type cancelAfterArtifactStore struct {
	Store
	cancel   context.CancelFunc
	artifact string
}

// targetRun is the run the judge in these tests scores.
const targetRun = "target"

func (s cancelAfterArtifactStore) Put(ctx context.Context, id string, data []byte, meta Meta) error {
	if id == s.artifact {
		err := s.Store.Put(context.WithoutCancel(ctx), id, data, meta)
		s.cancel()
		return err
	}
	return s.Store.Put(ctx, id, data, meta)
}

func TestJudgeOutputStoredThenExtractionSubmitted(t *testing.T) {
	contract := `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"output_contract":{"format":"json","mappings":[{"path":"/score","name":"quality","type":"number"},{"path":"/finding","name":"finding","type":"string"}]}}`
	f := newFakes(t, contract, respond(`{"choices":[{"message":{"role":"assistant","content":"{\"score\":0.8,\"finding\":\"good\"}"}}]}`))
	var extraction extractionRequest
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		artifact := append([]byte(nil), f.blobs[planArtifact]...)
		f.mu.Unlock()
		if string(artifact) != `{"score":0.8,"finding":"good"}` {
			t.Errorf("artifact not durable before extraction: %q", artifact)
		}
		if err := json.NewDecoder(r.Body).Decode(&extraction); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer eval.Close()
	f.svc.EvaluationURL = eval.URL
	req := request()
	req.ProducerRunID = "producer"
	req.TargetRunID = targetRun
	req.TargetExecutionID = "target-exec"
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if format, ok := f.llmReqs[0]["response_format"].(map[string]any); !ok || format["type"] != "json_object" {
		t.Fatalf("response format %v", f.llmReqs[0]["response_format"])
	}
	if extraction.ProducerRunID != "producer" || extraction.TargetRunID != targetRun || extraction.TargetExecutionID != "target-exec" || extraction.ArtifactID != planArtifact || extraction.AgentRef != intakeRef || extraction.MappingVersion != intakeRef || len(extraction.Mappings) != 2 {
		t.Fatalf("extraction %+v", extraction)
	}
}

func TestInvalidJudgeOutputIsPreservedWithoutPaidRerun(t *testing.T) {
	contract := `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"output_contract":{"format":"json","mappings":[{"path":"/score","name":"quality","type":"number"}]}}`
	f := newFakes(t, contract, respond(`{"choices":[{"message":{"role":"assistant","content":"not json"}}]}`))
	calls := 0
	status := http.StatusUnprocessableEntity
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
	}))
	defer eval.Close()
	f.svc.EvaluationURL = eval.URL
	req := request()
	req.TargetRunID = targetRun
	req.TargetExecutionID = "exec"
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(f.llmReqs) != 1 || calls != 1 || string(f.blobs[planArtifact]) != "not json" {
		t.Fatalf("llm=%d extraction=%d artifact=%q", len(f.llmReqs), calls, f.blobs[planArtifact])
	}
	var record RunRecord
	if err := json.Unmarshal(f.blobs["runs/t1/plan.run.json"], &record); err != nil {
		t.Fatal(err)
	}
	if record.OutputValidation == "" || record.OutputValidation == outputValid {
		t.Fatalf("validation %q", record.OutputValidation)
	}
	f.mu.Lock()
	failedStatus := false
	pending := false
	for id, data := range f.blobs {
		if strings.HasPrefix(id, extractionIntentPrefix+"/") {
			var intent extractionIntent
			if err := json.Unmarshal(data, &intent); err != nil {
				t.Errorf("stored intent %s: %v", id, err)
			}
			failedStatus = intent.Status == intentFailed && intent.LastError != ""
		}
		pending = pending || strings.HasPrefix(id, extractionPendingPrefix+"/")
	}
	f.mu.Unlock()
	if !failedStatus || !pending {
		t.Fatalf("durable failure status=%v pending=%v", failedStatus, pending)
	}
	status = http.StatusCreated
	if _, err := f.svc.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(f.llmReqs) != 1 {
		t.Fatalf("replay calls=%d llm=%d", calls, len(f.llmReqs))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.blobs {
		if strings.HasPrefix(id, extractionPendingPrefix+"/") {
			t.Fatalf("pending marker survived replay: %s", id)
		}
	}
}

func TestValidateOutputMappings(t *testing.T) {
	c := OutputContract{Format: "json", Mappings: []OutputMapping{{Path: "/nested/~1value", Name: "n", Type: "number"}, {Path: "/text", Name: "t", Type: "string"}}}
	if got := validateOutput(`{"nested":{"/value":1.25},"text":"ok"}`, c); got != outputValid {
		t.Fatal(got)
	}
	if got := validateOutput(`{"nested":{"/value":"bad"},"text":"ok"}`, c); got == outputValid {
		t.Fatal("wrong type accepted")
	}
}

func TestCancellationAfterArtifactDoesNotLoseExtractionOrRerun(t *testing.T) {
	contract := `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"],"output_contract":{"format":"json","mappings":[{"path":"/score","name":"quality","type":"number"}]}}`
	f := newFakes(t, contract, respond(`{"choices":[{"message":{"role":"assistant","content":"{\"score\":1}"}}]}`))
	calls := 0
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusCreated) }))
	defer eval.Close()
	f.svc.EvaluationURL = eval.URL
	ctx, cancel := context.WithCancel(context.Background())
	f.svc.Store = cancelAfterArtifactStore{Store: f.svc.Store, artifact: planArtifact, cancel: cancel}
	req := request()
	req.TargetRunID = targetRun
	req.TargetExecutionID = "exec"
	if _, err := f.svc.Process(ctx, req); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(f.llmReqs) != 1 {
		t.Fatalf("extraction=%d llm=%d", calls, len(f.llmReqs))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for id, data := range f.blobs {
		if strings.HasPrefix(id, extractionIntentPrefix+"/") {
			var intent extractionIntent
			if err := json.Unmarshal(data, &intent); err != nil {
				t.Fatal(err)
			}
			found = found || intent.Status == intentDelivered
		}
	}
	if !found {
		t.Fatal("durable delivered extraction status missing")
	}
}

func TestReconcileExtractionRecreatesMissingPendingMarker(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, respond(finalResponse))
	intent := extractionIntent{ID: "orphan", Request: extractionRequest{ArtifactID: planArtifact, AgentRef: intakeRef}, Status: intentFailed, LastError: "evaluation unavailable"}
	data, _ := json.Marshal(intent)
	f.mu.Lock()
	f.blobs[extractionIntentPrefix+"/orphan.json"] = data
	f.mu.Unlock()
	if err := f.svc.reconcileExtractionIntents(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.blobs[extractionPendingPrefix+"/orphan.json"]; !ok {
		t.Fatal("orphan extraction was not made replayable")
	}
}

func TestPeriodicRunReconciliationDeliversObligationWithoutPaidCall(t *testing.T) {
	f := newFakes(t, `{"schema_version":1,"name":"intake","unpack_inputs":true,"models":["m1"]}`, respond(finalResponse))
	calls := 0
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusCreated) }))
	defer eval.Close()
	f.svc.EvaluationURL = eval.URL
	intent := &extractionIntent{ID: "from-run", Request: extractionRequest{ProducerRunID: "producer", TargetRunID: targetRun, TargetExecutionID: "exec", ArtifactID: planArtifact, AgentRef: intakeRef, MappingVersion: intakeRef}, Status: intentPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	record := RunRecord{Agent: intakeRef, RunID: "producer", Attempt: 1, Status: statusCompleted, ExtractionObligation: intent}
	data, _ := json.Marshal(record)
	f.mu.Lock()
	f.blobs[planArtifact] = []byte(`{"score":1}`)
	f.blobs["runs/t1/plan.run.json"] = data
	f.mu.Unlock()
	if err := f.svc.reconcileCompletedRunObligations(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(f.llmReqs) != 0 {
		t.Fatalf("extraction=%d paid_calls=%d", calls, len(f.llmReqs))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delivered := false
	for id, data := range f.blobs {
		if strings.HasPrefix(id, extractionIntentPrefix+"/") {
			var got extractionIntent
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			delivered = delivered || got.Status == intentDelivered
		}
	}
	if !delivered {
		t.Fatal("reconciled delivery status missing")
	}
}
