package platform

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// limitedUsers binds alice and bob as engineers in the default project.
func limitedUsers(t *testing.T) (*Policy, Caller, Caller) {
	t.Helper()
	scope := Scope{Kind: authz.ScopeProject, ID: artifact.DefaultProject}
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Principal: testCaller.Principal.ID}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Principal: "user:alice"}, Role: "engineer", Scope: scope},
		{Subject: Subject{Principal: "user:bob"}, Role: "engineer", Scope: scope},
	})
	if err != nil {
		t.Fatal(err)
	}
	user := func(id string) Caller {
		return Caller{Principal: Principal{ID: id, Kind: PrincipalUser}, Client: "test"}
	}
	return access, user("user:alice"), user("user:bob")
}

func wantPrincipalLimit(t *testing.T, err error, key string) {
	t.Helper()
	wantCode(t, err, CodeResourceExhausted)
	var e *Error
	if !errors.As(err, &e) || !e.Retryable || !strings.Contains(e.Message, key) {
		t.Fatalf("limit refusal = %v", err)
	}
}

// TestRunsPerPrincipalCountsEveryHeldSlot: a principal's reserved,
// preparing, queued and running runs count against its limit, other
// principals are unaffected, and a finished run frees its slot.
func TestRunsPerPrincipalCountsEveryHeldSlot(t *testing.T) {
	access, alice, bob := limitedUsers(t)
	started := make(chan struct{}, 8)
	s := New(Options{Access: access, Limits: PrincipalLimits{Runs: 3}, QueueCapacity: 10, Prepare: unprepared,
		Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	run := func(c Caller) (*Handle, error) {
		return s.submit(s.ctx, c, ModeQuick, request(c.Principal.ID), time.Time{})
	}

	running, err := run(alice)
	if err != nil {
		t.Fatal(err)
	}
	receive(t, started)
	queued, err := run(alice)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SubmitSuite(ctx, alice, suiteRequest("one", "two"))
	wantPrincipalLimit(t, err, "limits.runs_per_principal")
	_, err = s.submitPair(ctx, alice, ModeQuick, request("before"), request("after"))
	wantPrincipalLimit(t, err, "limits.runs_per_principal")
	if _, err := run(alice); err != nil {
		t.Fatal(err)
	}
	_, err = run(alice)
	wantPrincipalLimit(t, err, "limits.runs_per_principal")
	if _, err := s.SubmitSuite(ctx, bob, suiteRequest("one", "two")); err != nil {
		t.Fatalf("another principal = %v", err)
	}

	_ = queued.cancelWithCause(nil)
	if _, err := run(alice); err != nil {
		t.Fatalf("after a cancelled run = %v", err)
	}
	_ = running.cancelWithCause(nil)
	waitRun(t, running)
	if _, err := run(alice); err != nil {
		t.Fatalf("after a finished run = %v", err)
	}
	s.mu.Lock()
	holds := len(s.holds)
	s.mu.Unlock()
	if holds != 0 {
		t.Fatalf("slots still held after admission: %v", s.holds)
	}
}

// TestRunsPerPrincipalHoldsAcrossInstances: instances sharing a store count
// a principal's runs together, so a second instance refuses the run that
// would exceed the limit even though it holds none of them.
func TestRunsPerPrincipalHoldsAcrossInstances(t *testing.T) {
	access, alice, bob := limitedUsers(t)
	data := memory.NewData()
	block := func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	open := func() *Service {
		return openPlatform(t, data, Options{Shared: true, Access: access, Limits: PrincipalLimits{Runs: 2},
			QueueCapacity: 10, Prepare: unprepared, Execute: block})
	}
	a, b := open(), open()
	run := func(s *Service, c Caller) (*Handle, error) {
		return s.submit(s.ctx, c, ModeQuick, request(c.Principal.ID), time.Time{})
	}
	first, err := run(a, alice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(a, alice); err != nil {
		t.Fatal(err)
	}
	_, err = run(b, alice)
	wantPrincipalLimit(t, err, "limits.runs_per_principal")
	if _, err := run(b, bob); err != nil {
		t.Fatalf("another principal = %v", err)
	}
	_ = first.cancelWithCause(nil)
	waitRun(t, first)
	if _, err := run(b, alice); err != nil {
		t.Fatalf("after a finished run = %v", err)
	}
}

// TestASuiteHoldsThePrincipalLimitAcrossInstances: a suite's members are
// admitted in one write that counts the principal's runs on every
// instance, so a suite that would exceed the limit is refused whole rather
// than admitted in part, and a suite within it is admitted.
func TestASuiteHoldsThePrincipalLimitAcrossInstances(t *testing.T) {
	access, alice, _ := limitedUsers(t)
	data := memory.NewData()
	block := func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	open := func() *Service {
		return openPlatform(t, data, Options{Shared: true, Access: access, Limits: PrincipalLimits{Runs: 3},
			QueueCapacity: 10, Prepare: unprepared, Execute: block})
	}
	a, b := open(), open()
	for range 2 {
		if _, err := a.submit(a.ctx, alice, ModeQuick, request("held"), time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	// b holds none of alice's runs, so only the store sees the overshoot.
	_, err := b.SubmitSuite(context.Background(), alice, suiteRequest("first", "second"))
	wantPrincipalLimit(t, err, "limits.runs_per_principal")
	stored, err := b.repository.Runs(context.Background())
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored runs = %d, %v; want the 2 held", len(stored), err)
	}
	// b may follow a's runs through the change feed, but keeps none of the
	// suite's.
	b.mu.Lock()
	var left []string
	for id := range b.runs {
		if !slices.ContainsFunc(stored, func(r artifact.RunRecord) bool { return r.ID == id }) {
			left = append(left, id)
		}
	}
	jobs, reserved, preparing, suiteRuns := len(b.jobs), b.reserved, b.preparing, len(b.suiteRuns)
	b.mu.Unlock()
	if len(left) != 0 || jobs != 0 || reserved != 0 || preparing != 0 || suiteRuns != 0 {
		t.Fatalf("a refused suite left runs %v, %d jobs, %d reserved, %d preparing and %d suite runs", left, jobs, reserved, preparing, suiteRuns)
	}
	if _, err := b.SubmitSuite(context.Background(), alice, suiteRequest("within")); err != nil {
		t.Fatalf("a suite within the limit = %v", err)
	}
}

// TestCapturesPerPrincipalCountsOpenCaptures: a principal may hold one open
// capture; others are unaffected, and an ended capture frees the slot.
func TestCapturesPerPrincipalCountsOpenCaptures(t *testing.T) {
	access, alice, bob := limitedUsers(t)
	f := newFleetFixture(t, func(o *Options) {
		o.Access, o.Limits = access, PrincipalLimits{Captures: 1}
	}, fakePeer(0, logsEgress), fakePeer(1, logsEgress))
	ctx := context.Background()
	open, err := f.s.CreateCaptureSession(ctx, alice, f.req)
	if err != nil {
		t.Fatal(err)
	}
	f.req.CollectorIDs = []string{f.fleet.peers[1].ID} // a Collector serves one capture
	_, err = f.s.CreateCaptureSession(ctx, alice, f.req)
	wantPrincipalLimit(t, err, "limits.captures_per_principal")
	if _, err := f.s.CreateCaptureSession(ctx, bob, f.req); err != nil {
		t.Fatalf("another principal = %v", err)
	}
	f.finish(t, open.ID, "completed")
	f.req.CollectorIDs = []string{f.fleet.peers[0].ID}
	if _, err := f.s.CreateCaptureSession(ctx, alice, f.req); err != nil {
		t.Fatalf("after the capture ended = %v", err)
	}
	f.s.collectors.mu.Lock()
	pending := len(f.s.collectors.pendingBy)
	f.s.collectors.mu.Unlock()
	if pending != 0 {
		t.Fatalf("reservations left: %v", f.s.collectors.pendingBy)
	}
}
