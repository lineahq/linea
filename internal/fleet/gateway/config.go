package gateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// Reconciler decides config delivery. The gateway holds one only when config
// management is enabled; reporting alone never sends configuration.
type Reconciler interface {
	Refresh(ctx context.Context) error
	Reconcile(ctx context.Context, s configmgmt.Session) (configmgmt.Decision, error)
}

// ConfigStore persists failure holds of owned agents and the component
// sets of agent builds.
type ConfigStore interface {
	SetConfigHold(ctx context.Context, id string, generation uint64, expected, next string) (string, error)
	ConfigHolds(ctx context.Context) (map[string]string, error)
	// ComponentSet returns fleet.ErrNotFound for an unknown build.
	ComponentSet(ctx context.Context, hash string) (configmgmt.Components, error)
	SaveComponentSet(ctx context.Context, hash string, c configmgmt.Components) error
}

const (
	// configSweep rereads desired state and holds and re-evaluates every
	// local session, covering lost notifications.
	configSweep = 60 * time.Second
	// configPace spaces asynchronous sends: 200 per second per replica.
	configPace = 5 * time.Millisecond
	// reoffer is how long one hash is not sent again to a session that has
	// not yet reported it.
	reoffer = 30 * time.Second
	// maxComponentSets bounds the builds cached per replica; the cache
	// restarts when full.
	maxComponentSets = 1024
	// componentsWait is how long delivery waits for a component list asked
	// for; after it, the agent is treated as not reporting components.
	componentsWait = 10 * time.Second
	// stateWait is how long delivery waits for the full state after a
	// reconnect; after it, the stored status is used.
	stateWait = 10 * time.Second
)

type configState struct {
	reconciler Reconciler
	holds      ConfigStore
	// wake carries hints to the loop; refresh and holds say what they need.
	wake             chan struct{}
	refresh, resync  atomic.Bool
	done             chan struct{}
	responses, async atomic.Uint64
	errors, failures atomic.Uint64
	holdsSet         atomic.Uint64
	// sweeps counts finished loop passes.
	sweeps atomic.Uint64
	// sets caches component sets by build hash; they never change.
	setsMu                  sync.Mutex
	sets                    map[string]configmgmt.Components
	asked, stored, withheld atomic.Uint64
}

// SetConfigManagement enables config delivery. It must be called before
// serving agents. Desired state is read once here; on failure the loop
// retries and nothing is sent until it succeeds.
func (g *Gateway) SetConfigManagement(r Reconciler, holds ConfigStore) {
	c := &configState{reconciler: r, holds: holds, wake: make(chan struct{}, 1), done: make(chan struct{}), sets: map[string]configmgmt.Components{}}
	ctx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
	if err := r.Refresh(ctx); err != nil {
		c.failures.Add(1)
		c.refresh.Store(true)
		c.wake <- struct{}{}
	}
	cancel()
	g.config = c
	go g.configLoop()
}

// ConfigChanged is the fleet_config hint: an empty agent ID means desired
// state changed; otherwise that agent's hold was cleared. It never blocks.
func (g *Gateway) ConfigChanged(agentID string) {
	c := g.config
	if c == nil {
		return
	}
	if agentID == "" {
		c.refresh.Store(true)
	} else {
		c.resync.Store(true)
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (g *Gateway) configLoop() {
	c := g.config
	defer close(c.done)
	ticker := time.NewTicker(configSweep)
	defer ticker.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-c.wake:
		case <-ticker.C:
			c.refresh.Store(true)
			c.resync.Store(true)
		}
		if c.refresh.Swap(false) {
			ctx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
			if err := c.reconciler.Refresh(ctx); err != nil {
				c.failures.Add(1)
			}
			cancel()
		}
		if c.resync.Swap(false) {
			g.resyncHolds()
		}
		g.sweep()
		c.sweeps.Add(1)
	}
}

// resyncHolds adopts the stored holds of local sessions, so a hold cleared
// through the API is seen even when its notification was missed.
func (g *Gateway) resyncHolds() {
	ctx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
	holds, err := g.config.holds.ConfigHolds(ctx)
	cancel()
	if err != nil {
		g.config.failures.Add(1)
		return
	}
	for _, s := range g.sessions() {
		s.mu.Lock()
		if s.identified && holds[s.id] != s.held {
			s.held, s.offered = holds[s.id], nil
		}
		s.mu.Unlock()
	}
}

func (g *Gateway) sessions() []*session {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*session, 0, len(g.agents))
	for _, e := range g.agents {
		if s := g.connections[e.conn]; s != nil {
			out = append(out, s)
		}
	}
	return out
}

