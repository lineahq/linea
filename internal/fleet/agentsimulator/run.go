package agentsimulator

import (
	"context"
	crand "crypto/rand"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/protobufs"
)

const (
	controlReconnect = iota + 1
	controlChurn
)

type peer struct {
	uid        [16]byte // owned by the peer goroutine while no client runs
	control    chan int
	registered chan struct{} // the server answered a new session
	progress   atomic.Uint64
	healthy    bool // peer goroutine
	client     client.OpAMPClient
	healthSent atomic.Int64 // unix nanoseconds of the unanswered health report
	// Set by the client's connect goroutine, one attempt at a time.
	attempt, failed uint64
	pending         *tap
	acked, stopping atomic.Bool
	configs         atomic.Int64
	lostIn, backIn  atomic.Int64 // storm last counted
	// The last remote config status. It survives restarts like the
	// Supervisor's persisted state, and is cleared with a churned identity.
	status atomic.Pointer[protobufs.RemoteConfigStatus]
}
type runner struct {
	config      Config
	stats       statistics
	padding     []byte
	tls         bool
	started     time.Time
	ctx         context.Context
	fatal       chan string
	ready       chan struct{}
	reconnected chan struct{}
	builds      []*protobufs.AvailableComponents // the last is the minimal build
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	// Storm accounting for the current storm. outage makes connection loss
	// expected; the times are unix nanoseconds of the first and last loss and
	// recovery.
	outage                    atomic.Bool
	storm                     atomic.Int64
	stormLost, stormFirstBack atomic.Int64
	stormLastLost, stormLast  atomic.Int64
	lostCount, backCount      atomic.Int64
	stormEnd                  time.Time // Run goroutine only
}

// stormPhase names a storm's checkpoint file; the first keeps the plain name.
func stormPhase(phase string, storm int) string {
	if storm == 1 {
		return phase
	}
	return phase + "-" + strconv.Itoa(storm)
}

// awaitRecovery waits until every session lost in this storm is back and none
// was lost for stormSettle. A driver also ends the window with a storm_end
// checkpoint file, after which the window stays open for stormSettle, so an
// outage that loses no session (a database failover the replicas ride
// through) still completes; without a driver at least one session must have
// been lost.
func (r *runner) awaitRecovery(ctx context.Context, storm int) error {
	deadline := time.NewTimer(r.config.StormTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	var ended time.Time
	for {
		select {
		case <-ctx.Done():
			return errors.New("simulation failed during storm")
		case <-deadline.C:
			return errors.New("sessions did not recover within the storm timeout")
		case <-poll.C:
		}
		lost := r.lostCount.Load()
		if r.backCount.Load() != lost || time.Since(time.Unix(0, r.stormLastLost.Load())) < stormSettle {
			continue
		}
		if dir := r.config.CheckpointDirectory; dir == "" {
			if lost > 0 {
				return nil
			}
		} else if !ended.IsZero() {
			if time.Since(ended) >= stormSettle {
				return nil
			}
		} else if info, err := os.Stat(filepath.Join(dir, stormPhase("storm_end", storm))); err == nil && info.Mode().IsRegular() {
			ended = time.Now()
		}
	}
}

// stormSettle is how long no further session may be lost before a storm
// counts as recovered. Killing one of several replicas loses only its own
// sessions, so recovery means every lost session is back, not every session.
const stormSettle = 3 * time.Second

// A driver can hold a phase for at most 20 seconds while it independently
// verifies public state and collects metrics. All sessions keep heartbeating.
func checkpoint(ctx context.Context, directory, phase string) error {
	if directory == "" {
		return nil
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		info, err := os.Stat(filepath.Join(directory, phase))
		if err == nil && info.Mode().IsRegular() {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("checkpoint unavailable")
		}
		select {
		case <-ctx.Done():
			return errors.New("checkpoint canceled")
		case <-timer.C:
			return errors.New("checkpoint timed out")
		case <-tick.C:
		}
	}
}

func (r *runner) fail(reason string) {
	if r.ctx.Err() != nil {
		return
	}
	r.stats.update(func(c *Counters) { c.UnexpectedErrors++ })
	select {
	case r.fatal <- reason:
	default:
	}
	r.cancel()
}

// register starts a client and waits until the server answers its session.
// opamp-go retries refusals with its own backoff meanwhile.
func (r *runner) register(ctx context.Context, p *peer) string {
	select {
	case <-p.registered:
	default:
	}
	if err := r.start(p); err != nil {
		return "client_start_failed"
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "canceled"
	case <-timer.C:
		return "registration_timeout"
	case <-p.registered:
		return ""
	}
}

func (r *runner) session(ctx context.Context, p *peer, index int) {
	defer r.wg.Done()
	defer r.stop(p)
	if reason := r.register(ctx, p); reason != "" {
		r.fail(reason)
		return
	}
	r.stats.update(func(c *Counters) { c.Ready++ })
	r.ready <- struct{}{}
	// Health changes are staggered; opamp-go sends heartbeats by itself.
	every := r.config.Heartbeat * time.Duration(r.config.HealthChangeEvery)
	if every <= 0 {
		r.control(ctx, p, nil)
		return
	}
	timer := time.NewTimer(every * time.Duration(index+1) / time.Duration(r.config.Agents))
	defer timer.Stop()
	r.health(ctx, p, timer, every)
}

// health runs the session loop for agents whose health changes on a timer.
func (r *runner) health(ctx context.Context, p *peer, timer *time.Timer, every time.Duration) {
	for {
		if !r.control(ctx, p, timer.C) {
			return
		}
		p.healthy = !p.healthy
		p.healthSent.Store(time.Now().UnixNano())
		if p.client.SetHealth(&protobufs.ComponentHealth{Healthy: p.healthy, StartTimeUnixNano: uint64(r.started.UnixNano())}) == nil {
			r.stats.update(func(c *Counters) { c.HealthChanges++ })
		}
		timer.Reset(every)
	}
}

// control serves planned reconnects and churn until tick fires (true) or the
// session ends (false). A nil tick serves until the session ends.
func (r *runner) control(ctx context.Context, p *peer, tick <-chan time.Time) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-tick:
			return true
		case kind := <-p.control:
			// A planned restart: the Collector stops and starts again, under
			// a fresh identity when churned.
			r.stop(p)
			if kind == controlChurn {
				if _, err := crand.Read(p.uid[:]); err != nil {
					r.fail("identity_generation_failed")
					return false
				}
				p.status.Store(nil)
			}
			if reason := r.register(ctx, p); reason != "" {
				r.fail(reason)
				return false
			}
			if kind == controlChurn {
				r.stats.update(func(c *Counters) { c.Churned++ })
			} else {
				r.stats.update(func(c *Counters) { c.Reconnects++ })
				r.reconnected <- struct{}{}
			}
		}
	}
}

