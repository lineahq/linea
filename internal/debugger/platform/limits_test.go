package platform

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
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