// sweep evaluates every local session and sends asynchronously, paced.
func (g *Gateway) sweep() {
	next := time.Now()
	for _, s := range g.sessions() {
		if g.ctx.Err() != nil || len(g.config.wake) > 0 {
			// A newer hint restarts the sweep from fresh state.
			return
		}
		ctx, cancel := context.WithTimeout(g.ctx, 3*time.Second)
		s.mu.Lock()
		var message *protobufs.ServerToAgent
		if s.identified && !s.closing && g.owns(s) {
			if remote := g.deliver(ctx, s, statusOf(s.report.RemoteConfig), false); remote != nil {
				message = &protobufs.ServerToAgent{InstanceUid: s.uid, Capabilities: g.capabilities(), RemoteConfig: remote}
			}
		}
		s.mu.Unlock()
		if message != nil {
			if wait := time.Until(next); wait > 0 {
				select {
				case <-g.ctx.Done():
					cancel()
					return
				case <-time.After(wait):
				}
			}
			next = time.Now().Add(configPace)
			if err := g.sendFrame(ctx, s.conn, s, message); err != nil {
				disconnect(s.conn)
			} else {
				g.config.async.Add(1)
			}
		}
		cancel()
	}
}

// owns reports whether s is this replica's current holder of its identity.
func (g *Gateway) owns(s *session) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.agents[s.id]
	return ok && e.conn == s.conn && e.generation == s.generation
}

// deliver runs under s.mu on a committed session. prev is the remote config
// status before the message being handled; a change applies the failure
// hold. It returns the config to send, if any.
func (g *Gateway) deliver(ctx context.Context, s *session, prev configmgmt.Status, transition bool) *protobufs.AgentRemoteConfig {
	c := g.config
	if !s.stateAwaited.IsZero() && time.Since(s.stateAwaited) < stateWait {
		return nil
	}
	status := statusOf(s.report.RemoteConfig)
	held := decodeHash(s.held)
	if transition {
		held = configmgmt.NextHold(prev, status, held)
	}
	d, err := c.reconciler.Reconcile(ctx, configmgmt.Session{AgentID: s.id, Credential: s.principal.ID, Attributes: s.report.Attributes,
		Capabilities: s.report.Capabilities, Status: status, Held: held, Components: s.components})
	if err != nil {
		c.errors.Add(1)
		d = configmgmt.Decision{}
	}
	if len(d.Missing) > 0 {
		c.withheld.Add(1)
	}
	if d.ClearHold {
		held = nil
	}
	if next := hex.EncodeToString(held); next != s.held {
		stored, err := c.holds.SetConfigHold(ctx, s.id, s.generation, s.held, next)
		if err != nil {
			c.failures.Add(1)
			return nil
		}
		s.held = stored
		if stored != next {
			// Changed meanwhile, for example cleared through the API; the
			// next evaluation starts from the stored hold.
			return nil
		}
		if next != "" {
			c.holdsSet.Add(1)
		}
	}
	if !d.Send {
		return nil
	}
	if !s.awaiting.IsZero() && time.Since(s.awaiting) < componentsWait {
		return nil
	}
	if bytes.Equal(s.offered, d.Plan.Hash) && time.Since(s.offeredAt) < reoffer {
		return nil
	}
	s.offered, s.offeredAt = d.Plan.Hash, time.Now()
	files := make(map[string]*protobufs.AgentConfigObject, len(d.Plan.Entries))
	for _, e := range d.Plan.Entries {
		files[e.Name] = &protobufs.AgentConfigObject{Body: e.Body, ContentType: "text/yaml"}
	}
	return &protobufs.AgentRemoteConfig{Config: &protobufs.AgentConfigMap{ConfigMap: files}, ConfigHash: d.Plan.Hash}
}

