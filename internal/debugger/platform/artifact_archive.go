package platform

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/lineahq/linea/examples/ci"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
)

// The CI archive layout. Its paths match the defaults of
// examples/ci/github-actions.yml, so the archive unpacks into a repository
// whose workflow runs as is.
const (
	ciConfigPath   = "collector/config.yaml"
	ciTestsDir     = "collector/tests/"
	ciWorkflowPath = ".github/workflows/collector-regression.yml"
	ciManifestPath = "linea.json"
	ciArchiveName  = "collector-ci.zip"
	ciSchema       = 1
	// MaxCIArchiveBytes bounds an uploaded archive; each entry is also bounded
	// by the workspace's blob limit.
	MaxCIArchiveBytes = 16 << 20
	maxCIEntries      = 64
	// ciScenarioLine is the workflow's scenario for the pull request diff; the
	// export points it at the first exported scenario.
	ciScenarioLine = "  SCENARIO: collector/tests/regression.scenario.yaml\n"
)

// ciDirectories are the directory entries an archive may hold.
var ciDirectories = []string{"collector/", ciTestsDir, ".github/", ".github/workflows/"}

// ExportCIRequest names saved artifacts to export for CI: one configuration
// and 1 to MaxSuiteMembers scenarios, each at its head revision.
type ExportCIRequest struct {
	Configuration string
	Scenarios     []string
}

// CIExport is a zip archive and the file name to offer it as.
type CIExport struct {
	Name    string
	Archive []byte
}

// ImportedArtifact is what Import did with one archive file: "created" a new
// artifact, "revised" one with a new revision, or left it "unchanged".
type ImportedArtifact struct {
	Path     string           `json:"path"`
	Status   string           `json:"status"`
	Artifact ArtifactMetadata `json:"artifact"`
}

// ArtifactImport lists an import's files in archive order.
type ArtifactImport struct {
	Items []ImportedArtifact `json:"items"`
}

// ciManifest records which saved revision each archive file was exported
// from, so an import of an edited archive revises those artifacts.
type ciManifest struct {
	Schema    int              `json:"schema"`
	Workspace string           `json:"workspace_id"`
	Files     []ciManifestFile `json:"files"`
}

type ciManifestFile struct {
	Path     string `json:"path"`
	Artifact string `json:"artifact_id"`
	Revision string `json:"revision_id"`
	Label    string `json:"label"`
}

// ExportArtifactsForCI builds a zip of a saved configuration and scenarios at
// their head revisions, the examples/ci GitHub Actions workflow and a
// manifest of the exported revisions. Contents are the saved bytes:
// environment references stay references and no value is read.
func (s *Service) ExportArtifactsForCI(ctx context.Context, caller Caller, req ExportCIRequest) (_ *CIExport, err error) {
	defer s.audit(&caller, "ExportArtifactsForCI", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactRead)
	if err != nil {
		return nil, err
	}
	if req.Configuration == "" || len(req.Scenarios) == 0 || len(req.Scenarios) > MaxSuiteMembers {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("export for CI needs a configuration and 1 to %d scenarios", MaxSuiteMembers)}
	}
	type file struct {
		manifest ciManifestFile
		content  []byte
	}
	read := func(id string, kind ArtifactKind) (file, error) {
		m, err := s.artifactIn(ctx, caller, authz.ArtifactRead, repository, id)
		if err != nil {
			return file{}, err
		}
		if m.Kind != kind {
			return file{}, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("artifact %s is a %s, not a %s", id, m.Kind, kind)}
		}
		rev, err := repository.Revision(ctx, artifact.Ref{WorkspaceID: m.WorkspaceID, ArtifactID: m.ID, RevisionID: m.Head})
		if err != nil {
			return file{}, artifactError(err)
		}
		return file{ciManifestFile{Artifact: m.ID, Revision: m.Head, Label: m.Label}, rev.Content}, nil
	}
	config, err := read(req.Configuration, artifact.Configuration)
	if err != nil {
		return nil, err
	}
	config.manifest.Path = ciConfigPath
	files := []file{config}
	names := map[string]bool{}
	for _, id := range req.Scenarios {
		if slices.ContainsFunc(files, func(f file) bool { return f.manifest.Artifact == id }) {
			return nil, &Error{Code: CodeInvalidRequest, Message: "artifact " + id + " is exported twice"}
		}
		f, err := read(id, artifact.Scenario)
		if err != nil {
			return nil, err
		}
		f.manifest.Path = ciTestsDir + uniqueName(names, f.manifest.Label)
		files = append(files, f)
	}

	manifest := ciManifest{Schema: ciSchema, Workspace: repository.Info().ID}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, content []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err == nil {
			_, err = w.Write(content)
		}
		return err
	}
	for _, f := range files {
		manifest.Files = append(manifest.Files, f.manifest)
		if err := write(f.manifest.Path, f.content); err != nil {
			return nil, classify(err, CodeInternal)
		}
	}
	workflow := bytes.Replace(ci.GitHubActions, []byte(ciScenarioLine), []byte("  SCENARIO: "+files[1].manifest.Path+"\n"), 1)
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, classify(err, CodeInternal)
	}
	if err := errors.Join(write(ciWorkflowPath, workflow), write(ciManifestPath, append(raw, '\n')), zw.Close()); err != nil {
		return nil, classify(err, CodeInternal)
	}
	return &CIExport{Name: ciArchiveName, Archive: buf.Bytes()}, nil
}

