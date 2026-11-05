// Package livecapturedebug is the OpenTelemetry Collector Pipeline
// Debugger's provider for the livecapture extension: during a debugger run
// it hands the extension the sessions a scenario declares under
// mocks.providers.livecapture, serving its control stream when
// control.endpoint is set and otherwise posting them to its development
// local_control API, and reports what the extension said about them.
package livecapturedebug

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lineahq/linea/collector/provider"
	"google.golang.org/grpc"

	"github.com/lineahq/linea/collector/livecapture/controlpb"
	"github.com/lineahq/linea/collector/livecapture/livecaptureextension"
)

// DebuggerProvider is found by the debugger's runtime builds.
func DebuggerProvider() provider.Provider {
	return provider.Provider{
		Name:       "livecapture",
		Components: []provider.Type{{Kind: "extension", Type: "livecapture"}},
		Endpoints:  []provider.Endpoint{{Path: "control"}},
		Prepare:    prepare,
		Check:      check,
	}
}

// settings are the scenario's mocks.providers.livecapture.
type settings struct {
	// Sessions are the desired sessions the control service sends. A
	// session without expires_at gets one an hour after the run starts.
	Sessions []livecaptureextension.AuthorizedRequest `json:"sessions"`
	// Disabled sends the desired state disabled.
	Disabled bool `json:"disabled"`
}

// sessionWait bounds how long OnReady waits for the extension to report
// every desired session.
const sessionWait = 15 * time.Second

func check(run provider.Static) []provider.Finding {
	var s settings
	if err := run.Settings(&s); err != nil {
		return []provider.Finding{{Message: "mocks.providers.livecapture: " + err.Error()}}
	}
	var out []provider.Finding
	for _, c := range run.Components() {
		if !enabled(c) {
			out = append(out, provider.Finding{Component: c.ID, Message: "the extension is not enabled, so no session starts",
				Hint: "set enabled: true in its configuration"})
			continue
		}
		if controlEndpoint(c) == "" && localEndpoint(c) == "" {
			out = append(out, provider.Finding{Component: c.ID, Message: "neither control.endpoint nor local_control is configured, so no session can start",
				Hint: "configure control.endpoint (the run points it at the debugger) or enable local_control"})
		}
		if len(s.Sessions) == 0 && !s.Disabled {
			f := provider.Finding{Component: c.ID, Message: "the scenario declares no capture session",
				Hint: "add mocks.providers.livecapture.sessions"}
			if session, ok := proposeSession(run.Pipelines()); ok {
				f.Settings = map[string]any{"sessions": []any{session}}
			}
			out = append(out, f)
		}
	}
	return out
}

// proposeSession is a session at each pipeline's first capture processor,
// for the debugger to offer when the scenario declares none: the point a
// capture processor registers is its pipeline setting (by default its
// signal) and its name, and a raw point needs raw authorization.
func proposeSession(pipelines []provider.Pipeline) (map[string]any, bool) {
	var points []livecaptureextension.Point
	var raw bool
	seen := map[livecaptureextension.Point]bool{}
	signals := map[string]bool{}
	for _, p := range pipelines {
		for _, proc := range p.Processors {
			name, ok := strings.CutPrefix(proc.ID, "processor/capture")
			if !ok || (name != "" && !strings.HasPrefix(name, "/")) {
				continue
			}
			point := livecaptureextension.Point{Pipeline: p.Signal, Point: strings.TrimPrefix(name, "/")}
			if v, _ := proc.Config["pipeline"].(string); v != "" {
				point.Pipeline = v
			}
			if !seen[point] {
				seen[point] = true
				points = append(points, point)
			}
			if v, _ := proc.Config["sensitivity"].(string); v == "raw" {
				raw = true
			}
			signals[p.Signal] = true
			break
		}
	}
	var sigs []string
	for _, sig := range []string{"traces", "metrics", "logs"} {
		if signals[sig] {
			sigs = append(sigs, sig)
		}
	}
	session := map[string]any{"request": map[string]any{
		"session_id": "debugger", "principal": "debugger", "capture_points": points, "signals": sigs,
	}}
	if raw {
		session["authorized_raw"] = true
	}
	return session, len(points) > 0
}

