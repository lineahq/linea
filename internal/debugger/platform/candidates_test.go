package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/statementedit"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

const candidateConfig = `receivers:
  otlp:
    protocols:
      grpc: {}
processors:
  transform/shared:
    log_statements:
      - 'set(log.body, "old")'
exporters:
  debug: {}
service:
  pipelines:
    logs/one:
      receivers: [otlp]
      processors: [transform/shared]
      exporters: [debug]
    logs/two:
      receivers: [otlp]
      processors: [transform/shared]
      exporters: [debug]
`

func candidateEdit() *StatementEdit {
	return &StatementEdit{Processor: "transform/shared", Signal: "logs", Op: statementedit.Replace, Text: `set(log.body, "new")`, Expected: `set(log.body, "old")`}
}

func candidateService(t *testing.T, persistent bool) *Service {
	t.Helper()
	if !persistent {
		s := New(Options{})
		t.Cleanup(s.Close)
		return s
	}
	return openPlatform(t, memory.NewData(), Options{})
}

func TestCandidateSharedProcessorAndQuickModeWriteNothing(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "ephemeral", true: "workspace"}[persistent], func(t *testing.T) {
			s := candidateService(t, persistent)
			var before map[string]string
			if persistent {
				before = repositorySnapshot(t, s)
			}
			out, err := s.CreateCandidate(context.Background(), testCaller, CandidateRequest{Base: Source{Text: candidateConfig}, Statement: candidateEdit()})
			if err != nil || !out.Valid || out.Saved != nil || !reflect.DeepEqual(out.AffectedPipelines, []string{"logs/one", "logs/two"}) {
				t.Fatalf("candidate = %+v, %v", out, err)
			}
			if !strings.HasPrefix(out.Patch, "--- base\n+++ candidate\n") || !strings.Contains(out.Patch, `-      - 'set(log.body, "old")'`) || !strings.Contains(out.Patch, `+      - 'set(log.body, "new")'`) {
				t.Fatalf("patch = %s", out.Patch)
			}
			if persistent && !reflect.DeepEqual(before, repositorySnapshot(t, s)) {
				t.Fatal("quick candidate wrote workspace files")
			}
			runs, _ := allRuns(context.Background(), s, testCaller, RunFilter{})
			if len(runs) != 0 {
				t.Fatal("candidate executed a run")
			}
		})
	}
}

func TestCandidateErrorMappings(t *testing.T) {
	s := candidateService(t, false)
	e := candidateEdit()
	e.Expected = "stale private text"
	_, err := s.CreateCandidate(context.Background(), testCaller, CandidateRequest{Base: Source{Text: candidateConfig}, Statement: e})
	wantCode(t, err, CodeConflict)
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.CurrentStatement != candidateEdit().Expected || conflict.Message != "the statement has changed since it was read" || strings.Contains(conflict.Message, "set(") || strings.Contains(conflict.Message, "private") {
		t.Fatalf("conflict = %+v", err)
	}
	for _, req := range []CandidateRequest{
		{Base: Source{Text: candidateConfig}},
		{Base: Source{Text: candidateConfig}, YAML: candidateConfig, Statement: candidateEdit()},
		{Base: Source{Text: strings.Replace(candidateConfig, `- 'set(log.body, "old")'`, "- |\n        set(log.body, \"old\")", 1)}, Statement: candidateEdit()},
		{Base: Source{Text: candidateConfig}, Statement: &StatementEdit{Processor: "missing", Signal: "logs", Op: statementedit.Replace, Text: "new", Expected: "old"}},
	} {
		_, err := s.CreateCandidate(context.Background(), testCaller, req)
		wantCode(t, err, CodeInvalidRequest)
	}
	_, err = s.CreateCandidate(context.Background(), testCaller, CandidateRequest{Save: true, Base: Source{Text: candidateConfig}, Statement: candidateEdit()})
	wantCode(t, err, CodeUnavailable)
	if !errors.Is(err, ErrWorkspaceRequired) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.CreateCandidate(ctx, testCaller, CandidateRequest{})
	wantCode(t, err, CodeCancelled)
	_, err = s.CreateCandidate(context.Background(), Caller{}, CandidateRequest{})
	wantCode(t, err, CodeInvalidRequest)
}

