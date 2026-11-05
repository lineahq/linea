package platform

import (
	"context"
	"errors"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestWorkspaceReopenAndTrustedArtifactActor(t *testing.T) {
	data := memory.NewData()
	s, err := openMemory(data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	info, err := s.GetWorkspaceInfo(context.Background(), testCaller)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveArtifact(context.Background(), Caller{Principal: LocalPrincipal(), Client: "forged-client"}, SaveArtifactRequest{WorkspaceID: info.ID, Kind: artifact.Setup, Content: []byte("invalid draft")})
	if err != nil || saved.Revision.Actor != "local-owner" {
		t.Fatalf("save = %+v, %v", saved, err)
	}
	s.Close()
	s, err = openMemory(data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.GetArtifactRevision(context.Background(), testCaller, saved.Revision.Ref.ArtifactID, saved.Revision.Ref.RevisionID); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := s.SaveArtifact(context.Background(), testCaller, SaveArtifactRequest{}); !errors.Is(err, ErrClosed) || ErrorCode(err) != CodeUnavailable {
		t.Fatalf("save after close = %v", err)
	}
}

func TestQuickRunsDoNotRequireOrAutomaticallySaveNamedArtifacts(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "ephemeral", true: "persistent sources"}[persistent], func(t *testing.T) {
			opts := Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
				return report(req, runresult.StatusCompleted), nil
			}}
			var s *Service
			if persistent {
				var err error
				s, err = openMemory(memory.NewData(), opts)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				s = New(opts)
			}
			t.Cleanup(s.Close)
			r := submit(t, s, request("quick"))
			if v := waitRun(t, r); v.Mode != ModeQuick || v.Report.Assertions != nil {
				t.Fatalf("quick = %+v", v)
			}
			page, err := s.ListArtifacts(context.Background(), testCaller, ListArtifactsRequest{})
			if persistent && (err != nil || len(page.Items) != 0) {
				t.Fatalf("quick run created named artifacts: %+v, %v", page, err)
			}
			if !persistent && !errors.Is(err, ErrWorkspaceRequired) {
				t.Fatalf("ephemeral artifact lookup = %v", err)
			}
		})
	}
}