// components resolves the session's component set when its reported build
// changes. An unknown build is asked for in full once per session; the list
// the agent then sends is stored for every agent of that build. It reports
// whether to ask, and whether the resolved set changed.
func (g *Gateway) components(ctx context.Context, s *session, ac *protobufs.AvailableComponents) (ask, changed bool) {
	hash := s.report.AvailableComponents
	if hash == s.componentsHash {
		return false, false
	}
	c := g.config
	if s.components != nil {
		// A new build: unknown until resolved, so the check is skipped.
		s.components, changed = nil, true
	}
	if ac != nil && len(ac.Components) > 0 && hex.EncodeToString(ac.Hash) == hash {
		set := availableComponents(ac)
		if set == nil {
			// An unusable list: the check stays skipped for this build.
			s.componentsHash, s.awaiting = hash, time.Time{}
			return false, true
		}
		if err := c.holds.SaveComponentSet(ctx, hash, set); err != nil {
			c.failures.Add(1)
			return false, changed
		}
		c.stored.Add(1)
		c.cache(hash, set)
		s.components, s.componentsHash, s.awaiting = set, hash, time.Time{}
		return false, true
	}
	set, ok := c.cached(hash)
	if !ok {
		if s.componentsAsked == hash {
			// Asked already: wait for the list instead of querying per message.
			return false, changed
		}
		stored, err := c.holds.ComponentSet(ctx, hash)
		if errors.Is(err, fleet.ErrNotFound) {
			s.componentsAsked, s.awaiting = hash, time.Now()
			c.asked.Add(1)
			return true, changed
		}
		if err != nil {
			c.failures.Add(1)
			return false, changed
		}
		set = stored
		c.cache(hash, set)
	}
	s.components, s.componentsHash, s.awaiting = set, hash, time.Time{}
	return false, true
}

// availableComponents reads the Collector's report: one entry per section,
// whose sub-components are keyed by component type.
func availableComponents(ac *protobufs.AvailableComponents) configmgmt.Components {
	byKind := map[string][]string{}
	for kind, d := range ac.Components {
		if d == nil {
			continue
		}
		for typ := range d.SubComponentMap {
			byKind[kind] = append(byKind[kind], typ)
		}
	}
	return configmgmt.NewComponents(byKind)
}

func (c *configState) cached(hash string) (configmgmt.Components, bool) {
	c.setsMu.Lock()
	defer c.setsMu.Unlock()
	set, ok := c.sets[hash]
	return set, ok
}

func (c *configState) cache(hash string, set configmgmt.Components) {
	c.setsMu.Lock()
	defer c.setsMu.Unlock()
	if len(c.sets) >= maxComponentSets {
		clear(c.sets)
	}
	c.sets[hash] = set
}

// capabilities advertises remote config only with config management.
func (g *Gateway) capabilities() uint64 {
	caps := protobufs.ServerCapabilities_ServerCapabilities_AcceptsStatus
	if g.config != nil {
		caps |= protobufs.ServerCapabilities_ServerCapabilities_OffersRemoteConfig
	}
	return uint64(caps)
}

func statusOf(rc *fleet.RemoteConfig) configmgmt.Status {
	if rc == nil {
		return configmgmt.Status{}
	}
	return configmgmt.Status{Hash: decodeHash(rc.Hash), State: configmgmt.State(rc.Status)}
}

func decodeHash(h string) []byte {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) == 0 {
		return nil
	}
	return b
}

func sameStatus(a, b configmgmt.Status) bool {
	return a.State == b.State && bytes.Equal(a.Hash, b.Hash)
}

func (g *Gateway) writeConfigMetrics(w io.Writer) {
	c := g.config
	if c == nil {
		return
	}
	fmt.Fprintf(w, "# TYPE linea_fleet_config_sends_total counter\nlinea_fleet_config_sends_total{path=\"response\"} %d\nlinea_fleet_config_sends_total{path=\"async\"} %d\n", c.responses.Load(), c.async.Load())
	fmt.Fprintf(w, "# TYPE linea_fleet_config_reconcile_errors_total counter\nlinea_fleet_config_reconcile_errors_total %d\n# TYPE linea_fleet_config_storage_errors_total counter\nlinea_fleet_config_storage_errors_total %d\n# TYPE linea_fleet_config_holds_total counter\nlinea_fleet_config_holds_total %d\n", c.errors.Load(), c.failures.Load(), c.holdsSet.Load())
	fmt.Fprintf(w, "# TYPE linea_fleet_config_component_sets_requested_total counter\nlinea_fleet_config_component_sets_requested_total %d\n# TYPE linea_fleet_config_component_sets_stored_total counter\nlinea_fleet_config_component_sets_stored_total %d\n# TYPE linea_fleet_config_incompatible_total counter\nlinea_fleet_config_incompatible_total %d\n", c.asked.Load(), c.stored.Load(), c.withheld.Load())
	if b, ok := c.reconciler.(interface{ CachedBodyBytes() int64 }); ok {
		fmt.Fprintf(w, "# TYPE linea_fleet_config_body_cache_bytes gauge\nlinea_fleet_config_body_cache_bytes %d\n", b.CachedBodyBytes())
	}
}
