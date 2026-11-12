package platform

import (
	"context"
	"sync"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// maxCachedManifestBytes bounds the manifests manifestCache keeps.
const maxCachedManifestBytes = 4 << 20

// manifestCache keeps the manifests of recent server builds. A build's
// manifest never changes after admission, and a client polling a build's
// status asks for it every time; the cache spares an object storage read
// per poll. It is safe for concurrent use; its zero value is empty.
type manifestCache struct {
	mu    sync.Mutex
	byID  map[string][]byte
	order []string
	bytes int
}

// get returns build id's manifest, reading it from builds once.
func (c *manifestCache) get(ctx context.Context, builds artifact.Builds, id string) ([]byte, error) {
	c.mu.Lock()
	m, ok := c.byID[id]
	c.mu.Unlock()
	if ok {
		return m, nil
	}
	m, err := builds.BuildManifest(ctx, id)
	if err != nil {
		return nil, err
	}
	c.put(id, m)
	return m, nil
}

// put keeps m as build id's manifest, evicting the oldest beyond the bound.
func (c *manifestCache) put(id string, m []byte) {
	if len(m) > maxCachedManifestBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byID[id]; ok {
		return
	}
	if c.byID == nil {
		c.byID = map[string][]byte{}
	}
	for c.bytes+len(m) > maxCachedManifestBytes && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		c.bytes -= len(c.byID[old])
		delete(c.byID, old)
	}
	c.byID[id] = m
	c.order = append(c.order, id)
	c.bytes += len(m)
}
