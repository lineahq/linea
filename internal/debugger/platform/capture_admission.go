package platform

import (
	"errors"
	"fmt"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// admissible applies the caller's capture.raw permission and the Collectors
// open captures hold to a resolved selection. Collector and
// agent selections are strict: a raw stream or a held Collector refuses the
// request. Groups and selectors keep the Collectors the caller may capture
// from and are free, as fleet does for busy ones. pinned says fleet must get
// the kept Collectors as explicit targets, because its own choice could
// include one that was dropped.
func (s *Service) admissible(caller Caller, sel CaptureSelection, snap *peerSnapshot, targets []Collector) (_ []Collector, pinned bool, _ error) {
	strict := len(sel.CollectorIDs) > 0 || len(sel.AgentIDs) > 0
	if project := caller.Project; !s.visible(caller, authz.CaptureRaw, project) {
		kept := []Collector{}
		for _, c := range targets {
			if !capturesRaw(sel, c.ID, snap) {
				kept = append(kept, c)
			} else if strict {
				kept = nil
				break
			}
		}
		if len(kept) == 0 {
			caller.decide(authz.CaptureRaw, "project:"+project, false)
			return nil, false, &Error{Code: CodeForbidden, Message: fmt.Sprintf(
				"forbidden: the selected points are raw on a selected Collector; capturing them needs the %s permission in project %s", authz.CaptureRaw, project)}
		}
		pinned, targets = len(kept) != len(targets), kept
	}
	free, busy := s.holders(caller, targets)
	if busy != nil && (strict || len(free) == 0) {
		return nil, false, busy
	}
	return free, pinned || len(free) != len(targets), nil
}

// capturesRaw reports whether a selection captures a raw stream from a
// Collector. A Collector without a catalog in the snapshot counts as raw.
func capturesRaw(sel CaptureSelection, id string, snap *peerSnapshot) bool {
	points, ok := snap.points[id]
	if !ok {
		return true
	}
	for _, s := range selectedStreams(sel, id, points) {
		if s.sensitivity == "raw" {
			return true
		}
	}
	return false
}

// holders splits targets into the Collectors no open capture of this
// debugger holds and an error naming the holder of the first held one.
func (s *Service) holders(caller Caller, targets []Collector) (free []Collector, busy error) {
	r := s.collectors
	r.mu.Lock()
	defer r.mu.Unlock()
	free = []Collector{}
	for _, c := range targets {
		holder := r.holderLocked(c.ID)
		if holder == nil {
			free = append(free, c)
			continue
		}
		if busy == nil {
			busy = s.heldError(caller, c, holder)
		}
	}
	return free, busy
}

// holderLocked returns the open capture whose unfinished target is
// Collector id, or nil.
func (r *collectorRegistry) holderLocked(id string) *captureRecord {
	for _, c := range r.sessions {
		if c.view.Terminal {
			continue
		}
		for _, t := range c.view.Targets {
			if t.CollectorID == id && !t.Terminal {
				return c
			}
		}
	}
	return nil
}

// heldError names the capture holding a Collector. A caller who cannot read
// the holder's project learns only when the Collector is free again.
func (s *Service) heldError(caller Caller, c Collector, holder *captureRecord) error {
	who := "another capture"
	switch {
	case holder.view.Caller.Principal == caller.Principal.ID:
		who = "your capture " + holder.view.ID
	case s.visible(caller, authz.CaptureRead, holder.view.Project):
		who = fmt.Sprintf("%s (capture %s)", holder.view.Caller.Principal, holder.view.ID)
	}
	return &Error{Code: CodeConflict, Message: fmt.Sprintf(
		"Collector %s is being captured by %s until %s; a Collector serves one capture at a time, so stop that capture or wait for it to end",
		collectorLabel(c), who, holder.view.ExpiresAt.UTC().Format("15:04:05 UTC"))}
}

// collectorLabel is how messages name a Collector: its pod when it reports
// one, else its fleet peer ID.
func collectorLabel(c Collector) string {
	if c.Pod != "" && collectorText(c.Pod, 256, true) {
		if c.Namespace != "" && collectorText(c.Namespace, 256, true) {
			return c.Namespace + "/" + c.Pod
		}
		return c.Pod
	}
	return c.ID
}

// collectorBusy reports fleet's refusal of a Collector that serves another
// capture.
func collectorBusy(err error) bool {
	var refused *fleetcapture.Error
	return errors.As(err, &refused) && refused.Code == "collector_busy"
}
