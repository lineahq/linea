package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/translog"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// Reconciler decides config delivery. The gateway holds one only when config
// management is enabled; reporting alone never sends configuration.
type Reconciler interface {
	Refresh(ctx context.Context) error
	Reconcile(ctx context.Context, s configmgmt.Session) (configmgmt.Decision, error)
	// Managed reports whether a credential opted in to remote config.
	Managed(credential string) bool
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
	// for; after it, config stays withheld until the build is resolved.
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
	// unverified counts sends withheld for an unresolved build; unmatched
	// counts component lists refused because they do not match their hash.
	unverified, unmatched atomic.Uint64
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
			s.adoptHold(holds[s.id])
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

// adoptHold takes a hold that changed in the store. A hold that was cleared
// owes one retry of the desired config, even when the agent reports that very
// hash as failed.
func (s *session) adoptHold(stored string) {
	if s.held != "" && stored == "" {
		s.retryOwed = true
	}
	s.held, s.offered = stored, nil
}

// owns reports whether s is this replica's current holder of its identity.
func (g *Gateway) owns(s *session) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.agents[s.id]
	return ok && e.conn == s.conn && e.generation == s.generation
}

// attachConfig puts the config to deliver, if any, into an answer to the
// agent. With the push in this file, it is the only place that sends remote
// config (TestOnlyConfigFileSendsAgentCommands).
func (g *Gateway) attachConfig(ctx context.Context, s *session, prev configmgmt.Status, transition bool, response *protobufs.ServerToAgent) {
	if remote := g.deliver(ctx, s, prev, transition); remote != nil {
		response.RemoteConfig = remote
		g.config.responses.Add(1)
	}
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
		if status.State == configmgmt.Failed || status.State == configmgmt.Applied {
			// The agent answered the retry, if one was out.
			s.retrySent = nil
		}
	}
	d, err := c.reconciler.Reconcile(ctx, configmgmt.Session{AgentID: s.id, Credential: s.principal.ID, Attributes: s.report.Attributes,
		Capabilities: s.report.Capabilities, Status: status, Held: held, Retry: s.retryOwed, Components: s.components,
		Unverified: s.unresolved()})
	if err != nil {
		c.errors.Add(1)
		d = configmgmt.Decision{}
	}
	if len(d.Missing) > 0 || d.Unverified {
		why := translog.ReasonIncompatibleComponents
		if d.Unverified {
			c.unverified.Add(1)
			why = translog.ReasonUnverifiedComponents
		} else {
			c.withheld.Add(1)
		}
		// One line per session and withheld plan, not per evaluation.
		if !bytes.Equal(s.withheld, d.Plan.Hash) {
			s.withheld = bytes.Clone(d.Plan.Hash)
			g.log.Limited(translog.ConfigWithheld, translog.ID(translog.KeyAgent, s.id), translog.Why(why))
		}
	} else {
		s.withheld = nil
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
		if stored != next {
			// Changed meanwhile, for example cleared through the API; the
			// next evaluation starts from the stored hold.
			s.adoptHold(stored)
			return nil
		}
		s.held = stored
		if next != "" {
			c.holdsSet.Add(1)
			g.log.Limited(translog.ConfigWithheld, translog.ID(translog.KeyAgent, s.id), translog.Why(translog.ReasonAgentReportedFailure))
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
	if s.retryOwed {
		s.retryOwed, s.retrySent = false, d.Plan.Hash
	}
	files := make(map[string]*protobufs.AgentConfigObject, len(d.Plan.Entries))
	for _, e := range d.Plan.Entries {
		files[e.Name] = &protobufs.AgentConfigObject{Body: e.Body, ContentType: "text/yaml"}
	}
	return &protobufs.AgentRemoteConfig{Config: &protobufs.AgentConfigMap{ConfigMap: files}, ConfigHash: d.Plan.Hash}
}

// components resolves the session's component set when its reported build
// changes. Only a credential that opted in to remote config resolves builds.
// An unknown build is asked for in full once per session; the list the
// agent then sends is stored for every agent of that build only when it
// hashes to the reported build hash. It reports whether to ask, and whether
// the resolved set changed. A build that stays unresolved withholds config.
func (g *Gateway) components(ctx context.Context, s *session, ac *protobufs.AvailableComponents) (ask, changed bool) {
	hash := s.report.AvailableComponents
	if hash == s.componentsHash {
		return false, false
	}
	c := g.config
	if !c.reconciler.Managed(s.principal.ID) {
		// Nothing is sent to this credential; resolve once it opts in.
		return false, false
	}
	if s.components != nil {
		// A new build: unknown until resolved, so the check is skipped.
		s.components, changed = nil, true
	}
	if ac != nil && len(ac.Components) > 0 && hex.EncodeToString(ac.Hash) == hash {
		set := availableComponents(ac)
		if set == nil || !componentsHashMatches(ac) {
			// A list that is unusable or does not match its hash leaves the
			// build unresolved for this session: config stays withheld.
			c.unmatched.Add(1)
			s.componentsHash, s.awaiting = hash, time.Time{}
			return false, true
		}
		if err := c.holds.SaveComponentSet(ctx, hash, set); err != nil {
			c.failures.Add(1)
			if errors.Is(err, fleet.ErrCapacity) {
				// Full: the build stays unresolved for this session.
				s.componentsHash, s.awaiting = hash, time.Time{}
				return false, true
			}
			// Ask again with a later message.
			s.componentsAsked = ""
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

// unresolved reports a build that the session reported but could not
// resolve, once any wait for its component list is over.
func (s *session) unresolved() bool {
	return s.report.AvailableComponents != "" && s.components == nil &&
		(s.awaiting.IsZero() || time.Since(s.awaiting) >= componentsWait)
}

// componentKinds maps the report's sections to the names the Collector's
// OpAMP extension hashes, in its order.
var componentKinds = []struct{ section, kind string }{
	{"receivers", "receiver"}, {"processors", "processor"}, {"exporters", "exporter"},
	{"extensions", "extension"}, {"connectors", "connector"},
}

// componentsHashMatches recomputes the build hash the way the Collector's
// OpAMP extension does (verified against opampextension v0.162.0): SHA-256
// of "kind:" followed by "type=module;" per sorted type, for each kind. A
// list that does not hash to the reported value cannot describe that build.
func componentsHashMatches(ac *protobufs.AvailableComponents) bool {
	if len(ac.Components) > len(componentKinds) {
		return false
	}
	var b strings.Builder
	for _, k := range componentKinds {
		d := ac.Components[k.section]
		types := make([]string, 0)
		if d != nil {
			for typ := range d.SubComponentMap {
				types = append(types, typ)
			}
		}
		sort.Strings(types)
		b.WriteString(k.kind + ":")
		for _, typ := range types {
			module := ""
			for _, kv := range d.SubComponentMap[typ].GetMetadata() {
				if kv.GetKey() == "code.namespace" {
					module = kv.GetValue().GetStringValue()
				}
			}
			b.WriteString(typ + "=" + module + ";")
		}
	}
	for section := range ac.Components {
		if !slices.ContainsFunc(componentKinds, func(k struct{ section, kind string }) bool { return k.section == section }) {
			return false
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return bytes.Equal(sum[:], ac.Hash)
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
	fmt.Fprintf(w, "# TYPE linea_fleet_config_component_sets_requested_total counter\nlinea_fleet_config_component_sets_requested_total %d\n# TYPE linea_fleet_config_component_sets_stored_total counter\nlinea_fleet_config_component_sets_stored_total %d\n# TYPE linea_fleet_config_incompatible_total counter\nlinea_fleet_config_incompatible_total %d\n# TYPE linea_fleet_config_unverified_total counter\nlinea_fleet_config_unverified_total %d\n# TYPE linea_fleet_config_component_sets_unmatched_total counter\nlinea_fleet_config_component_sets_unmatched_total %d\n", c.asked.Load(), c.stored.Load(), c.withheld.Load(), c.unverified.Load(), c.unmatched.Load())
	if b, ok := c.reconciler.(interface{ CachedBodyBytes() int64 }); ok {
		fmt.Fprintf(w, "# TYPE linea_fleet_config_body_cache_bytes gauge\nlinea_fleet_config_body_cache_bytes %d\n", b.CachedBodyBytes())
	}
}
