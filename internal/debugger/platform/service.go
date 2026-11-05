package platform

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

const (
	DefaultQueueCapacity = 32
	DefaultHistoryLimit  = 20
)

var (
	ErrClosed     = errors.New("platform is closed")
	ErrQueueFull  = errors.New("platform run queue is full")
	ErrUnknownRun = errors.New("unknown run")
)

// ExecuteFunc is the engine boundary. Implementations must honor cancellation
// and return only after Collector shutdown and event observation have finished.
type ExecuteFunc func(context.Context, engine.Request) (*engine.Report, error)

// PrepareFunc captures immutable sources without starting the Collector.
// Production wrappers must preserve engine.Prepare's capture contract and honor
// cancellation; scheduler tests can inject a source-free implementation.
type PrepareFunc func(context.Context, engine.Request) (engine.Request, error)

// Runtime is a runtime outside the service's process: a worker serving a
// debug Collector the debugger built. Host reads stay in the
// service, under its policy; the runtime receives captured inputs only. The
// composing application owns it and closes it after the service.
type Runtime interface {
	// Target names the runtime in the bundles of the runs it executes.
	Target() engine.RuntimeTarget
	// Execute runs a prepared request as ExecuteFunc does.
	Execute(context.Context, engine.Request) (*engine.Report, error)
	// Inspect and EvaluateOTTL complete what the service captured.
	Inspect(context.Context, *engine.InspectionInput) (*engine.Inspection, error)
	EvaluateOTTL(context.Context, *engine.OTTLInput) (*engine.OTTLEvaluation, error)
}

// Options configure one shared service and worker. Nonpositive capacities use
// the finite defaults. Prepare and Execute default to the real engine boundaries.
type Options struct {
	Logger        *zap.Logger
	QueueCapacity int
	// HistoryLimit is how many terminal runs a service without a
	// repository keeps; RunCache how many a service with one keeps in
	// memory after reading them (DefaultRunCache).
	HistoryLimit int
	RunCache     int
	// Retention, when set, evicts the runs a repository no longer keeps; zero
	// keeps every run.
	Retention RetentionPolicy
	Execute   ExecuteFunc
	Prepare   PrepareFunc
	// Runtimes, when set, builds and starts the runtimes a request may name;
	// nil leaves only the built-in runtime and connected workers.
	Runtimes RuntimeStore
	// DefaultRuntime, when set, names the runtime of every request that
	// names none, and without BuiltInOnWorkers of every request that names
	// the built-in one, so nothing executes in this process (team mode), until the team chooses a default (SetDefaultRuntime).
	DefaultRuntime string
	// BuiltInOnWorkers runs the built-in runtime on connected workers of
	// this release, which register it when they connect, never in this
	// process (team mode).
	BuiltInOnWorkers bool
	// Versions is the support table builds are checked against;
	// nil means the table embedded in this debugger.
	Versions *versions.Table
	// Environment is trusted capture policy. Zero permits only declared values
	// and URI defaults. Only api.New without an injected platform and the
	// low-level engine and config entry points select LegacyHost; no CLI
	// command or linea-server does.
	Environment config.EnvironmentPolicy
	// Paths is trusted host-read policy: which files a request may make the
	// service read. Zero has no roots and accepts inline content only; the
	// CLI selects Unrestricted; linea-server leaves it zero, so a team server
	// reads no host files.
	Paths PathPolicy
	// Budgets bound every run and evidence query (see Budgets). Requests can
	// only lower them, through fields they already have.
	Budgets Budgets
	// Capture connects live capture to fleet, supplied only at composition.
	Capture CaptureOptions
	// Access decides what each principal may do; nil is local
	// mode's policy, where the local owner is admin everywhere.
	Access *Policy
	// Limits bound what one principal holds at once (team mode);
	// zero fields are unlimited.
	Limits PrincipalLimits
	// Lease is how long the worker's claim on a run lasts without a
	// heartbeat; nonpositive is DefaultLease. Now is the clock leases are
	// measured on; nil is time.Now.
	Lease time.Duration
	Now   func() time.Time
	// WorkerToken is the registration token workers that dial the service
	// present; empty refuses them.
	WorkerToken string
	// BuilderToken is the build token builders that dial the service
	// present; empty refuses them.
	BuilderToken string
	// BuildExport is the repository builders push each built runtime's
	// image to, tagged with the build's name; empty exports
	// none.
	BuildExport string
	// GitCloneBytes bounds each git source the service clones, its
	// history and checkout together; zero leaves clones unbounded.
	GitCloneBytes int64
	// Shared is set when other services use the repository at once (team
	// mode with instances): the service follows the
	// repository's change feed and adopts any instance's queued runs.
	Shared bool
}

