package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/manifest"
	"github.com/lineahq/linea/internal/debugger/release"
)

// Server builds: on a team server, which has no Docker, a
// build is a durable job in the repository that a builder claims. The
// manifest and the uploaded source archives are its stored inputs; builds
// of identical inputs reuse the earlier done build across the server.

const (
	// serverBuildsPerPrincipal and serverBuildsTotal bound unfinished builds.
	serverBuildsPerPrincipal = 2
	serverBuildsTotal        = 20
	// serverBuildAttempts is how many builders may hold a build before a
	// lost lease fails it.
	serverBuildAttempts = 2
	// serverBuildsRetained is how many finished builds keep their logs and
	// inputs.
	serverBuildsRetained = 100
)

// builderVersion is part of a build's key: a builder of another version
// builds again rather than reusing.
var builderVersion = "linea-builder " + release.ProductVersion

// serverBuildSpec is a server build's stored description. It never holds
// a credential.
type serverBuildSpec struct {
	Name             string   `json:"name"`
	CollectorVersion string   `json:"collector_version"`
	Verified         bool     `json:"verified"`
	Warnings         []string `json:"warnings,omitempty"`
	GoImage          string   `json:"go_image"`
	Builder          string   `json:"builder"`
}

// serverBuildResult is a server build's stored outcome: an error, or the
// built runtime's identity, its worker's hello and the binary's platform,
// which a build reusing it registers too.
type serverBuildResult struct {
	Error   string          `json:"error,omitempty"`
	Runtime string          `json:"runtime,omitempty"`
	Hello   json.RawMessage `json:"hello,omitempty"`
	OS      string          `json:"os,omitempty"`
	Arch    string          `json:"arch,omitempty"`
	// Image is the exported image by digest, and ExportError why the
	// export failed; a failed export leaves the build done.
	Image       string `json:"image,omitempty"`
	ExportError string `json:"export_error,omitempty"`
}

// serverBuilds reports whether builds go to the repository's build queue:
// a service without a runtime store of its own, over a repository.
func (s *Service) serverBuilds() (artifact.Builds, bool) {
	if s.runtimes != nil || s.repository == nil {
		return nil, false
	}
	return s.repository, true
}

// queueServerBuild checks a build request and admits it to the
// repository's build queue. Every source is an uploaded archive; the
// manifest's local directories must each have one.
func (s *Service) queueServerBuild(ctx context.Context, caller Caller, builds artifact.Builds, req BuildRuntimeRequest) (*RuntimeBuild, error) {
	if req.GoImage != "" || req.ModuleProxy != "" || req.Offline || req.ManifestDir != "" {
		return nil, &Error{Code: CodeInvalidRequest, Message: "a team server's builders choose the Go image and module proxy: leave the Go image, module proxy, offline and manifest directory unset"}
	}
	if req.Name == BuiltInRuntime || !validRuntimeName(req.Name) {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf(
			"runtime name %q: use lower-case letters, digits, '.', '_' and '-', at most 63 characters, not %q", req.Name, BuiltInRuntime)}
	}
	src := req.Manifest
	if src.Path != "" {
		return nil, &Error{Code: CodeInvalidRequest, Message: "a team server reads no manifest file: send the manifest's text or a saved manifest"}
	}
	if _, err := s.resolveSource(ctx, caller, &src, artifact.Manifest, "manifest"); err != nil {
		return nil, err
	}
	raw := []byte(src.Text)
	if len(raw) > maxManifestBytes {
		return nil, manifestDiagnostics("manifest_too_large", fmt.Sprintf("the manifest is larger than %d bytes", maxManifestBytes))
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, classify(manifestDiagnostics("invalid_manifest", err.Error()), CodeValidationFailed)
	}
	support, err := s.versionTable().Check(m.CollectorVersion())
	if err != nil {
		return nil, classify(manifestDiagnostics("unsupported_collector_version", err.Error()), CodeValidationFailed)
	}
	sources, err := serverBuildSources(m, req.Sources)
	if err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
	spec := serverBuildSpec{Name: req.Name, CollectorVersion: m.CollectorVersion(), Verified: support == versions.Verified,
		Warnings: manifest.Warnings(raw), GoImage: container.GoImage, Builder: builderVersion}
	var modules []string
	for _, e := range m.Entries() {
		modules = append(modules, e.ModulePath())
	}
	for _, f := range features.Lacking(modules) {
		spec.Warnings = append(spec.Warnings, fmt.Sprintf("the manifest has no %s, so the runtime lacks the %s feature and its reports say so", f.Component, f.Name))
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	record, err := builds.AdmitBuild(ctx, artifact.BuildAdmission{
		ID: instrument.NewRunID(), Principal: caller.Principal.ID, Name: req.Name, Key: buildKey(raw, sources, spec),
		Spec: encoded, Manifest: raw, Sources: sources, Created: time.Now().UTC(),
	}, artifact.BuildLimits{PerPrincipal: serverBuildsPerPrincipal, Total: serverBuildsTotal})
	if errors.Is(err, artifact.ErrLimit) {
		return nil, &Error{Code: CodeResourceExhausted, Retryable: true, Message: fmt.Sprintf(
			"too many unfinished runtime builds: at most %d per principal and %d on the server; wait for one to finish", serverBuildsPerPrincipal, serverBuildsTotal)}
	}
	if err != nil {
		return nil, artifactError(err)
	}
	caller.touch(record.ID)
	s.buildChanged(record.ID)
	if record.State == artifact.BuildDone {
		// Reused: the identical build's runtime takes this build's name.
		if err := s.registerBuilt(ctx, record); err != nil {
			return nil, classify(err, CodeStorageError)
		}
	}
	return s.serverBuildView(ctx, builds, record, 0, true)
}

