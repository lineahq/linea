package configmgmt

import (
	"bytes"
	"container/list"
	"context"
	"encoding/hex"
	"errors"
	"sync"
)

// DefaultBodyCacheBytes bounds the version bodies a replica keeps in memory.
const DefaultBodyCacheBytes = 64 << 20

// AcceptsRemoteConfig is the OpAMP agent capability bit for remote config.
const AcceptsRemoteConfig = 0x2

// ErrNotLoaded means desired state has not been read yet; nothing is sent.
var ErrNotLoaded = errors.New("desired state not loaded")

// Source reads desired state for the reconciler.
type Source interface {
	Targets(ctx context.Context) ([]Target, error)
	GetVersion(ctx context.Context, configID string, version int, body bool) (Version, error)
	// ManagedCredentials returns the labels of every active agent credential
	// with remote_config enabled, keyed by credential ID.
	ManagedCredentials(ctx context.Context) (map[string]map[string]string, error)
}

// Session is everything the send rule reads about one agent session.
type Session struct {
	AgentID      string
	Credential   string
	Attributes   map[string]string
	Capabilities uint64
	Status       Status
	Held         []byte
	// Components is the agent's reported build; nil skips the
	// compatibility check.
	Components Components
}

// Reconciler applies the send rule from cached desired state. It is the
// only code that decides to send remote config; the gateway holds none
// unless config management is enabled.
type Reconciler struct {
	src    Source
	mu     sync.RWMutex
	loaded bool
	// targets and credentials are replaced whole on refresh, never mutated.
	targets     []Target
	credentials map[string]map[string]string
	bodies      *bodyCache
	uses        *UsesCache
}

// NewReconciler caches up to cacheBytes of version bodies.
func NewReconciler(src Source, cacheBytes int64) *Reconciler {
	return &Reconciler{src: src, bodies: newBodyCache(cacheBytes), uses: NewUsesCache()}
}

