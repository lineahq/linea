package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// Builders dial the server with the build token and claim
// queued builds up to their capacity. A build's inputs reach the builder
// and its binary comes back through presigned object-storage URLs; the
// server checks the stored binary, ends the build and registers the
// runtime in the catalog under the build's name.

var (
	// buildLease is how long a builder's claim lasts without a renewal;
	// builds renew every buildRenew. buildPoll is how often a builder with
	// free capacity looks for queued builds without a change waking it.
	// Tests shorten them.
	buildLease = 2 * time.Minute
	buildRenew = 30 * time.Second
	buildPoll  = 15 * time.Second
	// buildFlush is how often a running build's log is stored.
	buildFlush = time.Second
)

const (
	// serverBuildTimeout bounds one build on a builder.
	serverBuildTimeout = 30 * time.Minute
	// buildURLLifetime is how long a build's presigned URLs last.
	buildURLLifetime = serverBuildTimeout + 5*time.Minute
	// buildLogPending bounds the log lines a build holds before they are
	// stored; older ones are dropped.
	buildLogPending = 4 * artifact.MaxBuildLogLines
)

// Why a build's context ended: only a requested cancellation ends the
// build cancelled, and only the timeout fails it; anything else leaves it
// to expire, so another attempt runs it.
var (
	errBuildCancelRequested = errors.New("the build was cancelled")
	errBuildLeaseLost       = errors.New("the builder no longer holds the build")
	errBuildTimeout         = fmt.Errorf("the build exceeded %s", serverBuildTimeout)
	errBuilderGone          = errors.New("the builder disconnected")
)

// builderTransfers reports the repository's build queue and transfers when
// the service takes builders: a team server whose repository has object
// storage, with a build token.
func (s *Service) builderTransfers() (artifact.Builds, artifact.Transfers, artifact.BuildTransfers, bool) {
	builds, ok := s.serverBuilds()
	if !ok || s.builderToken == "" {
		return nil, nil, nil, false
	}
	transfers, ok := s.repository.(artifact.Transfers)
	if !ok {
		return nil, nil, nil, false
	}
	bt, ok := s.repository.(artifact.BuildTransfers)
	if !ok {
		return nil, nil, nil, false
	}
	return builds, transfers, bt, true
}

// ConnectBuilder serves a builder that dials the server until
// its connection ends. The builder presents the build token; a service
// without one, or whose repository has no object storage, refuses every
// builder. It is the HTTP adapter's handler for the builder endpoint, which
// carries the builder protocol rather than an operation's request.
func (s *Service) ConnectBuilder(w http.ResponseWriter, r *http.Request) {
	builds, transfers, bt, ok := s.builderTransfers()
	if !ok {
		http.Error(w, "this server accepts no builders: it needs a build token and object storage", http.StatusNotFound)
		return
	}
	b, err := worker.AcceptBuilder(w, r, func(token string) bool { return worker.ConstantTimeEqual(token, s.builderToken) })
	if err != nil {
		s.logWorker("builder connection refused", r.RemoteAddr, "", err)
		return
	}
	h := b.Hello()
	holder := builderHolder(h.Name)
	s.logWorker("builder connected", r.RemoteAddr, h.Name, nil, zap.String("builder", holder), zap.Int("capacity", h.Capacity),
		zap.String("os", h.OS), zap.String("arch", h.Arch), zap.String("go", h.GoVersion))
	s.builders.add(h.Capacity)
	defer s.builders.remove(h.Capacity)
	stop := context.AfterFunc(s.ctx, func() { _ = b.Close() })
	defer stop()
	ctx, cancel := context.WithCancel(s.ctx)
	go func() {
		<-b.Done()
		cancel()
	}()
	s.serveBuilder(ctx, &builderLink{conn: b, holder: holder, builds: builds, transfers: transfers, bt: bt})
	_ = b.Close()
	s.logWorker("builder disconnected", r.RemoteAddr, h.Name, nil, zap.String("builder", holder))
}

