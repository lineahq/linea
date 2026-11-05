package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Rollouts (M6 step 6b). Every change to what agents run is a rollout: the
// API admits it with a snapshot of its targets, and the leader's controller
// releases waves, records outcomes and finishes it. The controller only
// changes desired state; replicas deliver it like any other change.

const activeRollout = "('running','paused','rolling_back')"

const rolloutColumns = "r.id,r.assignment_id,r.config_id,r.from_version,r.to_version,r.state,r.current_wave,r.wave_count,r.targets,r.phase_started_at,r.policy,r.reason,r.created_at,r.created_by,r.updated_at"

// scanRollout reads rolloutColumns and returns the phase start beside the
// view, followed by any extra columns.
func scanRollout(row pgx.Row, extra ...any) (configmgmt.RolloutView, *time.Time, error) {
	var r configmgmt.RolloutView
	var from, to *int
	var phase *time.Time
	var policy []byte
	var reason *string
	dest := append([]any{&r.ID, &r.AssignmentID, &r.ConfigID, &from, &to, &r.State, &r.CurrentWave, &r.WaveCount, &r.Targets, &phase, &policy, &reason, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return r, nil, err
	}
	if from != nil {
		r.FromVersion = *from
	}
	if to != nil {
		r.ToVersion = *to
	}
	r.Reason = deref(reason)
	if err := json.Unmarshal(policy, &r.Policy); err != nil {
		return r, nil, fleet.ErrUnavailable
	}
	return r, phase, nil
}

func nonZero(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

// CreateAssignment stores an assignment without an active version and starts
// the rollout that introduces it at a.Version.
func (s *Store) CreateAssignment(ctx context.Context, actor string, a configmgmt.Assignment, req configmgmt.RolloutRequest) (configmgmt.Assignment, configmgmt.RolloutView, error) {
	var view configmgmt.RolloutView
	a, err := configmgmt.NormalizeAssignment(a, auth.ValidLabels)
	if err != nil {
		return a, view, err
	}
	req.AssignmentID, req.ToVersion, req.Delete = a.ID, a.Version, false
	if err := req.Validate(); err != nil {
		return a, view, err
	}
	labels := []byte("{}")
	if a.CredentialLabels != nil {
		labels, _ = json.Marshal(a.CredentialLabels)
	}
	target := a.Version
	err = s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		// Serialize creations so the bound holds under concurrency.
		if _, err := tx.Exec(ctx, "LOCK TABLE fleet.config_assignments IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.config_assignments").Scan(&count); err != nil {
			return err
		}
		if count >= configmgmt.MaxAssignments {
			return fleet.ErrCapacity
		}
		if err := versionExists(ctx, tx, a.ConfigID, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO fleet.config_assignments(id,name,config_id,version,layer,group_id,selector,credential_labels,revision,created_at,created_by)
			VALUES($1,$2,$3,NULL,$4,$5,$6,$7::jsonb,1,now(),$8)`,
			a.ID, a.Name, a.ConfigID, a.Layer, nonEmpty(a.GroupID), nonEmpty(a.Selector), string(labels), actor); err != nil {
			return err
		}
		stored, err := scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+assignmentFrom+" WHERE a.id=$1", a.ID))
		if err != nil {
			return err
		}
		if view, err = s.startRollout(ctx, tx, actor, req, stored); err != nil {
			return err
		}
		stored.RolloutID = view.ID
		a = stored
		changed := a
		changed.Version = target
		return s.assignmentChanged(ctx, tx, actor, "created", changed)
	})
	return a, view, err
}

// versionExists refuses a reference to a missing or pruned version.
func versionExists(ctx context.Context, tx pgx.Tx, configID string, version int) error {
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.config_versions WHERE config_id=$1 AND version=$2)", configID, version).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fleet.Invalid("the config has no such version; it may have been pruned")
	}
	return nil
}

// CreateRollout moves an assignment to another version, or removes its
// layer, through a rollout.
func (s *Store) CreateRollout(ctx context.Context, actor string, req configmgmt.RolloutRequest) (configmgmt.RolloutView, error) {
	var view configmgmt.RolloutView
	if err := req.Validate(); err != nil {
		return view, err
	}
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		a, err := scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+assignmentFrom+" WHERE a.id=$1 FOR UPDATE OF a", req.AssignmentID))
		if err != nil {
			return err
		}
		if a.RolloutID != "" || a.Version == 0 {
			return configmgmt.ErrRolloutActive
		}
		if !req.Delete {
			if req.ToVersion == a.Version {
				return fleet.Invalid("to_version is already the assignment's version")
			}
			if err := versionExists(ctx, tx, a.ConfigID, req.ToVersion); err != nil {
				return err
			}
		}
		view, err = s.startRollout(ctx, tx, actor, req, a)
		return err
	})
	return view, err
}

// startRollout admits a rollout of a: it snapshots the eligible agents the
// assignment targets, orders them into waves and stores everything. The
// controller releases the first wave.
func (s *Store) startRollout(ctx context.Context, tx pgx.Tx, actor string, req configmgmt.RolloutRequest, a configmgmt.Assignment) (configmgmt.RolloutView, error) {
	view := configmgmt.RolloutView{ID: req.ID, AssignmentID: a.ID, ConfigID: a.ConfigID, FromVersion: a.Version, ToVersion: req.ToVersion,
		State: configmgmt.Running, CurrentWave: -1, Policy: req.Policy, CreatedBy: actor}
	// Serialize admissions so the active bound holds under concurrency.
	if _, err := tx.Exec(ctx, "LOCK TABLE fleet.rollouts IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return view, err
	}
	var active int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.rollouts WHERE state IN "+activeRollout).Scan(&active); err != nil {
		return view, err
	}
	if active >= configmgmt.MaxActiveRollouts {
		return view, fleet.ErrCapacity
	}
	agents, err := eligibleAgents(ctx, tx, a, nil)
	if err != nil {
		return view, err
	}
	if len(agents) > configmgmt.MaxRolloutTargets {
		return view, configmgmt.ErrTooManyTargets
	}
	var canary func(string) bool
	if req.CanaryGroupID != "" {
		members, err := groupAgents(ctx, tx, req.CanaryGroupID, agents)
		if err != nil {
			return view, err
		}
		canary = func(id string) bool { _, ok := members[id]; return ok }
	}
	order, waves, count := configmgmt.PlanWaves(req.ID, agents, canary, req.Waves)
	view.WaveCount, view.Targets = count, len(order)
	policy, _ := json.Marshal(req.Policy)
	err = tx.QueryRow(ctx, `INSERT INTO fleet.rollouts(id,assignment_id,config_id,from_version,to_version,state,wave_count,targets,policy,created_at,created_by,updated_at)
		VALUES($1,$2,$3,$4,$5,'running',$6,$7,$8::jsonb,now(),$9,now()) RETURNING created_at,updated_at`,
		req.ID, a.ID, a.ConfigID, nonZero(a.Version), nonZero(req.ToVersion), count, len(order), string(policy), actor).Scan(&view.CreatedAt, &view.UpdatedAt)
	if err != nil {
		return view, activeConflict(err)
	}
	if len(order) > 0 {
		wave32 := make([]int32, len(waves))
		for i, w := range waves {
			wave32[i] = int32(w)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO fleet.rollout_targets(rollout_id,agent_id,wave)
			SELECT $1, t.agent_id, t.wave FROM unnest($2::text[], $3::int4[]) AS t(agent_id, wave)`, req.ID, order, wave32); err != nil {
			return view, activeConflict(err)
		}
	}
	detail := mustJSON(map[string]any{"assignment_id": a.ID, "from_version": a.Version, "to_version": req.ToVersion, "targets": len(order), "waves": count})
	return view, s.event(ctx, tx, "rollout.created", req.ID, actor, detail)
}

// activeConflict maps a violation of the one-active-rollout indexes.
func activeConflict(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) && p.Code == "23505" && (p.ConstraintName == "rollouts_active_assignment_idx" || p.ConstraintName == "rollout_targets_active_agent_idx") {
		return configmgmt.ErrRolloutActive
	}
	return err
}