// Refresh rereads assignments and managed credentials. On error the previous
// state stays in use.
func (r *Reconciler) Refresh(ctx context.Context) error {
	targets, err := r.src.Targets(ctx)
	if err != nil {
		return err
	}
	credentials, err := r.src.ManagedCredentials(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.targets, r.credentials, r.loaded = targets, credentials, true
	r.mu.Unlock()
	return nil
}

// Reconcile applies the send rule to one session. A decision to send carries
// the entries with their bodies. Ownership of the session is checked by the
// caller.
func (r *Reconciler) Reconcile(ctx context.Context, s Session) (Decision, error) {
	r.mu.RLock()
	loaded, targets := r.loaded, r.targets
	labels, allowed := r.credentials[s.Credential]
	r.mu.RUnlock()
	if !loaded {
		return Decision{}, ErrNotLoaded
	}
	agent := Agent{Enabled: true, CredentialAllowed: allowed, AcceptsRemoteConfig: s.Capabilities&AcceptsRemoteConfig != 0, Status: s.Status, Held: s.Held}
	if !agent.CredentialAllowed || !agent.AcceptsRemoteConfig {
		return Decision{}, nil
	}
	matched := Match(targets, s.AgentID, s.Attributes, labels)
	if len(matched) == 0 {
		return Decide(agent, Plan{}, false), nil
	}
	desired, err := Describe(matched, "")
	if err != nil {
		return Decision{}, err
	}
	hash, _ := hex.DecodeString(desired.Hash)
	d := Decide(agent, Plan{Hash: hash}, true)
	if !d.Send {
		return d, nil
	}
	// Bodies are read only for a send; the hash came from stored digests.
	d.Plan.Entries = make([]Entry, len(matched))
	for i, t := range matched {
		body, err := r.body(ctx, t)
		if err != nil {
			return Decision{}, err
		}
		d.Plan.Entries[i] = Entry{Name: desired.Layers[i].Name, Body: body}
	}
	if s.Components != nil {
		uses := Components{}
		for i, t := range matched {
			body := d.Plan.Entries[i].Body
			used, _ := r.uses.Get(t.ConfigID, t.Version, func() ([]byte, error) { return body, nil })
			uses = uses.Merge(used)
		}
		if missing := s.Components.Missing(uses); len(missing) > 0 {
			d.Send, d.Plan.Entries, d.Missing = false, nil, missing
		}
	}
	return d, nil
}

// maxUses bounds the cached component types; the cache restarts when full.
const maxUses = 4096

// UsesCache keeps the component types of immutable versions.
type UsesCache struct {
	mu   sync.Mutex
	uses map[versionKey]Components
}

func NewUsesCache() *UsesCache { return &UsesCache{uses: map[versionKey]Components{}} }

// Get returns the types a version uses, reading its body with load on a miss.
func (u *UsesCache) Get(configID string, version int, load func() ([]byte, error)) (Components, error) {
	key := versionKey{configID, version}
	u.mu.Lock()
	c, ok := u.uses[key]
	u.mu.Unlock()
	if ok {
		return c, nil
	}
	body, err := load()
	if err != nil {
		return nil, err
	}
	c = Uses(body)
	u.mu.Lock()
	if len(u.uses) >= maxUses {
		clear(u.uses)
	}
	u.uses[key] = c
	u.mu.Unlock()
	return c, nil
}

func (r *Reconciler) body(ctx context.Context, t Target) ([]byte, error) {
	key := versionKey{t.ConfigID, t.Version}
	return r.bodies.get(key, func() ([]byte, error) {
		v, err := r.src.GetVersion(ctx, t.ConfigID, t.Version, true)
		if err != nil {
			return nil, err
		}
		if v.Body == nil {
			return nil, errors.New("version body missing")
		}
		body := []byte(*v.Body)
		// Versions are immutable; a mismatch means the cache or the store
		// is inconsistent, and sending would break the hash contract.
		if !bytes.Equal(digestOf(body), t.Digest) {
			return nil, errors.New("version body does not match its digest")
		}
		return body, nil
	})
}

// CachedBodyBytes reports the size of cached version bodies.
func (r *Reconciler) CachedBodyBytes() int64 { return r.bodies.size() }

type versionKey struct {
	config  string
	version int
}

// bodyCache is a byte-bounded LRU of immutable version bodies. Concurrent
// misses for one key share a single load.
type bodyCache struct {
	mu       sync.Mutex
	limit    int64
	bytes    int64
	order    *list.List // front is most recent; values are *bodyEntry
	entries  map[versionKey]*list.Element
	inflight map[versionKey]*load
}

type bodyEntry struct {
	key  versionKey
	body []byte
}

type load struct {
	done chan struct{}
	body []byte
	err  error
}

func newBodyCache(limit int64) *bodyCache {
	return &bodyCache{limit: limit, order: list.New(), entries: map[versionKey]*list.Element{}, inflight: map[versionKey]*load{}}
}

func (c *bodyCache) get(key versionKey, fetch func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		c.order.MoveToFront(e)
		body := e.Value.(*bodyEntry).body
		c.mu.Unlock()
		return body, nil
	}
	if l, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-l.done
		return l.body, l.err
	}
	l := &load{done: make(chan struct{})}
	c.inflight[key] = l
	c.mu.Unlock()
	l.body, l.err = fetch()
	c.mu.Lock()
	delete(c.inflight, key)
	if l.err == nil && int64(len(l.body)) <= c.limit {
		c.entries[key] = c.order.PushFront(&bodyEntry{key: key, body: l.body})
		c.bytes += int64(len(l.body))
		for c.bytes > c.limit {
			oldest := c.order.Back()
			e := oldest.Value.(*bodyEntry)
			c.order.Remove(oldest)
			delete(c.entries, e.key)
			c.bytes -= int64(len(e.body))
		}
	}
	c.mu.Unlock()
	close(l.done)
	return l.body, l.err
}

func (c *bodyCache) size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}