// serverBuildSources checks the uploaded archives against the manifest:
// each source is an archive of a module directory, and every local
// directory the manifest names has one.
func serverBuildSources(m *manifest.Manifest, overrides []SourceOverride) ([]artifact.BuildSource, error) {
	var problems []string
	var out []artifact.BuildSource
	seen := map[string]bool{}
	for _, o := range overrides {
		switch {
		case o.Module == "":
			problems = append(problems, "a source names no module")
			continue
		case seen[o.Module]:
			problems = append(problems, fmt.Sprintf("%s has more than one source", o.Module))
			continue
		case o.Dir != "" || o.Git != "" || o.Ref != "" || o.Subdir != "" || len(o.Archive) == 0:
			problems = append(problems, fmt.Sprintf("the source of %s: a team server builds from an uploaded archive of the module's directory", o.Module))
			continue
		}
		seen[o.Module] = true
		if err := checkSourceArchive(o.Module, o.Archive); err != nil {
			problems = append(problems, fmt.Sprintf("the source of %s: %v", o.Module, err))
			continue
		}
		out = append(out, artifact.BuildSource{Module: o.Module, Archive: o.Archive})
	}
	local, err := localModules(m)
	if err != nil {
		return nil, err
	}
	for _, module := range local {
		if !seen[module] {
			problems = append(problems, fmt.Sprintf("the manifest builds %s from a local directory: upload that directory with the build", module))
		}
	}
	if len(problems) > 0 {
		return nil, manifestDiagnostics("manifest_sources", problems...)
	}
	slices.SortFunc(out, func(a, b artifact.BuildSource) int { return strings.Compare(a.Module, b.Module) })
	return out, nil
}

// localModules returns, sorted, the modules the manifest builds from a
// local directory: `path:` entries and replaces with a directory.
func localModules(m *manifest.Manifest) ([]string, error) {
	local := map[string]bool{}
	for _, e := range m.Entries() {
		if e.Path != "" {
			local[e.ModulePath()] = true
		}
	}
	replaces, err := m.ParseReplaces()
	if err != nil {
		return nil, manifestDiagnostics("invalid_manifest", err.Error())
	}
	for _, r := range replaces {
		if r.Local() {
			local[r.Old] = true
		}
	}
	out := make([]string, 0, len(local))
	for module := range local {
		out = append(out, module)
	}
	slices.Sort(out)
	return out, nil
}

// checkSourceArchive checks an uploaded module archive with
// the rules the builder applies again before it unpacks it.
func checkSourceArchive(module string, archive []byte) error {
	return container.CheckModuleArchive(module, archive)
}

// buildKey identifies a build's inputs: builds of the same key produce the
// same binary, whatever their name.
func buildKey(manifest []byte, sources []artifact.BuildSource, spec serverBuildSpec) string {
	h := sha256.New()
	field := func(s string) { fmt.Fprintf(h, "%d:%s\n", len(s), s) }
	field(blobDigest(manifest))
	for _, src := range sources {
		field(src.Module)
		field(blobDigest(src.Archive))
	}
	field(spec.GoImage)
	field(spec.Builder)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func blobDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// manages reports whether caller holds runtime.manage, which reads and
// cancels every principal's builds.
func (s *Service) manages(caller Caller) bool {
	return s.effective.Load().Allowed(identity(caller.Principal), authz.RuntimeManage, authz.All)
}

// serverBuildView is a stored build as callers see it, with its log from
// logFrom on unless noLog. Reused says only that an identical build
// existed, never whose.
func (s *Service) serverBuildView(ctx context.Context, builds artifact.Builds, r artifact.BuildRecord, logFrom int, noLog bool) (*RuntimeBuild, error) {
	var spec serverBuildSpec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, classify(fmt.Errorf("runtime build %s: %w: %v", r.ID, artifact.ErrCorrupt, err), CodeInternal)
	}
	v := &RuntimeBuild{
		ID: r.ID, Caller: Actor{Principal: r.Principal}, Name: r.Name, State: BuildState(r.State),
		Created: r.Created, Started: r.Started, Finished: r.Finished, LogLines: r.LogLines, Log: []string{}, LogFrom: r.LogLines,
		CollectorVersion: spec.CollectorVersion, Verified: spec.Verified, Warnings: spec.Warnings, Reused: r.ReusedFrom != "",
	}
	if v.Warnings == nil {
		v.Warnings = []string{}
	}
	if len(r.Result) > 0 {
		var result serverBuildResult
		if err := json.Unmarshal(r.Result, &result); err != nil {
			return nil, classify(fmt.Errorf("runtime build %s: %w: %v", r.ID, artifact.ErrCorrupt, err), CodeInternal)
		}
		v.Error, v.Image, v.ExportError = result.Error, result.Image, result.ExportError
	}
	if !noLog {
		lines, first, err := builds.BuildLog(ctx, r.ID, max(logFrom, 0))
		if err != nil {
			return nil, artifactError(err)
		}
		v.Log, v.LogFrom = lines, first
		if v.Log == nil {
			v.Log = []string{}
		}
	}
	return v, nil
}

