package platform

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"go.uber.org/zap"
)

// RetentionPolicy bounds the terminal runs a team server keeps: at most Runs
// runs, none admitted longer than Age ago, and the stored bytes within the
// repository's storage limit. Retention removes the oldest runs that are not
// kept, in no suite and the origin of no saved artifact.
type RetentionPolicy struct {
	Runs int
	Age  time.Duration
}

// Bounds of a retention policy, the server's or a project's.
const (
	MaxRetentionRuns = 1_000_000
	MinRetentionAge  = time.Hour
	MaxRetentionAge  = 100 * 365 * 24 * time.Hour
)

// set reports whether p bounds anything; a zero policy runs no retention.
func (p RetentionPolicy) set() bool { return p.Runs > 0 || p.Age > 0 }

// PrincipalSystem is the principal of what the platform does on its own,
// such as retention. It appears in audit records only; no caller has it.
const PrincipalSystem PrincipalKind = "system"

const (
	// retentionInterval is how often a service runs a retention pass.
	retentionInterval = 5 * time.Minute
	// retentionSpacing is the least time between two passes an admission
	// asks for.
	retentionSpacing = 30 * time.Second
)

// retain runs retention passes until the service stops: every
// retentionInterval, and early when an admission asks (nudgeRetention). With
// a shared repository only the upkeep's leader runs them; it
// learns of other instances' admissions from the change feed.
func (s *Service) retain(r artifact.Retention) {
	defer close(s.retaining)
	t := time.NewTicker(retentionInterval)
	defer t.Stop()
	var last time.Time
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		case <-s.retentionNudge:
			if time.Since(last) < retentionSpacing {
				continue
			}
		}
		last = time.Now()
		if err := s.retentionPass(s.ctx, r); err != nil {
			s.logError("run retention", err)
		}
	}
}

// retentionPass evicts what r's policy no longer retains, unless r is
// shared upkeep that another instance leads.
func (s *Service) retentionPass(ctx context.Context, r artifact.Retention) error {
	if u, ok := r.(artifact.Upkeep); ok {
		led, err := u.Lead(ctx)
		if err != nil || !led {
			return err
		}
	}
	return s.evictRuns(ctx, r)
}

// nudgeRetention asks for an early retention pass after an admission, this
// instance's or, read from the change feed, another's.
func (s *Service) nudgeRetention() {
	select {
	case s.retentionNudge <- struct{}{}:
	default:
	}
}

// evictRuns runs one retention pass: batches until nothing is due. The
// service forgets each evicted run as DeleteRun does, and one audit event
// records the pass as the system principal, with a count per reason.
func (s *Service) evictRuns(ctx context.Context, r artifact.Retention) (err error) {
	reasons := map[string]int{}
	defer func() {
		if len(reasons) != 0 || err != nil {
			s.recordEviction(reasons, err)
		}
	}()
	for {
		p := s.retentionPolicy()
		if !s.retention.set() && len(p.Projects) == 0 {
			return nil
		}
		batch, err := r.EvictRuns(ctx, p, time.Now())
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		ids := make([]string, 0, len(batch))
		for _, e := range batch {
			ids = append(ids, e.ID)
			reasons[e.Reason]++
		}
		s.mu.Lock()
		s.forgetRunsLocked(ids...)
		s.mu.Unlock()
	}
}

// retentionPolicy is the installation's retention with each project's own
// settings.
func (s *Service) retentionPolicy() artifact.RetentionPolicy {
	p := artifact.RetentionPolicy{Runs: s.retention.Runs, Age: s.retention.Age}
	for _, r := range s.projectState().Projects {
		if r.RetentionRuns > 0 || r.RetentionAge > 0 || r.StorageLimit > 0 {
			p.Projects = append(p.Projects, artifact.ProjectRetention{Project: r.ID, Runs: r.RetentionRuns, Age: r.RetentionAge, Bytes: r.StorageLimit})
		}
	}
	return p
}

// recordEviction audits a retention pass. Its resources are
// "<reason>:<runs>" for each reason that evicted runs, never run IDs.
func (s *Service) recordEviction(reasons map[string]int, err error) {
	e := artifact.AuditEvent{Time: time.Now(), Principal: string(PrincipalSystem), PrincipalKind: string(PrincipalSystem), Client: "retention",
		Operation: "EvictRuns", Scope: "*", Outcome: artifact.AuditAllowed}
	for _, reason := range []string{artifact.EvictCount, artifact.EvictAge, artifact.EvictStorage} {
		if n := reasons[reason]; n > 0 {
			e.Resources = append(e.Resources, fmt.Sprintf("%s:%d", reason, n))
		}
	}
	if err != nil {
		e.Code = string(resultCode(artifactError(err)))
	}
	if _, err := s.auditLog.AppendAudit(context.Background(), e); err != nil && s.logger != nil {
		s.logger.Warn("audit event not recorded", zap.String("operation", e.Operation), zap.Error(err))
	}
}

// forgetRunsLocked drops deleted runs from memory and remembers their IDs.
// Handles already held stay valid, as after eviction.
func (s *Service) forgetRunsLocked(ids ...string) {
	gone := make(map[string]bool, len(ids))
	for _, id := range ids {
		delete(s.runs, id)
		s.deletedRuns.add(id, time.Now())
		s.listings.drop(id)
		gone[id] = true
	}
	s.order = slices.DeleteFunc(s.order, func(id string) bool { return gone[id] })
}

