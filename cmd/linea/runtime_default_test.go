package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
)

// TestRuntimeDefaultReadsAndChoosesTheTeamsDefault covers the
// command line: before a choice the default is the built-in runtime, a
// runtime the catalog does not hold is refused, and a choice names who made
// it.
func TestRuntimeDefaultReadsAndChoosesTheTeamsDefault(t *testing.T) {
	team := strings.NewReplacer("role: viewer", "role: admin").Replace(teamYAML())
	server := startTeam(t, writeTeam(t, team))
	resp := teamRequests(t, server)(http.MethodPost, "/api/v1/tokens", api.TokenRequest{Label: "default"},
		map[string]string{"X-Forwarded-User": "alice", "X-Forwarded-Groups": "platform"})
	var created api.CreatedToken
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token: %d, %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	t.Setenv(serverTokenEnv, created.Secret)

	code, out, stderr := runCLI(t, "runtime", "default", "--server", server)
	if code != exitOK || !strings.Contains(out, "default runtime: built-in") || !strings.Contains(out, "none was chosen") {
		t.Fatalf("default before a choice: exit %d, %s%s", code, out, stderr)
	}
	if code, out, stderr = runCLI(t, "runtime", "default", "gone", "--server", server); code != exitError || !strings.Contains(stderr, "gone") {
		t.Fatalf("default of an unknown runtime: exit %d, %s%s", code, out, stderr)
	}
	code, out, stderr = runCLI(t, "runtime", "default", "built-in", "--server", server)
	if code != exitOK || !strings.Contains(out, "default runtime: built-in") || !strings.Contains(out, "chosen by ") || strings.Contains(out, "none was chosen") {
		t.Fatalf("choosing the built-in runtime: exit %d, %s%s", code, out, stderr)
	}
	code, out, stderr = runCLI(t, "runtime", "default", "--format", "json", "--server", server)
	var d api.RuntimeDefault
	if code != exitOK || json.Unmarshal([]byte(out), &d) != nil || d.Name != "built-in" || d.SetBy == "" || d.Set == nil {
		t.Fatalf("default as json: exit %d, %s%s", code, out, stderr)
	}
}

// TestRuntimeDefaultNeedsAServerAndOneName refuses the forms that cannot
// work: the default lives on a team server, and one runtime is chosen.
func TestRuntimeDefaultNeedsAServerAndOneName(t *testing.T) {
	for _, args := range [][]string{
		{"runtime", "default"},
		{"runtime", "default", "a", "b", "--server", "http://127.0.0.1:1"},
		{"runtime", "default", "--format", "yaml", "--server", "http://127.0.0.1:1"},
	} {
		if code, _, stderr := runCLI(t, args...); code != exitError || stderr == "" {
			t.Errorf("%v: exit %d, %q", args, code, stderr)
		}
	}
}

// TestRuntimeTableMarksTheDefault marks the default runtime by its name.
func TestRuntimeTableMarksTheDefault(t *testing.T) {
	var out bytes.Buffer
	err := writeRuntimes(&out, []api.Runtime{{Name: "built-in", CollectorVersion: "0.162.0", Verified: true},
		{Name: "edge", CollectorVersion: "0.162.0", Verified: true, Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || strings.Join(strings.Fields(lines[1]), " ") != "built-in 0.162.0 true -" ||
		strings.Join(strings.Fields(lines[2]), " ") != "edge (default) 0.162.0 true -" {
		t.Fatalf("table:\n%s", out.String())
	}
}

// TestRuntimeDefaultTextNamesTheRuntimeThenWhoChoseIt puts the runtime on
// the first line and the choice, or its absence, under it.
func TestRuntimeDefaultTextNamesTheRuntimeThenWhoChoseIt(t *testing.T) {
	set := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		d    api.RuntimeDefault
		want string
	}{
		{api.RuntimeDefault{Name: "edge", SetBy: "alice", Set: &set}, "default runtime: edge\n  chosen by alice at 2026-01-02T03:04:05Z\n"},
		{api.RuntimeDefault{Name: "built-in"}, "default runtime: built-in\n  none was chosen: requests that name no runtime run on the built-in runtime\n"},
		{api.RuntimeDefault{Name: "edge"}, "default runtime: edge\n  none was chosen: the server started with it\n"},
	} {
		var out bytes.Buffer
		if err := writeRuntimeDefault(&out, &tc.d); err != nil || out.String() != tc.want {
			t.Errorf("default %+v:\n%s\nwant:\n%s", tc.d, out.String(), tc.want)
		}
	}
}
