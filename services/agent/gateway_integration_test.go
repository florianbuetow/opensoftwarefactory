package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func integrationBuild(t *testing.T, dir, target string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fixture")
	cmd := exec.Command("go", "build", "-o", out, target) //nolint:gosec // permitted: the test builds its fixture into its own temp folder
	cmd.Dir = dir
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v: %s", target, err, data)
	}
	return out
}

// integrationGet sends a GET request to endpoint.
func integrationGet(endpoint string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(request)
}

func integrationAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func integrationLaunch(t *testing.T, env []string, binary string, args ...string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), env...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

func integrationWaitHTTP(t *testing.T, endpoint string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := integrationGet(endpoint)
		if err == nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if response.StatusCode/100 == 2 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("endpoint did not become ready: %s", endpoint)
}

func integrationPutConfig(t *testing.T, gateway, name string, config map[string]any) {
	t.Helper()
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPut, gateway+"/api/services/"+name, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("configure %s: HTTP %d: %s", name, response.StatusCode, data)
	}
}

func integrationToolCall(name, arguments string) toolCall {
	var call toolCall
	call.ID = "test-" + name
	call.Type = "function"
	call.Function.Name = name
	call.Function.Arguments = arguments
	return call
}

func TestAgentUsesRealGatewayForHTTPAndStdioTools(t *testing.T) {
	blobBinary := integrationBuild(t, "../blobstore", ".")
	gatewayBinary := integrationBuild(t, "../mcp-gateway", ".")
	mockBinary := integrationBuild(t, "../mcp-gateway", "./testdata/mock")

	blobAddress := integrationAddr(t)
	blobURL := "http://" + blobAddress
	integrationLaunch(t, nil, blobBinary, "-addr", blobAddress, "-data", t.TempDir())
	integrationWaitHTTP(t, blobURL+"/healthz")

	mockAddress := integrationAddr(t)
	mockURL := "http://" + mockAddress
	integrationLaunch(t, nil, mockBinary, "-http", mockAddress)
	integrationWaitHTTP(t, mockURL+"/calls")

	gatewayAddress := integrationAddr(t)
	gatewayURL := "http://" + gatewayAddress
	integrationLaunch(t, []string{"BLOBSTORE_URL=" + blobURL}, gatewayBinary, "-addr", gatewayAddress)
	integrationWaitHTTP(t, gatewayURL+"/healthz")

	policy := map[string]any{"allowlist": []string{"echo", "blocked"}, "denylist": []string{"blocked"}}
	httpConfig := map[string]any{"transport": "streamable-http", "url": mockURL + "/mcp"}
	integrationPutConfig(t, gatewayURL, "docs", map[string]any{"mcpconfig": httpConfig, "allowlist": policy["allowlist"], "denylist": policy["denylist"]})
	const stdioCallsName = "stdio-calls"
	stdioDir := t.TempDir()
	stdioCalls := filepath.Join(stdioDir, stdioCallsName)
	stdioConfig := map[string]any{"transport": "stdio", "command": mockBinary, "args": []string{}, "env": map[string]string{"MOCK_CALLS_FILE": stdioCalls}}
	integrationPutConfig(t, gatewayURL, "code", map[string]any{"mcpconfig": stdioConfig, "allowlist": policy["allowlist"], "denylist": policy["denylist"]})

	service := &Service{
		MCPGatewayURL: gatewayURL,
		HTTP:          http.DefaultClient,
		Logger:        slog.New(slog.DiscardHandler),
	}
	modelTools, routes, err := service.discoverTools(context.Background(), []string{"docs/echo", "code/echo", "docs/blocked"}, service.Logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(modelTools) != 2 || routes["docs__echo"].client == nil || routes["code__echo"].client == nil {
		t.Fatalf("discovered tools/routes = %v / %v", modelTools, routes)
	}
	if _, exists := routes["docs__blocked"]; exists {
		t.Fatal("gateway-denied tool was exposed to the agent")
	}
	defer closeToolRoutes(routes, service.Logger)

	for _, tc := range []struct {
		alias, text string
	}{
		{"docs__echo", "from http"},
		{"code__echo", "from stdio"},
	} {
		result, failed, runErr := service.runTool(context.Background(), routes, integrationToolCall(tc.alias, `{"text":"`+tc.text+`"}`))
		if runErr != nil {
			t.Fatal(runErr)
		}
		if failed {
			t.Fatalf("%s was reported as a tool failure", tc.alias)
		}
		if result != tc.text {
			t.Fatalf("%s result = %q, want %q", tc.alias, result, tc.text)
		}
	}

	deniedClient := &mcpClient{url: gatewayURL + "/mcp/docs", http: http.DefaultClient}
	if connectErr := deniedClient.connect(context.Background()); connectErr != nil {
		t.Fatal(connectErr)
	}
	defer func() {
		if closeErr := deniedClient.close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if _, _, callErr := deniedClient.callTool(context.Background(), "blocked", json.RawMessage(`{"text":"must not run"}`)); callErr == nil {
		t.Fatal("direct gateway call to denied tool succeeded")
	}

	response, err := integrationGet(mockURL + "/calls")
	if err != nil {
		t.Fatal(err)
	}
	var httpCalls []string
	decodeErr := json.NewDecoder(response.Body).Decode(&httpCalls)
	if closeErr := response.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if len(httpCalls) != 1 || httpCalls[0] != "echo" {
		t.Fatalf("HTTP upstream calls = %v", httpCalls)
	}
	// The mock wrote its calls into a folder this test owns; the read is confined to that folder.
	stdioRoot, err := os.OpenRoot(stdioDir)
	if err != nil {
		t.Fatal(err)
	}
	stdioData, err := stdioRoot.ReadFile(stdioCallsName)
	if closeErr := stdioRoot.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stdioData)) != "echo" {
		t.Fatalf("stdio upstream calls = %q", stdioData)
	}
}
