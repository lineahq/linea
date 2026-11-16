package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// DefaultUnstagedLimit is how many connected agents a group change or a
// credential opt-in may give a new config without max_affected.
const DefaultUnstagedLimit = 10

// SetUnstagedLimit sets how many connected agents a group change or a
// credential opt-in may give a new config before the request must carry
// max_affected; 0 keeps the default.
func (h *Handler) SetUnstagedLimit(n int) {
	if n > 0 {
		h.unstagedLimit = n
	}
}

// errTooManyAffected refuses a change above the unstaged limit.
type errTooManyAffected struct{ affected, limit int }

func (e *errTooManyAffected) Error() string {
	return fmt.Sprintf("the change gives %d connected agents a new config, above %d; resend with max_affected=%d or stage it with a rollout", e.affected, e.limit, e.affected)
}

// maxAffected reads the optional max_affected query parameter; -1 means unset.
func maxAffected(w http.ResponseWriter, r *http.Request) (int, bool) {
	q := r.URL.Query()
	if len(q) == 0 {
		return -1, true
	}
	v, ok := q["max_affected"]
	if len(q) != 1 || !ok || len(v) != 1 {
		failure(w, fleet.Invalid("the only query parameter is max_affected, at most once"))
		return 0, false
	}
	n, err := strconv.Atoi(v[0])
	if err != nil || n < 0 {
		failure(w, fleet.Invalid("max_affected must be a non-negative integer"))
		return 0, false
	}
	return n, true
}

// checkGroupChange refuses a group change that would give more connected
// agents a new config than allowed: max, or the server's limit when the
// request does not set it. A rollout cannot stage a group change, so this is
// the guard against an edit that reconfigures a fleet at once. The count is
// taken before the write and is not atomic with it.
func (h *Handler) checkGroupChange(ctx context.Context, next fleet.Group, max int) error {
	n, err := h.affectedByGroup(ctx, next)
	if err != nil {
		return err
	}
	return h.checkAffected(n, max)
}

// checkAffected refuses n affected agents above max, or above the server's
// limit when the request does not set max.
func (h *Handler) checkAffected(n, max int) error {
	limit := h.unstagedLimit
	if max >= 0 {
		limit = max
	}
	if n > limit {
		return &errTooManyAffected{affected: n, limit: limit}
	}
	return nil
}

// checkOptIn refuses opting a credential in to remote config when that gives
// more connected agents a new config than allowed, like a group change: a
// rollout cannot stage it either. Opting out sends nothing, so it is never
// refused. The count is taken before the write and is not atomic with it.
func (h *Handler) checkOptIn(ctx context.Context, id string, enabled bool, max int) error {
	if !enabled || h.configs == nil || h.credentials == nil {
		return nil
	}
	c, err := h.credentials.Repository.Credential(ctx, id)
	if err != nil {
		// The write reports a missing credential itself.
		return nil
	}
	if c.RemoteConfig || c.RevokedAt != nil {
		// Already opted in, or no agent can connect with it.
		return nil
	}
	targets, err := h.configs.Targets(ctx)
	if err != nil {
		return err
	}
	connected := true
	q := fleet.AgentQuery{Credential: id, Connected: &connected, Limit: 500}
	count := 0
	for {
		page, err := h.store.ListAgents(ctx, q)
		if err != nil {
			return err
		}
		for _, a := range page.Items {
			if a.Capabilities&configmgmt.AcceptsRemoteConfig == 0 {
				continue
			}
			d, _ := describe(targets, a, c.Labels)
			if d != nil && (a.RemoteConfig == nil || a.RemoteConfig.Hash != d.Hash) {
				count++
			}
		}
		if page.NextAfter == "" {
			break
		}
		q.After = page.NextAfter
	}
	return h.checkAffected(count, max)
}

// affectedByGroup counts the connected agents whose desired config changes
// when the group becomes next. It reads the current and the new members.
func (h *Handler) affectedByGroup(ctx context.Context, next fleet.Group) (int, error) {
	if h.configs == nil || h.control == nil {
		return 0, nil
	}
	targets, err := h.configs.Targets(ctx)
	if err != nil {
		return 0, err
	}
	changed := make([]configmgmt.Target, len(targets))
	uses := false
	for i, t := range targets {
		changed[i] = t
		if t.GroupID != next.ID {
			continue
		}
		uses = true
		nt, err := configmgmt.NewTarget(t.Assignment, t.Digest, next.Selector, next.AgentIDs)
		if err != nil {
			// An invalid group is refused by the write itself.
			return 0, nil
		}
		nt.Override = t.Override
		changed[i] = nt
	}
	if !uses {
		return 0, nil
	}
	// Candidates: the connected agents in the current or the new group.
	var queries []fleet.AgentQuery
	if old, err := h.control.GetGroup(ctx, next.ID); err == nil {
		q, err := groupQuery(old)
		if err != nil {
			return 0, err
		}
		queries = append(queries, q)
	} else if !errors.Is(err, fleet.ErrNotFound) {
		return 0, err
	}
	labels := h.labelCache(ctx)
	seen := map[string]struct{}{}
	count := 0
	visit := func(a fleet.Agent) error {
		if _, ok := seen[a.ID]; ok || !a.Connected {
			return nil
		}
		seen[a.ID] = struct{}{}
		l, err := labels(a.CredentialID)
		if err != nil {
			return err
		}
		before, _ := describe(targets, a, l)
		after, _ := describe(changed, a, l)
		if (before == nil) != (after == nil) || before != nil && before.Hash != after.Hash {
			count++
		}
		return nil
	}
	if next.Selector != "" {
		s, err := fleet.ParseSelector(next.Selector)
		if err != nil {
			return 0, nil
		}
		queries = append(queries, fleet.AgentQuery{Selector: s})
	} else {
		for _, id := range next.AgentIDs {
			a, err := h.store.GetAgent(ctx, id)
			if errors.Is(err, fleet.ErrNotFound) {
				continue
			}
			if err != nil {
				return 0, err
			}
			if err := visit(a); err != nil {
				return 0, err
			}
		}
	}
	connected := true
	for _, q := range queries {
		q.Connected, q.Limit = &connected, 500
		for {
			page, err := h.store.ListAgents(ctx, q)
			if err != nil {
				return 0, err
			}
			for _, a := range page.Items {
				if err := visit(a); err != nil {
					return 0, err
				}
			}
			if page.NextAfter == "" {
				break
			}
			q.After = page.NextAfter
		}
	}
	return count, nil
}

// groupQuery selects a stored group's members.
func groupQuery(g fleet.Group) (fleet.AgentQuery, error) {
	if g.Kind == fleet.GroupStatic {
		return fleet.AgentQuery{Group: g.ID}, nil
	}
	s, err := fleet.ParseSelector(g.Selector)
	if err != nil {
		return fleet.AgentQuery{}, fleet.ErrUnavailable
	}
	return fleet.AgentQuery{Selector: s}, nil
}
