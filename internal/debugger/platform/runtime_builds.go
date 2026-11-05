package platform

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/manifest"
)

// Runtime builds have their own slot and bounded queue, separate
// from the run slot, so a build never delays a run. Builds are kept in
// memory only: the runtime they produce lives in Docker's image store.

const (
	// buildQueueCapacity bounds queued builds beside the running one.
	buildQueueCapacity = 4
	// buildHistoryLimit is how many finished builds stay readable.
	buildHistoryLimit = 20
	// buildLogLimit is how many of a build's last log lines are kept.
	buildLogLimit = 2000
	// maxManifestBytes bounds a manifest read for a build.
	maxManifestBytes = 1 << 20
)

// BuildState is a runtime build's lifecycle state.
type BuildState string

const (
	BuildQueued    BuildState = "queued"
	BuildRunning   BuildState = "running"
	BuildDone      BuildState = "done"
	BuildFailed    BuildState = "failed"
	BuildCancelled BuildState = "cancelled"
)

// BuildRuntimeRequest builds a runtime from an OCB manifest.
type BuildRuntimeRequest struct {
	// Name is the runtime's name: lower-case letters, digits, '.', '_' and
	// '-', at most 63 characters, not "built-in". Building a name again
	// replaces that runtime once the build succeeds.
	Name string
	// Manifest is the OCB manifest: a path under the path policy, text or a
	// saved manifest revision. Its bytes are never changed.
	Manifest Source
	// ManifestDir is the directory an inline manifest's relative local
	// directories resolve against, like a scenario's directory; a manifest
	// file's own directory is used otherwise.
	ManifestDir string
	// Sources supply modules' sources beside the manifest; a manifest
	// file's own local directories (`path:`, replaces) are read relative
	// to it unless a source here replaces them.
	Sources []SourceOverride
	// GoImage overrides the pinned Go image the build compiles in.
	GoImage string
	// ModuleProxy is GOPROXY for this build; empty passes the debugger's
	// module environment through.
	ModuleProxy string
	// Offline builds from the module cache only, without network access.
	Offline bool
}

// RuntimeBuild is a build's state and the tail of its log.
type RuntimeBuild struct {
	ID       string
	Caller   Actor
	Name     string
	State    BuildState
	Created  time.Time
	Started  time.Time // Zero while queued.
	Finished time.Time // Zero until terminal.
	// Log holds the build's log lines from LogFrom on, at most the last
	// 2000; LogLines counts every line the build wrote.
	Log      []string
	LogFrom  int
	LogLines int
	// Runtime is the built runtime once the build is done.
	Runtime *RuntimeInfo
	// Error is why a build failed; the compiler output is in Log.
	Error string
	// CollectorVersion is the manifest's release; Verified is false for a
	// release outside the support table.
	CollectorVersion string
	Verified         bool
	// Warnings name the manifest fields the builder ignores and the
	// optional features the runtime will lack.
	Warnings []string
	// Reused is set when a team server took the binary of an earlier
	// build of identical inputs instead of building again.
	Reused bool
	// Image is the runtime's image by digest when the server exports
	// builds, tagged with the name of the build that made the
	// binary; ExportError is why that export failed. Neither fails a build.
	Image       string
	ExportError string
	// Manifest is the manifest the build was requested with, as sent, so
	// that any browser can rebuild it. Only GetRuntimeBuild fills it; the
	// sources are never returned.
	Manifest string
}

// BuildListRequest pages the runtime builds, newest first.
type BuildListRequest struct {
	// Limit is the page size: default 50, at most 500.
	Limit int
	// Cursor is the previous page's NextCursor.
	Cursor string
}

// RuntimeBuildPage is a page of builds without logs and the cursor of the
// next page, empty after the last.
type RuntimeBuildPage struct {
	Builds     []RuntimeBuild
	NextCursor string
}

// Terminal reports whether the build has ended.
func (b *RuntimeBuild) Terminal() bool {
	return b.State == BuildDone || b.State == BuildFailed || b.State == BuildCancelled
}

type buildJob struct {
	view    RuntimeBuild // Log, LogFrom and LogLines are maintained below.
	spec    RuntimeBuildSpec
	log     []string // the last buildLogLimit lines
	ctx     context.Context
	cancel  context.CancelFunc
	changed chan struct{}
}

func (j *buildJob) notifyLocked() {
	close(j.changed)
	j.changed = make(chan struct{})
}

