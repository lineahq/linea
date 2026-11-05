package postgres

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// expireBatch bounds one retention transaction.
const expireBatch = 500

// Presence is computed from the owner's liveness at read time. Every agent
// query binds the liveness window in milliseconds as $1.
const agentColumns = `a.id,a.generation,COALESCE(o.heartbeat_at > now() - $1 * interval '1 millisecond', false),
	a.first_seen_at,a.connected_at,a.disconnected_at,o.heartbeat_at,a.last_seen_at,a.capabilities::text,a.healthy,a.health_observed_at,a.identity_conflict,
	a.credential_id,a.attributes,a.health_status,a.health_error,a.components_unhealthy,a.config_hash,
	a.remote_config_hash,a.remote_config_status,a.remote_config_error,a.remote_config_at,a.config_hold_hash,a.available_components_hash`
const agentFrom = " FROM fleet.agents a LEFT JOIN fleet.replicas o ON o.id=a.owner_replica"
const aliveFilter = "COALESCE(o.heartbeat_at > now() - $1 * interval '1 millisecond', false)"

// scanAgent reads agentColumns, followed by components for a detail read.
func scanAgent(row scanner, detail bool) (fleet.Agent, error) {
	var a fleet.Agent
	var generation int64
	var caps string
	var healthy *bool
	var observed, ownerHeartbeat *time.Time
	var credential, status, healthError, hash *string
	var rcHash, rcStatus, rcError, rcHeld, available *string
	var rcAt *time.Time
	var unhealthy int
	var attributes, components []byte
	targets := []any{&a.ID, &generation, &a.Connected, &a.FirstSeenAt, &a.ConnectedAt, &a.DisconnectedAt, &ownerHeartbeat, &a.LastSeenAt, &caps, &healthy, &observed, &a.IdentityConflict,
		&credential, &attributes, &status, &healthError, &unhealthy, &hash, &rcHash, &rcStatus, &rcError, &rcAt, &rcHeld, &available}
	if detail {
		targets = append(targets, &components)
	}
	if err := row.Scan(targets...); err != nil {
		return a, err
	}
	a.Generation = uint64(generation)
	a.Presence = "disconnected"
	if a.Connected {
		a.Presence, a.DisconnectedAt = "connected", nil
	} else if a.DisconnectedAt == nil {
		// The owner died and the leader has not reaped it yet.
		a.DisconnectedAt = ownerHeartbeat
	}
	a.LastReportAt = a.LastSeenAt
	var err error
	if a.Capabilities, err = strconv.ParseUint(caps, 10, 64); err != nil {
		return a, err
	}
	if healthy != nil && observed != nil {
		a.Health = &fleet.Health{Healthy: *healthy, ObservedAt: *observed, Status: deref(status), Error: deref(healthError), ComponentsUnhealthy: unhealthy}
	}
	a.CredentialID, a.ConfigHash, a.AvailableComponentsHash = deref(credential), deref(hash), deref(available)
	if rc := remoteConfig(rcHash, rcStatus, rcError, rcHeld); rc != nil {
		a.RemoteConfig = &fleet.RemoteConfigState{RemoteConfig: *rc}
		if rcAt != nil {
			a.RemoteConfig.ObservedAt = *rcAt
		}
	}
	a.Attributes = map[string]string{}
	if err := json.Unmarshal(attributes, &a.Attributes); err != nil {
		return a, err
	}
	if components != nil {
		if err := json.Unmarshal(components, &a.Components); err != nil {
			return a, err
		}
	}
	return a, nil
}