// mayChangeRunLocked checks that caller may keep, unkeep or delete run r: it
// needs run.delete in the run's project, and a run another principal
// submitted, or one recorded without its caller, needs an admin. s.mu is held.
func (s *Service) mayChangeRunLocked(caller Caller, r *Handle) error {
	if err := s.authorizeIn(caller, authz.RunDelete, r.project); err != nil {
		return err
	}
	r.mu.Lock()
	owner := r.caller.Principal
	r.mu.Unlock()
	if owner != "" && owner == caller.Principal.ID {
		return nil
	}
	allowed := s.visible(caller, authz.Admin, r.project)
	caller.decide(authz.Admin, "project:"+r.project, allowed)
	if !allowed {
		return &Error{Code: CodeForbidden, Message: "forbidden: only an admin may change a run another principal submitted"}
	}
	return nil
}

// KeepRun protects a run from retention and pruning until UnkeepRun. The
// run's owner or an admin may keep it; it may be unfinished.
func (s *Service) KeepRun(ctx context.Context, caller Caller, id string) (_ Run, err error) {
	defer s.audit(&caller, "KeepRun", &err)()
	return s.keepRun(ctx, caller, authz.RunDelete, id, true)
}

// UnkeepRun makes a kept run subject to retention again.
func (s *Service) UnkeepRun(ctx context.Context, caller Caller, id string) (_ Run, err error) {
	defer s.audit(&caller, "UnkeepRun", &err)()
	return s.keepRun(ctx, caller, authz.RunDelete, id, false)
}

func (s *Service) keepRun(ctx context.Context, caller Caller, perm authz.Permission, id string, kept bool) (Run, error) {
	retention, err := s.retentionStore(ctx, caller, perm)
	if err != nil {
		return Run{}, err
	}
	if err := s.load(ctx, id); err != nil {
		return Run{}, err
	}
	s.mu.Lock()
	r, ok := s.runs[id]
	if !ok {
		err := classify(s.unknownRunLocked(id), CodeNotFound)
		s.mu.Unlock()
		return Run{}, err
	}
	caller.touch(id)
	err = s.mayChangeRunLocked(caller, r)
	s.mu.Unlock()
	if err != nil {
		return Run{}, err
	}
	if err := retention.KeepRun(ctx, id, kept); err != nil {
		return Run{}, artifactError(err)
	}
	r.mu.Lock()
	r.kept = kept
	r.mu.Unlock()
	return r.View(false), nil
}

// retentionStore begins a retention operation that needs perm: the
// repository must keep run history.
func (s *Service) retentionStore(ctx context.Context, caller Caller, perm authz.Permission) (artifact.Retention, error) {
	if err := s.begin(ctx, caller, perm); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, classify(ErrClosed, CodeUnavailable)
	}
	r, ok := s.repository.(artifact.Retention)
	if !ok {
		return nil, classify(ErrWorkspaceRequired, CodeUnavailable)
	}
	return r, nil
}

// RunPruneRequest selects the runs PruneRuns removes: the runs retention may
// evict (terminal, not kept, in no suite and the origin of no saved
// source's current revision) that match RunFilter, admitted before Before
// (zero: any time), in Project (empty: every project the caller
// administers). DryRun removes nothing.
type RunPruneRequest struct {
	RunFilter
	Before  time.Time
	Project string
	DryRun  bool
}

// RunPruneResult counts the runs a prune removed, or would remove in a dry
// run, and the bytes of stored objects only they referenced.
type RunPruneResult struct {
	Runs   int
	Bytes  int64
	DryRun bool
}

// PruneRuns removes the runs req selects; only an admin may prune. A dry
// run reports what the same request would remove now. The audit records
// counts, never run IDs: resources "runs:<n>", "bytes:<n>" and, for a dry
// run, "dry_run".
func (s *Service) PruneRuns(ctx context.Context, caller Caller, req RunPruneRequest) (_ *RunPruneResult, err error) {
	defer s.audit(&caller, "PruneRuns", &err)()
	retention, err := s.retentionStore(ctx, caller, authz.RunDelete)
	if err != nil {
		return nil, err
	}
	if err := req.RunFilter.validate(); err != nil {
		return nil, classify(err, CodeInvalidRequest)
	}
	projects := s.projects(caller, authz.Admin)
	switch {
	case req.Project != "":
		if err := s.authorizeIn(caller, authz.Admin, req.Project); err != nil {
			return nil, err
		}
		projects = []string{req.Project}
	case projects != nil && len(projects) == 0:
		caller.decide(authz.Admin, "*", false)
		return nil, &Error{Code: CodeForbidden, Message: "forbidden: only an admin may prune runs"}
	}
	got, err := retention.PruneRuns(ctx, artifact.PruneQuery{Projects: projects, Before: req.Before, Scenario: req.Scenario, Runtime: req.Runtime, Result: req.Result},
		req.DryRun, time.Now())
	if err != nil {
		return nil, artifactError(err)
	}
	if !req.DryRun && len(got.Runs) != 0 {
		s.mu.Lock()
		s.forgetRunsLocked(got.Runs...)
		s.mu.Unlock()
	}
	caller.touch(fmt.Sprintf("runs:%d", len(got.Runs)))
	caller.touch(fmt.Sprintf("bytes:%d", got.Bytes))
	if req.DryRun {
		caller.touch("dry_run")
	}
	return &RunPruneResult{Runs: len(got.Runs), Bytes: got.Bytes, DryRun: req.DryRun}, nil
}