// snapshotLocked copies the build with its log lines from index from on.
func (j *buildJob) snapshotLocked(from int) RuntimeBuild {
	v := j.view
	first := j.view.LogLines - len(j.log)
	from = max(from, first)
	if from < j.view.LogLines {
		v.Log = append([]string(nil), j.log[from-first:]...)
	} else {
		v.Log = []string{}
		from = j.view.LogLines
	}
	v.LogFrom = from
	if v.Runtime != nil {
		info := *v.Runtime
		v.Runtime = &info
	}
	return v
}

type buildQueue struct {
	s       *Service
	mu      sync.Mutex
	closed  bool
	started bool
	queue   []*buildJob
	jobs    map[string]*buildJob
	order   []string
	wake    chan struct{}
	done    chan struct{}
}

func newBuildQueue(s *Service) *buildQueue {
	return &buildQueue{s: s, jobs: map[string]*buildJob{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// close cancels every build and waits for the running one to stop.
func (q *buildQueue) close() {
	q.mu.Lock()
	q.closed = true
	started := q.started
	for _, j := range q.jobs {
		j.cancel()
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	q.mu.Unlock()
	if started {
		<-q.done
	}
}

// BuildRuntime checks a build request and queues it on the build slot. A
// manifest refused before compiling is validation_failed with one
// diagnostic per problem; a full build queue is resource_exhausted. It
// returns without waiting: WatchRuntimeBuild follows the build.
func (s *Service) BuildRuntime(ctx context.Context, caller Caller, req BuildRuntimeRequest) (_ *RuntimeBuild, err error) {
	defer s.audit(&caller, "BuildRuntime", &err)()
	// A team server queues builds for its builders.
	perm := authz.RuntimeManage
	builds, server := s.serverBuilds()
	if server {
		perm = authz.RuntimeBuild
	}
	if err := s.begin(ctx, caller, perm); err != nil {
		return nil, err
	}
	if server {
		return s.queueServerBuild(ctx, caller, builds, req)
	}
	store, err := s.runtimeStore()
	if err != nil {
		return nil, err
	}
	if req.Name == BuiltInRuntime || !validRuntimeName(req.Name) {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf(
			"runtime name %q: use lower-case letters, digits, '.', '_' and '-', at most 63 characters, not %q", req.Name, BuiltInRuntime)}
	}
	in, err := s.buildInputs(ctx, caller, req.Manifest, req.ManifestDir, req.Sources)
	if err != nil {
		return nil, err
	}
	spec := RuntimeBuildSpec{
		Name: req.Name, Manifest: in.raw, Release: in.release, Sources: in.sources,
		GoImage: req.GoImage, ModuleProxy: req.ModuleProxy, Offline: req.Offline,
	}
	if err := store.Check(spec); err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
	view := RuntimeBuild{CollectorVersion: in.m.CollectorVersion(), Verified: in.verified, Warnings: in.warnings("runtime")}
	return s.builds.add(caller, spec, view)
}

// buildInputs are a build's checked manifest and sources, what runtime and
// CLI builds take.
type buildInput struct {
	raw      []byte
	m        *manifest.Manifest
	verified bool
	// release is the manifest release's support table entry; nil for an
	// unverified release.
	release *versions.Release
	sources []BuildSource
}

// buildInputs reads a build's manifest, checks its release against the
// support table and reads its sources, relative to the manifest file's
// directory or else dir.
func (s *Service) buildInputs(ctx context.Context, caller Caller, src Source, dir string, overrides []SourceOverride) (*buildInput, error) {
	if _, err := s.resolveSource(ctx, caller, &src, artifact.Manifest, "manifest"); err != nil {
		return nil, err
	}
	raw, err := (config.Source{Path: src.Path, YAML: src.Text}).BytesWith(s.policy.Paths)
	if err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
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
	if src.Path != "" {
		dir = filepath.Dir(src.Path)
	}
	sources, err := s.buildSources(m, dir, overrides)
	if err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
	return &buildInput{raw: raw, m: m, verified: support == versions.Verified, release: s.versionTable().Release(m.CollectorVersion()), sources: sources}, nil
}

// warnings name the manifest fields the builder ignores and the optional
// features what is built (a runtime, the CLI) will lack.
func (in *buildInput) warnings(what string) []string {
	out := manifest.Warnings(in.raw)
	var modules []string
	for _, e := range in.m.Entries() {
		modules = append(modules, e.ModulePath())
	}
	for _, f := range features.Lacking(modules) {
		out = append(out, fmt.Sprintf("the manifest has no %s, so the %s lacks the %s feature and its reports say so", f.Component, what, f.Name))
	}
	return out
}

// validRuntimeName mirrors container.ValidName, which the platform's
// composition supplies; the rule is part of this operation's contract.
func validRuntimeName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

func (q *buildQueue) add(caller Caller, spec RuntimeBuildSpec, view RuntimeBuild) (*RuntimeBuild, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, ErrClosed
	}
	if len(q.queue) >= buildQueueCapacity {
		return nil, &Error{Code: CodeResourceExhausted, Message: fmt.Sprintf("%d runtime builds are already queued; wait for one to finish", buildQueueCapacity), Retryable: true}
	}
	ctx, cancel := context.WithCancel(q.s.ctx)
	view.ID, view.Caller, view.Name, view.State, view.Created = instrument.NewRunID(), caller.Actor(), spec.Name, BuildQueued, time.Now().UTC()
	if view.Warnings == nil {
		view.Warnings = []string{}
	}
	j := &buildJob{view: view, spec: spec, ctx: ctx, cancel: cancel, changed: make(chan struct{})}
	q.jobs[j.view.ID] = j
	q.order = append(q.order, j.view.ID)
	q.queue = append(q.queue, j)
	q.evictLocked()
	if !q.started {
		q.started = true
		go q.work()
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	v := j.snapshotLocked(0)
	return &v, nil
}

// evictLocked keeps unfinished builds and the newest finished ones.
func (q *buildQueue) evictLocked() {
	finished := 0
	for i := len(q.order) - 1; i >= 0; i-- {
		j := q.jobs[q.order[i]]
		if !j.view.Terminal() {
			continue
		}
		if finished++; finished > buildHistoryLimit {
			delete(q.jobs, q.order[i])
			q.order = append(q.order[:i], q.order[i+1:]...)
		}
	}
}

// active reports whether a build is queued or running.
func (q *buildQueue) active() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queue) > 0 {
		return true
	}
	for _, j := range q.jobs {
		if j.view.State == BuildRunning {
			return true
		}
	}
	return false
}

func (q *buildQueue) work() {
	defer close(q.done)
	for {
		q.mu.Lock()
		var j *buildJob
		for len(q.queue) > 0 && j == nil {
			j, q.queue = q.queue[0], q.queue[1:]
			if j.ctx.Err() != nil {
				q.finishLocked(j, BuildCancelled, nil, nil)
				j = nil
			}
		}
		closed := q.closed
		if j != nil {
			j.view.State, j.view.Started = BuildRunning, time.Now().UTC()
			j.notifyLocked()
		}
		q.mu.Unlock()
		if j == nil {
			if closed {
				return
			}
			<-q.wake
			continue
		}
		log := func(line string) { q.logLine(j, line) }
		for _, w := range j.view.Warnings {
			log("warning: " + w)
		}
		info, err := q.build(j, log)
		q.mu.Lock()
		switch {
		case err == nil:
			q.finishLocked(j, BuildDone, info, nil)
		case j.ctx.Err() != nil:
			q.finishLocked(j, BuildCancelled, nil, nil)
		default:
			q.finishLocked(j, BuildFailed, nil, err)
		}
		q.evictLocked()
		q.mu.Unlock()
	}
}

// build fetches the build's git sources on the host, then builds in the
// store.
func (q *buildQueue) build(j *buildJob, log func(string)) (*RuntimeInfo, error) {
	if err := fetchGitSources(j.ctx, j.spec.Sources, q.s.policy.Paths.Unrestricted, q.s.gitClone, log); err != nil {
		return nil, err
	}
	return q.s.runtimes.Build(j.ctx, j.spec, log)
}

func (q *buildQueue) logLine(j *buildJob, line string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j.log = append(j.log, strings.TrimRight(line, "\r\n"))
	if len(j.log) > buildLogLimit {
		j.log = append(j.log[:0], j.log[len(j.log)-buildLogLimit:]...)
	}
	j.view.LogLines++
	j.notifyLocked()
}

func (q *buildQueue) finishLocked(j *buildJob, state BuildState, info *RuntimeInfo, err error) {
	j.view.State, j.view.Finished, j.view.Runtime = state, time.Now().UTC(), info
	if err != nil {
		j.view.Error = err.Error()
	}
	j.cancel()
	j.notifyLocked()
}

func (q *buildQueue) job(id string) (*buildJob, error) {
	j := q.jobs[id]
	if j == nil {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("runtime build %s not found", id)}
	}
	return j, nil
}

