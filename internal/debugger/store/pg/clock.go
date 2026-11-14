package pg

import (
	"context"
	"sync"
	"time"
)

// Lease times come from the callers' clocks, and instances' clocks drift
// apart: an instance whose clock runs ahead would requeue a run another
// instance still renews. The store therefore moves every lease time onto
// the database's clock by an offset it measures and refreshes, so a lease is
// written and judged by one clock whichever instance does it.
const (
	// clockRefresh is how long a measured offset is used before the next
	// measurement.
	clockRefresh = 30 * time.Second
	// clockTolerance keeps the offset while a new measurement stays within
	// it, so round-trip noise never moves a lease boundary between two calls.
	clockTolerance = 250 * time.Millisecond
	// clockMaxRoundTrip discards a later measurement whose round trip is too
	// slow to bound its error; the first one is always taken.
	clockMaxRoundTrip = time.Second
)

// dbClock is the database clock's offset from this process's clock.
type dbClock struct {
	mu sync.Mutex
	// offset is the database's time minus this process's, in whole
	// microseconds so a time round-trips exactly through timestamptz.
	offset time.Duration
	// measured is the local time of the last measurement, zero before the
	// first.
	measured time.Time
	// local is this process's clock; nil is time.Now. Tests set it to
	// stand for an instance whose clock is off.
	local func() time.Time
}

func (c *dbClock) now() time.Time {
	if c.local != nil {
		return c.local()
	}
	return time.Now()
}

// clockOffset returns the database clock's offset from this process's,
// measuring it when the last measurement is older than clockRefresh. The
// lock is not held across the query.
func (s *Store) clockOffset(ctx context.Context) (time.Duration, error) {
	c := &s.clock
	c.mu.Lock()
	offset, measured := c.offset, c.measured
	c.mu.Unlock()
	if !measured.IsZero() && c.now().Sub(measured) < clockRefresh {
		return offset, nil
	}
	before := c.now()
	var db time.Time
	err := s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&db)
	after := c.now()
	if err != nil {
		if !measured.IsZero() {
			// Keep the last offset; the caller's own statement reports an
			// outage.
			return offset, nil
		}
		return 0, storageErr("read the database clock", err)
	}
	roundTrip := after.Sub(before)
	if !measured.IsZero() && roundTrip > clockMaxRoundTrip {
		return offset, nil
	}
	fresh := db.Sub(before.Add(roundTrip / 2)).Truncate(time.Microsecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.measured.IsZero() || (fresh-c.offset).Abs() > clockTolerance {
		c.offset = fresh
	}
	c.measured = after
	return c.offset, nil
}

// leaseClock returns the functions that move a caller's time onto the
// database's clock and back.
func (s *Store) leaseClock(ctx context.Context) (toDB, fromDB func(time.Time) time.Time, err error) {
	offset, err := s.clockOffset(ctx)
	if err != nil {
		return nil, nil, err
	}
	toDB = func(t time.Time) time.Time { return t.Add(offset).UTC() }
	fromDB = func(t time.Time) time.Time { return t.Add(-offset).UTC() }
	return toDB, fromDB, nil
}
