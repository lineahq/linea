// Smoke exercises the real Collector binary with synthetic OTLP over HTTP and gRPC.
// Build from collector/ to use the distribution's pinned dependencies.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type logBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *logBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: smoke COLLECTOR CONFIG")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(binary, config string) (result error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, port := range []string{"4317", "4318"} {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return fmt.Errorf("port %s is already occupied", port)
		}
	}
	var logs logBuffer
	cmd := exec.CommandContext(ctx, binary, "--config", config)
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		close(done)
	}()
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			result = errors.Join(result, errors.New("the Collector did not shut down within 3 seconds"))
		}
		if exitErr != nil {
			result = errors.Join(result, fmt.Errorf("the Collector exited: %w", exitErr))
		}
		if result != nil {
			fmt.Fprint(os.Stderr, logs.text())
		}
	}()
	readyCtx, readyCancel := context.WithTimeout(ctx, 20*time.Second)
	defer readyCancel()
	for _, port := range []string{"4317", "4318"} {
		for {
			select {
			case <-done:
				return fmt.Errorf("the Collector exited during startup: %v", exitErr)
			default:
			}
			conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
			if err == nil {
				conn.Close()
				break
			}
			select {
			case <-readyCtx.Done():
				return fmt.Errorf("the Collector did not become ready: %w", readyCtx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}

	conn, err := grpc.NewClient("127.0.0.1:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	var markers []string
	for _, protocol := range []string{"http", "grpc"} {
		for _, signal := range []string{"traces", "metrics", "logs"} {
			marker := fmt.Sprintf("smoke-%s-%s-%d", protocol, signal, time.Now().UnixNano())
			payload := makePayload(signal, marker)
			reqCtx, reqCancel := context.WithTimeout(ctx, 5*time.Second)
			var response []byte
			var err error
			if protocol == "grpc" {
				response, err = exportGRPC(reqCtx, conn, signal, payload)
			} else {
				response, err = exportHTTP(reqCtx, client, signal, payload)
			}
			reqCancel()
			if err != nil {
				return fmt.Errorf("%s %s: %w", protocol, signal, err)
			}
			if err := checkResponse(response); err != nil {
				return fmt.Errorf("%s %s: %w", protocol, signal, err)
			}
			markers = append(markers, marker)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		output := logs.text()
		allSeen := true
		for _, marker := range markers {
			if !strings.Contains(output, marker) {
				allSeen = false
			}
		}
		if allSeen {
			fmt.Println("Smoke passed: traces, metrics, and logs reached the debug exporter over HTTP and gRPC.")
			return nil
		}
		select {
		case <-done:
			return fmt.Errorf("the Collector exited before all signals were exported: %v", exitErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.New("not all six records reached the debug exporter")
		}
	}
}

func makePayload(signal, marker string) []byte {
	now := time.Now().UnixNano()
	switch signal {
	case "traces":
		return []byte(fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"livecapture-smoke"}}]},"scopeSpans":[{"scope":{"name":"livecapture.bootstrap"},"spans":[{"traceId":"0123456789abcdef0123456789abcdef","spanId":"0123456789abcdef","name":%q,"kind":1,"startTimeUnixNano":"%d","endTimeUnixNano":"%d"}]}]}]}`, marker, now, now))
	case "metrics":
		return []byte(fmt.Sprintf(`{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"livecapture-smoke"}}]},"scopeMetrics":[{"scope":{"name":"livecapture.bootstrap"},"metrics":[{"name":%q,"gauge":{"dataPoints":[{"timeUnixNano":"%d","asInt":"1"}]}}]}]}]}`, marker, now))
	default:
		return []byte(fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"livecapture-smoke"}}]},"scopeLogs":[{"scope":{"name":"livecapture.bootstrap"},"logRecords":[{"timeUnixNano":"%d","severityNumber":9,"body":{"stringValue":%q}}]}]}]}`, now, marker))
	}
}

func exportHTTP(ctx context.Context, client *http.Client, signal string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:4318/v1/"+signal, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", response.StatusCode, body)
	}
	return body, nil
}

func exportGRPC(ctx context.Context, conn *grpc.ClientConn, signal string, payload []byte) ([]byte, error) {
	switch signal {
	case "traces":
		request := ptraceotlp.NewExportRequest()
		if err := request.UnmarshalJSON(payload); err != nil {
			return nil, err
		}
		response, err := ptraceotlp.NewGRPCClient(conn).Export(ctx, request)
		if err != nil {
			return nil, err
		}
		return response.MarshalJSON()
	case "metrics":
		request := pmetricotlp.NewExportRequest()
		if err := request.UnmarshalJSON(payload); err != nil {
			return nil, err
		}
		response, err := pmetricotlp.NewGRPCClient(conn).Export(ctx, request)
		if err != nil {
			return nil, err
		}
		return response.MarshalJSON()
	default:
		request := plogotlp.NewExportRequest()
		if err := request.UnmarshalJSON(payload); err != nil {
			return nil, err
		}
		response, err := plogotlp.NewGRPCClient(conn).Export(ctx, request)
		if err != nil {
			return nil, err
		}
		return response.MarshalJSON()
	}
}

func checkResponse(body []byte) error {
	var response struct {
		PartialSuccess map[string]json.RawMessage `json:"partialSuccess"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("invalid OTLP response: %w", err)
	}
	for key, value := range response.PartialSuccess {
		if key == "errorMessage" {
			var message string
			if err := json.Unmarshal(value, &message); err != nil || message != "" {
				return fmt.Errorf("partial success: %s", body)
			}
		} else if strings.HasPrefix(key, "rejected") {
			count := strings.Trim(string(value), `"`)
			if count != "0" {
				return fmt.Errorf("partial success: %s", body)
			}
		}
	}
	return nil
}