// GetRuntimeBuild returns a build with its log lines from logFrom on.
func (s *Service) GetRuntimeBuild(ctx context.Context, caller Caller, id string, logFrom int) (_ *RuntimeBuild, err error) {
	defer s.audit(&caller, "GetRuntimeBuild", &err)()
	v, _, err := s.WatchRuntimeBuild(ctx, caller, id, logFrom)
	if err != nil {
		return nil, err
	}
	// WatchRuntimeBuild checked that the caller may see the build.
	if builds, ok := s.serverBuilds(); ok {
		raw, err := builds.BuildManifest(ctx, id)
		if err != nil {
			return nil, artifactError(err)
		}
		v.Manifest = string(raw)
		return v, nil
	}
	q := s.builds
	q.mu.Lock()
	defer q.mu.Unlock()
	j, err := q.job(id)
	if err != nil {
		return nil, err
	}
	v.Manifest = string(j.spec.Manifest)
	return v, nil
}

// WatchRuntimeBuild returns a build as GetRuntimeBuild does and a channel
// closed on its next change. Watching never cancels a build.
func (s *Service) WatchRuntimeBuild(ctx context.Context, caller Caller, id string, logFrom int) (_ *RuntimeBuild, _ <-chan struct{}, err error) {
	defer s.audit(&caller, "WatchRuntimeBuild", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeRead); err != nil {
		return nil, nil, err
	}
	if builds, ok := s.serverBuilds(); ok {
		return s.watchServerBuild(ctx, caller, builds, id, logFrom)
	}
	q := s.builds
	q.mu.Lock()
	defer q.mu.Unlock()
	j, err := q.job(id)
	if err != nil {
		return nil, nil, err
	}
	v := j.snapshotLocked(logFrom)
	return &v, j.changed, nil
}

