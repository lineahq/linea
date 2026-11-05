package platform

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"go.uber.org/zap"
)

// Every operation records one audit event after it returns:
// its first statement is
//
//	defer s.audit(&caller, "<Operation>", &err)()
//
// The authorization checks fill in the record as the operation runs: the
// permission and scope they decided, the resources they found, and whether
// they refused. An operation another operation calls with the same caller
// adds to the outer record instead of writing its own.

// AuditOutcome is whether authorization allowed an operation.
type AuditOutcome string

const (
	AuditAllowed AuditOutcome = artifact.AuditAllowed
	AuditDenied  AuditOutcome = artifact.AuditDenied
)

// AuditEvent is one operation's audit record. It names resources by ID and
// never holds request content, configuration, environment values, tokens or
// capture data. Code is an allowed operation's error code, empty on success.
type AuditEvent struct {
	Seq           uint64
	Time          time.Time
	Principal     string
	PrincipalKind PrincipalKind
	Client        string
	RequestID     string
	Operation     string
	Permission    Permission
	// Scope is "*" for platform-wide operations, "project:<id>" for one
	// resource's project, and empty for lists filtered by project.
	Scope     string
	Resources []string
	Outcome   AuditOutcome
	Code      Code
}

// AuditFilter selects audit events, newest first; empty fields match every
// event. Cursor continues a previous page.
type AuditFilter struct {
	Principal string
	Operation string
	// Project selects the events that decided in that project.
	Project string
	Outcome AuditOutcome
	Since   time.Time
	Limit   int
	Cursor  string
}

// AuditPage is a page of audit events; NextCursor is empty on the last page.
type AuditPage struct {
	Events     []AuditEvent
	NextCursor string
}

const (
	defaultAuditPage = 100
	maxAuditPage     = 500
	// memoryAuditEvents bounds the audit log of a service without a workspace.
	memoryAuditEvents = 10000
)

// ListAuditEvents pages the audit log, newest first.
func (s *Service) ListAuditEvents(ctx context.Context, caller Caller, f AuditFilter) (_ *AuditPage, err error) {
	defer s.audit(&caller, "ListAuditEvents", &err)()
	if err := s.begin(ctx, caller, authz.AuditRead); err != nil {
		return nil, err
	}
	q := artifact.AuditQuery{Principal: f.Principal, Operation: f.Operation, Outcome: string(f.Outcome), Since: f.Since, Limit: f.Limit}
	if f.Project != "" {
		if !artifact.ValidProject(f.Project) {
			return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request: a project ID is 1 to 64 lowercase letters, digits and hyphens"}
		}
		q.Scope = "project:" + f.Project
	}
	if f.Outcome != "" && f.Outcome != AuditAllowed && f.Outcome != AuditDenied {
		return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request: outcome must be allowed or denied"}
	}
	if q.Limit < 0 || q.Limit > maxAuditPage {
		return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request: limit must be between 1 and 500"}
	}
	if q.Limit == 0 {
		q.Limit = defaultAuditPage
	}
	if f.Cursor != "" {
		before, err := strconv.ParseUint(f.Cursor, 10, 64)
		if err != nil || before == 0 {
			return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request: invalid cursor"}
		}
		q.Before = before
	}
	// One more than the page tells whether another page follows.
	q.Limit++
	events, err := s.auditLog.AuditEvents(ctx, q)
	if err != nil {
		return nil, classify(err, CodeStorageError)
	}
	page := &AuditPage{Events: []AuditEvent{}}
	for i, e := range events {
		if i == q.Limit-1 {
			page.NextCursor = strconv.FormatUint(page.Events[i-1].Seq, 10)
			break
		}
		page.Events = append(page.Events, AuditEvent{Seq: e.Seq, Time: e.Time, Principal: e.Principal, PrincipalKind: PrincipalKind(e.PrincipalKind),
			Client: e.Client, RequestID: e.RequestID, Operation: e.Operation, Permission: Permission(e.Permission), Scope: e.Scope,
			Resources: e.Resources, Outcome: AuditOutcome(e.Outcome), Code: Code(e.Code)})
	}
	return page, nil
}

// auditRecord collects one operation's decisions until it returns.
type auditRecord struct {
	mu         sync.Mutex
	done       bool
	permission authz.Permission
	scope      string
	denied     bool
	resources  []string
}

// audit starts the record of an operation and returns the function that
// writes it, which reads the operation's final error through err.
func (s *Service) audit(caller *Caller, operation string, err *error) func() {
	if r := caller.audit; r != nil {
		r.mu.Lock()
		done := r.done
		r.mu.Unlock()
		if !done {
			return func() {}
		}
	}
	r := &auditRecord{}
	caller.audit = r
	c := *caller
	return func() { s.record(c, operation, r, *err) }
}

func (s *Service) record(c Caller, operation string, r *auditRecord, err error) {
	r.mu.Lock()
	r.done = true
	e := artifact.AuditEvent{Time: time.Now(), Principal: c.Principal.ID, PrincipalKind: string(c.Principal.Kind), Client: c.Client,
		RequestID: c.RequestID, Operation: operation, Permission: string(r.permission), Scope: r.scope, Resources: r.resources,
		Outcome: artifact.AuditAllowed}
	if r.denied {
		e.Outcome = artifact.AuditDenied
	} else if err != nil {
		e.Code = string(resultCode(err))
	}
	r.mu.Unlock()
	// An invalid caller is an adapter defect refused before any decision.
	if c.validate() != nil {
		return
	}
	if _, err := s.auditLog.AppendAudit(context.Background(), e); err != nil && s.logger != nil {
		s.logger.Warn("audit event not recorded", zap.String("operation", operation), zap.Error(err))
	}
}

func resultCode(err error) Code {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e.Code
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return CodeCancelled
	}
	return CodeInternal
}

// decide records an authorization decision. A refusal is final; the latest
// scope names where the operation acted.
func (c Caller) decide(perm authz.Permission, scope string, allowed bool) {
	r := c.audit
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done || r.denied {
		return
	}
	if !allowed {
		r.denied, r.permission, r.scope = true, perm, scope
		return
	}
	if r.permission == "" {
		r.permission = perm
	}
	if scope != "" {
		r.scope = scope
	}
}

// touch records the ID of an existing resource the operation acted on.
func (c Caller) touch(id string) {
	r := c.audit
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.done && len(r.resources) < 16 && !slices.Contains(r.resources, id) {
		r.resources = append(r.resources, id)
	}
}

// memoryAudit is the audit log of a service without a workspace.
type memoryAudit struct {
	mu     sync.Mutex
	last   uint64
	events []artifact.AuditEvent
}

func (m *memoryAudit) AppendAudit(_ context.Context, e artifact.AuditEvent) (artifact.AuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last++
	e.Seq, e.Resources = m.last, slices.Clone(e.Resources)
	if len(m.events) == memoryAuditEvents {
		m.events = slices.Delete(m.events, 0, 1)
	}
	m.events = append(m.events, e)
	return e, nil
}

func (m *memoryAudit) AuditEvents(ctx context.Context, q artifact.AuditQuery) ([]artifact.AuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []artifact.AuditEvent
	for i := len(m.events) - 1; i >= 0 && len(out) < q.Limit; i-- {
		e := m.events[i]
		if (q.Before == 0 || e.Seq < q.Before) && (q.Principal == "" || e.Principal == q.Principal) &&
			(q.Operation == "" || e.Operation == q.Operation) && (q.Outcome == "" || e.Outcome == q.Outcome) && !e.Time.Before(q.Since) {
			out = append(out, e)
		}
	}
	return out, ctx.Err()
}
