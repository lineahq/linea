package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/mcp"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestCaptureManagementAcrossCLIHTTPAndMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service, _, server := fleetPlatform(t)
	var out, stderr bytes.Buffer
	if code := captureCmd(ctx, []string{"group", "--server", server.URL, "--group", fleetcapturetest.Group}, &out, &stderr); code != exitOK {
		t.Fatal(stderr.String())
	}
	var group platform.CollectorGroup
	if err := json.Unmarshal(out.Bytes(), &group); err != nil || group.Kind != "static" {
		t.Fatal(group, err)
	}
	out.Reset()
	if code := captureCmd(ctx, []string{"group-members", "--server", server.URL, "--group", group.ID}, &out, &stderr); code != exitOK {
		t.Fatal(stderr.String())
	}
	var members platform.CollectorGroupMembersPage
	if err := json.Unmarshal(out.Bytes(), &members); err != nil || len(members.Items) != 1 || members.Items[0].CollectorIDs[0] != fleetcapturetest.Peer.ID {
		t.Fatal(members, err)
	}
	for _, args := range [][]string{{"group-create", "--server", server.URL}, {"group-delete", "--server", server.URL, "--group", group.ID}} {
		if captureCmd(ctx, args, &out, &stderr) != exitError {
			t.Fatalf("accepted %v", args)
		}
	}
	c, err := client.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.ListCollectorGroups(ctx, platform.CollectorPageRequest{})
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	mcpServer := httptest.NewServer(mcp.New(mcp.Options{Platform: service}))
	defer mcpServer.Close()
	session, err := sdk.NewClient(&sdk.Implementation{Name: "management-test", Version: "1"}, nil).Connect(ctx, &sdk.StreamableClientTransport{Endpoint: mcpServer.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "linea_list_collector_group_members", Arguments: map[string]any{"id": group.ID}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	owner := platform.Caller{Principal: platform.LocalPrincipal(), Client: "http"}
	capture, err := service.CreateCaptureSession(ctx, owner, platform.CaptureSelection{GroupID: group.ID, CapturePoints: []platform.CapturePointSelection{{Pipeline: "logs", Point: "egress"}}, Signals: []string{"logs"}})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "metadata.json")
	if err = os.WriteFile(file, []byte(`{"revision":1,"name":"CLI capture","description":"investigate checkout"}`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := captureCmd(ctx, []string{"update", "--server", server.URL, "--session", capture.ID, "--request", file}, &out, &stderr); code != exitOK {
		t.Fatal(stderr.String())
	}
	result, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "linea_update_capture_metadata", Arguments: map[string]any{"session_id": capture.ID, "request": map[string]any{"revision": 2, "name": "MCP capture", "description": "notes"}}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	got, err := service.GetCaptureSession(ctx, owner, capture.ID)
	if err != nil || got.Metadata.Name != "MCP capture" || got.Metadata.Revision != 3 {
		t.Fatal(got, err)
	}
	// Strict adapters reject attempted immutable-field edits and stale updates.
	request, _ := http.NewRequest(http.MethodPatch, server.URL+"/api/v1/captures/"+capture.ID, strings.NewReader(`{"revision":3,"name":"bad","description":"","selection":{}}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("immutable edit accepted", response.StatusCode)
	}
	if _, err = c.UpdateCaptureMetadata(ctx, capture.ID, platform.CaptureMetadataUpdate{Revision: 1, Name: "stale"}); err == nil {
		t.Fatal("stale metadata accepted")
	}
	result, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "linea_get_capture_session", Arguments: map[string]any{"session_id": capture.ID}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok && strings.Contains(text.Text, "notes") {
			t.Fatal("MCP summary exposed user content")
		}
	}
}