// WaitRuntimeBuild waits until a build ends or ctx is done, then returns
// it with its log from logFrom on; ctx ending is not an error and never
// cancels the build.
func (s *Service) WaitRuntimeBuild(ctx context.Context, caller Caller, id string, logFrom int) (_ *RuntimeBuild, err error) {
	defer s.audit(&caller, "WaitRuntimeBuild", &err)()
	for {
		v, changed, err := s.WatchRuntimeBuild(context.WithoutCancel(ctx), caller, id, logFrom)
		if err != nil || v.Terminal() {
			return v, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return v, nil
		}
	}
}

// ListRuntimeBuilds pages the retained builds, newest first, without
// logs. On a team server a caller sees their own builds, and every build
// with runtime.manage. A local service retains at most a few dozen builds
// and returns them on one page.
func (s *Service) ListRuntimeBuilds(ctx context.Context, caller Caller, req BuildListRequest) (_ *RuntimeBuildPage, err error) {
	defer s.audit(&caller, "ListRuntimeBuilds", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeRead); err != nil {
		return nil, err
	}
	if req.Limit < 0 || req.Limit > artifact.MaxBuildPage {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("limit must be 0 to %d", artifact.MaxBuildPage)}
	}
	if builds, ok := s.serverBuilds(); ok {
		return s.listServerBuilds(ctx, caller, builds, req)
	}
	if req.Cursor != "" {
		return nil, &Error{Code: CodeInvalidRequest, Message: "a local service lists its builds on one page: send no cursor"}
	}
	q := s.builds
	q.mu.Lock()
	defer q.mu.Unlock()
	out := &RuntimeBuildPage{Builds: make([]RuntimeBuild, 0, len(q.order))}
	for i := len(q.order) - 1; i >= 0; i-- {
		v := q.jobs[q.order[i]].snapshotLocked(0)
		v.Log, v.LogFrom = []string{}, v.LogLines
		out.Builds = append(out.Builds, v)
	}
	return out, nil
}

// CancelRuntimeBuild cancels a queued or running build; cancelling a
// finished build changes nothing.
func (s *Service) CancelRuntimeBuild(ctx context.Context, caller Caller, id string) (_ *RuntimeBuild, err error) {
	defer s.audit(&caller, "CancelRuntimeBuild", &err)()
	// On a team server the owner may cancel their build, runtime.manage
	// any build.
	perm := authz.RuntimeManage
	builds, server := s.serverBuilds()
	if server {
		perm = authz.RuntimeRead
	}
	if err := s.begin(ctx, caller, perm); err != nil {
		return nil, err
	}
	if server {
		return s.cancelServerBuild(ctx, caller, builds, id)
	}
	q := s.builds
	q.mu.Lock()
	defer q.mu.Unlock()
	j, err := q.job(id)
	if err != nil {
		return nil, err
	}
	if j.view.State == BuildQueued {
		for i, queued := range q.queue {
			if queued == j {
				q.queue = append(q.queue[:i], q.queue[i+1:]...)
				break
			}
		}
		q.finishLocked(j, BuildCancelled, nil, nil)
	} else {
		j.cancel() // The build slot records the cancellation when it stops.
	}
	v := j.snapshotLocked(0)
	v.Log, v.LogFrom = []string{}, v.LogLines
	return &v, nil
}