func TestInvalidCandidateResultIsNotSavedAndUsesBaseTopology(t *testing.T) {
	s := candidateService(t, true)
	before := repositorySnapshot(t, s)
	for _, req := range []CandidateRequest{
		{Base: Source{Text: candidateConfig}, YAML: "processors: [", Save: true},
		{Base: Source{Text: candidateConfig}, Statement: func() *StatementEdit { e := candidateEdit(); e.Text = "invalid OTTL"; return e }(), Save: true},
	} {
		out, err := s.CreateCandidate(context.Background(), testCaller, req)
		if err != nil || out.Valid || out.Saved != nil || len(out.Diagnostics) == 0 {
			t.Fatalf("invalid candidate = %+v, %v", out, err)
		}
		if req.Statement != nil && !reflect.DeepEqual(out.AffectedPipelines, []string{"logs/one", "logs/two"}) {
			t.Fatalf("base topology not used: %+v", out.AffectedPipelines)
		}
		if req.Statement == nil && len(out.AffectedPipelines) != 0 {
			t.Fatal("invalid YAML has affected pipelines")
		}
		if !reflect.DeepEqual(before, repositorySnapshot(t, s)) {
			t.Fatal("invalid proposal was saved")
		}
	}
	base := strings.Replace(candidateConfig, `set(log.body, "old")`, "invalid OTTL", 1)
	out, err := s.CreateCandidate(context.Background(), testCaller, CandidateRequest{Base: Source{Text: base}, Statement: &StatementEdit{Processor: "transform/shared", Signal: "logs", Op: statementedit.Replace, Text: "still invalid", Expected: "invalid OTTL"}})
	if err != nil || out.Valid || len(out.AffectedPipelines) != 0 {
		t.Fatalf("both invalid: %+v %v", out, err)
	}
}

func TestSavedCandidateCreatesNewArtifactAndPreservesBase(t *testing.T) {
	s := candidateService(t, true)
	ctx := context.Background()
	info, _ := s.GetWorkspaceInfo(ctx, testCaller)
	base, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: info.ID, Kind: artifact.Configuration, Label: "original", Content: []byte(candidateConfig)})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetArtifactRevision(ctx, testCaller, base.Artifact.ID, base.Artifact.Head)
	for _, full := range []bool{false, true} {
		req := CandidateRequest{Base: Source{Text: candidateConfig}, Save: true}
		if full {
			req.YAML = strings.Replace(candidateConfig, `"old"`, `"new"`, 1)
			req.Label = "full proposal"
		} else {
			req.Statement = candidateEdit()
		}
		out, err := s.CreateCandidate(ctx, Local("candidate-test"), req)
		if err != nil || !out.Valid || out.Saved == nil {
			t.Fatalf("saved = %+v %v", out, err)
		}
		saved := out.Saved
		if saved.Artifact.ID == base.Artifact.ID || saved.Artifact.Generation != 1 || saved.Revision.Parent != "" || saved.Revision.Actor != LocalOwner {
			t.Fatalf("new artifact = %+v", saved)
		}
		label := "candidate.yaml"
		if full {
			label = "full proposal.yaml"
		}
		if saved.Artifact.Label != label {
			t.Fatal("wrong label")
		}
		origin := saved.Revision.Origin
		sum := sha256.Sum256([]byte(candidateConfig))
		if origin == nil || origin.Kind != "candidate" || origin.BaseDigest != hex.EncodeToString(sum[:]) || origin.BaseRevision != "" {
			t.Fatalf("origin = %+v", origin)
		}
		if full {
			if len(origin.Edit) != 0 {
				t.Fatal("complete YAML carried an edit")
			}
		} else {
			var edit StatementEdit
			if json.Unmarshal(origin.Edit, &edit) != nil || edit != *req.Statement {
				t.Fatalf("edit = %s", origin.Edit)
			}
			// The persisted origin uses the documented JSON field names.
			var keys map[string]any
			if json.Unmarshal(origin.Edit, &keys) != nil || keys["processor"] != req.Statement.Processor || keys["op"] != string(req.Statement.Op) {
				t.Fatalf("persisted edit keys = %s", origin.Edit)
			}
		}
		stored, _ := s.GetArtifactRevision(ctx, testCaller, saved.Artifact.ID, saved.Artifact.Head)
		if string(stored.Content) != out.Proposed {
			t.Fatal("candidate bytes changed")
		}
		after, _ := s.GetArtifactRevision(ctx, testCaller, base.Artifact.ID, base.Artifact.Head)
		head, _ := s.GetArtifact(ctx, testCaller, base.Artifact.ID)
		if !reflect.DeepEqual(before, after) || head != base.Artifact {
			t.Fatal("base head, generation or bytes changed")
		}
	}
}

