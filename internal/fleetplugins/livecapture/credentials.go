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
	unlinkedRetry        = 2 * time.Second
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
	if s.repo.Ready(ctx) != nil {
		return credential{}, status.Error(codes.Unavailable, "capture authority unavailable")
	}
	return entry.credential, nil
}

type linkCandidate struct {
	id         string
	generation uint64
	credential string
	identity   Identity
}

// revalidate re-introspects connected peers' credentials, closing peers whose
// credential was revoked, expired or lost the capture label, and links peers
// to fleet agents.
func (s *Server) revalidate() {
	defer s.wg.Done()
	tick := time.NewTicker(s.revalidateEvery)
	defer tick.Stop()
	lastIntrospection := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		case <-s.linkWake:
		}
		s.mu.Lock()
		session, ready := s.session, s.hostOK
		s.mu.Unlock()
		if !ready {
			continue
		}
		if time.Since(lastIntrospection) >= s.revalidateEvery {
			lastIntrospection = time.Now()
			s.recheckCredentials(session)
		}
		s.linkPeers(session)
	}
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

func (s *Server) linkPeers(session pluginsdk.Session) {
	now := time.Now()
	s.mu.Lock()
	candidates := []linkCandidate{}
	for _, p := range s.peers {
		due := p.linkedAt.IsZero() || (p.info.AgentID == "" && now.Sub(p.linkedAt) >= unlinkedRetry) || now.Sub(p.linkedAt) >= linkedRefresh
		if due {
			candidates = append(candidates, linkCandidate{id: p.info.ID, generation: p.info.Generation, credential: p.info.CredentialID, identity: p.info.Identity})
		}
	}
	s.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].id < candidates[j].id })
	if len(candidates) > maxLinksPerPass {
		candidates = candidates[:maxLinksPerPass]
	}
	for _, c := range candidates {
		agent, ok := s.resolveAgent(session, c)
		if !ok {
			continue
		}
		s.mu.Lock()
		if p := s.peers[c.id]; p != nil && p.info.Generation == c.generation {
			p.info.AgentID, p.linkedAt = agent, time.Now()
		}
		s.mu.Unlock()
	}
}

// resolveAgent links a capture peer to the one connected fleet agent that
// used the same credential and reports the same Kubernetes identity. It links
// only a unique match; host.name equal to the pod name is the fallback.
func (s *Server) resolveAgent(session pluginsdk.Session, c linkCandidate) (string, bool) {
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
			return "", false
		}
		if len(out.AgentIDs) == 1 && !out.More {
			return out.AgentIDs[0], true
		}
		if len(out.AgentIDs) > 1 {
			return "", true
		}
	}
	return "", true
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
