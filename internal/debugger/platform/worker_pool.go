package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// Connected workers. A worker started with --connect dials
// ConnectWorker with the registration token and announces a name, its
// runtime identity and its capacity. Requests reach it through the runtime
// catalog, by identity; its name labels it in logs. Each connected worker
// is an executor with its own identity in the queue: the scheduler claims runs of
// its runtime up to its free capacity, and the service renews its leases
// while it stays connected. A worker that disconnects is no longer renewed,
// so its runs return to the queue when their leases end. Inspections and
// previews take a free slot on a worker of their runtime ahead of queued
// runs. Each instance records its workers in the repository (presence.go),
// so the catalog counts the workers of every instance.
//
// A pool worker says hello with the platform it runs and
// serves every runtime built on the server for that platform: it claims
// the queued runs pinned to a binary of its platform, and each request it
// gets names the binary, with a presigned URL it fetches the binary from.

const (
	// bundleURLLifetime bounds a presigned bundle download; the worker
	// starts it on assignment.
	bundleURLLifetime = 5 * time.Minute
	// binaryURLLifetime bounds a presigned runtime binary download; a pool
	// worker starts it when it gets a request for a runtime it has not
	// cached.
	binaryURLLifetime = 15 * time.Minute
	// resultURLMargin is how long a result upload stays possible after the
	// run's execution budget ends.
	resultURLMargin = 5 * time.Minute
	// maxResultBytes bounds a result a worker uploads, as a frame bounds
	// one sent inline.
	maxResultBytes = 256 << 20
)

// executor runs claimed jobs under one identity in the queue: the
// service's own worker (in process, or a runtime the service started), or
// a connected worker.
type executor struct {
	id       string
	capacity int
	// busy counts the slots taken: claimed runs, a claim's reservation
	// while it is in progress, and interactive requests.
	busy    int
	renewed time.Time
	// conn is a connected worker; nil for the service's own executor.
	conn *connectedWorker
	gone bool
}

type connectedWorker struct {
	client *worker.Client
	hello  worker.Hello
	since  time.Time
}

func (e *executor) free() int { return e.capacity - e.busy }

// pool is the platform ("linux/amd64") a pool worker runs binaries for;
// empty for a worker of one runtime and for the service's executor.
func (e *executor) pool() string {
	if e.conn == nil || e.conn.hello.Pool == nil {
		return ""
	}
	return e.conn.hello.Pool.Platform()
}

// own is the runtime a pool runs from its own build, without a binary; empty
// for a pool that does not and for any other executor.
func (e *executor) own() string {
	if e.conn == nil || e.conn.hello.Pool == nil || !e.conn.hello.Pool.Own {
		return ""
	}
	return e.conn.hello.Runtime
}

// held reports whether an interactive request waits for e's slots, so the
// scheduler claims no runs for it.
func (e *executor) held(waiting map[string]int) bool {
	return waiting[e.presence()] > 0 || e.own() != "" && waiting[e.own()] > 0
}

// presence is the identity a connected worker is counted under: its
// runtime's, or for a pool poolPresence of its platform.
func (e *executor) presence() string {
	if p := e.pool(); p != "" {
		return poolPresence(p)
	}
	return e.conn.hello.Runtime
}

// poolPresence is the identity the pool workers of platform are counted
// under, which no runtime identity ("sha256:<hex>") can be.
func poolPresence(platform string) string { return artifact.PoolIdentity(platform) }

// serves reports whether e takes jobs of runtime key: the service's
// executor takes jobs on its own runtimes, a connected worker those of its
// runtime that were bound to connected workers, and a pool those of any
// runtime bound to connected workers, of which it claims the runs pinned
// to a binary of its platform.
func (e *executor) serves(pooled bool, key string) bool {
	if e.conn == nil {
		return !pooled
	}
	return pooled && (e.pool() != "" || key == e.conn.hello.Runtime)
}

