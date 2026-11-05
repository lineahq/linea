package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// rolloutFailure reports the rollout conflicts with their own codes.
func rolloutFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, configmgmt.ErrRolloutActive):
		fail(w, 409, "rollout_conflict", "the assignment or one of its targets is already in an active rollout")
	case errors.Is(err, configmgmt.ErrRolloutState):
		fail(w, 409, "invalid_state", "the rollout's state does not allow this action")
	case errors.Is(err, configmgmt.ErrTooManyTargets):
		fail(w, 409, "too_many_targets", "the rollout would target more agents than allowed")
	default:
		failure(w, err)
	}
}

// rolloutBody changes an assignment's version, or deletes it with delete.
type rolloutBody struct {
	AssignmentID string `json:"assignment_id"`
	ToVersion    int    `json:"to_version"`
	Delete       bool   `json:"delete"`
	rolloutPolicy
}

func (h *Handler) rolloutCollection(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !method(w, r, "GET", "POST") {
		return
	}
	ctx := r.Context()
	if r.Method == "POST" {
		if !admin(w, p) || !noQuery(w, r) {
			return
		}
		body := rolloutBody{rolloutPolicy: *defaultRollout()}
		if !decode(w, r, &body) {
			return
		}
		body.complete()
		view, err := h.configs.CreateRollout(ctx, p.ID, configmgmt.RolloutRequest{ID: body.ID, AssignmentID: body.AssignmentID,
			ToVersion: body.ToVersion, Delete: body.Delete, Policy: body.Policy})
		if err != nil {
			rolloutFailure(w, err)
			return
		}
		w.Header().Set("Location", "/api/v1/rollouts/"+view.ID)
		writeJSON(w, 201, view)
		return
	}
	q, limit, ok := query(w, r, "after")
	if !ok {
		return
	}
	after, ok := afterID(w, q)
	if !ok {
		return
	}
	page, err := h.configs.ListRollouts(ctx, after, limit)
	if err != nil {
		failure(w, err)
		return
	}
	writeJSON(w, 200, page)
}

// rolloutItem serves a rollout, its targets and its controls
// (POST /rollouts/{id}:pause, :resume, :rollback, :cancel).
func (h *Handler) rolloutItem(w http.ResponseWriter, r *http.Request, p auth.Principal, rest string) {
	ctx := r.Context()
	if id, ok := strings.CutSuffix(rest, "/targets"); ok {
		if !fleet.ValidID(id) {
			failure(w, fleet.ErrNotFound)
			return
		}
		if !method(w, r, "GET") {
			return
		}
		q, limit, ok := query(w, r, "after", "outcome")
		if !ok {
			return
		}
		after, ok := afterID(w, q)
		if !ok {
			return
		}
		outcome := configmgmt.Outcome(q.Get("outcome"))
		if q.Has("outcome") && !configmgmt.ValidOutcome(outcome) {
			failure(w, fleet.Invalid("outcome must be one of "+configmgmt.OutcomeList()))
			return
		}
		page, err := h.configs.ListRolloutTargets(ctx, id, outcome, after, limit)
		if err != nil {
			failure(w, err)
			return
		}
		writeJSON(w, 200, page)
		return
	}
	id, action, control := strings.Cut(rest, ":")
	if !fleet.ValidID(id) {
		failure(w, fleet.ErrNotFound)
		return
	}
	if control {
		c := configmgmt.Control(action)
		switch c {
		case configmgmt.ControlPause, configmgmt.ControlResume, configmgmt.ControlRollback, configmgmt.ControlCancel:
		default:
			failure(w, fleet.ErrNotFound)
			return
		}
		if !method(w, r, "POST") || !admin(w, p) || !noQuery(w, r) {
			return
		}
		view, err := h.configs.ControlRollout(ctx, p.ID, id, c)
		if err != nil {
			rolloutFailure(w, err)
			return
		}
		writeJSON(w, 200, view)
		return
	}
	if !method(w, r, "GET") || !noQuery(w, r) {
		return
	}
	view, err := h.configs.GetRollout(ctx, id)
	if err != nil {
		failure(w, err)
		return
	}
	writeJSON(w, 200, view)
}
