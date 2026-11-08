package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
)

// The catalog prints the same JSON locally, through a server and over HTTP.
func TestRuntimeCatalogParity(t *testing.T) {
	ctx := context.Background()
	baseURL := startServe(t)
	local := parityCLI(t, ctx, []string{"runtime", "catalog", "--format", "json"})
	connected := parityCLI(t, ctx, []string{"runtime", "catalog", "--format", "json", "--server", baseURL})
	support := parityCLI(t, ctx, []string{"runtime", "versions", "--format", "json"})
	if !bytes.Equal(local, connected) || !bytes.Contains(local, []byte(`"components"`)) {
		t.Fatalf("local and connected catalogs differ:\n%.300s\n---\n%.300s", local, connected)
	}
	var table struct {
		Verified []string `json:"verified"`
	}
	decodeParityJSON(t, support, &table)
	newest := table.Verified[len(table.Verified)-1]
	httpJSON := parityHTTP(t, ctx, http.MethodGet, baseURL+"/api/v1/collector-versions/"+newest+"/catalog", nil, http.StatusOK)
	assertParityBytes(t, "catalog", bytes.TrimSpace(reindent(t, httpJSON)), bytes.TrimSpace(local))
	if code, out, _ := runCLI(t, "runtime", "catalog"); code != exitOK || !strings.HasPrefix(out, "Collector "+newest+": ") || !strings.Contains(out, "KIND") {
		t.Fatalf("catalog text = %d %.200s", code, out)
	}
	if code, _, errOut := runCLI(t, "runtime", "catalog", "--version", "0.1.0"); code != exitError || !strings.Contains(errOut, "has no catalog") {
		t.Fatalf("catalog of an unverified release = %d %s", code, errOut)
	}
}

// reindent formats JSON the way the CLI prints it.
func reindent(t *testing.T, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestBuildsTextIsATableOrSaysThereAreNone prints each build's state and
// creation time in a table, and no builds in words.
func TestBuildsTextIsATableOrSaysThereAreNone(t *testing.T) {
	var out bytes.Buffer
	if err := writeBuilds(&out, nil, ""); err != nil || out.String() != "no builds\n" {
		t.Fatalf("no builds = %q, %v", out.String(), err)
	}
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	out.Reset()
	err := writeBuilds(&out, []api.RuntimeBuild{{ID: "b1", Name: "edge", State: api.BuildDone, CollectorVersion: "0.162.0", Reused: true, Created: created}}, "")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "BUILD NAME STATE COLLECTOR REUSED CREATED" ||
		strings.Join(strings.Fields(lines[1]), " ") != "b1 edge done 0.162.0 yes "+created.Local().Format(time.DateTime) {
		t.Fatalf("builds:\n%s", out.String())
	}
}