// PrincipalLimits bound one principal's share of a shared server. Runs
// counts its runs that are reserved, preparing, queued or running; Captures
// its captures that have not ended. A request beyond either is refused as
// resource_exhausted and is retryable.
type PrincipalLimits struct {
	Runs     int
	Captures int
}

// Budgets are run and query limits, composition policy that no request can
// raise. Zero or negative fields use the defaults: Execution 10 minutes;
// EvidenceBytes the workspace's per-run archive allowance minus 1 MiB of
// headroom for the report's own analysis (64 MiB when ephemeral; a larger
// value is lowered to the workspace allowance); Events 200,000; PageBytes
// 64 KiB and PageItems 100, capped at 1 MiB and 1,000.
type Budgets struct {
	Execution     time.Duration
	EvidenceBytes int64
	Events        int
	PageBytes     int
	PageItems     int
}

const (
	defaultExecutionBudget = 10 * time.Minute
	defaultEvidenceBytes   = 64 << 20
	evidenceHeadroom       = 1 << 20
	defaultEventBudget     = 200_000
	defaultPageBytes       = 64 << 10
	maxPageBytes           = 1 << 20
	defaultPageItems       = 100
	maxPageItems           = 1000
)

// normalized applies the defaults and caps; runBytes is the workspace's
// per-run archive allowance, or zero when ephemeral.
func (b Budgets) normalized(runBytes int64) Budgets {
	if b.Execution <= 0 {
		b.Execution = defaultExecutionBudget
	}
	evidence := int64(defaultEvidenceBytes)
	if runBytes > 0 {
		evidence = runBytes - evidenceHeadroom
		if evidence < evidenceHeadroom {
			evidence = runBytes / 2
		}
	}
	if b.EvidenceBytes <= 0 || runBytes > 0 && b.EvidenceBytes > evidence {
		b.EvidenceBytes = evidence
	}
	if b.Events <= 0 {
		b.Events = defaultEventBudget
	}
	if b.PageBytes <= 0 {
		b.PageBytes = defaultPageBytes
	}
	b.PageBytes = min(b.PageBytes, maxPageBytes)
	if b.PageItems <= 0 {
		b.PageItems = defaultPageItems
	}
	b.PageItems = min(b.PageItems, maxPageItems)
	return b
}

// limits are the engine limits of every run.
func (b Budgets) limits() engine.Limits {
	return engine.Limits{Execution: b.Execution, EvidenceBytes: b.EvidenceBytes, Events: b.Events}
}

type pendingRun struct {
	handle  *Handle
	ctx     context.Context
	request engine.Request
	// runtime is the built runtime the run executes on; nil is the
	// service's own runtime.
	runtime *boundRuntime
	// restore is set for a recovered run until the worker decodes request
	// from its bundle.
	restore bool
	// assignment is the executor's claim on the run, once taken, and exec
	// the executor while it holds a slot for the run.
	assignment artifact.Assignment
	exec       *executor
	// pooled is set for a run bound to connected workers.
	pooled bool
}