// runs reports whether e is a connected worker that runs target, whose
// stored binary is binary (nil for a runtime registered from an image): a
// worker of its identity, a pool of the binary's platform, or a pool whose
// own build it is.
func (e *executor) runs(target engine.RuntimeTarget, binary *artifact.RunBinary) bool {
	switch {
	case e.conn == nil || e.gone:
		return false
	case e.pool() != "":
		return binary != nil && binary.Platform == e.pool() || binary == nil && target.ID != "" && target.ID == e.own()
	}
	return e.conn.hello.Runtime == target.ID
}

// waitKeys are the keys of s.waiting that hold an interactive request for
// target back: its identity, and its binary's pool.
func waitKeys(target engine.RuntimeTarget, binary *artifact.RunBinary) []string {
	if binary == nil {
		return []string{target.ID}
	}
	return []string{target.ID, poolPresence(binary.Platform)}
}

// ConnectWorker serves a worker that dials the server until
// its connection ends. The worker presents the registration token; a
// service without one refuses every worker. It is the HTTP adapter's
// handler for the worker endpoint, which carries the worker protocol
// rather than an operation's request.
func (s *Service) ConnectWorker(w http.ResponseWriter, r *http.Request) {
	if s.workerToken == "" {
		http.Error(w, "this server accepts no workers: it has no registration token", http.StatusNotFound)
		return
	}
	c, err := worker.Accept(w, r, func(token string) bool { return worker.ConstantTimeEqual(token, s.workerToken) })
	if err != nil {
		s.logWorker("worker connection refused", r.RemoteAddr, "", err)
		return
	}
	h := c.Hello()
	if h.Name == BuiltInRuntime || !validRuntimeName(h.Name) || h.Capacity < 1 {
		s.logWorker("worker connection refused", r.RemoteAddr, h.Name, fmt.Errorf("runtime name %q or capacity %d is invalid", h.Name, h.Capacity))
		_ = c.Close()
		return
	}
	if h.Pool != nil {
		_, stored := s.repository.(artifact.BuildTransfers)
		var err error
		switch {
		case !stored:
			err = errors.New("this server stores no runtime binaries for pool workers")
		case !(artifact.RunBinary{Digest: h.Runtime, Bytes: 1, Platform: h.Pool.Platform()}).Valid() || h.Pool.Warm < 0:
			err = fmt.Errorf("pool platform %q or warm %d is invalid", h.Pool.Platform(), h.Pool.Warm)
		}
		if err != nil {
			s.logWorker("worker connection refused", r.RemoteAddr, h.Name, err)
			_ = c.Close()
			return
		}
	}
	e := &executor{id: newExecutorID(), capacity: h.Capacity, renewed: s.now(), conn: &connectedWorker{client: c, hello: h, since: s.now()}}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return
	}
	s.executors = append(s.executors, e)
	s.wakeLocked()
	s.mu.Unlock()
	if p := e.pool(); p != "" {
		s.logWorker("pool worker connected", r.RemoteAddr, h.Name, nil, zap.String("platform", p), zap.Int("capacity", h.Capacity), zap.Int("warm", h.Pool.Warm), zap.String("worker", e.id))
	} else {
		s.logWorker("worker connected", r.RemoteAddr, h.Name, nil, zap.String("runtime", h.Runtime), zap.Int("capacity", h.Capacity), zap.String("worker", e.id))
	}
	// A pool serves its own runtime only when it says so.
	if e.pool() == "" || e.own() != "" {
		s.confirmRuntimes(h.Runtime)
		s.registerBuiltIn(h)
	}
	s.publishWorkers(s.ctx, s.now())
	if e.pool() != "" {
		go s.followWarm(e)
	}
	<-c.Exited()
	s.mu.Lock()
	e.gone = true
	s.executors = slices.DeleteFunc(s.executors, func(x *executor) bool { return x == e })
	s.freedLocked()
	s.mu.Unlock()
	s.logWorker("worker disconnected", r.RemoteAddr, h.Name, nil, zap.String("worker", e.id))
	s.publishWorkers(s.ctx, s.now())
}