// eligibleAgents returns the agents an assignment targets that can receive
// remote config: an active credential with remote_config whose labels
// satisfy the assignment, and the AcceptsRemoteConfig capability. Selectors
// use the inventory's SQL rendering. within, if set, restricts the result.
// At most MaxRolloutTargets+1 IDs are returned, in ID order.
func eligibleAgents(ctx context.Context, tx pgx.Tx, a configmgmt.Assignment, within []string) ([]string, error) {
	p := &params{}
	where := []string{"c.remote_config", "c.revoked_at IS NULL", "c.expires_at > now()", "mod(trunc(a.capabilities / 2), 2) = 1"}
	if len(a.CredentialLabels) > 0 {
		labels, _ := json.Marshal(a.CredentialLabels)
		where = append(where, "c.labels @> "+p.add(string(labels))+"::jsonb")
	}
	match, err := targetSQL(ctx, tx, a.GroupID, a.Selector, p)
	if err != nil {
		return nil, err
	}
	where = append(where, match...)
	if within != nil {
		where = append(where, "a.id = ANY("+p.add(within)+"::text[])")
	}
	rows, err := tx.Query(ctx, "SELECT a.id FROM fleet.agents a JOIN fleet.credentials c ON c.id=a.credential_id WHERE "+strings.Join(where, " AND ")+
		" ORDER BY a.id LIMIT "+p.add(configmgmt.MaxRolloutTargets+1), p.args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// targetSQL renders a group or inline selector target as conditions on "a".
func targetSQL(ctx context.Context, tx pgx.Tx, groupID, selector string, p *params) ([]string, error) {
	if groupID != "" {
		var kind string
		var groupSelector *string
		if err := tx.QueryRow(ctx, "SELECT kind,selector FROM fleet.groups WHERE id=$1", groupID).Scan(&kind, &groupSelector); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fleet.Invalid("group_id names no group")
			}
			return nil, err
		}
		if kind == string(fleet.GroupStatic) {
			return []string{"EXISTS(SELECT 1 FROM fleet.group_members m WHERE m.group_id=" + p.add(groupID) + " AND m.agent_id=a.id)"}, nil
		}
		selector = deref(groupSelector)
	}
	sel, err := fleet.ParseSelector(selector)
	if err != nil || sel.Empty() {
		return nil, fleet.ErrUnavailable
	}
	return selectorSQL(sel, p), nil
}

