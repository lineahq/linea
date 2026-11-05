package livecapture

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lineahq/linea/pluginsdk"
)

type Repository interface {
	AuthorityID() string
	Ready(context.Context) error
	NextGeneration(context.Context, string) (uint64, error)
	Get(context.Context, string) (Operation, error)
	Insert(context.Context, Operation) error
	Save(context.Context, Operation, int64) error
}
type activeOperation struct {
	op      Operation
	request CreateRequest
	raw     bool
}
type Server struct {
	mu              sync.Mutex
	config          Config
	repo            Repository
	ctx             context.Context
	cancel          context.CancelFunc
	instance        string
	host            pluginsdk.HostClient
	epoch           string
	session         pluginsdk.Session
	hostOK, dbOK    bool
	permissionsOK   bool
	draining        bool
	peers           map[string]*peer
	operations      map[string]*activeOperation
	channels        map[string]*channel
	credentials     map[[32]byte]cachedCredential
	linkWake        chan struct{}
	revalidateEvery time.Duration
	dataGate        chan struct{}
	fatal           chan error
	wg              sync.WaitGroup
	observed        pluginMetrics
	budgets         [3]*connectionBudget
}

func New(ctx context.Context, c Config, repo Repository) (*Server, error) {
	if err := repo.Ready(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Server{config: c, repo: repo, ctx: ctx, cancel: cancel, instance: pluginsdk.RandomID(), dbOK: true, peers: map[string]*peer{}, operations: map[string]*activeOperation{}, channels: map[string]*channel{}, credentials: map[[32]byte]cachedCredential{}, linkWake: make(chan struct{}, 1), revalidateEvery: credentialTTL, dataGate: make(chan struct{}, 16), fatal: make(chan error, 1)}
	s.host = pluginsdk.HostClient{Endpoint: c.HostEndpoint, Token: c.hostToken, InstanceID: s.instance, HTTP: pluginsdk.NewHTTPClient()}
	for i, limit := range []int{c.peerLimit() + 16, 64, 64} {
		s.budgets[i] = &connectionBudget{slots: make(chan struct{}, limit)}
	}
	return s, nil
}
func (s *Server) Start() { s.wg.Add(3); go s.monitorHost(); go s.maintain(); go s.revalidate() }
func (s *Server) Drain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
	s.hostOK = false
	s.stopAllLocked("stopping")
}
func (s *Server) Close() {
	s.mu.Lock()
	s.hostOK = false
	s.stopAllLocked("stopping")
	s.mu.Unlock()
	// Give authenticated peers a bounded chance to observe empty desired state.
	select {
	case <-time.After(300 * time.Millisecond):
	case <-s.ctx.Done():
	}
	s.cancel()
	s.mu.Lock()
	for _, p := range s.peers {
		p.close()
	}
	for _, c := range s.channels {
		s.closeChannelLocked(c)
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.host.HTTP.CloseIdleConnections()
}
func (s *Server) Fatal() <-chan error { return s.fatal }
func (s *Server) fatalLocked() {
	s.dbOK = false
	select {
	case s.fatal <- ErrUnavailable:
	default:
	}
}
func (s *Server) saveLocked(a *activeOperation) error {
	previous := a.op.Revision
	a.op.Revision++
	if err := s.repo.Save(s.ctx, a.op, previous); err != nil {
		s.fatalLocked()
		return err
	}
	return nil
}
func (s *Server) stopLocked(a *activeOperation, state string) error {
	if a.op.State != "starting" && a.op.State != "active" && a.op.State != "stopping" {
		return nil
	}
	var result error
	if a.op.State != state {
		if state == "stopping" {
			s.observed.withdrawn.Add(1)
		}
		a.op.State = state
		if state == "expired" {
			expireTargets(a.op.Targets)
		}
		result = s.saveLocked(a)
	}
	for _, t := range a.op.Targets {
		if p := s.peers[t.PeerID]; p != nil && p.info.Generation == t.Generation && p.operation == a.op.ID {
			p.operation = ""
			p.wakeup()
		}
	}
	return result
}

// expireTargets ends targets with the operation's absolute expiry, which every
// Collector enforces too; reports arriving after the operation ended are dropped.
func expireTargets(targets []Target) {
	for i := range targets {
		if !terminal(targets[i].State) {
			targets[i].State = "expired"
		}
	}
}
func (s *Server) stopAllLocked(state string) {
	for _, a := range s.operations {
		if s.stopLocked(a, state) != nil {
			break
		}
	}
}
func (s *Server) monitorHost() {
	defer s.wg.Done()
	for {
		session, err := s.host.Session(s.ctx)
		s.mu.Lock()
		// Without both host permissions Collectors cannot be authenticated or linked.
		s.permissionsOK = err == nil && hasPermissions(session)
		if err == nil && !s.permissionsOK {
			err = ErrUnavailable
		}
		if err != nil || s.epoch != "" && s.epoch != session.Epoch {
			if s.hostOK {
				s.hostOK = false
				for _, a := range s.operations {
					if a.op.Reason == "" {
						a.op.Reason = "host_authority_lost"
					}
				}
				s.stopAllLocked("stopping")
				for id, c := range s.channels {
					s.closeChannelLocked(c)
					delete(s.channels, id)
				}
			}
			// A new host epoch may follow credential changes; forget cached answers.
			clear(s.credentials)
		}
		if err == nil && !s.draining {
			s.epoch, s.session = session.Epoch, session
			s.hostOK = true
		}
		s.mu.Unlock()
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
func (s *Server) maintain() {
	defer s.wg.Done()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	lastDB := time.Time{}
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-tick.C:
			if now.Sub(lastDB) >= time.Second {
				err := s.repo.Ready(s.ctx)
				s.mu.Lock()
				if err != nil {
					s.fatalLocked()
					s.mu.Unlock()
					return
				}
				for hash, entry := range s.credentials {
					if !now.Before(entry.until) {
						delete(s.credentials, hash)
					}
				}
				s.mu.Unlock()
				lastDB = now
			}
			s.mu.Lock()
			for id, c := range s.channels {
				if now.Sub(c.lastSeen) > 5*time.Second || !now.Before(c.expires) {
					s.closeChannelLocked(c)
					delete(s.channels, id)
				}
			}
			for _, p := range s.peers {
				if now.Sub(p.lastSeen) > 6*time.Second {
					p.close()
				}
			}
			for id, a := range s.operations {
				if !now.Before(a.op.ExpiresAt) {
					if a.op.Reason == "" {
						a.op.Reason = "absolute_expiry"
					}
					_ = s.stopLocked(a, "expired")
					delete(s.operations, id)
					continue
				}
				if a.op.State == "starting" || a.op.State == "active" {
					if c := s.channels[a.op.ChannelID]; c == nil || now.Sub(c.lastSeen) > 5*time.Second {
						if a.op.Reason == "" {
							a.op.Reason = "consumer_unavailable"
						}
						_ = s.stopLocked(a, "stopping")
					}
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) Peers() []PeerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []PeerInfo{}
	for _, p := range s.peers {
		info := p.info
		info.Identity.Points = append([]CatalogPoint(nil), info.Identity.Points...)
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
func validPeerID(id string) bool {
	v, err := hex.DecodeString(id)
	return err == nil && len(v) == 32 && hex.EncodeToString(v) == id
}

// uidRule is the reason for an ID pluginsdk.ValidUID refuses.
const uidRule = "must be 32 lowercase hexadecimal characters"

// invalid is ErrInvalid with a reason the API returns. It names the field
// and the rule, never the value.
func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrInvalid, reason) }

func validateCreate(r CreateRequest) error {
	modes := 0
	for _, used := range []bool{len(r.Targets) > 0, len(r.Agents) > 0, r.Selector != "", r.GroupID != ""} {
		if used {
			modes++
		}
	}
	switch {
	case modes != 1:
		return invalid("set exactly one of targets, agents, selector and group_id")
	case len(r.Targets) > 16 || len(r.Agents) > 16:
		return invalid("targets and agents allow at most 16 entries")
	case len(r.Selector) > 4096 || strings.ContainsFunc(r.Selector, unicode.IsControl):
		return invalid("selector must be at most 4,096 bytes without control characters")
	case r.GroupID != "" && !pluginsdk.ValidUID(r.GroupID):
		return invalid("group_id " + uidRule)
	case !pluginsdk.ValidUID(r.AuthorityID) || !pluginsdk.ValidUID(r.PluginInstance) || !pluginsdk.ValidUID(r.ID) || !pluginsdk.ValidUID(r.ChannelID):
		return invalid("authority_id, plugin_instance, id and channel_id " + uidRule)
	case len(r.Points) < 1 || len(r.Points) > 64:
		return invalid("capture_points needs 1 to 64 entries")
	case len(r.Signals) < 1 || len(r.Signals) > 3:
		return invalid("signals needs 1 to 3 entries")
	case !r.ExpiresAt.After(time.Now()) || r.ExpiresAt.After(time.Now().Add(5*time.Minute)):
		return invalid("expires_at must be in the future and at most 5 minutes away")
	}
	seen := map[string]bool{}
	for _, t := range r.Targets {
		if !validPeerID(t.PeerID) || t.Generation == 0 || seen[t.PeerID] {
			return invalid("targets need unique peer IDs of 64 lowercase hexadecimal characters and a generation")
		}
		seen[t.PeerID] = true
	}
	for _, agent := range r.Agents {
		if !pluginsdk.ValidUID(agent) || seen[agent] {
			return invalid("agents must be unique and each " + uidRule)
		}
		seen[agent] = true
	}
	seen = map[string]bool{}
	for _, signal := range r.Signals {
		if (signal != "traces" && signal != "logs" && signal != "metrics") || seen[signal] {
			return invalid("signals must be unique traces, logs or metrics")
		}
		seen[signal] = true
	}
	seen = map[string]bool{}
	for _, point := range r.Points {
		if !safeName(point.Pipeline) || !safeName(point.Point) || seen[point.Pipeline+"/"+point.Point] {
			return invalid("capture_points must be unique, with pipeline and point names of 1 to 128 printable bytes")
		}
		seen[point.Pipeline+"/"+point.Point] = true
	}
	if r.Sampling != nil && (math.IsNaN(r.Sampling.Rate) || r.Sampling.Rate < 0 || r.Sampling.Rate > 1 || (r.Sampling.Mode != "" && r.Sampling.Mode != "hash" && r.Sampling.Mode != "random")) {
		return invalid("sampling needs a rate from 0 to 1 and the mode hash or random")
	}
	for _, v := range []int64{r.Limits.BytesPerSecond, r.Limits.RecordsPerSecond, r.Limits.TotalBytes, r.Limits.TotalRecords, r.Limits.RecordBytes} {
		if v < 0 {
			return invalid("limits cannot be negative")
		}
	}
	if r.Limits.BytesPerSecond > 1<<20 || r.Limits.RecordsPerSecond > 5000 || r.Limits.TotalBytes > 16<<20 || r.Limits.TotalRecords > 10000 || r.Limits.RecordBytes > 64<<10 {
		return invalid("limits allow at most 1 MiB/s, 5,000 records/s, 16 MiB, 10,000 records and 64 KiB per record")
	}
	conditionBytes := 0
	for _, conditions := range [][]string{r.Filter.Resource, r.Filter.Traces.Span, r.Filter.Logs.Log, r.Filter.Metrics.Metric, r.Filter.Metrics.Datapoint} {
		if len(conditions) > 32 {
			return invalid("each filter list allows at most 32 conditions")
		}
		for _, condition := range conditions {
			if len(condition) > 4096 {
				return invalid("a filter condition allows at most 4,096 bytes")
			}
			conditionBytes += len(condition)
		}
	}
	if conditionBytes > 16<<10 {
		return invalid("the filter allows at most 16 KiB of conditions")
	}
	return nil
}
func (s *Server) Create(ctx context.Context, owner string, request CreateRequest) (result Operation, resultErr error) {
	defer func() {
		if resultErr != nil {
			s.observed.admissionRefused.Add(1)
		}
	}()
	encoded, encodeErr := json.Marshal(request)
	if encodeErr != nil {
		return Operation{}, ErrInvalid
	}
	var owned CreateRequest
	if json.Unmarshal(encoded, &owned) != nil {
		return Operation{}, ErrInvalid
	}
	request = owned
	hash := sha256.Sum256(encoded)
	if !pluginsdk.ValidUID(request.ID) {
		return Operation{}, ErrInvalid
	}
	previous, err := s.repo.Get(ctx, request.ID)
	if err == nil {
		if previous.Owner != owner {
			return Operation{}, ErrForbidden
		}
		if subtle.ConstantTimeCompare(previous.Hash, hash[:]) != 1 {
			return Operation{}, ErrConflict
		}
		return previous, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Operation{}, err
	}
	if err := validateCreate(request); err != nil {
		return Operation{}, err
	}
	// Fleet groups and selectors resolve before the lock; admission rechecks peers.
	var candidates map[string]bool
	if request.Selector != "" || request.GroupID != "" {
		if candidates, err = s.resolveTargets(ctx, request); err != nil {
			return Operation{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hostOK || !s.dbOK || s.draining {
		return Operation{}, ErrUnavailable
	}
	if expected, ok := ctx.Value(authorityContextKey{}).(string); ok && expected != s.epoch {
		return Operation{}, ErrConflict
	}
	if request.AuthorityID != s.repo.AuthorityID() || request.PluginInstance != s.instance {
		return Operation{}, ErrConflict
	}
	if len(s.operations) >= 32 {
		return Operation{}, ErrCapacity
	}
	c := s.channels[request.ChannelID]
	if c == nil || c.owner != owner || time.Since(c.lastSeen) > 5*time.Second || !c.expires.After(request.ExpiresAt) {
		return Operation{}, ErrConflict
	}
	raw := slices.Contains(s.config.RawPrincipals, owner)
	op := Operation{AuthorityID: s.repo.AuthorityID(), ID: request.ID, Owner: owner, Hash: hash[:], ChannelID: request.ChannelID, State: "starting", Revision: 1, ExpiresAt: request.ExpiresAt}
	if acting, ok := ctx.Value(actingContextKey{}).(pluginsdk.Acting); ok {
		op.ActingFor = &acting
	}
	peers, err := s.selectPeersLocked(request, candidates, raw)
	if err != nil {
		return Operation{}, err
	}
	for _, p := range peers {
		op.Targets = append(op.Targets, Target{PeerID: p.info.ID, Generation: p.info.Generation, AgentID: p.info.AgentID, WireID: pluginsdk.RandomID(), State: "pending"})
	}
	if err := s.repo.Insert(ctx, op); err != nil {
		return Operation{}, err
	}
	s.observed.admitted.Add(1)
	a := &activeOperation{op: op, request: request, raw: raw}
	s.operations[op.ID] = a
	if ctx.Err() != nil {
		a.op.State = "interrupted"
		a.op.Reason = "admission_cancelled"
		_ = s.saveLocked(a)
		delete(s.operations, op.ID)
		return Operation{}, ErrUnavailable
	}
	for _, target := range op.Targets {
		p := s.peers[target.PeerID]
		p.operation = op.ID
		p.wakeup()
	}
	return cloneOperation(op), nil
}

// resolveTargets asks the fleet host for the connected agents of a group or
// selector.
func (s *Server) resolveTargets(ctx context.Context, request CreateRequest) (map[string]bool, error) {
	s.mu.Lock()
	session, ready := s.session, s.hostOK && !s.draining
	s.mu.Unlock()
	if !ready {
		return nil, ErrUnavailable
	}
	connected := true
	out, err := s.host.ResolveGroup(ctx, session, pluginsdk.GroupResolveRequest{GroupID: request.GroupID, Selector: request.Selector, Connected: &connected})
	if err != nil {
		switch pluginsdk.StatusOf(err) {
		case 400:
			return nil, ErrInvalid
		case 404:
			return nil, ErrNotFound
		case 422:
			return nil, ErrCapacity
		}
		return nil, ErrUnavailable
	}
	agents := make(map[string]bool, len(out.AgentIDs))
	for _, id := range out.AgentIDs {
		agents[id] = true
	}
	return agents, nil
}

// selectPeersLocked maps the request onto connected peers. Peer and agent
// targets are strict; group and selector targets admit only the linked peers
// that can serve the request, between 1 and 16 of them.
func (s *Server) selectPeersLocked(request CreateRequest, candidates map[string]bool, raw bool) ([]*peer, error) {
	selected := []*peer{}
	switch {
	case len(request.Targets) > 0:
		for _, target := range request.Targets {
			p := s.peers[target.PeerID]
			if p == nil || p.info.Generation != target.Generation {
				return nil, ErrConflict
			}
			if err := s.capturableLocked(p, request, raw); err != nil {
				return nil, err
			}
			selected = append(selected, p)
		}
	case len(request.Agents) > 0:
		for _, agent := range request.Agents {
			var match *peer
			matches := 0
			for _, p := range s.peers {
				if p.info.AgentID == agent {
					match, matches = p, matches+1
				}
			}
			if matches != 1 {
				return nil, ErrConflict
			}
			if err := s.capturableLocked(match, request, raw); err != nil {
				return nil, err
			}
			selected = append(selected, match)
		}
	default:
		busy := false
		var tooLarge error
		for _, p := range s.peers {
			if p.info.AgentID == "" || !candidates[p.info.AgentID] {
				continue
			}
			err := s.capturableLocked(p, request, raw)
			if err == nil {
				selected = append(selected, p)
			}
			busy = busy || err == ErrPeerBusy
			if errors.Is(err, errFilterTooLarge) {
				tooLarge = err
			}
		}
		sort.Slice(selected, func(i, j int) bool { return selected[i].info.ID < selected[j].info.ID })
		if len(selected) == 0 && busy {
			return nil, ErrPeerBusy
		}
		// Otherwise capable Collectors that compile no filter this large say
		// why better than a bare conflict.
		if len(selected) == 0 && tooLarge != nil {
			return nil, tooLarge
		}
		if len(selected) == 0 {
			return nil, ErrConflict
		}
		if len(selected) > 16 {
			return nil, ErrCapacity
		}
	}
	return selected, nil
}

// errFilterTooLarge refuses a filter larger than a Collector reports it
// compiles. The Collector checks IsMatch patterns itself.
var errFilterTooLarge = fmt.Errorf("%w: filter too large", ErrInvalid)

// fitFilter checks a filter against a Collector's filter limits.
func fitFilter(f Filter, limits FilterLimits) error {
	limits = limits.orV1()
	total := 0
	for _, conditions := range [][]string{f.Resource, f.Traces.Span, f.Logs.Log, f.Metrics.Metric, f.Metrics.Datapoint} {
		total += len(conditions)
		for _, condition := range conditions {
			if n := utf8.RuneCountInString(condition); n > limits.MaxConditionChars {
				return fmt.Errorf("%w: a condition has %d characters; the Collector compiles at most %d", errFilterTooLarge, n, limits.MaxConditionChars)
			}
		}
	}
	if total > limits.MaxConditions {
		return fmt.Errorf("%w: %d conditions; the Collector compiles at most %d", errFilterTooLarge, total, limits.MaxConditions)
	}
	return nil
}

// capturableLocked checks that an idle, enabled peer offers every requested
// point and signal, compiles the filter, and that raw points are limited to
// raw principals.
func (s *Server) capturableLocked(p *peer, request CreateRequest, raw bool) error {
	if !p.alive() || !p.info.Identity.Enabled || time.Since(p.lastSeen) > 6*time.Second {
		return ErrConflict
	}
	if p.operation != "" {
		return ErrPeerBusy
	}
	if err := fitFilter(request.Filter, p.info.Identity.FilterLimits); err != nil {
		return err
	}
	for _, point := range request.Points {
		found := false
		for _, catalog := range p.info.Identity.Points {
			if catalog.Pipeline == point.Pipeline && catalog.Point == point.Point && slices.Contains(request.Signals, catalog.Signal) {
				if catalog.Sensitivity == "raw" && !raw {
					return ErrForbidden
				}
				found = true
			}
		}
		if !found {
			return invalid("the Collector offers no capture point " + point.Pipeline + "/" + point.Point + " for the requested signals")
		}
	}
	for _, signal := range request.Signals {
		found := false
		for _, catalog := range p.info.Identity.Points {
			for _, point := range request.Points {
				found = found || (catalog.Signal == signal && catalog.Point == point.Point && catalog.Pipeline == point.Pipeline)
			}
		}
		if !found {
			return invalid("no requested capture point offers " + signal)
		}
	}
	return nil
}

func (s *Server) Get(ctx context.Context, owner, id string) (Operation, error) {
	if !pluginsdk.ValidUID(id) {
		return Operation{}, ErrInvalid
	}
	op, err := s.repo.Get(ctx, id)
	if err == nil && op.Owner != owner {
		return Operation{}, ErrForbidden
	}
	if err == nil && !time.Now().Before(op.ExpiresAt) && (op.State == "starting" || op.State == "active" || op.State == "stopping") {
		s.mu.Lock()
		if active := s.operations[id]; active != nil {
			defer s.mu.Unlock()
			if active.op.Owner != owner {
				return Operation{}, ErrForbidden
			}
			if active.op.Reason == "" {
				active.op.Reason = "absolute_expiry"
			}
			err = s.stopLocked(active, "expired")
			return cloneOperation(active.op), err
		}
		// Unlocked before liveStats below, which takes s.mu again.
		s.mu.Unlock()
		previous := op.Revision
		op.State = "expired"
		op.Reason = "absolute_expiry"
		expireTargets(op.Targets)
		op.Revision++
		if err := s.repo.Save(ctx, op, previous); err != nil {
			return s.repo.Get(ctx, id)
		}
	}
	if err == nil {
		s.liveStats(&op)
	}
	return op, err
}

// liveStats overlays counters reported since the operation was last saved.
func (s *Server) liveStats(op *Operation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.operations[op.ID]
	if active == nil {
		return
	}
	for i, t := range op.Targets {
		for _, live := range active.op.Targets {
			if live.PeerID == t.PeerID && live.Generation == t.Generation && live.Stats != nil {
				op.Targets[i].Stats = live.Stats
			}
		}
	}
}
func (s *Server) Stop(ctx context.Context, owner, id string, revision int64) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.operations[id]
	if a == nil {
		op, err := s.repo.Get(ctx, id)
		if err == nil && op.Owner != owner {
			return Operation{}, ErrForbidden
		}
		if err == nil && op.Revision != revision {
			return Operation{}, ErrPrecondition
		}
		if err == nil && (op.State == "starting" || op.State == "active" || op.State == "stopping") {
			previous := op.Revision
			op.State = "interrupted"
			op.Reason = "stop_unconfirmed"
			op.Revision++
			err = s.repo.Save(ctx, op, previous)
		}
		return op, err
	}
	if a.op.Owner != owner {
		return Operation{}, ErrForbidden
	}
	if a.op.Revision != revision {
		return Operation{}, ErrPrecondition
	}
	if a.op.Reason == "" {
		a.op.Reason = "requested_stop"
	}
	err := s.stopLocked(a, "stopping")
	return cloneOperation(a.op), err
}

func cloneOperation(op Operation) Operation {
	op.Targets = append([]Target(nil), op.Targets...)
	op.Hash = append([]byte(nil), op.Hash...)
	if op.ActingFor != nil {
		acting := *op.ActingFor
		op.ActingFor = &acting
	}
	return op
}
func identityKey(credentialID string, i Identity) string {
	b, _ := json.Marshal([]string{credentialID, i.Cluster, i.Namespace, i.Pod, i.Group})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func validIdentity(i Identity, c credential) bool {
	if !safeName(i.Pod) || !safeName(i.Group) || !c.allows(i.Group) || len(i.Cluster) > 128 || len(i.Namespace) > 128 || len(i.CollectorVersion) > 128 || len(i.DistributionVersion) > 128 || len(i.Points) > 192 {
		return false
	}
	seen := map[string]bool{}
	for _, p := range i.Points {
		key := p.Pipeline + "/" + p.Point + "/" + p.Signal
		if !safeName(p.Pipeline) || !safeName(p.Point) || !slices.Contains([]string{"traces", "logs", "metrics"}, p.Signal) || !slices.Contains([]string{"raw", "redacted"}, p.Sensitivity) || seen[key] {
			return false
		}
		seen[key] = true
	}
	return !strings.ContainsAny(i.Cluster+i.Namespace, "\r\n\x00")
}