// followWarm records a pool again whenever the runtimes it holds running
// change, until it disconnects, and looks for runs it may now claim.
func (s *Service) followWarm(e *executor) {
	for {
		_, changed := e.conn.client.Warm()
		select {
		case <-changed:
		case <-e.conn.client.Exited():
			return
		case <-s.ctx.Done():
			return
		}
		s.publishWorkers(s.ctx, s.now())
		s.mu.Lock()
		s.wakeLocked()
		s.mu.Unlock()
	}
}

func (s *Service) logWorker(msg, peer, name string, err error, fields ...zap.Field) {
	if s.logger == nil {
		return
	}
	fields = append(fields, zap.String("peer", peer))
	if name != "" {
		fields = append(fields, zap.String("name", name))
	}
	if err != nil {
		s.logger.Warn(msg, append(fields, zap.Error(err))...)
		return
	}
	s.logger.Info(msg, fields...)
}

// closeConnections closes every connected worker; the service is closing
// and nothing runs on them any more.
func (s *Service) closeConnections() {
	s.mu.Lock()
	var clients []*worker.Client
	for _, e := range s.executors {
		if e.conn != nil {
			clients = append(clients, e.conn.client)
		}
	}
	s.mu.Unlock()
	for _, c := range clients {
		_ = c.Close()
	}
}

// pooledByIdentity binds the runtime a connected worker serves with the
// identity of target, for a recovered run whose bundle names it: a worker
// of that identity, or the catalog entry of a runtime built on the server,
// which pool workers run. It is nil when neither exists.
func (s *Service) pooledByIdentity(ctx context.Context, target engine.RuntimeTarget) (*boundRuntime, error) {
	s.mu.Lock()
	for _, e := range s.executors {
		if c := e.conn; c != nil && (e.pool() == "" || e.own() != "") && c.hello.Runtime == target.ID {
			h := c.hello
			h.Pool = nil
			s.mu.Unlock()
			return pooledRuntime(s, target.Name, h, nil), nil
		}
	}
	s.mu.Unlock()
	entries, err := s.catalog.RuntimeEntries(ctx)
	if err != nil {
		return nil, classify(err, CodeStorageError)
	}
	for _, e := range entries {
		if e.Identity != target.ID || e.Binary == nil {
			continue
		}
		var h worker.Hello
		if err := json.Unmarshal(e.Hello, &h); err != nil {
			return nil, classify(fmt.Errorf("%w: runtime %s: %w", artifact.ErrCorrupt, e.Name, err), CodeStorageError)
		}
		return pooledRuntime(s, target.Name, h, e.Binary), nil
	}
	return nil, nil
}

// poolWorker completes requests outside the queue (inspections, previews)
// on a free slot of a connected worker of its runtime: one of its identity,
// or when it was built on the server a pool of its binary's platform.
type poolWorker struct {
	s      *Service
	target engine.RuntimeTarget
	binary *artifact.RunBinary
	// gates are every feature gate the runtime's registry holds, from its
	// hello, for upgrade checks; nil when its worker did not list them.
	gates []release.Gate
}

func (p *poolWorker) Target() engine.RuntimeTarget { return p.target }

// FeatureGates lists the runtime's feature gates, as an in-process or
// child-process worker does.
func (p *poolWorker) FeatureGates() []release.Gate { return p.gates }
func (p *poolWorker) Close() error                 { return nil }

func (p *poolWorker) Execute(ctx context.Context, req engine.Request) (*engine.Report, error) {
	e, release, err := p.s.acquireSlot(ctx, p.target, p.binary)
	if err != nil {
		return nil, err
	}
	defer release()
	if e.pool() == "" {
		return e.conn.client.Execute(ctx, req)
	}
	b, err := p.s.binaryFor(ctx, e, p.target, p.binary)
	if err != nil {
		return nil, err
	}
	if req.Runtime == nil {
		req.Runtime = &p.target
	}
	bundle, _, err := engine.EncodeBundle(req)
	if err != nil {
		return nil, err
	}
	rep, err := e.conn.client.ExecuteJob(ctx, worker.Job{RunID: req.RunID, Bundle: bundle, Observer: req.Observer, Log: req.Log, Binary: b})
	return rep, p.s.suspected(e, b, err)
}