// Service owns scheduling and run history for all attached adapters.
// New keeps history and the run queue in memory; Open durably publishes
// lifecycle, evidence and the queue to a repository.
type Service struct {
	logger   *zap.Logger
	executor ExecuteFunc
	// defaultRuntime is Options.DefaultRuntime.
	defaultRuntime string
	// chosenDefault is the default runtime the team chose, as the
	// repository last said; its name is empty while none was chosen.
	defaultMu     sync.Mutex
	chosenDefault artifact.DefaultRuntime
	// builtInOnWorkers is Options.BuiltInOnWorkers.
	builtInOnWorkers bool
	prepare          PrepareFunc
	prepareRemote    PrepareFunc
	prepareGate      chan struct{}
	preparations     sync.WaitGroup
	queueCapacity    int
	historyLimit     int
	runCache         int
	ctx              context.Context
	stop             context.CancelFunc
	wake             chan struct{}
	done             chan struct{}
	repository       artifact.Repository
	closeStorage     sync.Once
	// runQueue is the repository, or localQueue for a service without one.
	runQueue   artifact.Queue
	localQueue *memory.Queue
	// instanceID identifies this server instance: its own executor in the
	// queue's leases and its worker-presence record.
	instanceID string
	// workerToken is Options.WorkerToken; builderToken
	// Options.BuilderToken, buildExport Options.BuildExport.
	workerToken  string
	builderToken string
	buildExport  string
	// gitClone is Options.GitCloneBytes.
	gitClone int64
	// builders counts the builders connected to this instance and their
	// slots; buildCounts caches the queue's counts (build_metrics.go).
	builders    builderCount
	buildCounts buildCountCache
	lease       time.Duration
	now         func() time.Time
	// beating is closed when the heartbeat loop stops.
	beating chan struct{}
	// auditLog is the repository's audit log, or an in-memory one.
	auditLog artifact.Audit
	// policy is Options.Environment and Options.Paths for operations that
	// start no Collector (inspection and preview); runs apply it through prepare.
	policy       engine.Policy
	remotePolicy engine.Policy
	// access is Options.Access, or local mode's policy. effective is access
	// extended by the managed role bindings, managed (role_bindings.go).
	access    *authz.Policy
	effective atomic.Pointer[authz.Policy]
	bindingMu sync.Mutex
	managed   artifact.RoleBindingSet
	// projectSet is the repository's project set (projects.go); projectMu
	// orders its updates.
	projectSet atomic.Pointer[artifact.ProjectSet]
	projectMu  sync.Mutex
	// budgets is Options.Budgets with defaults applied.
	budgets    Budgets
	collectors *collectorRegistry

	mu     sync.Mutex
	closed bool
	// draining is set by Drain: the service claims no more work.
	draining  bool
	preparing int
	reserved  int
	// holds counts each principal's preparing and reserved slots.
	holds  map[string]int
	limits PrincipalLimits
	// jobs are the unfinished runs this service admitted or recovered, by
	// ID, until they end.
	jobs map[string]*pendingRun
	// runs are every run without a repository; with one, a cache
	// (run_cache.go). order is their admission order; uses counts reads.
	runs            map[string]*Handle
	order           []string
	uses            uint64
	suites          map[string]Suite
	suiteOrder      []string
	suiteSubmitting int
	suiteRuns       map[string]bool // includes members being admitted before publication
	deletedRuns     deletedIDs
	deletedSuites   deletedIDs
	// executors are the service's own executor, first, and the connected
	// workers (worker_pool.go); executing counts the jobs they run.
	executors []*executor
	executing sync.WaitGroup
	// waiting counts the interactive requests waiting for a slot, by
	// runtime identity; freed is closed and replaced when a slot frees.
	waiting map[string]int
	freed   chan struct{}

	// runtimes builds and starts built runtimes; workers holds the started
	// ones by image ID (nil once closed), under runtimeMu.
	runtimes RuntimeStore
	// catalog is the repository's runtime catalog, or an in-memory one.
	catalog   artifact.Runtimes
	runtimeMu sync.Mutex
	workers   map[string]*boundRuntime
	builds    *buildQueue
	versions  *versions.Table

	// shared is Options.Shared with a repository (instances.go). cursor is
	// the change feed position the cache reflects, under followMu;
	// retention is the policy retain applies; retentionNudge asks it for
	// an early pass, and retaining is closed when it stops.
	retention      RetentionPolicy
	retentionNudge chan struct{}
	retaining      chan struct{}
	// following is closed when the follower stops.
	shared    bool
	followMu  sync.Mutex
	cursor    artifact.ChangeCursor
	following chan struct{}
	// jobWaiters wake the requests waiting for their interactive job's
	// answer, by job ID; jobsQueued is set when posted jobs may wait for a
	// claim (interactive_jobs.go). Both under mu.
	jobWaiters map[string]chan struct{}
	// buildWatchers wake the watchers of a server build, by build ID,
	// under mu (server_builds.go).
	buildWatchers map[string]chan struct{}
	// buildQueued is closed on any build change, which wakes connected
	// builders (builders.go), under mu.
	buildQueued chan struct{}
	jobsQueued  bool
	// presenceMu serializes the writes of this instance's worker record
	// (presence.go); the record is keyed by instanceID.
	presenceMu sync.Mutex
	// warmMu guards the runtimes other instances' pools hold running, as
	// last read (warmElsewhere).
	warmMu   sync.Mutex
	warmRead time.Time
	warm     map[string]bool
	// graceAt is when dispatch looks again at runs a cold pool left to a
	// warm pool of another instance (poolGrace), under mu.
	graceAt time.Time
	// sweep asks the next dispatch to claim for every runtime the
	// connected workers serve as well, not only for the runtimes of the
	// jobs this service holds (sweepKeysLocked), under mu.
	sweep bool
}

