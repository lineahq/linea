package platform

import (
	"context"
	"time"

	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// run consumes one capture's fleet channel until the operation is terminal
// and the channel is drained, polling the operation for target states and
// counters. It sends the stops the platform requests and, on shutdown,
// stops the capture and deletes the channel.
func (r *collectorRegistry) run(c *captureRecord) {
	defer r.runners.Done()
	ctx := r.ctx
	var lastStatus time.Time
	for ctx.Err() == nil {
		r.mu.Lock()
		stop, terminal, deadline := c.stopReason, c.view.Terminal, c.view.ExpiresAt.Add(captureGuard)
		c.stopReason = ""
		r.mu.Unlock()
		if stop != "" && !terminal {
			if op, err := r.stopFleet(ctx, c); err == nil {
				r.observe(c, op)
			} else if ctx.Err() == nil {
				r.mu.Lock()
				c.stopReason = stop // retried on the next pass
				r.mu.Unlock()
			}
		}
		if time.Since(lastStatus) >= captureStatusPeriod {
			op, err := r.fleet.GetCapture(ctx, c.fleetID)
			switch {
			case err == nil:
				lastStatus = time.Now()
				r.observe(c, op)
				r.checkCatalog(ctx, c)
			case fleetcapture.StatusOf(err) == 404 || fleetcapture.StatusOf(err) == 403:
				r.lose(c, "fleet_no_longer_reports_capture")
				return
			}
		}
		e, err := r.fleet.Next(ctx, c.channel)
		switch status := fleetcapture.StatusOf(err); {
		case err == nil && e != nil:
			accepted := r.ingest(ctx, c, e)
			_ = r.fleet.Acknowledge(ctx, c.channel, fleetcapture.Ack{BatchID: e.BatchID, Accepted: accepted})
			continue
		case status == 404 || status == 410 || status == 403:
			r.lose(c, "capture_channel_lost")
			return
		case err == nil:
			r.mu.Lock()
			drained := c.view.Terminal
			r.mu.Unlock()
			if drained && time.Since(lastStatus) < captureStatusPeriod {
				r.finish(c)
				return
			}
			if drained {
				lastStatus = time.Time{} // confirm the final state before leaving
				continue
			}
		}
		if !r.now().Before(deadline) {
			r.lose(c, "no_terminal_state_from_fleet_after_expiry")
			return
		}
		select {
		case <-ctx.Done():
		case <-c.wake:
		case <-time.After(captureIdle):
		}
	}
	r.shutdown(c)
}

// observe applies an operation read and persists a state change.
func (r *collectorRegistry) observe(c *captureRecord, op *fleetcapture.Operation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applyOperationLocked(c, op) {
		if r.checkpointCaptureLocked(context.Background(), c, nil) != nil {
			r.requestStopLocked(c, "capture_storage_error")
		}
	}
	c.notify()
}

// checkCatalog stops a capture when a target's catalog changed the
// sensitivity of a selected stream: the admission-time classification is no
// longer safe, so later batches from that target are refused.
func (r *collectorRegistry) checkCatalog(ctx context.Context, c *captureRecord) {
	snap, err := r.snapshot(ctx, false)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.view.Terminal {
		return
	}
	for _, t := range c.view.Targets {
		peer, ok := snap.collectors[t.CollectorID]
		if !ok || peer.Generation != t.Generation || c.refused[t.CollectorID] {
			continue
		}
		if !sameSensitivities(c, t.CollectorID, snap.points[t.CollectorID]) {
			c.refused[t.CollectorID] = true
			r.requestStopLocked(c, "capture_catalog_changed")
		}
	}
}

// sameSensitivities compares a peer's selected streams with admission.
func sameSensitivities(c *captureRecord, peer string, points []CapturePoint) bool {
	now := map[string]string{}
	for _, key := range selectedStreams(c.view.Selection, peer, points) {
		now[key.key] = key.sensitivity
	}
	n := 0
	for key, sensitivity := range c.sensitivities {
		if len(key) > len(peer) && key[:len(peer)] == peer && key[len(peer)] == 0 {
			n++
			if now[key] != sensitivity {
				return false
			}
		}
	}
	return n == len(now)
}

// finish freezes a drained capture's data and deletes its channel.
func (r *collectorRegistry) finish(c *captureRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), captureCleanup)
	defer cancel()
	_ = r.fleet.DeleteChannel(ctx, c.channel)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.freezeLocked(c)
}

// lose ends a capture the debugger can no longer follow.
func (r *collectorRegistry) lose(c *captureRecord, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), captureCleanup)
	defer cancel()
	_ = r.fleet.DeleteChannel(ctx, c.channel)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lostLocked(c, reason)
	r.freezeLocked(c)
}

// shutdown stops an unfinished capture when the platform closes. The data
// was frozen by close.
func (r *collectorRegistry) shutdown(c *captureRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), captureCleanup)
	defer cancel()
	r.mu.Lock()
	terminal := c.view.Terminal
	r.mu.Unlock()
	if !terminal {
		_, _ = r.stopFleet(ctx, c)
	}
	_ = r.fleet.DeleteChannel(ctx, c.channel)
	r.mu.Lock()
	defer r.mu.Unlock()
	c.running = false
}

// freezeLocked closes a capture's data after its runner stops.
func (r *collectorRegistry) freezeLocked(c *captureRecord) {
	c.running = false
	if c.view.Data.Enabled && (c.view.Data.State == "open" || c.view.Data.State == "draining") {
		c.view.Data.State = "frozen"
	}
	if !r.closed {
		_ = r.checkpointCaptureLocked(context.Background(), c, nil)
	}
	r.offloadLocked(c)
	c.notify()
}