// groupAgents returns which of agents belong to a group.
func groupAgents(ctx context.Context, tx pgx.Tx, groupID string, agents []string) (map[string]struct{}, error) {
	p := &params{}
	where, err := targetSQL(ctx, tx, groupID, "", p)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, "SELECT a.id FROM fleet.agents a WHERE "+strings.Join(where, " AND ")+" AND a.id = ANY("+p.add(agents)+"::text[])", p.args...)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}

func (s *Store) GetRollout(ctx context.Context, id string) (configmgmt.RolloutView, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	r, _, err := scanRollout(s.pool.QueryRow(ctx, "SELECT "+rolloutColumns+" FROM fleet.rollouts r WHERE r.id=$1", id))
	if err != nil {
		return r, normalize(err)
	}
	waves := make([]configmgmt.WaveProgress, r.WaveCount)
	for i := range waves {
		waves[i] = configmgmt.WaveProgress{Wave: i, Outcomes: map[configmgmt.Outcome]int{}}
	}
	rows, err := s.pool.Query(ctx, `SELECT wave,outcome,count(*),min(released_at) FROM fleet.rollout_targets WHERE rollout_id=$1 GROUP BY wave,outcome`, id)
	if err != nil {
		return r, normalize(err)
	}
	defer rows.Close()
	for rows.Next() {
		var wave, n int
		var outcome configmgmt.Outcome
		var released *time.Time
		if err := rows.Scan(&wave, &outcome, &n, &released); err != nil {
			return r, normalize(err)
		}
		if wave < 0 || wave >= len(waves) {
			continue
		}
		w := &waves[wave]
		w.Targets += n
		w.Outcomes[outcome] = n
		if released != nil && (w.ReleasedAt == nil || released.Before(*w.ReleasedAt)) {
			w.ReleasedAt = released
		}
	}
	r.Waves = waves
	return r, normalize(rows.Err())
}

// ListRollouts pages rollouts in ID order, without wave progress.
func (s *Store) ListRollouts(ctx context.Context, after string, limit int) (configmgmt.RolloutPage, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	p := configmgmt.RolloutPage{Items: []configmgmt.RolloutView{}}
	rows, err := s.pool.Query(ctx, "SELECT "+rolloutColumns+" FROM fleet.rollouts r WHERE r.id>$1 ORDER BY r.id LIMIT $2", after, limit+1)
	if err != nil {
		return p, normalize(err)
	}
	defer rows.Close()
	for rows.Next() {
		r, _, err := scanRollout(rows)
		if err != nil {
			return p, normalize(err)
		}
		p.Items = append(p.Items, r)
	}
	if err := rows.Err(); err != nil {
		return p, normalize(err)
	}
	if len(p.Items) > limit {
		p.Items = p.Items[:limit]
		p.NextAfter = p.Items[limit-1].ID
	}
	return p, nil
}