// serverBuild reads a build the caller may see: their own, or any with
// runtime.manage. Another principal's build is not_found, as an unknown
// one is.
func (s *Service) serverBuild(ctx context.Context, caller Caller, builds artifact.Builds, id string) (artifact.BuildRecord, error) {
	r, err := builds.Build(ctx, id)
	if err == nil && r.Principal != caller.Principal.ID && !s.manages(caller) {
		err = artifact.ErrNotFound
	}
	if errors.Is(err, artifact.ErrNotFound) {
		return r, &Error{Code: CodeNotFound, Message: fmt.Sprintf("runtime build %s not found", id)}
	}
	if err != nil {
		return r, artifactError(err)
	}
	caller.touch(id)
	return r, nil
}

// watchServerBuild returns the build with its log and a channel closed on
// the build's next change. The channel is taken before the build is read,
// so no change between the two is missed.
func (s *Service) watchServerBuild(ctx context.Context, caller Caller, builds artifact.Builds, id string, logFrom int) (*RuntimeBuild, <-chan struct{}, error) {
	changed := s.buildWatch(id)
	r, err := s.serverBuild(ctx, caller, builds, id)
	if err != nil {
		return nil, nil, err
	}
	v, err := s.serverBuildView(ctx, builds, r, logFrom, false)
	return v, changed, err
}

// listServerBuilds pages the builds the caller may see, newest first.
func (s *Service) listServerBuilds(ctx context.Context, caller Caller, builds artifact.Builds, req BuildListRequest) (*RuntimeBuildPage, error) {
	q := artifact.BuildQuery{Limit: req.Limit, Cursor: req.Cursor}
	if !s.manages(caller) {
		q.Principal = caller.Principal.ID
	}
	page, err := builds.ListBuilds(ctx, q)
	if err != nil {
		return nil, artifactError(err)
	}
	out := &RuntimeBuildPage{Builds: make([]RuntimeBuild, 0, len(page.Builds)), NextCursor: page.NextCursor}
	for _, r := range page.Builds {
		v, err := s.serverBuildView(ctx, builds, r, 0, true)
		if err != nil {
			return nil, err
		}
		out.Builds = append(out.Builds, *v)
	}
	return out, nil
}

// cancelServerBuild cancels the caller's build, or any with runtime.manage.
// A finished build is returned unchanged.
func (s *Service) cancelServerBuild(ctx context.Context, caller Caller, builds artifact.Builds, id string) (*RuntimeBuild, error) {
	r, err := s.serverBuild(ctx, caller, builds, id)
	if err != nil {
		return nil, err
	}
	if !r.State.Terminal() {
		cancelled, err := builds.CancelBuild(ctx, id, time.Now().UTC())
		switch {
		case err == nil:
			r = cancelled
			s.buildChanged(id)
		case errors.Is(err, artifact.ErrConflict):
			// It ended meanwhile.
			if r, err = builds.Build(ctx, id); err != nil {
				return nil, artifactError(err)
			}
		default:
			return nil, artifactError(err)
		}
	}
	return s.serverBuildView(ctx, builds, r, 0, true)
}

// buildUpkeep returns builds whose builder was lost to the queue, or
// fails them after their last attempt, and trims old finished builds.
func (s *Service) buildUpkeep(ctx context.Context, builds artifact.Builds, now time.Time) error {
	failed, err := json.Marshal(serverBuildResult{Error: fmt.Sprintf("the builder stopped responding %d times", serverBuildAttempts)})
	if err != nil {
		return err
	}
	changed, err := builds.ExpireBuilds(ctx, now, serverBuildAttempts, failed)
	if err != nil {
		return err
	}
	for _, r := range changed {
		s.buildChanged(r.ID)
	}
	_, err = builds.TrimBuilds(ctx, serverBuildsRetained)
	return err
}

// buildWatch returns a channel closed on the build's next change.
func (s *Service) buildWatch(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.buildWatchers[id]
	if ch == nil {
		ch = make(chan struct{})
		s.buildWatchers[id] = ch
	}
	return ch
}

// buildChanged wakes the build's watchers: on this instance's own writes
// and on the change feed's build changes.
func (s *Service) buildChanged(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch := s.buildWatchers[id]; ch != nil {
		close(ch)
		delete(s.buildWatchers, id)
	}
	if s.buildQueued != nil {
		close(s.buildQueued)
		s.buildQueued = nil
	}
}
