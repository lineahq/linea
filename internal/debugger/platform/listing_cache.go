package platform

import (
	"sync"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// listingCacheSize bounds listingCache: four of the largest run pages.
const listingCacheSize = 4 * MaxRunPage

// listingCache keeps the list entries of terminal runs a service listed
// without holding them, so a run list does not read each run's report and
// bundle from the repository again. A terminal run's entry never changes
// except Kept, which the record carries; an entry is used only for the same
// record generation. It holds at most max entries, dropping the oldest.
type listingCache struct {
	mu      sync.Mutex
	max     int
	entries map[string]listingEntry
	order   []string
}

type listingEntry struct {
	generation uint64
	run        Run
}

// get returns the cached entry of a terminal record.
func (c *listingCache) get(record artifact.RunRecord) (Run, bool) {
	if !record.Terminal {
		return Run{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[record.ID]
	if !ok || e.generation != record.Generation {
		return Run{}, false
	}
	v := e.run
	v.Kept = record.Kept
	return v, true
}

// put caches a terminal record's list entry.
func (c *listingCache) put(record artifact.RunRecord, v Run) {
	if !record.Terminal || c.max <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]listingEntry{}
	}
	if _, ok := c.entries[record.ID]; !ok {
		c.order = append(c.order, record.ID)
	}
	c.entries[record.ID] = listingEntry{generation: record.Generation, run: v}
	for len(c.order) > c.max {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
}

// drop forgets a run, for example a deleted one.
func (c *listingCache) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}