func TestCandidatePathBaseUsesHostPolicy(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(allowed, "config.yaml")
	if err := os.WriteFile(path, []byte(candidateConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	denied := filepath.Join(outside, "config.yaml")
	if err := os.WriteFile(denied, []byte(candidateConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := hostfs.Policy{Roots: []string{allowed}}
	s := New(Options{Paths: policy})
	t.Cleanup(s.Close)
	_, err := s.CreateCandidate(context.Background(), testCaller, CandidateRequest{Base: Source{Path: denied}, Statement: candidateEdit()})
	wantCode(t, err, CodeForbidden)
	out, err := s.CreateCandidate(context.Background(), testCaller, CandidateRequest{Base: Source{Path: path}, Statement: candidateEdit()})
	if err != nil || !out.Valid {
		t.Fatalf("allowed = %+v, %v", out, err)
	}
	bytes, _ := os.ReadFile(path)
	if string(bytes) != candidateConfig {
		t.Fatal("host file changed")
	}
}

func TestCompleteYAMLCandidateAffectedProcessors(t *testing.T) {
	s := candidateService(t, false)
	for _, tc := range []struct {
		base, proposed string
		want           []string
	}{
		{candidateConfig, strings.Replace(candidateConfig, `"old"`, `"new"`, 1), []string{"logs/one", "logs/two"}},
		{candidateConfig, candidateConfig + "# comment only\n", []string{}},
		{"processors: [", candidateConfig, []string{}},
	} {
		out, err := s.CreateCandidate(context.Background(), testCaller, CandidateRequest{Base: Source{Text: tc.base}, YAML: tc.proposed})
		if err != nil || !out.Valid || !reflect.DeepEqual(out.AffectedPipelines, tc.want) {
			t.Fatalf("affected = %+v, %v", out, err)
		}
	}
}

func TestCandidatePatchHasThreeContextLinesAndEOFMarkers(t *testing.T) {
	s := candidateService(t, false)
	base := strings.TrimSuffix(candidateConfig, "\n")
	proposed := strings.TrimSuffix(strings.Replace(candidateConfig, "exporters: [debug]", "exporters: []", 2), "\n")
	out, err := s.CreateCandidate(context.Background(), testCaller, CandidateRequest{Base: Source{Text: base}, YAML: proposed})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Patch, "--- base\n+++ candidate\n@@ -13,8 +13,8 @@\n") || !strings.HasSuffix(out.Patch, "+      exporters: []\n\\ No newline at end of file\n") {
		t.Fatalf("patch:\n%s", out.Patch)
	}
	if strings.Count(out.Patch, "\\ No newline at end of file") != 2 {
		t.Fatalf("missing EOF markers:\n%s", out.Patch)
	}
}
