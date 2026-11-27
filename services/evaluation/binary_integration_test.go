package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBinaryMeasurementJourney launches the real blobstore and evaluation
// executables.
func TestBinaryMeasurementJourney(t *testing.T) {
	tmp := t.TempDir()
	blobBin := filepath.Join(tmp, "blobstore")
	evaluationBin := filepath.Join(tmp, "evaluation")
	runBuild(t, "../blobstore", blobBin)
	runBuild(t, ".", evaluationBin)
	blobAddr := freeAddress(t)
	evaluationAddr := freeAddress(t)
	blob := exec.Command(blobBin, "-data", filepath.Join(tmp, "blob-data"), "-addr", blobAddr) //nolint:gosec // permitted: the test starts the binary it has just built
	blob.Env = os.Environ()
	if err := blob.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(blob) })
	waitHTTP(t, "http://"+blobAddr+"/healthz")
	orchestrator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer orchestrator.Close()
	openobserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "projection intentionally unavailable in this canonical-write journey", http.StatusServiceUnavailable)
	}))
	defer openobserve.Close()
	config := filepath.Join(tmp, "openobserve.env")
	if err := os.WriteFile(config, []byte("ZO_ROOT_USER_EMAIL=test@example.com\nZO_ROOT_USER_PASSWORD=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	evaluation := exec.Command(evaluationBin) //nolint:gosec // permitted: the test starts the binary it has just built
	evaluation.Env = append(os.Environ(), "EVALUATION_ADDR="+evaluationAddr, "BLOBSTORE_URL=http://"+blobAddr, "ORCHESTRATOR_URL="+orchestrator.URL, "OPENOBSERVE_URL="+openobserve.URL, "OPENOBSERVE_CONFIG="+config, "EVALUATION_LOCK_PATH="+filepath.Join(tmp, "writer.lock"), "EVALUATION_PROJECTION_STREAM=factory_measurements_test_binary")
	if err := evaluation.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(evaluation) })
	waitHTTP(t, "http://"+evaluationAddr+"/healthz")
	price := `{"vendor":"v","model":"m","effort":"none","modes":{},"from":"2026-01-01T00:00:00Z","to":null,"currency":"EUR","unit_tokens":1000,"input_uncached":"2","input_cached":"1","output":"4"}`
	if status, body := integrationRequest(t, http.MethodPost, "http://"+evaluationAddr+"/api/prices", price); status != 200 {
		t.Fatalf("price %d %s", status, body)
	}
	var prices struct {
		Prices []Price `json:"prices"`
	}
	integrationGET(t, "http://"+evaluationAddr+"/api/prices", &prices)
	if len(prices.Prices) != 1 || prices.Prices[0].Vendor != "v" || prices.Prices[0].Revision == "" {
		t.Fatalf("price list %+v", prices)
	}
	usage := `{"schema_version":1,"id":"attempt","kind":"usage","run_id":"run","execution_id":"execution","occurred_at":"2026-11-01T12:00:00Z","vendor":"v","model":"m","effort":"none","modes":{},"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50,"status":"reported","missing":[],"raw_usage":{"prompt_tokens":1000}}`
	if status, body := integrationRequest(t, http.MethodPut, "http://"+evaluationAddr+"/api/measurements/attempt", usage); status != 200 {
		t.Fatalf("usage %d %s", status, body)
	}
	var measurements struct {
		Measurements []Measurement `json:"measurements"`
	}
	integrationGET(t, "http://"+evaluationAddr+"/api/measurements?kind=usage", &measurements)
	if len(measurements.Measurements) != 1 || measurements.Measurements[0].ID != "attempt" || measurements.Measurements[0].Revision == "" {
		t.Fatalf("measurement list %+v", measurements)
	}
	var cost CurrentCost
	waitJSON(t, "http://"+blobAddr+"/blobs/measurement-costs/attempt.json", &cost)
	if cost.Amount != "2" || cost.ConvertedAmount != "2" || cost.Currency != "EUR" || cost.DisplayCurrency != "EUR" || cost.Status != costAvailable {
		t.Fatalf("current cost %+v prices=%+v", cost, prices.Prices)
	}
}

func runBuild(t *testing.T, pkg, out string) {
	t.Helper()
	cmd := exec.Command("go", "-C", pkg, "build", "-o", out, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, output)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready", url)
}

func integrationRequest(t *testing.T, method, url, body string) (status int, answer string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-None-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp.Body)
	var payload any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	return resp.StatusCode, fmt.Sprint(payload)
}

func integrationGET(t *testing.T, url string, out any) {
	t.Helper()
	resp := get(t, url)
	defer closeBody(t, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func waitJSON(t *testing.T, url string, out any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == 200 {
			err = json.NewDecoder(resp.Body).Decode(out)
			_ = resp.Body.Close()
			if err == nil {
				return
			}
		} else if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s was not available", url)
}

func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}
