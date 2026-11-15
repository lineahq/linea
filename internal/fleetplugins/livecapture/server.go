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

	"github.com/lineahq/linea/internal/fleetplugins/livecapture/translog"
	"github.com/lineahq/linea/pluginsdk"
)

type Repository interface {
	AuthorityID() string
	Ready(context.Context) error
	NextGeneration(context.Context, string) (uint64, error)
	// MaintainPeers marks the connected peers as seen and deletes at most limit
	// peers unused for idle (see Store.MaintainPeers).
	MaintainPeers(ctx context.Context, connected []string, idle time.Duration, limit int) (int, error)
	Get(context.Context, string) (Operation, error)
	Insert(context.Context, Operation) error
	Save(context.Context, Operation, int64) error
}
type activeOperation struct {
	op      Operation
	request CreateRequest
	raw     bool
	// logged is the state the transition log last reported.
	logged translog.State
	// inserting holds the operation back from its peers until the writer
	// inserted it.
	inserting bool
	inserted  chan struct{}
}
type Server struct {
	mu           sync.Mutex
	config       Config
	repo         Repository
	ctx          context.Context
	cancel       context.CancelFunc
	instance     string
	host         pluginsdk.HostClient
	epoch        string
	session      pluginsdk.Session
	hostOK, dbOK bool
	// hostMisses counts consecutive session polls that got no answer.
	hostMisses int
	// storageFailures counts consecutive failed pings; only maintain changes it.
	storageFailures int
	permissionsOK   bool
	draining        bool
	peers           map[string]*peer
	// joining holds the peer identities whose generation is being minted.
	joining         map[string]bool
	operations      map[string]*activeOperation
	channels        map[string]*channel
	credentials     map[[32]byte]cachedCredential
	linkWake        chan struct{}
	revalidateEvery time.Duration
	// peerIdle is how long a peer may go unused before the database row is deleted.
	peerIdle time.Duration
	// pending is the bytes buffered across all channels.
	pending int
	// now is the clock of the delivery buffer; tests replace it.
	now      func() time.Time
	fatal    chan error
	wg       sync.WaitGroup
	observed pluginMetrics
	budgets  [3]*connectionBudget
	// log is the transition log (nil logs nothing) and authFailures counts
	// rejected credentials per listener for its summary lines.
	log          *translog.Logger
	authFailures *translog.FailureSummary
	// saves is the writer's queue (persist.go); persistDone closes when the
	// writer stopped.
	saves       *saveQueue
	persistDone chan struct{}
}

// New returns a server over repo. log receives the transition log; nil logs
// nothing.
func New(ctx context.Context, c Config, repo Repository, log *translog.Logger) (*Server, error) {
	if err := repo.Ready(ctx); err != nil {
		return nil, WithCause(translog.ReasonStorageUnavailable, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Server{config: c, repo: repo, ctx: ctx, cancel: cancel, instance: pluginsdk.RandomID(), dbOK: true, peers: map[string]*peer{}, joining: map[string]bool{}, operations: map[string]*activeOperation{}, channels: map[string]*channel{}, credentials: map[[32]byte]cachedCredential{}, linkWake: make(chan struct{}, 1), revalidateEvery: credentialTTL, peerIdle: peerRetention, now: time.Now, fatal: make(chan error, 1), log: log, authFailures: translog.NewFailureSummary(log, translog.SummaryEvery, time.Now())}
	s.host = pluginsdk.HostClient{Endpoint: c.HostEndpoint, Token: c.hostToken, InstanceID: s.instance, HTTP: pluginsdk.NewHTTPClient(), SessionHTTP: pluginsdk.NewHTTPClient()}
	for i, limit := range []int{c.peerLimit() + 16, 64, 64} {
		s.budgets[i] = &connectionBudget{slots: make(chan struct{}, limit)}
	}
	s.saves, s.persistDone = newSaveQueue(), make(chan struct{})
	s.wg.Add(1)
	go func() { defer close(s.persistDone); s.persist() }()
	return s, nil
}
func (s *Server) Start() { s.wg.Add(3); go s.monitorHost(); go s.maintain(); go s.revalidate() }
func (s *Server) Drain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		s.log.Log(translog.EventDrainStarted)
	}
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
		s.retirePeerLocked(p, translog.ReasonShutdown)
	}
	for _, c := range s.channels {
		s.closeChannelLocked(c)
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.authFailures.Flush(time.Now())
	s.host.HTTP.CloseIdleConnections()
	s.host.SessionHTTP.CloseIdleConnections()
}
func (s *Server) Fatal() <-chan error { return s.fatal }