// Run owns all sessions and emits only bounded aggregate observations.
func Run(parent context.Context, c Config, emit func(Observation)) (result Observation, err error) {
	if err = c.Validate(); err != nil {
		return result, err
	}
	// Ramp, reconnect and churn, plus at most 20 s per checkpoint and the storm.
	budget := time.Duration((c.Agents+c.reconnectCount())/c.ConnectRate+90)*time.Second + c.Duration + c.Hold + 4*20*time.Second
	budget += time.Duration(c.Storms) * (c.StormTimeout + 20*time.Second)
	ctx, finish := context.WithTimeout(parent, budget)
	defer finish()
	ctx, cancel := context.WithCancel(ctx)
	r := &runner{config: c, padding: make([]byte, c.PayloadBytes), tls: strings.HasPrefix(c.Endpoint, "wss:"), started: time.Now(), ctx: ctx, fatal: make(chan string, 1), ready: make(chan struct{}, c.Agents), reconnected: make(chan struct{}, c.Agents), cancel: cancel}
	if c.Builds > 0 {
		r.builds = builds(c.Builds)
	}
	started := r.started
	steadyMinimum := uint64(0)
	observe := func(phase string, success bool) Observation {
		o := r.stats.snapshot()
		o.Phase = phase
		o.Success = success
		o.ElapsedSeconds = time.Since(started).Seconds()
		o.SteadyMinAcknowledged = steadyMinimum
		o.Storm, o.StormLostNow, o.StormRecoveredNow = int(r.storm.Load()), r.lostCount.Load(), r.backCount.Load()
		if lost, back := r.stormLost.Load(), r.stormFirstBack.Load(); !r.stormEnd.IsZero() && lost != 0 && back != 0 {
			o.StormSeconds = r.stormEnd.Sub(time.Unix(0, lost)).Seconds()
			o.StormFromFirstReconnectSeconds = r.stormEnd.Sub(time.Unix(0, back)).Seconds()
		}
		return o
	}
	output := func(phase string) {
		if emit != nil {
			emit(observe(phase, false))
		}
	}
	defer func() {
		if err == nil && ctx.Err() != nil {
			err = errors.New("simulation canceled before completion")
		}
		cancel()
		r.wg.Wait()
		result = observe("stopped", err == nil)
		if result.UnexpectedErrors != 0 {
			result.Success = false
			if err == nil {
				err = errors.New("simulation failed before shutdown")
			}
		}
		if err != nil {
			select {
			case result.Failure = <-r.fatal:
			default:
				result.Failure = "canceled_or_timed_out"
			}
		}
		if emit != nil {
			emit(result)
		}
	}()
	peers := make([]*peer, c.Agents)
	for i := range peers {
		p := &peer{control: make(chan int, 1), registered: make(chan struct{}, 1), healthy: true}
		if _, err = crand.Read(p.uid[:]); err != nil {
			return result, errors.New("identity generation failed")
		}
		peers[i] = p
	}
	output("ramping")
	ramp := time.NewTicker(time.Second / time.Duration(c.ConnectRate))
	defer ramp.Stop()
	for i, p := range peers {
		select {
		case <-ctx.Done():
			return result, errors.New("simulation failed during ramp")
		case <-ramp.C:
			r.wg.Add(1)
			go r.session(ctx, p, i)
		}
	}
	for range peers {
		select {
		case <-ctx.Done():
			return result, errors.New("simulation failed during admission")
		case <-r.ready:
		}
	}
	before := make([]uint64, len(peers))
	output("ready")
	if err = checkpoint(ctx, c.CheckpointDirectory, "ready"); err != nil {
		return result, err
	}
	// Exclude the driver's registration snapshot from steady progress accounting.
	for i, p := range peers {
		before[i] = p.progress.Load()
	}
	steady := time.NewTimer(c.Duration)
	defer steady.Stop()
	var churn <-chan time.Time
	if c.ChurnRate > 0 {
		ticker := time.NewTicker(time.Second / time.Duration(c.ChurnRate))
		defer ticker.Stop()
		churn = ticker.C
	}
waitSteady:
	for {
		select {
		case <-ctx.Done():
			return result, errors.New("simulation failed during steady reporting")
		case <-churn:
			select {
			case peers[rand.IntN(len(peers))].control <- controlChurn:
			default:
			}
		case <-steady.C:
			break waitSteady
		}
	}
	steadyMinimum = ^uint64(0)
	for i, p := range peers {
		steadyMinimum = min(steadyMinimum, p.progress.Load()-before[i])
	}
	if steadyMinimum == 0 {
		return result, errors.New("a simulated peer made no steady report progress")
	}
	output("steady_complete")
	if err = checkpoint(ctx, c.CheckpointDirectory, "steady_complete"); err != nil {
		return result, err
	}
	for _, p := range peers[:c.reconnectCount()] {
		select {
		case <-ctx.Done():
			return result, errors.New("simulation failed during reconnect")
		case <-ramp.C:
			p.control <- controlReconnect
		}
	}
	for range c.reconnectCount() {
		select {
		case <-ctx.Done():
			return result, errors.New("simulation failed during reconnect")
		case <-r.reconnected:
		}
	}
	output("reconnected")
	if err = checkpoint(ctx, c.CheckpointDirectory, "reconnected"); err != nil {
		return result, err
	}
	for storm := 1; storm <= c.Storms; storm++ {
		r.stormLost.Store(0)
		r.stormFirstBack.Store(0)
		r.stormLastLost.Store(0)
		r.stormLast.Store(0)
		r.lostCount.Store(0)
		r.backCount.Store(0)
		r.stormEnd = time.Time{}
		r.storm.Store(int64(storm))
		r.outage.Store(true)
		output("storm_armed")
		if err = r.awaitRecovery(ctx, storm); err != nil {
			return result, err
		}
		r.outage.Store(false)
		if back := r.stormLast.Load(); back != 0 {
			r.stormEnd = time.Unix(0, back)
		}
		output("recovered")
		if err = checkpoint(ctx, c.CheckpointDirectory, stormPhase("recovered", storm)); err != nil {
			return result, err
		}
	}
	hold := time.NewTimer(c.Hold)
	defer hold.Stop()
	select {
	case <-ctx.Done():
		return result, errors.New("simulation failed during verification hold")
	case <-hold.C:
	}
	output("finished")
	return result, nil
}