// builderHolder names one builder connection in leases: two builders of
// one name never share leases.
func builderHolder(name string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	if len(name) > 64 {
		name = name[:64]
	}
	return name + "-" + hex.EncodeToString(b[:])
}

// builderLink is one connected builder and the repository it builds for.
type builderLink struct {
	conn      *worker.BuilderConn
	holder    string
	builds    artifact.Builds
	transfers artifact.Transfers
	bt        artifact.BuildTransfers
}

// serveBuilder claims builds for the builder while it has free capacity,
// renews their leases and cancels those whose cancellation was requested
// or whose lease was lost, until ctx ends; it then cancels what still runs
// and waits for it. A build that never finishes returns to the queue when
// its lease ends. A draining builder gets no new build; once
// its builds ended, the server tells it so and returns.
func (s *Service) serveBuilder(ctx context.Context, l *builderLink) {
	var (
		mu      sync.Mutex
		running = map[string]context.CancelCauseFunc{}
		wg      sync.WaitGroup
	)
	finished := make(chan struct{}, 1)
	defer func() {
		mu.Lock()
		for _, cancel := range running {
			cancel(errBuilderGone)
		}
		mu.Unlock()
		wg.Wait()
	}()
	renew := time.NewTicker(buildRenew)
	defer renew.Stop()
	poll := time.NewTicker(buildPoll)
	defer poll.Stop()
	capacity := l.conn.Hello().Capacity
	draining, stopping := l.conn.Draining(), false
	for {
		queued := s.buildQueuedWatch()
		mu.Lock()
		free := capacity - len(running)
		mu.Unlock()
		if stopping && free == capacity {
			if err := l.conn.Drained(); err != nil && ctx.Err() == nil {
				s.logWorker("draining the builder failed", l.conn.Endpoint(), l.conn.Hello().Name, err)
			}
			return
		}
		if free > 0 && !stopping {
			now := s.now().UTC()
			claimed, err := l.builds.ClaimBuilds(ctx, l.holder, free, now, now.Add(buildLease))
			if err != nil && ctx.Err() == nil {
				s.logWorker("claiming builds failed", l.conn.Endpoint(), l.conn.Hello().Name, err)
			}
			for _, r := range claimed {
				s.buildChanged(r.ID)
				bctx, cancel := context.WithCancelCause(ctx)
				mu.Lock()
				running[r.ID] = cancel
				mu.Unlock()
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.runBuild(bctx, l, r)
					cancel(nil)
					mu.Lock()
					delete(running, r.ID)
					mu.Unlock()
					select {
					case finished <- struct{}{}:
					default:
					}
				}()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-draining:
			draining, stopping = nil, true
			s.logWorker("builder draining", l.conn.Endpoint(), l.conn.Hello().Name, nil, zap.String("builder", l.holder))
			continue
		case <-queued:
		case <-finished:
		case <-poll.C:
		case <-renew.C:
			held, err := l.builds.RenewBuilds(ctx, l.holder, s.now().UTC().Add(buildLease))
			if err != nil {
				if ctx.Err() == nil {
					s.logWorker("renewing builds failed", l.conn.Endpoint(), l.conn.Hello().Name, err)
				}
				continue
			}
			cancelled := map[string]bool{}
			for _, r := range held {
				cancelled[r.ID] = r.CancelRequested
			}
			mu.Lock()
			for id, cancel := range running {
				if c, ok := cancelled[id]; !ok {
					cancel(errBuildLeaseLost)
				} else if c {
					cancel(errBuildCancelRequested)
				}
			}
			mu.Unlock()
		}
	}
}