// uniqueName turns a label into a scenario file name that is safe in an
// archive and not yet taken.
func uniqueName(taken map[string]bool, label string) string {
	base := path.Base(strings.ReplaceAll(label, `\`, "/"))
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".yaml"), ".yml")
	base = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, base)
	base = strings.Trim(base, ".-")
	if base == "" {
		base = "scenario"
	}
	name := base + ".yaml"
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d.yaml", base, i)
	}
	taken[name] = true
	return name
}

// ImportArtifacts saves the files of a CI archive: the configuration
// and scenarios of ExportArtifactsForCI's layout. A file the manifest names
// as exported from this workspace becomes a new revision of that artifact,
// or is left unchanged when its bytes equal the head; any other file becomes
// a new artifact labelled with its name. The workflow is not saved. Unknown,
// duplicate or oversized entries refuse the import with invalid_request, and
// an artifact revised since its export refuses it with conflict, all before
// the first save. New artifacts join the caller's project; a file exported
// from an artifact of another project becomes a new artifact too.
func (s *Service) ImportArtifacts(ctx context.Context, caller Caller, archive []byte) (_ *ArtifactImport, err error) {
	defer s.audit(&caller, "ImportArtifacts", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactWrite)
	if err != nil {
		return nil, err
	}
	if err := s.workIn(&caller, authz.ArtifactWrite); err != nil {
		return nil, err
	}
	project := caller.Project
	refuse := func(format string, a ...any) error {
		return &Error{Code: CodeInvalidRequest, Message: "import refused: " + fmt.Sprintf(format, a...)}
	}
	if len(archive) > MaxCIArchiveBytes {
		return nil, refuse("the archive exceeds %d bytes", MaxCIArchiveBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, refuse("not a zip archive")
	}
	if len(zr.File) > maxCIEntries {
		return nil, refuse("the archive has more than %d entries", maxCIEntries)
	}
	limit := repository.Limits().BlobBytes
	contents := map[string][]byte{}
	var order []string
	var manifest *ciManifest
	for _, f := range zr.File {
		name := f.Name
		if slices.Contains(ciDirectories, name) && f.Mode().IsDir() {
			continue
		}
		known := name == ciConfigPath || name == ciWorkflowPath || name == ciManifestPath || isCITest(name)
		if !known {
			return nil, refuse("unknown entry %q", name)
		}
		if _, dup := contents[name]; dup {
			return nil, refuse("duplicate entry %q", name)
		}
		raw, err := readEntry(f, limit)
		if err != nil {
			return nil, refuse("entry %q: %v", name, err)
		}
		contents[name] = raw
		switch name {
		case ciWorkflowPath:
		case ciManifestPath:
			manifest = &ciManifest{}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if dec.Decode(manifest) != nil || manifest.Schema != ciSchema {
				return nil, refuse("%s is not a schema %d manifest", ciManifestPath, ciSchema)
			}
		default:
			order = append(order, name)
		}
	}
	if len(order) == 0 {
		return nil, refuse("no %s or %s*.yaml entry", ciConfigPath, ciTestsDir)
	}
	// The configuration first (it sorts before collector/tests/), then the
	// scenarios by name.
	slices.Sort(order)
	// The manifest's labels apply in any workspace; its artifacts only in
	// the workspace it was exported from.
	exported := map[string]ciManifestFile{}
	if manifest != nil {
		targets := map[string]bool{}
		for _, f := range manifest.Files {
			if targets[f.Artifact] {
				return nil, refuse("%s names artifact %s twice", ciManifestPath, f.Artifact)
			}
			if manifest.Workspace != repository.Info().ID {
				f.Artifact, f.Revision = "", ""
			}
			targets[f.Artifact], exported[f.Path] = f.Artifact != "", f
		}
	}

	type plan struct {
		item ImportedArtifact
		req  artifact.SaveRequest
	}
	plans := make([]plan, 0, len(order))
	for _, name := range order {
		kind := artifact.Scenario
		if name == ciConfigPath {
			kind = artifact.Configuration
		}
		content := contents[name]
		p := plan{item: ImportedArtifact{Path: name, Status: "created"}, req: artifact.SaveRequest{WorkspaceID: repository.Info().ID, Project: project, Kind: kind, Label: path.Base(name), Content: content, Actor: caller.Principal.ID}}
		if from, ok := exported[name]; ok {
			if from.Label != "" {
				p.req.Label = from.Label
			}
			var m ArtifactMetadata
			err := artifact.ErrNotFound
			if from.Artifact != "" {
				m, err = repository.Get(ctx, from.Artifact)
				if err == nil && m.Project != project {
					err = artifact.ErrNotFound
				}
			}
			switch {
			case errors.Is(err, artifact.ErrNotFound):
			case err != nil:
				return nil, artifactError(err)
			case m.Kind != kind:
				return nil, refuse("%s was exported from %s, which is a %s", name, m.ID, m.Kind)
			case m.Head != from.Revision:
				return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("import refused: %s (%s) was revised after its export; export it again", name, m.Label)}
			default:
				if err := s.authorizeIn(caller, authz.ArtifactWrite, m.Project); err != nil {
					return nil, err
				}
				head, err := repository.Revision(ctx, artifact.Ref{WorkspaceID: m.WorkspaceID, ArtifactID: m.ID, RevisionID: m.Head})
				if err != nil {
					return nil, artifactError(err)
				}
				p.item.Artifact = m
				if bytes.Equal(head.Content, content) {
					p.item.Status = "unchanged"
				} else {
					p.item.Status = "revised"
					p.req.ArtifactID, p.req.ExpectedGeneration, p.req.ExpectedHead, p.req.Label, p.req.Project = m.ID, m.Generation, m.Head, m.Label, m.Project
				}
			}
		}
		plans = append(plans, p)
	}

	out := &ArtifactImport{Items: make([]ImportedArtifact, 0, len(plans))}
	for _, p := range plans {
		if p.item.Status != "unchanged" {
			saved, err := saveArtifact(ctx, repository, p.req)
			if err != nil {
				return nil, artifactError(err)
			}
			p.item.Artifact = saved.Artifact
		}
		out.Items = append(out.Items, p.item)
	}
	return out, nil
}

// isCITest reports whether name is a scenario directly under collector/tests.
func isCITest(name string) bool {
	rest, ok := strings.CutPrefix(name, ciTestsDir)
	ext := strings.ToLower(path.Ext(rest))
	return ok && rest != "" && !strings.Contains(rest, "/") && !strings.HasPrefix(rest, ".") && (ext == ".yaml" || ext == ".yml")
}

// readEntry reads one archive file, trusting neither its declared size nor
// its compression ratio.
func readEntry(f *zip.File, limit int64) ([]byte, error) {
	if f.Mode().IsDir() || !f.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("exceeds %d bytes", limit)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("exceeds %d bytes", limit)
	}
	return raw, nil
}