// ListRolloutTargets pages a rollout's targets in agent ID order, optionally
// with one outcome.
func (s *Store) ListRolloutTargets(ctx context.Context, id string, outcome configmgmt.Outcome, after string, limit int) (configmgmt.RolloutTargetPage, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	p := configmgmt.RolloutTargetPage{Items: []configmgmt.RolloutTarget{}}
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.rollouts WHERE id=$1)", id).Scan(&exists); err != nil {
		return p, normalize(err)
	}
	if !exists {
		return p, fleet.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT agent_id,wave,outcome,released_at,observed_at,accepted FROM fleet.rollout_targets
		WHERE rollout_id=$1 AND agent_id>$2 AND ($3='' OR outcome=$3) ORDER BY agent_id LIMIT $4`, id, after, string(outcome), limit+1)
	if err != nil {
		return p, normalize(err)
	}
	defer rows.Close()
	for rows.Next() {
		var t configmgmt.RolloutTarget
		if err := rows.Scan(&t.AgentID, &t.Wave, &t.Outcome, &t.ReleasedAt, &t.ObservedAt, &t.Accepted); err != nil {
			return p, normalize(err)
		}
		p.Items = append(p.Items, t)
	}
	if err := rows.Err(); err != nil {
		return p, normalize(err)
	}
	if len(p.Items) > limit {
		p.Items = p.Items[:limit]
		p.NextAfter = p.Items[limit-1].AgentID
	}
	return p, nil
}

// ControlRollout applies an operator action. Resume accepts the failures
// seen so far, so they no longer count against the thresholds.
func (s *Store) ControlRollout(ctx context.Context, actor, id string, action configmgmt.Control) (configmgmt.RolloutView, error) {
	if !fleet.ValidID(id) {
		return configmgmt.RolloutView{}, fleet.ErrNotFound
	}
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		r, _, err := scanRollout(tx.QueryRow(ctx, "SELECT "+rolloutColumns+" FROM fleet.rollouts r WHERE r.id=$1 FOR UPDATE", id))
		if err != nil {
			return err
		}
		next, err := action.Next(r.State)
		if err != nil {
			return err
		}
		switch next {
		case configmgmt.Paused:
			_, err = tx.Exec(ctx, "UPDATE fleet.rollouts SET state='paused',reason='operator',updated_at=now() WHERE id=$1", id)
		case configmgmt.Running:
			if _, err = tx.Exec(ctx, "UPDATE fleet.rollouts SET state='running',reason=NULL,updated_at=now() WHERE id=$1", id); err == nil {
				_, err = tx.Exec(ctx, "UPDATE fleet.rollout_targets SET accepted=true WHERE rollout_id=$1 AND outcome IN ('failed','unhealthy','timed_out') AND NOT accepted", id)
			}
		case configmgmt.RollingBack:
			err = startRollback(ctx, tx, id, "operator")
		case configmgmt.Cancelled:
			return s.finishRollout(ctx, tx, actor, r, configmgmt.Cancelled)
		}
		if err != nil {
			return err
		}
		event := map[configmgmt.Control]string{configmgmt.ControlPause: "rollout.paused", configmgmt.ControlResume: "rollout.resumed", configmgmt.ControlRollback: "rollout.rolling_back"}[action]
		return s.event(ctx, tx, event, id, actor, mustJSON(map[string]string{"reason": "operator"}))
	})
	if err != nil {
		return configmgmt.RolloutView{}, err
	}
	return s.GetRollout(ctx, id)
}

// startRollback stops the override: released targets return to the
// assignment's version, and the controller watches them get there.
func startRollback(ctx context.Context, tx pgx.Tx, id, reason string) error {
	if _, err := tx.Exec(ctx, "UPDATE fleet.rollouts SET state='rolling_back',reason=$2,phase_started_at=now(),updated_at=now() WHERE id=$1", id, reason); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "SELECT pg_notify('fleet_config','')")
	return err
}

// finishRollout ends an active rollout. Completion moves the assignment to
// the new version, or deletes it, in the same transaction; a rollout that
// created its assignment and did not complete removes it. Finished rollouts
// beyond KeepRollouts are pruned with their targets.
func (s *Store) finishRollout(ctx context.Context, tx pgx.Tx, actor string, r configmgmt.RolloutView, state configmgmt.RolloutState) error {
	if _, err := tx.Exec(ctx, "UPDATE fleet.rollouts SET state=$2,updated_at=now() WHERE id=$1", r.ID, string(state)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE fleet.rollout_targets SET active=false WHERE rollout_id=$1 AND active", r.ID); err != nil {
		return err
	}
	a, err := scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+assignmentFrom+" WHERE a.id=$1 FOR UPDATE OF a", r.AssignmentID))
	if err != nil {
		return err
	}
	action := ""
	switch {
	case state == configmgmt.Completed && r.ToVersion > 0:
		_, err = tx.Exec(ctx, "UPDATE fleet.config_assignments SET version=$2,revision=revision+1 WHERE id=$1", a.ID, r.ToVersion)
		a.Version = r.ToVersion
		action = "updated"
	case state == configmgmt.Completed, r.FromVersion == 0:
		_, err = tx.Exec(ctx, "DELETE FROM fleet.config_assignments WHERE id=$1", a.ID)
		action = "deleted"
	default:
		_, err = tx.Exec(ctx, "SELECT pg_notify('fleet_config','')")
	}
	if err != nil {
		return err
	}
	if action != "" {
		if err := s.assignmentChanged(ctx, tx, actor, action, a); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM fleet.rollouts WHERE id IN (SELECT id FROM fleet.rollouts WHERE state IN ('completed','rolled_back','cancelled')
		ORDER BY updated_at DESC, id DESC OFFSET $1)`, configmgmt.KeepRollouts); err != nil {
		return err
	}
	return s.event(ctx, tx, "rollout."+string(state), r.ID, actor, nil)
}