func (p *poolWorker) Inspect(ctx context.Context, in *engine.InspectionInput) (*engine.Inspection, error) {
	return onSlot(ctx, p, artifact.JobInspect, in, func(c *worker.Client, b *worker.PoolBinary) (*engine.Inspection, error) {
		return c.InspectOn(ctx, b, in)
	})
}

func (p *poolWorker) EvaluateOTTL(ctx context.Context, in *engine.OTTLInput) (*engine.OTTLEvaluation, error) {
	return onSlot(ctx, p, artifact.JobOTTL, in, func(c *worker.Client, b *worker.PoolBinary) (*engine.OTTLEvaluation, error) {
		return c.EvaluateOTTLOn(ctx, b, in)
	})
}

// onSlot completes an interactive request of p's runtime through call: with
// shared instances on a free slot of this instance or as a job, else on a
// slot of this service's workers, waiting for one. call gets the binary to
// run when the slot is a pool's.
func onSlot[T any](ctx context.Context, p *poolWorker, kind artifact.JobKind, in any, call func(*worker.Client, *worker.PoolBinary) (*T, error)) (*T, error) {
	if p.s.shared {
		return interactive(ctx, p.s, p.target, p.binary, kind, in, call)
	}
	e, release, err := p.s.acquireSlot(ctx, p.target, p.binary)
	if err != nil {
		return nil, err
	}
	defer release()
	return callOn(ctx, p.s, e, p.target, p.binary, call)
}

// callOn calls call on connected worker e for target, with the binary to
// run when e is a pool.
func callOn[T any](ctx context.Context, s *Service, e *executor, target engine.RuntimeTarget, binary *artifact.RunBinary, call func(*worker.Client, *worker.PoolBinary) (*T, error)) (*T, error) {
	b, err := s.binaryFor(ctx, e, target, binary)
	if err != nil {
		return nil, err
	}
	out, err := call(e.conn.client, b)
	return out, s.suspected(e, b, err)
}

// binaryFor is the binary connected worker e runs target on: none for a
// worker of one runtime and for a pool's own build, else the stored binary.
func (s *Service) binaryFor(ctx context.Context, e *executor, target engine.RuntimeTarget, binary *artifact.RunBinary) (*worker.PoolBinary, error) {
	if e.pool() == "" || binary == nil && target.ID != "" && target.ID == e.own() {
		return nil, nil
	}
	return s.poolBinary(ctx, target, binary)
}

// poolBinary is what a pool worker needs to run target: its stored binary
// with a URL to fetch it from.
func (s *Service) poolBinary(ctx context.Context, target engine.RuntimeTarget, binary *artifact.RunBinary) (*worker.PoolBinary, error) {
	transfers, ok := s.repository.(artifact.BuildTransfers)
	if !ok || binary == nil {
		return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("runtime %s is not available: this server stores no binary of it", target.Name)}
	}
	if err := s.suspectBinary(ctx, target.Name, binary.Digest); err != nil {
		return nil, err
	}
	u, err := transfers.RuntimeBinaryURL(ctx, binary.Digest, binaryURLLifetime)
	if errors.Is(err, artifact.ErrNotFound) {
		return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("runtime %s is not available: its binary is no longer stored", target.Name), cause: err}
	}
	if err != nil {
		return nil, persistenceError(err)
	}
	return &worker.PoolBinary{Identity: target.ID, URL: u.URL, Digest: binary.Digest, Size: binary.Bytes}, nil
}