// fatalLocked ends the plugin: it withdraws the database flag and sends the
// cause, once, to Run. Only the first cause is logged.
func (s *Server) fatalLocked(cause translog.Reason) {
	s.dbOK = false
	select {
	case s.fatal <- WithCause(cause, ErrUnavailable):
		s.log.Log(translog.EventPluginFatal, translog.Cause(cause))
	default:
	}
}

// retirePeerLocked closes p and remembers why, unless it was closed before.
func (s *Server) retirePeerLocked(p *peer, reason translog.Reason) {
	if p.closeReason == translog.ReasonNone {
		p.closeReason = reason
	}
	p.close()
}

// logOperation reports one operation state transition.
func (s *Server) logOperation(id string, from, to translog.State, reason string) {
	s.log.Log(translog.EventOperationState, translog.Operation(id), translog.From(from), translog.To(to), translog.Why(translog.ParseReason(reason)))
}
func (s *Server) stopLocked(a *activeOperation, state string) {
	if a.op.State != "starting" && a.op.State != "active" && a.op.State != "stopping" {
		return
	}
	if a.op.State != state {
		if state == "stopping" {
			s.observed.withdrawn.Add(1)
		}
		a.op.State = state
		if state == "expired" {
			expireTargets(a.op.Targets)
		}
		s.saveLocked(a)
	}
	for _, t := range a.op.Targets {
		if p := s.peers[t.PeerID]; p != nil && p.info.Generation == t.Generation && p.operation == a.op.ID {
			p.operation = ""
			p.wakeup()
		}
	}
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
		s.stopLocked(a, state)
	}
}
func (s *Server) monitorHost() {
	defer s.wg.Done()
	for {
		s.checkHost()
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// hostMissLimit is how many session polls in a row may get no answer (timeout,
// refused or broken connection) before the session is lost. An answer ends it
// at once, a refusal such as 421 or a changed epoch included: the host has
// spoken, and waiting cannot help. A silent host may only be one busy moment,
// and a lost capture, though cheap, is not worth one.
const hostMissLimit = 2

// checkHost reads the host session once and applies it: it ends host-bound work
// when the session is lost or its epoch changed, and adopts a usable session.
// The poll has its own HTTP client, so nothing else the plugin asks of the
// host can make it time out. The transitions are logged after the lock is
// released.
func (s *Server) checkHost() {
	session, err := s.host.Session(s.ctx)
	s.mu.Lock()
	if pluginsdk.Unreachable(err) && s.hostOK && s.ctx.Err() == nil {
		s.hostMisses++
		if s.hostMisses < hostMissLimit {
			s.mu.Unlock()
			return
		}
	}
	s.hostMisses = 0
	// unusable is why the session cannot serve, none when it can.
	unusable := translog.ReasonNone
	if err != nil {
		unusable = translog.ReasonHostUnreachable
	}
	// Without both host permissions Collectors cannot be authenticated or linked.
	s.permissionsOK = err == nil && hasPermissions(session)
	if err == nil && !s.permissionsOK {
		err = ErrUnavailable
		unusable = translog.ReasonPermissionsMissing
	}
	if err == nil && s.epoch != "" && s.epoch != session.Epoch {
		unusable = translog.ReasonEpochChanged
	}
	lost, established := translog.ReasonNone, false
	if unusable != translog.ReasonNone {
		if s.hostOK {
			lost = unusable
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
		established = !s.hostOK
		s.epoch, s.session = session.Epoch, session
		s.hostOK = true
	}
	instance, epoch := s.instance, s.epoch
	s.mu.Unlock()
	if lost != translog.ReasonNone {
		s.log.Log(translog.EventHostSessionLost, translog.Why(lost))
	}
	if established {
		s.log.Log(translog.EventHostSessionEstablished, translog.Instance(instance), translog.Epoch(epoch))
	}
}

// storageFailureLimit is how many consecutive failed database pings, sampled
// once a second, end the process. A ping that finds the connection closed ends
// it at once instead: the driver never reconnects, so waiting cannot help.
// A busy connection is no sample. A lost capture is cheap, a plugin exit ends
// every capture, so one slow answer must not be fatal.
const storageFailureLimit = 3

// observeStorageLocked records one probe result and reports whether it makes
// the database fatal. Only maintain calls it: every other path reads dbOK.
func (s *Server) observeStorageLocked(err error) bool {
	switch {
	case err == nil:
		if s.storageFailures > 0 {
			s.log.Log(translog.EventDatabaseRecovered, translog.Count(s.storageFailures))
		}
		s.storageFailures = 0
	case errors.Is(err, ErrStorageBusy):
	case errors.Is(err, ErrStorageLost):
		return true
	default:
		s.storageFailures++
		if s.storageFailures == 1 {
			s.log.Log(translog.EventDatabaseDegraded)
		}
		return s.storageFailures >= storageFailureLimit
	}
	return false
}

// maintain owns database liveness: it probes once a second and keeps dbOK.
// Requests, deliveries and control wakes read the flag and never ping.
func (s *Server) maintain() {
	defer s.wg.Done()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	lastDB, lastPeers := time.Time{}, time.Time{}
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-tick.C:
			s.authFailures.Tick(now)
			if now.Sub(lastDB) >= time.Second {
				err := s.repo.Ready(s.ctx)
				s.mu.Lock()
				if s.observeStorageLocked(err) {
					cause := translog.ReasonStorageFailures
					if errors.Is(err, ErrStorageLost) {
						cause = translog.ReasonStorageLost
					}
					s.log.Log(translog.EventDatabaseLost, translog.Why(cause))
					s.fatalLocked(cause)
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
					s.retirePeerLocked(p, translog.ReasonPeerStale)
				}
			}
			for id, a := range s.operations {
				if !now.Before(a.op.ExpiresAt) {
					if a.op.Reason == "" {
						a.op.Reason = "absolute_expiry"
					}
					s.stopLocked(a, "expired")
					delete(s.operations, id)
					continue
				}
				if a.op.State == "starting" || a.op.State == "active" {
					if c := s.channels[a.op.ChannelID]; c == nil || now.Sub(c.lastSeen) > 5*time.Second {
						if a.op.Reason == "" {
							a.op.Reason = "consumer_unavailable"
						}
						s.stopLocked(a, "stopping")
					}
				}
			}
			s.mu.Unlock()
			if now.Sub(lastPeers) >= peerMaintenanceEvery {
				lastPeers = now
				// A full batch means more peers wait: run again on the next tick.
				if s.retirePeers() >= peerPruneBatch {
					lastPeers = time.Time{}
				}
			}
		}
	}
}

// retirePeers refreshes the connected peers' last-seen time and deletes unused
// peers in the database. A failure is counted by the store and retried at the
// next pass; it never ends the plugin, because the peer table only matters
// when it is full.
func (s *Server) retirePeers() int {
	s.mu.Lock()
	connected := make([]string, 0, len(s.peers))
	for id := range s.peers {
		connected = append(connected, id)
	}
	s.mu.Unlock()
	deleted, err := s.repo.MaintainPeers(s.ctx, connected, s.peerIdle, peerPruneBatch)
	if err != nil {
		return 0
	}
	return deleted
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
		return Operation{}, ErrStaleAuthority
	}
	if request.AuthorityID != s.repo.AuthorityID() || request.PluginInstance != s.instance {
		return Operation{}, ErrConflict
	}
	// A concurrent identical create answers like a replay: it waits for the
	// first one's insert and returns the same operation.
	for {
		a := s.operations[request.ID]
		if a == nil || !a.inserting {
			break
		}
		inserted := a.inserted
		s.mu.Unlock()
		<-inserted
		s.mu.Lock()
	}
	if known, ok := s.knownLocked(request.ID); ok {
		if known.Owner != owner {
			return Operation{}, ErrForbidden
		}
		if subtle.ConstantTimeCompare(known.Hash, hash[:]) != 1 {
			return Operation{}, ErrConflict
		}
		return known, nil
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
	// The operation holds its peers and its slot while the writer inserts it,
	// but no peer learns of it before the insert succeeded.
	a := &activeOperation{op: op, request: request, raw: raw, logged: translog.StateStarting, inserting: true, inserted: make(chan struct{})}
	s.operations[op.ID] = a
	for _, target := range op.Targets {
		s.peers[target.PeerID].operation = op.ID
	}
	done := s.insertLocked(op)
	s.mu.Unlock()
	var insertErr error
	select {
	case insertErr = <-done:
	case <-s.persistDone:
		select {
		case insertErr = <-done:
		default:
			insertErr = ErrUnavailable
		}
	}
	s.mu.Lock()
	a.inserting = false
	close(a.inserted)
	if insertErr != nil {
		delete(s.operations, op.ID)
		for _, t := range a.op.Targets {
			if p := s.peers[t.PeerID]; p != nil && p.operation == op.ID {
				p.operation = ""
			}
		}
		return Operation{}, insertErr
	}
	s.observed.admitted.Add(1)
	if ctx.Err() != nil && a.op.State == "starting" {
		a.op.State = "interrupted"
		a.op.Reason = "admission_cancelled"
		s.saveLocked(a)
		delete(s.operations, op.ID)
		for _, t := range a.op.Targets {
			if p := s.peers[t.PeerID]; p != nil && p.operation == op.ID {
				p.operation = ""
			}
		}
		return Operation{}, ErrUnavailable
	}
	for _, t := range a.op.Targets {
		if p := s.peers[t.PeerID]; p != nil && p.operation == op.ID {
			p.wakeup()
		}
	}
	return cloneOperation(a.op), nil
}

// knownLocked is the decided state of an operation this authority holds in
// memory or has not finished writing; the database may still lag behind it.
func (s *Server) knownLocked(id string) (Operation, bool) {
	if a := s.operations[id]; a != nil {
		return cloneOperation(a.op), true
	}
	return s.saves.latest(id)
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
	// The decided state goes first: the writer may not have written it yet.
	s.mu.Lock()
	op, known := s.knownLocked(id)
	s.mu.Unlock()
	var err error
	if !known {
		op, err = s.repo.Get(ctx, id)
	}
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
			s.stopLocked(active, "expired")
			return cloneOperation(active.op), nil
		}
		// Unlocked before liveStats below, which takes s.mu again.
		s.mu.Unlock()
		previous, was := op.Revision, translog.ParseState(op.State)
		op.State = "expired"
		op.Reason = "absolute_expiry"
		expireTargets(op.Targets)
		op.Revision++
		if err := s.repo.Save(ctx, op, previous); err != nil {
			return s.repo.Get(ctx, id)
		}
		s.logOperation(op.ID, was, translog.StateExpired, op.Reason)
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
			if live.PeerID == t.PeerID && live.Generation == t.Generation {
				if live.Stats != nil {
					op.Targets[i].Stats = live.Stats
				}
				op.Targets[i].Delivery = live.Delivery
			}
		}
	}
}
func (s *Server) Stop(ctx context.Context, owner, id string, revision int64) (Operation, error) {
	s.mu.Lock()
	a := s.operations[id]
	if a == nil {
		// An operation no longer in memory is ended. Its final state may still
		// wait for the writer; otherwise it is read and written outside the
		// lock.
		op, queued := s.saves.latest(id)
		s.mu.Unlock()
		var err error
		if !queued {
			op, err = s.repo.Get(ctx, id)
		}
		if err == nil && op.Owner != owner {
			return Operation{}, ErrForbidden
		}
		if err == nil && op.Revision != revision {
			return Operation{}, ErrPrecondition
		}
		if err == nil && !queued && (op.State == "starting" || op.State == "active" || op.State == "stopping") {
			previous, was := op.Revision, translog.ParseState(op.State)
			op.State = "interrupted"
			op.Reason = "stop_unconfirmed"
			op.Revision++
			if err = s.repo.Save(ctx, op, previous); err == nil {
				s.logOperation(op.ID, was, translog.StateInterrupted, op.Reason)
			}
		}
		return op, err
	}
	defer s.mu.Unlock()
	if a.op.Owner != owner {
		return Operation{}, ErrForbidden
	}
	if a.op.Revision != revision {
		return Operation{}, ErrPrecondition
	}
	if a.op.Reason == "" {
		a.op.Reason = "requested_stop"
	}
	s.stopLocked(a, "stopping")
	return cloneOperation(a.op), nil
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

const (
	// peerRetention is how long a peer with no lease and no retained operation
	// may go unused before the plugin deletes its database row.
	peerRetention = 24 * time.Hour
	// peerMaintenanceEvery is the period of the last-seen refresh and the delete.
	peerMaintenanceEvery = time.Minute
	// peerPruneBatch bounds the peers deleted in one pass.
	peerPruneBatch = 500
)