// remoteConfig returns nil for an agent that never reported a remote config
// status and has no hold.
func remoteConfig(hash, status, message, held *string) *fleet.RemoteConfig {
	if hash == nil && status == nil && message == nil && held == nil {
		return nil
	}
	return &fleet.RemoteConfig{Hash: deref(hash), Status: deref(status), Error: deref(message), Held: deref(held)}
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func (s *Store) aliveMillis() int64 { return s.options.Alive.Milliseconds() }

func (s *Store) GetAgent(ctx context.Context, id string) (fleet.Agent, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	a, err := scanAgent(s.pool.QueryRow(ctx, "SELECT "+agentColumns+",a.components"+agentFrom+" WHERE a.id=$2", s.aliveMillis(), id), true)
	return a, normalize(err)
}

// params collects positional arguments for a generated statement.
type params struct{ args []any }

func (p *params) add(v any) string {
	p.args = append(p.args, v)
	return "$" + strconv.Itoa(len(p.args))
}

// selectorSQL renders selector terms: promoted keys use their indexed column,
// other keys use GIN containment and existence on attributes.
func selectorSQL(sel fleet.Selector, p *params) []string {
	where := make([]string, 0, len(sel.Terms))
	contains := func(key, value string) string {
		b, _ := json.Marshal(map[string]string{key: value})
		return "a.attributes @> " + p.add(string(b)) + "::jsonb"
	}
	for _, t := range sel.Terms {
		column, promoted := fleet.Promoted[t.Key]
		switch {
		case promoted && t.Op == "exists":
			where = append(where, "a."+column+"<>''")
		case promoted && t.Op == "=":
			where = append(where, "a."+column+"="+p.add(t.Values[0]))
		case promoted:
			where = append(where, "a."+column+"=ANY("+p.add(t.Values)+"::text[])")
		case t.Op == "exists":
			where = append(where, "a.attributes ? "+p.add(t.Key))
		default:
			any := make([]string, len(t.Values))
			for i, v := range t.Values {
				any[i] = contains(t.Key, v)
			}
			where = append(where, "("+strings.Join(any, " OR ")+")")
		}
	}
	return where
}

// ListAgents returns one keyset page in id order, or in (attribute, id) order
// when the query sorts by a promoted attribute.
func (s *Store) ListAgents(ctx context.Context, q fleet.AgentQuery) (fleet.Page, error) {
	if q.Limit < 1 {
		return fleet.Page{}, fleet.ErrInvalid
	}
	p := &params{}
	p.add(s.aliveMillis())
	where := selectorSQL(q.Selector, p)
	order := "a.id"
	sortColumn := ""
	if q.Sort != "" {
		column, ok := fleet.Promoted[q.Sort]
		if !ok {
			return fleet.Page{}, fleet.Invalid("sort " + fleet.PromotedRule)
		}
		sortColumn = "a." + column
		order = sortColumn + ",a.id"
	}
	if q.After != "" {
		if sortColumn == "" {
			where = append(where, "a.id>"+p.add(q.After))
		} else {
			value, id, err := fleet.DecodeAfter(q.After)
			if err != nil {
				return fleet.Page{}, err
			}
			where = append(where, "("+sortColumn+",a.id)>("+p.add(value)+"::text,"+p.add(id)+"::text)")
		}
	}
	if q.Connected != nil {
		where = append(where, aliveFilter+"="+p.add(*q.Connected)+"::boolean")
	}
	if q.Healthy != nil {
		where = append(where, "a.healthy="+p.add(*q.Healthy)+"::boolean")
	}
	if q.Credential != "" {
		where = append(where, "a.credential_id="+p.add(q.Credential))
	}
	if q.Group != "" {
		where = append(where, "EXISTS(SELECT 1 FROM fleet.group_members m WHERE m.group_id="+p.add(q.Group)+" AND m.agent_id=a.id)")
	}
	query := "SELECT " + agentColumns + agentFrom
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY " + order + " LIMIT " + p.add(q.Limit+1)
	ctx, cancel := bounded(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, query, p.args...)
	if err != nil {
		return fleet.Page{}, fleet.ErrUnavailable
	}
	defer rows.Close()
	page := fleet.Page{Items: []fleet.Agent{}}
	for rows.Next() {
		a, err := scanAgent(rows, false)
		if err != nil {
			return page, normalize(err)
		}
		page.Items = append(page.Items, a)
	}
	if err := rows.Err(); err != nil {
		return page, normalize(err)
	}
	if len(page.Items) > q.Limit {
		page.Items = page.Items[:q.Limit]
		last := page.Items[q.Limit-1]
		page.NextAfter = last.ID
		if q.Sort != "" {
			page.NextAfter = fleet.EncodeAfter(last.Attributes[q.Sort], last.ID)
		}
	}
	return page, nil
}

// Summary counts presence and, with groupBy, the largest groups of one
// promoted attribute; agents without it form the "" group.
func (s *Store) Summary(ctx context.Context, groupBy string) (fleet.Summary, error) {
	column, ok := fleet.Promoted[groupBy]
	if groupBy != "" && !ok {
		return fleet.Summary{}, fleet.Invalid("group_by " + fleet.PromotedRule)
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	var r fleet.Summary
	if err := s.pool.QueryRow(ctx, "SELECT count(*),count(*) FILTER(WHERE "+aliveFilter+")"+agentFrom, s.aliveMillis()).Scan(&r.Total, &r.Connected); err != nil {
		return r, normalize(err)
	}
	r.Disconnected = r.Total - r.Connected
	if groupBy == "" {
		return r, nil
	}
	rows, err := s.pool.Query(ctx, "SELECT a."+column+",count(*),count(*) FILTER(WHERE "+aliveFilter+")"+agentFrom+
		" GROUP BY a."+column+" ORDER BY count(*) DESC,a."+column+" LIMIT $2", s.aliveMillis(), fleet.MaxSummaryGroups)
	if err != nil {
		return r, fleet.ErrUnavailable
	}
	defer rows.Close()
	r.Groups = []fleet.SummaryGroup{}
	for rows.Next() {
		var g fleet.SummaryGroup
		if err := rows.Scan(&g.Value, &g.Total, &g.Connected); err != nil {
			return r, normalize(err)
		}
		r.Groups = append(r.Groups, g)
	}
	return r, normalize(rows.Err())
}

// expire deletes up to limit agents without an owner, longest disconnected
// first and optionally only before a cutoff. Static group memberships go with
// them, bumping each affected group's revision, and one agents.expired event
// records the count. Locked agents are skipped, never waited for.
func expire(ctx context.Context, tx pgx.Tx, before *time.Time, limit int, reason string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM fleet.agents WHERE owner_replica IS NULL AND ($1::timestamptz IS NULL OR disconnected_at < $1)
		ORDER BY disconnected_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`, before, limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	rows, err = tx.Query(ctx, "DELETE FROM fleet.group_members WHERE agent_id=ANY($1) RETURNING group_id", ids)
	if err != nil {
		return 0, err
	}
	groups, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	slices.Sort(groups)
	groups = slices.Compact(groups)
	if len(groups) > 0 {
		if _, err := tx.Exec(ctx, "UPDATE fleet.groups SET revision=revision+1 WHERE id=ANY($1)", groups); err != nil {
			return 0, err
		}
		for _, g := range groups {
			if err := appendEvent(ctx, tx, "group.updated", g, "server", nil); err != nil {
				return 0, err
			}
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM fleet.agents WHERE id=ANY($1)", ids); err != nil {
		return 0, err
	}
	detail := map[string]any{"count": len(ids)}
	if reason != "" {
		detail["reason"] = reason
	}
	b, _ := json.Marshal(detail)
	return len(ids), appendEvent(ctx, tx, "agents.expired", "agents", "server", b)
}

// retain removes agents disconnected longer than the retention period.
func (s *Store) retain(ctx context.Context) error {
	if s.options.Retention <= 0 {
		return nil
	}
	before := time.Now().Add(-s.options.Retention)
	var n int
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = expire(ctx, tx, &before, expireBatch, "")
		return err
	})
	if err == nil {
		s.expired.Add(uint64(n))
	}
	return err
}
