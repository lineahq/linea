package livecapture

import (
	"context"
	"crypto/sha256"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lineahq/linea/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Collectors present their fleet agent credential, unchanged, on the control
// and delivery streams. The plugin hashes it, asks the fleet host to
// introspect the hash and caches the answer briefly. The credential's capture
// label lists the collector groups it may serve ("*" allows any group).
const (
	credentialTTL        = 10 * time.Second
	negativeTTL          = 2 * time.Second
	maxCachedCredentials = 1024
	maxLinksPerPass      = 64
	linkedRefresh        = time.Minute
	linkBackoffMin       = 2 * time.Second
	linkBackoffMax       = time.Minute
	linkPassGap          = time.Second
)

type credential struct {
	id     string
	hash   [32]byte
	groups map[string]bool
	all    bool
}

func (c credential) allows(group string) bool { return c.all || c.groups[group] }

type cachedCredential struct {
	credential credential
	admitted   bool
	until      time.Time
}

// admit maps an introspection onto a capture credential. Active agent
// credentials without a valid capture label are not admitted.
func (s *Server) admit(hash [32]byte, in pluginsdk.Introspection, now time.Time) cachedCredential {
	entry := cachedCredential{until: now.Add(negativeTTL)}
	if !in.Active || in.Role != "agent" || !pluginsdk.ValidID(in.CredentialID) {
		return entry
	}
	value, ok := in.Labels[s.config.captureLabel()]
	if !ok {
		return entry
	}
	c := credential{id: in.CredentialID, hash: hash, groups: map[string]bool{}}
	for _, group := range strings.Split(value, ",") {
		group = strings.TrimSpace(group)
		switch {
		case group == "*":
			c.all = true
		case safeName(group):
			c.groups[group] = true
		default:
			return entry
		}
	}
	if !c.all && len(c.groups) == 0 {
		return entry
	}
	entry.credential, entry.admitted, entry.until = c, true, now.Add(credentialTTL)
	if in.ExpiresAt != nil && in.ExpiresAt.Before(entry.until) {
		entry.until = *in.ExpiresAt
	}
	return entry
}

func (s *Server) cacheLocked(hash [32]byte, entry cachedCredential, now time.Time) {
	if _, ok := s.credentials[hash]; !ok && len(s.credentials) >= maxCachedCredentials {
		for h, e := range s.credentials {
			if !now.Before(e.until) {
				delete(s.credentials, h)
			}
		}
		if len(s.credentials) >= maxCachedCredentials {
			return
		}
	}
	s.credentials[hash] = entry
}

func (s *Server) introspect(ctx context.Context, session pluginsdk.Session, hash [32]byte) (cachedCredential, error) {
	in, err := s.host.IntrospectHash(ctx, session, hash)
	if err != nil {
		return cachedCredential{}, err
	}
	now := time.Now()
	entry := s.admit(hash, in, now)
	s.mu.Lock()
	s.cacheLocked(hash, entry, now)
	s.mu.Unlock()
	return entry, nil
}

func (s *Server) authenticate(ctx context.Context) (credential, error) {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) > 1024 {
		return credential{}, status.Error(codes.Unauthenticated, "fleet agent credential required")
	}
	hash := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
	s.mu.Lock()
	entry, cached := s.credentials[hash]
	session, ready := s.session, s.hostOK && s.dbOK
	s.mu.Unlock()
	if !ready {
		return credential{}, status.Error(codes.Unavailable, "capture authority unavailable")
	}
	if !cached || !time.Now().Before(entry.until) {
		var err error
		if entry, err = s.introspect(ctx, session, hash); err != nil {
			return credential{}, status.Error(codes.Unavailable, "capture authority unavailable")
		}
	}
	if !entry.admitted {
		return credential{}, status.Error(codes.Unauthenticated, "fleet agent credential inactive or not allowed to capture")
	}
	return entry.credential, nil
}

type linkCandidate struct {
	id         string
	generation uint64
	credential string
	identity   Identity
	tried      time.Time
}