// runBuild gives one claimed build to the builder and ends it with the
// builder's result. When the builder is lost or the lease is, it leaves
// the build to expire, so another attempt runs it.
func (s *Service) runBuild(ctx context.Context, l *builderLink, r artifact.BuildRecord) {
	fail := func(msg string) { s.finishBuild(l, r, artifact.BuildFailed, serverBuildResult{Error: msg}, nil) }
	var spec serverBuildSpec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		fail("the build's stored description is unreadable")
		return
	}
	job, stage, err := s.buildJob(ctx, l, r, spec)
	if err != nil {
		if ctx.Err() == nil {
			fail(fmt.Sprintf("preparing the build's inputs failed: %v", err))
		}
		return
	}
	bctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	bctx, stopDeadline := context.WithDeadlineCause(bctx, job.Deadline, errBuildTimeout)
	defer stopDeadline()
	log := &buildLogBuffer{}
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		t := time.NewTicker(buildFlush)
		defer t.Stop()
		for {
			select {
			case <-bctx.Done():
				return
			case <-t.C:
				s.flushBuildLog(bctx, l, r, log, func() { cancel(errBuildLeaseLost) })
			}
		}
	}()
	result, err := l.conn.Build(bctx, job, log.add)
	cause := context.Cause(bctx)
	stopDeadline()
	<-flushed
	s.flushBuildLog(context.WithoutCancel(ctx), l, r, log, func() {})
	switch {
	case err != nil:
		s.logWorker("build lost its builder", l.conn.Endpoint(), l.conn.Hello().Name, err, zap.String("build", r.ID))
		return
	case errors.Is(cause, errBuildCancelRequested):
		s.finishBuild(l, r, artifact.BuildCancelled, serverBuildResult{Error: errBuildCancelRequested.Error()}, nil)
		return
	case errors.Is(cause, errBuildTimeout):
		fail(errBuildTimeout.Error())
		return
	case cause != nil:
		return // the lease was lost, the builder went or the server stops
	case result.Error != "":
		fail(artifact.ClipBuildLine(result.Error))
		return
	}
	if problem := checkBuiltRuntime(result, spec); problem != "" {
		fail(problem)
		return
	}
	err = l.bt.StoreBuildBinary(context.WithoutCancel(ctx), r.ID, l.holder, r.Attempt, stage, result.BinaryDigest, result.BinarySize, artifact.MaxRuntimeBinary)
	switch {
	case errors.Is(err, artifact.ErrConflict):
		return // the lease ended meanwhile
	case errors.Is(err, artifact.ErrCorrupt), errors.Is(err, artifact.ErrNotFound):
		fail("the uploaded binary is missing or differs from the one the builder reported")
		return
	case errors.Is(err, artifact.ErrLimit):
		fail(fmt.Sprintf("the binary exceeds %d bytes or the server's storage limit", artifact.MaxRuntimeBinary))
		return
	case err != nil:
		s.logWorker("storing a built binary failed", l.conn.Endpoint(), l.conn.Hello().Name, err, zap.String("build", r.ID))
		return // the lease expires and another attempt runs
	}
	hello, err := json.Marshal(result.Hello)
	if err != nil {
		fail("the built worker's hello is unreadable")
		return
	}
	done := serverBuildResult{Runtime: result.Hello.Runtime, Hello: hello, OS: result.OS, Arch: result.Arch}
	done.Image, done.ExportError = checkExport(result, s.buildExport)
	s.finishBuild(l, r, artifact.BuildDone, done,
		&artifact.RuntimeBinary{Digest: result.BinaryDigest, Bytes: result.BinarySize, OS: result.OS, Arch: result.Arch})
}

// buildJob is the build as the builder receives it: presigned URLs for its
// inputs and for the binary, under the staged object name it returns.
func (s *Service) buildJob(ctx context.Context, l *builderLink, r artifact.BuildRecord, spec serverBuildSpec) (worker.BuildJob, string, error) {
	job := worker.BuildJob{Build: r.ID, Attempt: r.Attempt, Name: r.Name, CollectorVersion: spec.CollectorVersion, Verified: spec.Verified,
		BinaryLimit: artifact.MaxRuntimeBinary, Deadline: s.now().Add(serverBuildTimeout)}
	for _, in := range r.Inputs {
		u, err := l.bt.BuildInputURL(ctx, r.ID, in.Digest, buildURLLifetime)
		if err != nil {
			return job, "", err
		}
		input := worker.BuildInput{Module: in.Module, URL: u.URL, Digest: u.Digest, Size: u.Size}
		if in.Module == "" {
			job.Manifest = input
		} else {
			job.Sources = append(job.Sources, input)
		}
	}
	if job.Manifest.URL == "" {
		return job, "", fmt.Errorf("%w: build %s has no manifest", artifact.ErrCorrupt, r.ID)
	}
	stage := fmt.Sprintf("build-%s-%d", r.ID, r.Attempt)
	u, err := l.transfers.StageURL(ctx, stage, buildURLLifetime)
	if err != nil {
		return job, "", err
	}
	job.BinaryURL = u
	if s.buildExport != "" {
		job.Export = &worker.BuildExport{Repository: s.buildExport, Tag: r.Name}
	}
	return job, stage, nil
}