func prepare(_ context.Context, run provider.Run) error {
	var s settings
	if err := run.Settings(&s); err != nil {
		return fmt.Errorf("mocks.providers.livecapture: %w", err)
	}
	expires := time.Now().Add(time.Hour).UTC()
	for i := range s.Sessions {
		if s.Sessions[i].Request.ExpiresAt.IsZero() {
			s.Sessions[i].Request.ExpiresAt = expires
		}
	}
	for _, c := range run.Components() {
		if controlEndpoint(c) == "" {
			if address := localEndpoint(c); address != "" {
				prepareLocal(run, c.ID, address, s)
			}
			continue
		}
		l, err := run.Listener(c.ID, "control")
		if err != nil {
			return err
		}
		ctl := &control{settings: s, reports: make(chan struct{}, 1)}
		srv := grpc.NewServer()
		controlpb.RegisterCaptureControlServer(srv, ctl)
		go func() { _ = srv.Serve(l) }()
		id := c.ID
		run.OnReady(func(ctx context.Context) error {
			if len(s.Sessions) == 0 {
				return nil
			}
			ctx, cancel := context.WithTimeout(ctx, sessionWait)
			defer cancel()
			for {
				if ctl.reported(s.Sessions) {
					return nil
				}
				select {
				case <-ctl.reports:
				case <-ctx.Done():
					run.Warning(id, fmt.Sprintf("the extension did not report every session within %s; inputs were injected anyway", sessionWait))
					return nil
				}
			}
		})
		run.OnInputsDone(func(ctx context.Context) error {
			// Sending the desired state again makes the extension report at
			// once, with the statistics of the inputs. Only a report that
			// applied the resent generation has them: an earlier signal may
			// still be buffered, and the extension also reports on a timer.
			generation := ctl.resend()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			for !ctl.applied(generation) {
				select {
				case <-ctl.reports:
				case <-ctx.Done():
					return nil
				}
			}
			return nil
		})
		run.OnClose(func() error {
			srv.Stop()
			ctl.declare(run, id, s, "the control service is the debugger's: sessions come from the scenario, status reports are only declared")
			return nil
		})
	}
	return nil
}

// control serves livecapture.control.v1.CaptureControl.
type control struct {
	controlpb.UnimplementedCaptureControlServer
	settings settings
	reports  chan struct{}

	mu         sync.Mutex
	generation uint64
	streams    []grpc.BidiStreamingServer[controlpb.CollectorReport, controlpb.DesiredState]
	statuses   []livecaptureextension.Status
	count      int
	// appliedGeneration is the last generation the extension reported
	// applying.
	appliedGeneration uint64
	// rejected is the extension's last desired-state error.
	rejected string
	// unreached says why the local control API was never reached.
	unreached string
}

// desired is the scenario's desired state under the next generation; c.mu
// must be held.
func (c *control) desired() *controlpb.DesiredState {
	c.generation++
	desired := &controlpb.DesiredState{Generation: c.generation, Disabled: c.settings.Disabled}
	for _, session := range c.settings.Sessions {
		desired.Sessions = append(desired.Sessions, session.Proto())
	}
	return desired
}

func (c *control) WatchCaptures(stream grpc.BidiStreamingServer[controlpb.CollectorReport, controlpb.DesiredState]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	c.keep(first)
	c.mu.Lock()
	c.streams = append(c.streams, stream)
	err = stream.Send(c.desired())
	c.mu.Unlock()
	if err != nil {
		return err
	}
	for {
		report, err := stream.Recv()
		if err != nil {
			return err
		}
		c.keep(report)
	}
}

// keep records a status report's sessions.
func (c *control) keep(report *controlpb.CollectorReport) {
	statuses := make([]livecaptureextension.Status, 0, len(report.GetSessions()))
	for _, st := range report.GetSessions() {
		stats := st.GetStats()
		statuses = append(statuses, livecaptureextension.Status{SessionID: st.GetSessionId(), State: strings.ToLower(strings.TrimPrefix(st.GetState().String(), "SESSION_STATE_")), Reason: st.GetReason(), ExpiresAt: st.GetExpiresAt().AsTime(), Stats: livecaptureextension.Stats{Selected: stats.GetSelected(), EnqueuedRecords: stats.GetEnqueuedRecords(), EnqueuedBytes: stats.GetEnqueuedBytes(), FilterErrors: stats.GetFilterErrors(), Dropped: stats.GetDropped()}})
	}
	c.mu.Lock()
	c.statuses = statuses
	c.rejected = report.GetDesiredStateError()
	c.appliedGeneration = max(c.appliedGeneration, report.GetAppliedGeneration())
	c.count++
	c.mu.Unlock()
	select {
	case c.reports <- struct{}{}:
	default:
	}
}