// rolloutLoop runs the controller while this replica holds the leader lease.
// It keeps no state between ticks; every decision is recomputed from the
// database, so a leader change resumes where the rollouts stand.
func (s *Store) rolloutLoop() {
	defer s.loops.Done()
	ticker := time.NewTicker(s.options.Rollouts)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		if s.leader.Load() && !s.stopped.Load() {
			if err := s.advanceRollouts(s.ctx); err != nil {
				s.rolloutErrors.Add(1)
			}
		}
	}
}

// advanceRollouts evaluates every active rollout, each in its own
// transaction holding the rollout row.
func (s *Store) advanceRollouts(ctx context.Context) error {
	qctx, cancel := bounded(ctx)
	rows, err := s.pool.Query(qctx, "SELECT id FROM fleet.rollouts WHERE state IN "+activeRollout+" ORDER BY id")
	var ids []string
	if err == nil {
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	cancel()
	if err != nil || len(ids) == 0 {
		return normalize(err)
	}
	// Assignments and credentials are read once per tick. Each rollout's own
	// override is rebuilt inside its transaction, and no agent is in two
	// active rollouts, so another rollout's progress never changes what this
	// one's targets should run.
	base, err := s.Targets(ctx)
	if err != nil {
		return err
	}
	credentials, err := s.ManagedCredentials(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, id := range ids {
		if err := s.advanceRollout(ctx, id, base, credentials); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Store) advanceRollout(ctx context.Context, id string, base []configmgmt.Target, credentials map[string]map[string]string) error {
	return s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var digest []byte
		var now time.Time
		r, phase, err := scanRollout(tx.QueryRow(ctx, "SELECT "+rolloutColumns+`,v.sha256,now() FROM fleet.rollouts r
			LEFT JOIN fleet.config_versions v ON v.config_id=r.config_id AND v.version=r.to_version WHERE r.id=$1 FOR UPDATE OF r`, id), &digest, &now)
		if err != nil || !r.State.Active() {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT agent_id FROM fleet.rollout_targets WHERE rollout_id=$1 AND released_at IS NOT NULL", id)
		if err != nil {
			return err
		}
		released, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		targets := withOverride(base, r, digest, released)
		required, err := s.required(ctx, tx, r)
		if err != nil {
			return err
		}
		state := configmgmt.Rollout{State: r.State, CurrentWave: r.CurrentWave, WaveCount: r.WaveCount, Policy: r.Policy}
		if phase != nil {
			state.PhaseStartedAt = *phase
		}
		if r.State == configmgmt.RollingBack {
			all, err := s.observe(ctx, tx, id, "", targets, credentials, nil)
			if err != nil || !configmgmt.Reverted(state, all, now) {
				return err
			}
			return s.finishRollout(ctx, tx, "server", r, configmgmt.RolledBack)
		}
		open, err := s.observe(ctx, tx, id, " AND t.outcome IN ('released','baking','offline')", targets, credentials, required)
		if err != nil {
			return err
		}
		settled, err := tallies(ctx, tx, id)
		if err != nil {
			return err
		}
		step := configmgmt.Evaluate(state, open, settled, now)
		if err := applyUpdates(ctx, tx, id, step.Updates); err != nil {
			return err
		}
		switch step.Action {
		case configmgmt.ReleaseWave:
			next := r.CurrentWave + 1
			// Targets whose build lacks a type the version uses are never
			// released; agents without a stored component set are.
			var releasedCount, incompatible int64
			err := tx.QueryRow(ctx, `WITH wave AS (
					SELECT t.agent_id,COALESCE(NOT cs.components @> $3::jsonb, false) AS incompatible
					FROM fleet.rollout_targets t LEFT JOIN fleet.agents a ON a.id=t.agent_id
					LEFT JOIN fleet.component_sets cs ON cs.hash=a.available_components_hash
					WHERE t.rollout_id=$1 AND t.wave=$2 AND t.outcome='pending'),
				changed AS (
					UPDATE fleet.rollout_targets t SET released_at=CASE WHEN w.incompatible THEN NULL ELSE now() END,
						outcome=CASE WHEN w.incompatible THEN 'incompatible' ELSE 'released' END
					FROM wave w WHERE t.rollout_id=$1 AND t.agent_id=w.agent_id RETURNING t.outcome)
				SELECT count(*) FILTER (WHERE outcome='released'),count(*) FILTER (WHERE outcome='incompatible') FROM changed`,
				id, next, required).Scan(&releasedCount, &incompatible)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE fleet.rollouts SET current_wave=$2,phase_started_at=now(),updated_at=now() WHERE id=$1", id, next); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_config','')"); err != nil {
				return err
			}
			return s.event(ctx, tx, "rollout.wave_released", id, "server", mustJSON(map[string]int64{"wave": int64(next), "targets": releasedCount, "incompatible": incompatible}))
		case configmgmt.Complete:
			return s.finishRollout(ctx, tx, "server", r, configmgmt.Completed)
		case configmgmt.PauseAction:
			if _, err := tx.Exec(ctx, "UPDATE fleet.rollouts SET state='paused',reason='threshold',updated_at=now() WHERE id=$1", id); err != nil {
				return err
			}
			return s.event(ctx, tx, "rollout.paused", id, "server", mustJSON(map[string]string{"reason": "threshold"}))
		case configmgmt.Rollback:
			if err := startRollback(ctx, tx, id, "threshold"); err != nil {
				return err
			}
			return s.event(ctx, tx, "rollout.rolling_back", id, "server", mustJSON(map[string]string{"reason": "threshold"}))
		}
		return nil
	})
}

// withOverride returns base with this rollout's override rebuilt from the
// rows read under its lock.
func withOverride(base []configmgmt.Target, r configmgmt.RolloutView, digest []byte, released []string) []configmgmt.Target {
	targets := slices.Clone(base)
	for i := range targets {
		if targets[i].ID != r.AssignmentID {
			continue
		}
		targets[i].Override = nil
		if r.State.Overriding() {
			o := &configmgmt.Override{RolloutID: r.ID, Version: r.ToVersion, Digest: digest, Released: make(map[string]struct{}, len(released))}
			for _, id := range released {
				o.Released[id] = struct{}{}
			}
			targets[i].Override = o
		}
	}
	return targets
}

// required returns the component types the rollout's version uses, in the
// stored form, or nil when there is nothing to check (a deletion).
func (s *Store) required(ctx context.Context, tx pgx.Tx, r configmgmt.RolloutView) (*string, error) {
	if !r.State.Overriding() || r.ToVersion == 0 {
		return nil, nil
	}
	uses, err := s.uses.Get(r.ConfigID, r.ToVersion, func() ([]byte, error) {
		var body []byte
		err := tx.QueryRow(ctx, "SELECT body FROM fleet.config_versions WHERE config_id=$1 AND version=$2", r.ConfigID, r.ToVersion).Scan(&body)
		return body, err
	})
	if err != nil || len(uses) == 0 {
		return nil, err
	}
	v := string(uses.JSON())
	return &v, nil
}

// observe reads released targets with their agents' state and computes each
// one's desired hash as a replica would. With required set, a target whose
// stored component set lacks one of those types is incompatible.
func (s *Store) observe(ctx context.Context, tx pgx.Tx, id, filter string, targets []configmgmt.Target, credentials map[string]map[string]string, required *string) ([]configmgmt.Observation, error) {
	rows, err := tx.Query(ctx, `SELECT t.agent_id,t.wave,t.outcome,t.observed_at,
		a.id IS NOT NULL AND COALESCE(o.heartbeat_at > now() - $2 * interval '1 millisecond', false),
		COALESCE(a.attributes,'{}'),a.credential_id,COALESCE(a.capabilities,0)::text,a.remote_config_hash,a.remote_config_status,a.config_hold_hash,a.healthy,
		COALESCE(NOT cs.components @> $3::jsonb, false)
		FROM fleet.rollout_targets t LEFT JOIN fleet.agents a ON a.id=t.agent_id LEFT JOIN fleet.replicas o ON o.id=a.owner_replica
		LEFT JOIN fleet.component_sets cs ON cs.hash=a.available_components_hash
		WHERE t.rollout_id=$1 AND t.released_at IS NOT NULL`+filter, id, s.aliveMillis(), required)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []configmgmt.Observation
	for rows.Next() {
		var o configmgmt.Observation
		var attributes map[string]string
		var credential, reported, status, held *string
		var capabilities string
		if err := rows.Scan(&o.AgentID, &o.Wave, &o.Outcome, &o.ObservedAt, &o.Connected, &attributes, &credential, &capabilities, &reported, &status, &held, &o.Healthy, &o.Incompatible); err != nil {
			return nil, err
		}
		o.Status = configmgmt.Status{Hash: decodeHex(deref(reported)), State: configmgmt.State(deref(status))}
		o.Held = decodeHex(deref(held))
		caps, _ := strconv.ParseUint(capabilities, 10, 64)
		labels, allowed := credentials[deref(credential)]
		if allowed && caps&configmgmt.AcceptsRemoteConfig != 0 {
			desired, err := configmgmt.Describe(configmgmt.Match(targets, o.AgentID, attributes, labels), deref(reported))
			switch {
			case err != nil:
				o.Undeliverable = true
			case desired != nil:
				o.Desired = decodeHex(desired.Hash)
			}
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func decodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil
	}
	return b
}

// tallies counts settled targets and unaccepted failures per wave.
func tallies(ctx context.Context, tx pgx.Tx, id string) (map[int]configmgmt.Tally, error) {
	rows, err := tx.Query(ctx, `SELECT wave,
		count(*) FILTER (WHERE outcome IN ('succeeded','unmatched','failed','unhealthy','timed_out')),
		count(*) FILTER (WHERE outcome IN ('failed','unhealthy','timed_out') AND NOT accepted)
		FROM fleet.rollout_targets WHERE rollout_id=$1 AND released_at IS NOT NULL GROUP BY wave`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]configmgmt.Tally{}
	for rows.Next() {
		var wave int
		var t configmgmt.Tally
		if err := rows.Scan(&wave, &t.Settled, &t.Failures); err != nil {
			return nil, err
		}
		out[wave] = t
	}
	return out, rows.Err()
}

func applyUpdates(ctx context.Context, tx pgx.Tx, id string, updates []configmgmt.TargetUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	agents, outcomes, observed := make([]string, len(updates)), make([]string, len(updates)), make([]*time.Time, len(updates))
	withdrawn := false
	for i, u := range updates {
		agents[i], outcomes[i], observed[i] = u.AgentID, string(u.Outcome), u.ObservedAt
		withdrawn = withdrawn || u.Outcome == configmgmt.OutcomeIncompatible
	}
	// An incompatible target is withdrawn from the release, so it returns
	// to its previous desired config.
	_, err := tx.Exec(ctx, `UPDATE fleet.rollout_targets t SET outcome=u.outcome,observed_at=u.observed,
			released_at=CASE WHEN u.outcome='incompatible' THEN NULL ELSE t.released_at END
		FROM unnest($2::text[],$3::text[],$4::timestamptz[]) AS u(agent_id,outcome,observed)
		WHERE t.rollout_id=$1 AND t.agent_id=u.agent_id`, id, agents, outcomes, observed)
	if err != nil || !withdrawn {
		return err
	}
	_, err = tx.Exec(ctx, "SELECT pg_notify('fleet_config','')")
	return err
}