// checkExport returns the image and export error a build stores from the
// builder's result: an image only by digest in repository, the configured
// one, and nothing when the server asked for no export.
func checkExport(r worker.BuildResult, repository string) (image, exportError string) {
	switch {
	case repository == "":
		return "", ""
	case r.ExportError != "":
		return "", artifact.ClipBuildLine(r.ExportError)
	}
	digest, ok := strings.CutPrefix(r.Image, repository+"@")
	if !ok || !artifact.ValidRuntimeIdentity(digest) {
		return "", "the builder reported no image in " + repository
	}
	return r.Image, ""
}

// checkBuiltRuntime reports why the server refuses a builder's result for a
// build of spec, or "".
func checkBuiltRuntime(r worker.BuildResult, spec serverBuildSpec) string {
	h := r.Hello
	switch {
	case !artifact.ValidRuntimeIdentity(r.BinaryDigest) || r.BinarySize <= 0 || r.BinarySize > artifact.MaxRuntimeBinary:
		return "the builder reported no valid binary"
	case h == nil || !artifact.ValidRuntimeIdentity(h.Runtime):
		return "the built worker's hello names no runtime identity"
	case h.Protocol != worker.ProtocolVersion || h.ProductVersion != release.ProductVersion:
		return fmt.Sprintf("the built worker is %s speaking protocol %d; this server is %s speaking %d",
			h.ProductVersion, h.Protocol, release.ProductVersion, worker.ProtocolVersion)
	case h.CollectorVersion != spec.CollectorVersion:
		return fmt.Sprintf("the built worker runs Collector %s, the manifest names %s", h.CollectorVersion, spec.CollectorVersion)
	case h.Verified != spec.Verified:
		return "the built worker's verification differs from the manifest's Collector version"
	}
	err := artifact.ValidateRuntimeEntry(artifact.RuntimeEntry{Name: "check", Identity: h.Runtime, Build: "check",
		Binary: &artifact.RuntimeBinary{Digest: r.BinaryDigest, Bytes: r.BinarySize, OS: r.OS, Arch: r.Arch}, Registered: time.Unix(1, 0)})
	if err != nil {
		return "the builder reported an invalid platform: " + err.Error()
	}
	return ""
}

// finishBuild ends a build the builder holds and, for a done one,
// registers its runtime. A lease the builder no longer holds leaves the
// build as it is.
func (s *Service) finishBuild(l *builderLink, r artifact.BuildRecord, state artifact.BuildState, result serverBuildResult, binary *artifact.RuntimeBinary) {
	ctx := context.WithoutCancel(s.ctx)
	raw, err := json.Marshal(result)
	if err != nil {
		return
	}
	o := artifact.BuildOutcome{State: state, Result: raw}
	if binary != nil {
		o.BinaryDigest, o.BinaryBytes = binary.Digest, binary.Bytes
	}
	if state == artifact.BuildDone {
		// Registered before the build is seen done, so a caller that runs on
		// the name once it is done finds the runtime.
		built := r
		built.Result, built.BinaryDigest, built.BinaryBytes = raw, o.BinaryDigest, o.BinaryBytes
		if err := s.registerBuilt(ctx, built); err != nil {
			s.logWorker("registering a built runtime failed", l.conn.Endpoint(), l.conn.Hello().Name, err, zap.String("build", r.ID))
		}
	}
	if _, err := l.builds.FinishBuild(ctx, r.ID, l.holder, r.Attempt, o, s.now().UTC()); err != nil {
		if !errors.Is(err, artifact.ErrConflict) {
			s.logWorker("ending a build failed", l.conn.Endpoint(), l.conn.Hello().Name, err, zap.String("build", r.ID))
		}
		return
	}
	s.buildChanged(r.ID)
}

