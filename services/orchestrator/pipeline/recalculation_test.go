package pipeline

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

type recalcRoundTrip func(*http.Request) (*http.Response, error)

func (f recalcRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRecalculationBatchUsesPinnedBoundedContract(t *testing.T) {
	client := &http.Client{Transport: recalcRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/recalculations/job-1/batch" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		body := string(data)
		for _, want := range []string{`"display_currency":"EUR"`, `"cursor":"20"`, `"limit":100`} {
			if !strings.Contains(body, want) {
				t.Fatalf("body %s missing %s", body, want)
			}
		}
		return &http.Response{StatusCode: 200, Status: "OK", Body: io.NopCloser(strings.NewReader(`{"processed":10,"pending":0,"done":true}`)), Header: make(http.Header)}, nil
	})}
	a := Activities{EvaluationURL: "http://evaluation:8080", HTTPClient: client}
	out, err := a.RecalculationBatch(context.Background(), RecalculationInput{ID: "job-1", DisplayCurrency: "EUR", Cursor: "20", Limit: 100})
	if err != nil || !out.Done || out.Processed != 10 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestRecalculationWaitsForProjectionCompletion(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	calls := 0
	env.RegisterActivityWithOptions(func(context.Context, RecalculationInput) (RecalculationBatchResult, error) {
		calls++
		if calls == 1 {
			return RecalculationBatchResult{Processed: 10, Pending: 0, Done: false, ProjectionPending: true, Error: "projection_pending", Status: "running"}, nil
		}
		return RecalculationBatchResult{Done: true, Status: "complete"}, nil
	}, activity.RegisterOptions{Name: RecalculationBatchActivity})
	env.ExecuteWorkflow(Recalculation, RecalculationInput{ID: "job", DisplayCurrency: "EUR"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("batch calls=%d", calls)
	}
}