// resend sends the desired state again under a new generation and returns
// that generation.
func (c *control) resend() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	desired := c.desired()
	for _, s := range c.streams {
		_ = s.Send(desired)
	}
	return desired.Generation
}

// applied says whether the extension reported applying generation.
func (c *control) applied(generation uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.appliedGeneration >= generation
}

func (c *control) reported(sessions []livecaptureextension.AuthorizedRequest) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, want := range sessions {
		found := false
		for _, st := range c.statuses {
			found = found || st.SessionID == want.Request.SessionID
		}
		if !found {
			return false
		}
	}
	return true
}

func (c *control) declare(run provider.Run, id string, s settings, limitation string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count == 0 {
		if c.unreached != "" {
			run.Warning(id, c.unreached)
		} else {
			run.Warning(id, "the extension never connected to the control service")
		}
		return
	}
	if s.Disabled {
		run.Supplied(id, "desired state: capture disabled")
	}
	if c.rejected != "" {
		run.Warning(id, "the extension rejected the desired state: "+c.rejected)
	}
	for _, want := range s.Sessions {
		run.Supplied(id, fmt.Sprintf("session %s for %d capture point(s), signals %v", want.Request.SessionID, len(want.Request.CapturePoints), want.Request.Signals))
	}
	for _, st := range c.statuses {
		line := fmt.Sprintf("session %s %s: %d selected, %d records (%d bytes) enqueued", st.SessionID, st.State, st.Stats.Selected, st.Stats.EnqueuedRecords, st.Stats.EnqueuedBytes)
		if st.Reason != "" {
			line += ", " + st.Reason
		}
		if st.State == "failed" {
			run.Warning(id, line)
		} else {
			run.Supplied(id, line)
		}
	}
	run.Limitation(id, limitation)
}

// prepareLocal starts the scenario's sessions through the extension's
// local_control API once the Collector runs. The extension serves it on
// loopback inside the run, and its local_control.allow_raw, not a
// session's authorized_raw, decides whether raw capture is allowed.
func prepareLocal(run provider.Run, id, address string, s settings) {
	ctl := &control{reports: make(chan struct{}, 1)}
	base := "http://" + address
	client := &http.Client{Timeout: 5 * time.Second}
	run.OnReady(func(ctx context.Context) error {
		if len(s.Sessions) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, sessionWait)
		defer cancel()
		for _, want := range s.Sessions {
			if err := startLocal(ctx, client, base, want.Request); err != nil {
				ctl.mu.Lock()
				ctl.unreached = fmt.Sprintf("session %s was not started through local_control at %s: %v", want.Request.SessionID, address, err)
				ctl.mu.Unlock()
				run.Warning(id, ctl.unreached)
				return nil
			}
		}
		ctl.poll(ctx, client, base)
		return nil
	})
	run.OnInputsDone(func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ctl.poll(ctx, client, base)
		return nil
	})
	run.OnClose(func() error {
		ctl.declare(run, id, s, "sessions were posted to the extension's local_control API from the scenario; raw capture follows local_control.allow_raw, not authorized_raw")
		return nil
	})
}

// startLocal posts one session, retrying until the extension's listener
// accepts connections or ctx ends.
func startLocal(ctx context.Context, client *http.Client, base string, request livecaptureextension.Request) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/sessions", strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(answer)))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// poll reads the extension's session statuses from local_control.
func (c *control) poll(ctx context.Context, client *http.Client, base string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/status", nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var r struct {
		Sessions []livecaptureextension.Status `json:"sessions"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&r) != nil {
		return
	}
	c.mu.Lock()
	c.statuses = r.Sessions
	c.count++
	c.mu.Unlock()
}

func controlEndpoint(c provider.Component) string {
	endpoint, _ := nested(c.Config, "control")["endpoint"].(string)
	return endpoint
}

// localEndpoint is local_control's address when it is enabled.
func localEndpoint(c provider.Component) string {
	local := nested(c.Config, "local_control")
	if on, _ := local["enabled"].(bool); !on {
		return ""
	}
	if endpoint, _ := local["endpoint"].(string); endpoint != "" {
		return endpoint
	}
	return "127.0.0.1:17777"
}

func enabled(c provider.Component) bool {
	v, _ := c.Config["enabled"].(bool)
	return v
}

func nested(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}
