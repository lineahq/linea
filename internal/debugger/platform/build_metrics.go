package platform

import (
	"context"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// Build metrics let an autoscaler size the builders to the
// build queue. They are counts only: no build, runtime, builder or
// principal is named.

// buildCountsFor is how long the queue's counts are reused, so a scraper
// polling every instance does not query the store each time. Tests
// shorten it.
var buildCountsFor = 10 * time.Second

// BuildMetrics are the build queue's counts and this instance's builders.
type BuildMetrics struct {
	// Queued and Running are the installation's builds, the same on every
	// instance.
	Queued, Running int
	// Builders are the builders connected to this instance, and
	// BuilderSlots the sum of their capacities.
	Builders, BuilderSlots int
}

// builderCount is the builders connected to this instance.
type builderCount struct {
	mu           sync.Mutex
	links, slots int
}

func (c *builderCount) add(capacity int) {
	c.mu.Lock()
	c.links++
	c.slots += capacity
	c.mu.Unlock()
}

func (c *builderCount) remove(capacity int) {
	c.mu.Lock()
	c.links--
	c.slots -= capacity
	c.mu.Unlock()
}

// buildCountCache holds the queue's last counts.
type buildCountCache struct {
	mu     sync.Mutex
	counts artifact.BuildCounts
	at     time.Time
}

// BuildMetrics reports the build queue and this instance's builders. It
// is served without a caller, on a listener of its own (linea-server
// --metrics-listen), so it reveals counts only. A service that takes no
// builds reports zeros.
func (s *Service) BuildMetrics(ctx context.Context) (BuildMetrics, error) {
	var m BuildMetrics
	s.builders.mu.Lock()
	m.Builders, m.BuilderSlots = s.builders.links, s.builders.slots
	s.builders.mu.Unlock()
	builds, ok := s.serverBuilds()
	if !ok {
		return m, nil
	}
	c := &s.buildCounts
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	if c.at.IsZero() || now.Sub(c.at) >= buildCountsFor || now.Before(c.at) {
		counts, err := builds.CountBuilds(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return BuildMetrics{}, classify(ctx.Err(), CodeCancelled)
			}
			return BuildMetrics{}, artifactError(err)
		}
		c.counts, c.at = counts, now
	}
	m.Queued, m.Running = c.counts.Queued, c.counts.Running
	return m, nil
}