// New starts the scheduler. The composing application must call Close.
func New(opts Options) *Service {
	s := newService(opts, nil)
	s.startWorker()
	return s
}

// startWorker starts the scheduler and its heartbeat.
func (s *Service) startWorker() {
	s.executors[0].renewed = s.now()
	go s.work()
	go s.heartbeats()
	if s.shared {
		go s.follow()
	} else {
		close(s.following)
	}
	if r, ok := s.repository.(artifact.Retention); ok {
		// Projects can set their own retention while the server runs.
		go s.retain(r)
	} else {
		close(s.retaining)
	}
}

func newService(opts Options, repository artifact.Repository) *Service {
	if opts.QueueCapacity <= 0 {
		opts.QueueCapacity = DefaultQueueCapacity
	}
	if opts.HistoryLimit <= 0 {
		opts.HistoryLimit = DefaultHistoryLimit
	}
	if opts.RunCache <= 0 {
		opts.RunCache = DefaultRunCache
	}
	if opts.Execute == nil {
		opts.Execute = engine.Execute
	}
	policy := engine.Policy{Environment: opts.Environment.Clone(), Paths: opts.Paths.Clone()}
	access := opts.Access
	if access == nil {
		access = authz.Local(LocalOwner)
	}
	var runBytes int64
	if repository != nil {
		runBytes = repository.Limits().RunBytes
	}
	budgets := opts.Budgets.normalized(runBytes)
	// A connected worker's runs carry only declared or imported environment
	// values, whatever the service's policy.
	remotePolicy := engine.Policy{Environment: config.EnvironmentPolicy{Host: policy.Environment.Host}.Clone(), Paths: policy.Paths}
	prepareRemote := opts.Prepare
	if opts.Prepare == nil {
		opts.Prepare = func(ctx context.Context, req engine.Request) (engine.Request, error) {
			return engine.PrepareWithPolicy(ctx, req, policy)
		}
		prepareRemote = func(ctx context.Context, req engine.Request) (engine.Request, error) {
			return engine.PrepareWithPolicy(ctx, req, remotePolicy)
		}
	}
	if opts.Lease <= 0 {
		opts.Lease = DefaultLease
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ctx, stop := context.WithCancel(context.Background())
	s := &Service{
		logger: opts.Logger, executor: opts.Execute, prepare: opts.Prepare,
		prepareGate:   make(chan struct{}, 1),
		queueCapacity: opts.QueueCapacity, historyLimit: opts.HistoryLimit, runCache: opts.RunCache,
		ctx: ctx, stop: stop, wake: make(chan struct{}, 1), done: make(chan struct{}),
		runs:             make(map[string]*Handle),
		jobs:             make(map[string]*pendingRun),
		instanceID:       newExecutorID(),
		workerToken:      opts.WorkerToken,
		builderToken:     opts.BuilderToken,
		buildExport:      opts.BuildExport,
		gitClone:         opts.GitCloneBytes,
		waiting:          make(map[string]int),
		freed:            make(chan struct{}),
		lease:            opts.Lease,
		now:              opts.Now,
		beating:          make(chan struct{}),
		following:        make(chan struct{}),
		retention:        opts.Retention,
		retentionNudge:   make(chan struct{}, 1),
		retaining:        make(chan struct{}),
		jobWaiters:       make(map[string]chan struct{}),
		buildWatchers:    make(map[string]chan struct{}),
		shared:           opts.Shared && repository != nil,
		holds:            make(map[string]int),
		limits:           opts.Limits,
		suites:           make(map[string]Suite),
		suiteRuns:        make(map[string]bool),
		deletedRuns:      newDeletedIDs(),
		deletedSuites:    newDeletedIDs(),
		repository:       repository,
		policy:           policy,
		remotePolicy:     remotePolicy,
		access:           access,
		prepareRemote:    prepareRemote,
		budgets:          budgets,
		runtimes:         opts.Runtimes,
		defaultRuntime:   opts.DefaultRuntime,
		builtInOnWorkers: opts.BuiltInOnWorkers,
		versions:         opts.Versions,
		workers:          make(map[string]*boundRuntime),
		collectors:       newCollectorRegistry(opts.Capture),
	}
	s.effective.Store(access)
	s.executors = []*executor{{id: s.instanceID, capacity: 1}}
	s.collectors.repository = repository
	s.collectors.perPrincipal = opts.Limits.Captures
	s.auditLog = &memoryAudit{}
	if repository != nil {
		s.auditLog, s.runQueue, s.catalog = repository, repository, repository
	} else {
		s.catalog = memory.NewRuntimes()
		s.localQueue = memory.NewQueue()
		s.runQueue = s.localQueue
	}
	s.builds = newBuildQueue(s)
	return s
}

// submit captures and validates sources before admission, cancelled by
// prepare. Accepted work belongs to the service, independently of the
// submitting connection; a nonzero deadline bounds its execution.
func (s *Service) submit(prepare context.Context, caller Caller, mode InputMode, req engine.Request, deadline time.Time) (*Handle, error) {
	return s.submitReserved(prepare, caller, mode, req, deadline, nil)
}

// submitReserved shares all validation, preparation and admission with submit.
// A suite slot transfers atomically from reserved to preparing under s.mu.
func (s *Service) submitReserved(prepare context.Context, caller Caller, mode InputMode, req engine.Request, deadline time.Time, reservation *suiteAdmission) (*Handle, error) {
	if err := s.enterPreparation(&caller, req, reservation); err != nil {
		return nil, err
	}
	defer s.preparations.Done()
	p, err := s.prepareRun(prepare, caller.Project, req)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.preparing--
	s.holdLocked(caller, -1)
	if s.closed {
		return nil, ErrClosed
	}
	if err != nil {
		return nil, err
	}
	if err := prepare.Err(); err != nil {
		return nil, err
	}
	return s.admitLocked(caller, mode, p, deadline, reservation)
}

// enterPreparation validates a request and takes a preparation slot: one of
// the reservation's, or a free queue slot. The caller must call
// s.preparations.Done and, under s.mu, decrement s.preparing.
func (s *Service) enterPreparation(caller *Caller, req engine.Request, reservation *suiteAdmission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reservation != nil {
		reservation.remaining--
		s.reserved--
	}
	if err := caller.validate(); err != nil {
		return err
	}
	if err := s.workIn(caller, authz.RunSubmit); err != nil {
		return err
	}
	if err := req.Validate(); err != nil {
		return err
	}
	if s.closed {
		return ErrClosed
	}
	if reservation == nil {
		if err := s.admitsLocked(*caller, 1); err != nil {
			return err
		}
		s.holdLocked(*caller, 1)
	}
	s.preparing++
	s.preparations.Add(1)
	return nil
}

// admitsLocked refuses n more runs from caller beyond the queue's free
// slots or the caller's limit.
func (s *Service) admitsLocked(caller Caller, n int) error {
	s.pruneJobsLocked()
	queued := 0
	for _, job := range s.jobs {
		if job.exec == nil {
			queued++
		}
	}
	if n > s.queueCapacity-queued-s.preparing-s.reserved {
		return ErrQueueFull
	}
	if s.limits.Runs <= 0 {
		return nil
	}
	id := caller.Principal.ID
	held := s.holds[id]
	for _, job := range s.jobs {
		if job.handle.caller.Principal == id {
			held++
		}
	}
	if held+n > s.limits.Runs {
		return &Error{Code: CodeResourceExhausted, Retryable: true, Message: fmt.Sprintf(
			"%s already has %d runs reserved, queued or running; at most %d are allowed (limits.runs_per_principal); wait for one to finish", id, held, s.limits.Runs)}
	}
	return nil
}

// holdLocked changes the preparing or reserved slots caller holds by n.
func (s *Service) holdLocked(caller Caller, n int) {
	id := caller.Principal.ID
	if s.holds[id] += n; s.holds[id] <= 0 {
		delete(s.holds, id)
	}
}

// preparedRun is a captured request with its private input bundle.
type preparedRun struct {
	req     engine.Request
	bundle  []byte
	info    *engine.BundleInfo
	runtime *boundRuntime
}

// prepareRun captures and validates the request's sources, one preparation at
// a time, cancelled by prepare or by closing the service.
func (s *Service) prepareRun(prepare context.Context, project string, req engine.Request) (preparedRun, error) {
	prepareCtx, releasePreparation := context.WithCancel(prepare)
	stop := context.AfterFunc(s.ctx, releasePreparation)
	defer stop()
	defer releasePreparation()
	var err error
	out := preparedRun{}
	// The policy is the run's limits whatever the request says; the bundle
	// records them at admission.
	req.Limits = s.budgets.limits()
	if req.Runtime != nil && req.Runtime.ID == "" && !s.selects(req.Runtime.Name) {
		req.Runtime = nil
	}
	if name := s.projectDefaultRuntime(project); req.Runtime == nil && name != "" {
		req.Runtime = runtimeSelection(name)
	}
	if req.Runtime != nil && req.Runtime.ID == "" && !s.selects(req.Runtime.Name) {
		// The built-in runtime in this process.
		req.Runtime = nil
	}
	if err := s.runtimeAvailable(req.Runtime, project); err != nil {
		return out, err
	}
	if req.Runtime != nil && req.Runtime.ID == "" {
		// A selected built runtime (runtimeSelection): start its worker and
		// record its identity in the bundle.
		b, err := s.bind(prepareCtx, req.Runtime.Name)
		if err != nil {
			return out, err
		}
		target := b.target
		req.Runtime, out.runtime = &target, b
	}
	select {
	case s.prepareGate <- struct{}{}:
		if err = prepareCtx.Err(); err == nil {
			if out.runtime != nil && out.runtime.pooled {
				req, err = s.prepareRemote(prepareCtx, req)
			} else {
				req, err = s.prepare(prepareCtx, req)
			}
			if err == nil && req.Prepared() {
				out.bundle, out.info, err = engine.EncodeBundle(req)
			}
		}
		<-s.prepareGate
	case <-prepareCtx.Done():
		err = prepareCtx.Err()
	}
	if err == nil {
		err = prepareCtx.Err()
	}
	out.req = req
	return out, err
}

// admitLocked journals a prepared run and queues it. s.mu is held.
func (s *Service) admitLocked(caller Caller, mode InputMode, p preparedRun, deadline time.Time, reservation *suiteAdmission) (*Handle, error) {
	req := p.req
	base := s.ctx
	releaseDeadline := func() {}
	if !deadline.IsZero() {
		base, releaseDeadline = context.WithDeadline(base, deadline)
	}
	ctx, cancelCause := context.WithCancelCause(base)
	cancel := func(cause error) {
		cancelCause(cause)
		releaseDeadline()
	}
	r := &Handle{
		id: instrument.NewRunID(), project: caller.Project, mode: mode, caller: caller.Actor(), state: StateQueued,
		created: time.Now().UTC(), changed: make(chan struct{}), done: make(chan struct{}), cancel: cancel,
		bundle: p.bundle, bundleInfo: p.info, runtimeKey: runtimeKey(req),
	}
	if p.runtime != nil {
		r.binary = p.runtime.binary
	}
	caller.touch(r.id)
	s.queueing(r)
	if s.repository != nil {
		r.persist = func(evidence bool) error { return s.persistRun(r, evidence) }
		if err := r.persist(false); err != nil {
			cancel(nil)
			return nil, persistenceError(err)
		}
	} else if err := s.localQueue.Admit(r.id, r.runtimeKey, 0); err != nil {
		cancel(nil)
		return nil, err
	}
	s.observeLocked(r, &req)
	s.runs[r.id] = r
	if reservation != nil {
		reservation.handles = append(reservation.handles, r)
		s.suiteRuns[r.id] = true
	}
	s.order = append(s.order, r.id)
	s.jobs[r.id] = &pendingRun{handle: r, ctx: ctx, request: req, runtime: p.runtime, pooled: p.runtime != nil && p.runtime.pooled}
	s.evictLocked()
	s.wakeLocked()
	return r, nil
}

// queueing connects a run's cancellation and end to the run queue.
func (s *Service) queueing(r *Handle) {
	r.requestCancel = s.requestCancelHook(r)
	if s.localQueue != nil {
		r.ended = func() { s.localQueue.Remove(r.id) }
	}
}

// observeLocked routes a request's log and events to its run. Each attempt
// records its own (see recordLocked).
func (s *Service) observeLocked(r *Handle, req *engine.Request) {
	req.RunID = r.id
	if s.logger != nil {
		req.Logger = s.logger
	}
	if req.Observer != nil {
		r.observer = req.Observer
	}
	req.Observer, req.Log = nil, nil
}

// execute is the foreground path of individual CLI runs. Unlike Wait,
// cancelling its context explicitly cancels its own submitted run and waits for
// shutdown, preserving any report the engine can produce.
func (s *Service) execute(ctx context.Context, caller Caller, mode InputMode, req engine.Request) (*engine.Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	r, err := s.submit(ctx, caller, mode, req, deadline)
	if err != nil {
		return nil, err
	}
	if err := r.Wait(ctx); err != nil {
		r.cancelWithCause(context.Cause(ctx))
		_ = r.Wait(context.Background())
	}
	v := r.View(true)
	if v.Report != nil {
		return v.Report, nil
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, context.Canceled
}

// work dispatches queued runs whenever something may have changed: a run
// admitted or requeued, a slot freed, a worker connected.
func (s *Service) work() {
	defer close(s.done)
	for {
		if s.dispatch() {
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
	}
}

// run executes a job on the connected worker that claimed it, on its built
// runtime, or with the service's executor.
func (s *Service) run(job *pendingRun) (*engine.Report, error) {
	b := job.runtime
	if b == nil {
		return s.executor(job.ctx, job.request)
	}
	var report *engine.Report
	var err error
	if job.pooled {
		report, err = s.runPooled(job)
	} else if report, err = b.worker.Execute(job.ctx, job.request); errors.Is(err, engine.ErrRuntimeUnavailable) {
		s.unbind(b)
	}
	if report != nil {
		b.stamp(report)
	}
	return report, err
}

// pruneJobsLocked drops the jobs of runs that ended.
func (s *Service) pruneJobsLocked() {
	for id, job := range s.jobs {
		if job.exec != nil {
			continue
		}
		job.handle.mu.Lock()
		terminal := job.handle.terminalLocked()
		job.handle.mu.Unlock()
		if terminal {
			delete(s.jobs, id)
		}
	}
}

// evictLocked retains active runs, suite members and the newest historyLimit
// other terminal runs. Members are pinned during admission as well.
// This is temporary in-memory history, not durable artifact retention policy.
// With a repository it bounds the run cache instead (evictCacheLocked).
func (s *Service) evictLocked() {
	if s.repository != nil {
		s.evictCacheLocked()
		return
	}
	finished := 0
	for i := len(s.order) - 1; i >= 0; i-- {
		id := s.order[i]
		r := s.runs[id]
		r.mu.Lock()
		terminal := r.terminalLocked()
		r.mu.Unlock()
		if !terminal || s.suiteRuns[id] {
			continue
		}
		finished++
		if finished > s.historyLimit {
			delete(s.runs, id)
			s.order = append(s.order[:i], s.order[i+1:]...)
		}
	}
}

// handle returns a run, read from the repository when not cached; handles
// already held remain valid after eviction.
func (s *Service) handle(ctx context.Context, id string) (*Handle, error) {
	if err := s.load(ctx, id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, s.unknownRunLocked(id)
	}
	return r, nil
}

// list returns a page of runs newest first, without report payloads, each
// with its summary. With a repository the repository pages and filters;
// without one the retained runs are paged in memory.
func (s *Service) list(ctx context.Context, q RunQuery, projects []string) (*RunPage, error) {
	if s.repository == nil {
		return s.listRetained(q, projects)
	}
	stored, err := s.repository.ListRuns(ctx, artifact.RunQuery{Projects: projects, Scenario: q.Scenario, Runtime: q.Runtime, Result: q.Result, Limit: q.Limit, Cursor: q.Cursor})
	if err != nil {
		return nil, err
	}
	page := &RunPage{Runs: make([]Run, 0, len(stored.Runs)), NextCursor: stored.NextCursor}
	for _, record := range stored.Runs {
		s.mu.Lock()
		r := s.runs[record.ID]
		s.mu.Unlock()
		if r == nil {
			// Not cached here, or admitted elsewhere and not yet followed:
			// read it from storage without caching it.
			if r, err = s.handleFromRecord(ctx, record); err != nil {
				return nil, err
			}
		}
		page.Runs = append(page.Runs, listed(r))
	}
	return page, nil
}

// listRetained pages the retained runs of a service without a repository.
// Its cursor is the last listed run's ID.
func (s *Service) listRetained(q RunQuery, projects []string) (*RunPage, error) {
	s.mu.Lock()
	runs := make([]*Handle, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		runs = append(runs, s.runs[s.order[i]])
	}
	s.mu.Unlock()
	limit := q.Limit
	if limit == 0 {
		limit = DefaultRunPage
	}
	start := 0
	if q.Cursor != "" {
		start = slices.IndexFunc(runs, func(r *Handle) bool { return r.id == q.Cursor }) + 1
		if start == 0 {
			return nil, &Error{Code: CodeInvalidRequest, Message: "invalid run cursor"}
		}
	}
	page := &RunPage{Runs: []Run{}}
	for _, r := range runs[start:] {
		v := listed(r)
		if !q.matches(v) || !inProjects(projects, v.Project) {
			continue
		}
		if len(page.Runs) == limit {
			page.NextCursor = page.Runs[limit-1].ID
			break
		}
		page.Runs = append(page.Runs, v)
	}
	return page, nil
}

// listed is a run's list entry: its view with a summary, without report.
func listed(r *Handle) Run {
	v := r.View(false)
	v.Listing = listing(v.Report)
	v.Report = nil
	return v
}

// Close atomically stops admission, cancels all unfinished work and waits for
// every executor's Collector shutdown. It is safe to call concurrently or repeatedly.
func (s *Service) Close() {
	var held []artifact.Assignment
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.collectors.close()
		s.stop()
		if s.shared {
			// The runs belong to every instance: stop only what this service
			// executes, publish nothing (finish) and hand the runs back.
			for id, job := range s.jobs {
				if job.exec != nil {
					held = append(held, job.assignment)
					r := s.runs[id]
					r.mu.Lock()
					r.cancel(context.Canceled)
					r.mu.Unlock()
				}
			}
		} else {
			for _, r := range s.runs {
				r.Cancel()
			}
		}
		clear(s.jobs)
	}
	s.mu.Unlock()
	s.collectors.wait()
	<-s.done
	s.executing.Wait()
	<-s.beating
	<-s.following
	<-s.retaining
	s.releaseHeld(held)
	s.closeConnections()
	s.withdrawWorkers()
	s.preparations.Wait()
	s.builds.close()
	s.closeRuntimes()
	s.closeStorage.Do(func() {
		if s.repository != nil {
			s.repository.Close()
		}
	})
}