// acquireSlot takes a free slot on a connected worker that runs target, a
// worker of its identity before a pool, waiting for one while every such
// worker is busy. While it waits, the scheduler claims no runs for those
// workers, so the request goes first.
func (s *Service) acquireSlot(ctx context.Context, target engine.RuntimeTarget, binary *artifact.RunBinary) (*executor, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := waitKeys(target, binary)
	for _, k := range keys {
		s.waiting[k]++
	}
	defer func() {
		for _, k := range keys {
			if s.waiting[k]--; s.waiting[k] <= 0 {
				delete(s.waiting, k)
			}
		}
	}()
	for {
		if s.closed {
			return nil, nil, ErrClosed
		}
		connected := false
		for _, pools := range []bool{false, true} {
			for _, e := range s.executors {
				if !e.runs(target, binary) || (e.pool() != "") != pools {
					continue
				}
				connected = true
				if e.free() > 0 {
					e.busy++
					return e, func() { s.releaseSlot(e) }, nil
				}
			}
		}
		if !connected {
			return nil, nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("runtime %s is not available: no worker of it is connected", target.Name)}
		}
		freed := s.freed
		s.mu.Unlock()
		select {
		case <-freed:
			s.mu.Lock()
		case <-ctx.Done():
			s.mu.Lock()
			return nil, nil, ctx.Err()
		}
	}
}

func (s *Service) releaseSlot(e *executor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.busy--
	s.freedLocked()
	s.wakeLocked()
}

// freedLocked wakes the requests waiting for a slot.
func (s *Service) freedLocked() {
	close(s.freed)
	s.freed = make(chan struct{})
}

// runPooled executes a job on the connected worker that claimed it. With
// object storage, a bundle above worker.InlineLimit travels as a presigned
// URL and a large result comes back through a staged upload.
func (s *Service) runPooled(job *pendingRun) (*engine.Report, error) {
	r := job.handle
	r.mu.Lock()
	raw, attempt := r.bundle, r.attempt
	r.mu.Unlock()
	j := worker.Job{RunID: r.id, Observer: job.request.Observer, Log: job.request.Log}
	var err error
	if job.exec.pool() != "" {
		// The binary the run was pinned to, which the pool claimed it for,
		// or none for the pool's own build.
		target := engine.RuntimeTarget{ID: job.assignment.Runtime}
		if rt := job.request.Runtime; rt != nil {
			target = *rt
		}
		if j.Binary, err = s.binaryFor(job.ctx, job.exec, target, job.assignment.Binary); err != nil {
			return nil, err
		}
	}
	transfers, _ := s.repository.(artifact.Transfers)
	switch {
	case transfers != nil && (raw == nil || len(raw) > worker.InlineLimit):
		var u artifact.ObjectURL
		if u, err = transfers.RunBundleURL(job.ctx, r.id, bundleURLLifetime); err != nil {
			return nil, persistenceError(err)
		}
		if u.Size > worker.InlineLimit {
			j.BundleURL, j.BundleDigest, j.BundleSize = u.URL, u.Digest, u.Size
		} else if raw == nil {
			raw, err = s.repository.RunBundle(job.ctx, r.id)
		}
	case raw == nil && s.repository != nil:
		raw, err = s.repository.RunBundle(job.ctx, r.id)
	case raw == nil:
		raw, _, err = engine.EncodeBundle(job.request)
	}
	if err != nil {
		return nil, persistenceError(err)
	}
	if j.BundleURL == "" {
		j.Bundle = raw
	}
	if transfers != nil {
		var b [4]byte
		_, _ = rand.Read(b[:])
		name := fmt.Sprintf("%s-%d-%s", r.id, attempt, hex.EncodeToString(b[:]))
		if j.ResultURL, err = transfers.StageURL(job.ctx, name, s.budgets.Execution+resultURLMargin); err != nil {
			return nil, persistenceError(err)
		}
		j.FetchResult = func(ctx context.Context, digest string, size int64) ([]byte, error) {
			return transfers.TakeStaged(ctx, name, digest, size, maxResultBytes)
		}
	}
	rep, err := job.exec.conn.client.ExecuteJob(job.ctx, j)
	return rep, s.suspected(job.exec, j.Binary, err)
}

// connectionLost reports whether a pooled job ended because its worker's
// connection did: the run is left to its lease, which requeues it.
func connectionLost(job *pendingRun, err error) bool {
	return job.pooled && errors.Is(err, worker.ErrWorkerExited)
}