// recheckSchedule decides when credentials are introspected again. It holds a
// deadline instead of counting ticks, so a link wakeup or a timer that fires
// slightly early neither skips a round nor moves the next one: a recheck runs
// at most revalidateEvery after the previous one started.
type recheckSchedule struct {
	every time.Duration
	next  time.Time
}

func newRecheckSchedule(now time.Time, every time.Duration) *recheckSchedule {
	return &recheckSchedule{every: every, next: now.Add(every)}
}

func (r *recheckSchedule) due(now time.Time) bool { return !now.Before(r.next) }

// started records a recheck beginning at now.
func (r *recheckSchedule) started(now time.Time) { r.next = now.Add(r.every) }

// wait is the time until the next recheck, or zero when it is due.
func (r *recheckSchedule) wait(now time.Time) time.Duration { return max(r.next.Sub(now), 0) }

// revalidateRetry is how long the loop waits when a recheck is due but the
// host is not ready, so a host outage does not spin it.
const revalidateRetry = 500 * time.Millisecond

// revalidate re-introspects connected peers' credentials, closing peers whose
// credential was revoked, expired or lost the capture label, and links peers
// to fleet agents.
func (s *Server) revalidate() {
	defer s.wg.Done()
	schedule := newRecheckSchedule(s.now(), s.revalidateEvery)
	wait := s.revalidateEvery
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		case <-s.linkWake:
		}
		wait = s.revalidatePass(schedule)
	}
}

// revalidatePass runs one loop round and returns how long to wait for the next
// scheduled recheck or due link attempt, whichever is first.
func (s *Server) revalidatePass(schedule *recheckSchedule) time.Duration {
	s.mu.Lock()
	session, ready := s.session, s.hostOK
	s.mu.Unlock()
	var link time.Duration
	if ready {
		if now := s.now(); schedule.due(now) {
			schedule.started(now)
			s.recheckCredentials(session)
		}
		link = s.linkPeers(session)
	}
	wait := schedule.wait(s.now())
	if wait <= 0 {
		return revalidateRetry
	}
	if link > 0 {
		wait = min(wait, link)
	}
	return wait
}

func (s *Server) recheckCredentials(session pluginsdk.Session) {
	s.mu.Lock()
	hashes := map[[32]byte]bool{}
	for _, p := range s.peers {
		hashes[p.credential] = true
	}
	s.mu.Unlock()
	results := map[[32]byte]cachedCredential{}
	for hash := range hashes {
		entry, err := s.introspect(s.ctx, session, hash)
		if err != nil {
			// An unreachable host keeps peers; captures already stop with host authority.
			continue
		}
		results[hash] = entry
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if entry, ok := results[p.credential]; ok && (!entry.admitted || !entry.credential.allows(p.info.Identity.Group)) {
			s.observed.credentialClosed.Add(1)
			p.close()
		}
	}
}

// Link states shown on a peer. A peer is pending until an attempt finishes
// without a host error; a host error leaves the state as it was.
const (
	linkPending   = "pending"
	linkLinked    = "linked"
	linkNoMatch   = "no_match"
	linkAmbiguous = "ambiguous"
)

// linkOutcomes are the results of one resolve attempt, each with a counter.
// An attempt that fails on the host call has no state of its own.
var linkOutcomes = [...]string{linkLinked, linkNoMatch, linkAmbiguous, "error"}

func linkOutcomeIndex(outcome string) int {
	for i, name := range linkOutcomes {
		if name == outcome {
			return i
		}
	}
	return len(linkOutcomes) - 1
}

// linkBackoff is the wait after the failures-th consecutive attempt that did
// not link: 2 s, doubling to 60 s.
func linkBackoff(failures int) time.Duration {
	wait := linkBackoffMin
	for i := 1; i < failures && wait < linkBackoffMax; i++ {
		wait *= 2
	}
	return min(wait, linkBackoffMax)
}

// linkOrder puts the peers whose last attempt is oldest first, never-tried
// peers before all others, so one pass's cut cannot favour the same peers.
func linkOrder(candidates []linkCandidate) {
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !a.tried.Equal(b.tried) {
			return a.tried.Before(b.tried)
		}
		return a.id < b.id
	})
}

