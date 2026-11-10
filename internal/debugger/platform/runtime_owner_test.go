package platform

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// TestOnlyTheOwnerOrAManagerReplacesARuntime: on a team server a runtime
// name registered by one principal is rebuilt by that principal or with
// runtime.manage, and a manager's replacement is audited on its own.
func TestOnlyTheOwnerOrAManagerReplacesARuntime(t *testing.T) {
	ctx := context.Background()
	s := serverBuildPlatform(t)
	registered := time.Now().UTC().Add(-time.Hour)
	if err := s.catalog.SaveRuntime(ctx, artifact.RuntimeEntry{Name: "edge", Identity: "sha256:" + strings.Repeat("a", 64), Build: "build-1",
		Binary:       &artifact.RuntimeBinary{Digest: "sha256:" + strings.Repeat("b", 64), Bytes: 1 << 20, OS: "linux", Arch: "amd64"},
		RegisteredBy: buildAlice.Principal.ID, Registered: registered}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		caller Caller
		build  string
		want   Code
	}{
		{"another engineer", buildBob, "edge", CodeForbidden},
		{"a free name", buildBob, "bobs", ""},
		{"the owner", buildAlice, "edge", ""},
		{"a manager", testCaller, "edge", ""},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.BuildRuntime(ctx, tt.caller, serverBuildRequest(tt.build, procArchive(t, strings.Repeat("//\n", i))))
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" {
				wantCode(t, err, tt.want)
				if !strings.Contains(err.Error(), "runtime edge") {
					t.Fatalf("error %q does not name the runtime", err)
				}
			}
		})
	}
	page, err := s.ListAuditEvents(ctx, testCaller, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var replaced, denied int
	for _, e := range page.Events {
		switch {
		case e.Operation == "ReplaceRuntime" && e.Principal == testCaller.Principal.ID && e.Scope == "runtime:edge" && e.Permission == "runtime.manage":
			replaced++
		case e.Operation == "BuildRuntime" && e.Principal == buildBob.Principal.ID && e.Outcome == AuditDenied && e.Scope == "runtime:edge":
			denied++
		}
	}
	if replaced != 1 || denied != 1 {
		t.Fatalf("audit has %d replacements and %d refusals, want 1 and 1: %+v", replaced, denied, page.Events)
	}
}

// TestABuildDoesNotReplaceANameTakenWhileItRan: a name another principal
// registers after the build was requested is not the build's to replace.
func TestABuildDoesNotReplaceANameTakenWhileItRan(t *testing.T) {
	ctx := context.Background()
	s := serverBuildPlatform(t)
	requested := time.Now().UTC()
	save := func(by string, at time.Time) {
		t.Helper()
		if err := s.catalog.SaveRuntime(ctx, artifact.RuntimeEntry{Name: "edge", Identity: "sha256:" + strings.Repeat("a", 64), Build: "build-1",
			Binary:       &artifact.RuntimeBinary{Digest: "sha256:" + strings.Repeat("b", 64), Bytes: 1 << 20, OS: "linux", Arch: "amd64"},
			RegisteredBy: by, Registered: at}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want bool) {
		t.Helper()
		if got, err := s.takenSince(ctx, "edge", buildAlice.Principal.ID, requested); err != nil || got != want {
			t.Fatalf("taken = %v, %v; want %v", got, err, want)
		}
	}
	check(false)
	save(buildBob.Principal.ID, requested.Add(-time.Minute))
	check(false) // registered before the request: checked then
	save(buildAlice.Principal.ID, requested.Add(time.Minute))
	check(false) // the build's own principal
	save(buildBob.Principal.ID, requested.Add(time.Minute))
	check(true)
}