// registerBuilt records a done build's runtime in the catalog under the
// build's name, replacing the name's previous entry. Runs
// already admitted keep the runtime they were admitted with.
func (s *Service) registerBuilt(ctx context.Context, r artifact.BuildRecord) error {
	var result serverBuildResult
	if err := json.Unmarshal(r.Result, &result); err != nil {
		return fmt.Errorf("%w: build %s result: %w", artifact.ErrCorrupt, r.ID, err)
	}
	var hello worker.Hello
	if err := json.Unmarshal(result.Hello, &hello); err != nil {
		return fmt.Errorf("%w: build %s hello: %w", artifact.ErrCorrupt, r.ID, err)
	}
	hello.Name, hello.Capacity = "", 0
	raw, err := json.Marshal(hello)
	if err != nil {
		return err
	}
	e := artifact.RuntimeEntry{Name: r.Name, Identity: hello.Runtime, Build: r.ID, Hello: raw,
		Binary:       &artifact.RuntimeBinary{Digest: r.BinaryDigest, Bytes: r.BinaryBytes, OS: result.OS, Arch: result.Arch},
		RegisteredBy: r.Principal, Registered: s.now().UTC()}
	// The entry keeps the build's manifest, which outlives the build's
	// retention. Without it the runtime still runs; it is listed without
	// components once the build is trimmed.
	if builds, ok := s.serverBuilds(); ok {
		if m, err := builds.BuildManifest(ctx, r.ID); err == nil && len(m) <= artifact.MaxRuntimeManifest {
			e.Manifest = m
		}
	}
	counts, err := s.workerCounts(ctx)
	if err != nil {
		return err
	}
	if counts[e.Identity].Workers > 0 {
		e.Confirmed = e.Registered
	}
	return s.catalog.SaveRuntime(ctx, e)
}

// buildLogBuffer holds a running build's log lines until they are stored.
type buildLogBuffer struct {
	mu      sync.Mutex
	lines   []string
	dropped int
}

func (b *buildLogBuffer) add(lines []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, line := range lines {
		b.lines = append(b.lines, artifact.ClipBuildLine(line))
	}
	if over := len(b.lines) - buildLogPending; over > 0 {
		b.lines = append(b.lines[:0], b.lines[over:]...)
		b.dropped += over
	}
}

func (b *buildLogBuffer) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := b.lines
	if b.dropped > 0 {
		lines = append([]string{fmt.Sprintf("(%d earlier lines dropped)", b.dropped)}, lines...)
		b.dropped = 0
	}
	b.lines = nil
	return lines
}

// flushBuildLog stores the buffered lines; a lost lease calls lost.
func (s *Service) flushBuildLog(ctx context.Context, l *builderLink, r artifact.BuildRecord, log *buildLogBuffer, lost func()) {
	lines := log.take()
	if len(lines) == 0 {
		return
	}
	err := l.builds.AppendBuildLog(ctx, r.ID, l.holder, r.Attempt, lines)
	switch {
	case err == nil:
		s.buildChanged(r.ID)
	case errors.Is(err, artifact.ErrConflict):
		lost()
	case ctx.Err() == nil:
		s.logWorker("storing a build log failed", l.conn.Endpoint(), l.conn.Hello().Name, err, zap.String("build", r.ID))
	}
}

// buildQueuedWatch returns a channel closed on the next build change, which
// may have queued a build.
func (s *Service) buildQueuedWatch() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buildQueued == nil {
		s.buildQueued = make(chan struct{})
	}
	return s.buildQueued
}