// linkPeers links one pass of due peers to fleet agents and returns how long
// until the next peer is due (zero when no peers are connected). Candidates
// are the due peers with the oldest last attempt, at most maxLinksPerPass. An
// attempt on any outcome, a host error included, moves the peer's last attempt
// and next due time, so no peer is retried ahead of the others.
func (s *Server) linkPeers(session pluginsdk.Session) time.Duration {
	now := s.now()
	s.mu.Lock()
	candidates := []linkCandidate{}
	for _, p := range s.peers {
		if !now.Before(p.linkNext) {
			candidates = append(candidates, linkCandidate{id: p.info.ID, generation: p.info.Generation, credential: p.info.CredentialID, identity: p.info.Identity, tried: p.linkTried})
		}
	}
	s.mu.Unlock()
	linkOrder(candidates)
	if len(candidates) > maxLinksPerPass {
		candidates = candidates[:maxLinksPerPass]
	}
	for _, c := range candidates {
		agent, outcome := s.resolveAgent(session, c)
		s.recordLink(c, agent, outcome, s.now())
	}
	return s.nextLinkWait()
}

// recordLink applies one attempt's outcome to its peer, if that peer
// generation is still connected.
func (s *Server) recordLink(c linkCandidate, agent, outcome string, now time.Time) {
	s.observed.linkOutcomes[linkOutcomeIndex(outcome)].Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[c.id]
	if p == nil || p.info.Generation != c.generation {
		return
	}
	p.linkTried = now
	if outcome == linkLinked {
		p.info.AgentID, p.info.Link.State = agent, linkLinked
		p.linkFailures, p.linkNext = 0, now.Add(linkedRefresh)
		return
	}
	if outcome == linkNoMatch || outcome == linkAmbiguous {
		p.info.AgentID, p.info.Link.State = "", outcome
	}
	p.linkFailures++
	p.linkNext = now.Add(linkBackoff(p.linkFailures))
}

// nextLinkWait is the time until the earliest peer is due, at least
// linkPassGap so a backlog beyond one pass does not spin the loop, or zero
// when there are no peers.
func (s *Server) nextLinkWait() time.Duration {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var earliest time.Time
	for _, p := range s.peers {
		if earliest.IsZero() || p.linkNext.Before(earliest) {
			earliest = p.linkNext
		}
	}
	if earliest.IsZero() {
		return 0
	}
	return max(earliest.Sub(now), linkPassGap)
}

// resolveAgent links a capture peer to the one connected fleet agent that
// used the same credential and reports the same Kubernetes identity. It links
// only a unique match; host.name equal to the pod name is the fallback. The
// outcome is one of linkLinked, linkNoMatch, linkAmbiguous or "error".
func (s *Server) resolveAgent(session pluginsdk.Session, c linkCandidate) (string, string) {
	attributes := map[string]string{"k8s.pod.name": c.identity.Pod}
	if c.identity.Namespace != "" {
		attributes["k8s.namespace.name"] = c.identity.Namespace
	}
	if c.identity.Cluster != "" {
		attributes["k8s.cluster.name"] = c.identity.Cluster
	}
	for _, terms := range []map[string]string{attributes, {"host.name": c.identity.Pod}} {
		out, err := s.host.ResolveAgents(s.ctx, session, pluginsdk.ResolveRequest{CredentialID: c.credential, Attributes: terms, Limit: 2})
		if err != nil {
			return "", "error"
		}
		if len(out.AgentIDs) == 1 && !out.More {
			return out.AgentIDs[0], linkLinked
		}
		if len(out.AgentIDs) > 1 {
			return "", linkAmbiguous
		}
	}
	return "", linkNoMatch
}

func (s *Server) wakeLinker() {
	select {
	case s.linkWake <- struct{}{}:
	default:
	}
}

func hasPermissions(session pluginsdk.Session) bool {
	return slices.Contains(session.Permissions, pluginsdk.AgentsRead) && slices.Contains(session.Permissions, pluginsdk.CredentialsIntrospect)
}
