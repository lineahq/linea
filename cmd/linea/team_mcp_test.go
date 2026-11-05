package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Startup writes synchronize tests with the real listener without polling or
// concurrent reads of a bytes.Buffer. Only startup notices reach this writer.
type serveNotices chan string

func (n serveNotices) Write(p []byte) (int, error) {
	n <- string(p)
	return len(p), nil
}

// bearerTransport presents $LINEA_TOKEN, the lab administrator's API token.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// mcpTransport connects an MCP client to a lab's /mcp with its API token.
func mcpTransport(baseURL string) *sdk.StreamableClientTransport {
	return &sdk.StreamableClientTransport{Endpoint: baseURL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{os.Getenv(serverTokenEnv)}}}
}

// startMCPServe starts a lab, whose serve mounts MCP, and returns a context
// that ends after 15 seconds with its URL. The third result is empty.
func startMCPServe(t *testing.T, opts ...labOption) (context.Context, string, string) {
	t.Helper()
	return startMCPServeFor(t, 15*time.Second, opts...)
}

// startMCPServeFor is startMCPServe with the timeout given.
func startMCPServeFor(t *testing.T, timeout time.Duration, opts ...labOption) (context.Context, string, string) {
	t.Helper()
	l := startLab(t, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx, l.URL, ""
}

func TestServeMountsMCPOnTheSharedPlatform(t *testing.T) {
	ctx, baseURL, _ := startMCPServe(t)
	session, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, mcpTransport(baseURL), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// The HTTP API shares the platform and the credential.
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/runs", nil)
	request.Header.Set("Authorization", "Bearer "+os.Getenv(serverTokenEnv))
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP runs status = %d", resp.StatusCode)
	}
	// MCP without the credential is refused.
	request, _ = http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/mcp", strings.NewReader(`{}`))
	anonymous, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("MCP without a token: status = %d", anonymous.StatusCode)
	}
}

// TestMCPServeReadsNoHostFileOrEnvironment: the HTTP API of a server that
// also serves MCP refuses host paths and reads no host environment variable.
func TestMCPServeReadsNoHostFileOrEnvironment(t *testing.T) {
	t.Setenv("OTEL_MCP_BINDING", "localhost")
	collectorYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {debug: {}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [debug]}}}\n"
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(collectorYAML), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, baseURL, _ := startMCPServe(t)
	post := func(body map[string]string, want int) map[string]any {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/config/validate", bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+os.Getenv(serverTokenEnv))
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("status = %d, want %d: %+v", resp.StatusCode, want, result)
		}
		return result
	}
	post(map[string]string{"config": file}, http.StatusBadRequest)
	result := post(map[string]string{"config_yaml": collectorYAML}, http.StatusOK)
	if result["valid"] != true {
		t.Fatalf("inline config = %+v", result)
	}
	envYAML := strings.Replace(collectorYAML, "debug: {}", "otlp: {endpoint: '${env:OTEL_MCP_BINDING}:4317'}", 1)
	envYAML = strings.Replace(envYAML, "exporters: [debug]", "exporters: [otlp]", 1)
	result = post(map[string]string{"config_yaml": envYAML}, http.StatusOK)
	// The host's value is never read: the name stands in with a warning.
	if encoded, _ := json.Marshal(result); result["valid"] != true || !strings.Contains(string(encoded), "reference_stand_in") {
		t.Fatalf("host environment read: %+v", result)
	}
}
