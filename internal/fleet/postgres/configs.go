package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Config management storage (M6). Bodies are read only through GetVersion
// with body=true and never enter events, notifications or errors.

const configColumns = "id,name,description,revision,latest_version,created_at,created_by"

func scanConfig(row pgx.Row) (configmgmt.Config, error) {
	var c configmgmt.Config
	err := row.Scan(&c.ID, &c.Name, &c.Description, &c.Revision, &c.LatestVersion, &c.CreatedAt, &c.CreatedBy)
	return c, err
}

func validConfig(c configmgmt.Config) error {
	switch {
	case !fleet.ValidID(c.ID):
		return fleet.Invalid("id " + fleet.IDRule)
	case !configmgmt.ValidAssignmentName(c.Name):
		return fleet.Invalid(configmgmt.AssignmentNameRule)
	case !configmgmt.ValidText(c.Description):
		return fleet.Invalid("description " + configmgmt.TextRule)
	}
	return nil
}

func (s *Store) CreateConfig(ctx context.Context, actor string, c configmgmt.Config) (configmgmt.Config, error) {
	if err := validConfig(c); err != nil {
		return c, err
	}
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.configs").Scan(&count); err != nil {
			return err
		}
		if count >= configmgmt.MaxConfigs {
			return fleet.ErrCapacity
		}
		var err error
		c, err = scanConfig(tx.QueryRow(ctx, `INSERT INTO fleet.configs(id,name,description,revision,created_at,created_by)
			VALUES($1,$2,$3,1,now(),$4) RETURNING `+configColumns, c.ID, c.Name, c.Description, actor))
		if err != nil {
			return err
		}
		return s.event(ctx, tx, "config.created", c.ID, actor, nil)
	})
	return c, err
}

func (s *Store) GetConfig(ctx context.Context, id string) (configmgmt.Config, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	c, err := scanConfig(s.pool.QueryRow(ctx, "SELECT "+configColumns+" FROM fleet.configs WHERE id=$1", id))
	return c, normalize(err)
}

func (s *Store) ListConfigs(ctx context.Context, after string, limit int) (configmgmt.ConfigPage, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	p := configmgmt.ConfigPage{Items: []configmgmt.Config{}}
	rows, err := s.pool.Query(ctx, "SELECT "+configColumns+" FROM fleet.configs WHERE id>$1 ORDER BY id LIMIT $2", after, limit+1)
	if err != nil {
		return p, normalize(err)
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return p, normalize(err)
		}
		p.Items = append(p.Items, c)
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

// UpdateConfig changes a config's name and description.
func (s *Store) UpdateConfig(ctx context.Context, actor string, c configmgmt.Config, expected int64) (configmgmt.Config, error) {
	if err := validConfig(c); err != nil {
		return c, err
	}
	if expected < 1 {
		return c, fleet.ErrInvalid
	}
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var revision int64
		if err := tx.QueryRow(ctx, "SELECT revision FROM fleet.configs WHERE id=$1 FOR UPDATE", c.ID).Scan(&revision); err != nil {
			return err
		}
		if revision != expected {
			return fleet.ErrPrecondition
		}
		var err error
		c, err = scanConfig(tx.QueryRow(ctx, "UPDATE fleet.configs SET name=$2,description=$3,revision=revision+1 WHERE id=$1 RETURNING "+configColumns, c.ID, c.Name, c.Description))
		if err != nil {
			return err
		}
		return s.event(ctx, tx, "config.updated", c.ID, actor, nil)
	})
	return c, err
}

// DeleteConfig removes a config and its versions while no assignment
// references it.
func (s *Store) DeleteConfig(ctx context.Context, actor, id string, expected int64) error {
	return s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var revision int64
		if err := tx.QueryRow(ctx, "SELECT revision FROM fleet.configs WHERE id=$1 FOR UPDATE", id).Scan(&revision); err != nil {
			return err
		}
		if revision != expected {
			return fleet.ErrPrecondition
		}
		var referenced bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.config_assignments WHERE config_id=$1)", id).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return fleet.ErrConflict
		}
		if _, err := tx.Exec(ctx, "DELETE FROM fleet.configs WHERE id=$1", id); err != nil {
			return err
		}
		return s.event(ctx, tx, "config.deleted", id, actor, nil)
	})
}

// CreateVersion stores the next version of a config. The caller validates
// the body; the store bounds it again. Versions beyond the newest
// KeepVersions are pruned unless an assignment references them.
func (s *Store) CreateVersion(ctx context.Context, actor, configID string, body []byte, message string) (configmgmt.Version, error) {
	v := configmgmt.Version{ConfigID: configID, Size: len(body), Message: message, CreatedBy: actor}
	switch {
	case !fleet.ValidID(configID):
		return v, fleet.Invalid("config id " + fleet.IDRule)
	case len(body) == 0 || len(body) > configmgmt.MaxBodyBytes:
		return v, fleet.Invalid(fmt.Sprintf("the body must be 1 byte to %d KiB", configmgmt.MaxBodyBytes/1024))
	case !configmgmt.ValidText(message):
		return v, fleet.Invalid("message " + configmgmt.TextRule)
	}
	digest := sha256.Sum256(body)
	v.SHA256 = hex.EncodeToString(digest[:])
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "UPDATE fleet.configs SET latest_version=latest_version+1 WHERE id=$1 RETURNING latest_version", configID).Scan(&v.Version); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO fleet.config_versions(config_id,version,sha256,body,message,created_at,created_by)
			VALUES($1,$2,$3,$4,$5,now(),$6) RETURNING created_at`, configID, v.Version, digest[:], body, message, actor).Scan(&v.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM fleet.config_versions v WHERE v.config_id=$1 AND v.version<=$2
			AND NOT EXISTS(SELECT 1 FROM fleet.config_assignments a WHERE a.config_id=v.config_id AND a.version=v.version)
			AND NOT EXISTS(SELECT 1 FROM fleet.rollouts r WHERE r.config_id=v.config_id AND v.version IN (r.from_version, r.to_version)
				AND r.state IN ('running','paused','rolling_back'))`, configID, v.Version-configmgmt.KeepVersions); err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]any{"version": v.Version, "sha256": v.SHA256})
		return s.event(ctx, tx, "config.version_created", configID, actor, detail)
	})
	return v, err
}

func (s *Store) GetVersion(ctx context.Context, configID string, version int, body bool) (configmgmt.Version, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	var v configmgmt.Version
	var digest, content []byte
	err := s.pool.QueryRow(ctx, `SELECT config_id,version,sha256,octet_length(body),message,created_at,created_by,CASE WHEN $3 THEN body END
		FROM fleet.config_versions WHERE config_id=$1 AND version=$2`, configID, version, body).Scan(&v.ConfigID, &v.Version, &digest, &v.Size, &v.Message, &v.CreatedAt, &v.CreatedBy, &content)
	if err != nil {
		return v, normalize(err)
	}
	v.SHA256 = hex.EncodeToString(digest)
	if body {
		text := string(content)
		v.Body = &text
	}
	return v, nil
}

// ListVersions pages a config's versions newest first; after is a version
// number, 0 for the first page.
func (s *Store) ListVersions(ctx context.Context, configID string, after, limit int) (configmgmt.VersionPage, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	p := configmgmt.VersionPage{Items: []configmgmt.Version{}}
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.configs WHERE id=$1)", configID).Scan(&exists); err != nil {
		return p, normalize(err)
	}
	if !exists {
		return p, fleet.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT config_id,version,sha256,octet_length(body),message,created_at,created_by FROM fleet.config_versions
		WHERE config_id=$1 AND ($2=0 OR version<$2) ORDER BY version DESC LIMIT $3`, configID, after, limit+1)
	if err != nil {
		return p, normalize(err)
	}
	defer rows.Close()
	for rows.Next() {
		var v configmgmt.Version
		var digest []byte
		if err := rows.Scan(&v.ConfigID, &v.Version, &digest, &v.Size, &v.Message, &v.CreatedAt, &v.CreatedBy); err != nil {
			return p, normalize(err)
		}
		v.SHA256 = hex.EncodeToString(digest)
		p.Items = append(p.Items, v)
	}
	if err := rows.Err(); err != nil {
		return p, normalize(err)
	}
	if len(p.Items) > limit {
		p.Items = p.Items[:limit]
		p.NextAfter = strconv.Itoa(p.Items[limit-1].Version)
	}
	return p, nil
}

// assignmentColumns are qualified by "a"; assignmentFrom joins the active
// rollout, whose ID ends the column list.
const assignmentColumns = "a.id,a.name,a.config_id,a.version,a.layer,a.group_id,a.selector,a.credential_labels,a.revision,a.created_at,a.created_by,r.id"
const assignmentFrom = ` FROM fleet.config_assignments a
	LEFT JOIN fleet.rollouts r ON r.assignment_id=a.id AND r.state IN ('running','paused','rolling_back')`

func scanAssignment(row pgx.Row, extra ...any) (configmgmt.Assignment, error) {
	var a configmgmt.Assignment
	var group, selector, rollout *string
	var version *int
	var labels []byte
	dest := append([]any{&a.ID, &a.Name, &a.ConfigID, &version, &a.Layer, &group, &selector, &labels, &a.Revision, &a.CreatedAt, &a.CreatedBy, &rollout}, extra...)
	if err := row.Scan(dest...); err != nil {
		return a, err
	}
	a.GroupID, a.Selector, a.RolloutID = deref(group), deref(selector), deref(rollout)
	if version != nil {
		a.Version = *version
	}
	if err := json.Unmarshal(labels, &a.CredentialLabels); err != nil {
		return a, fleet.ErrUnavailable
	}
	if len(a.CredentialLabels) == 0 {
		a.CredentialLabels = nil
	}
	return a, nil
}

// assignmentChanged records an assignment change and wakes every replica's
// assignment cache.
func (s *Store) assignmentChanged(ctx context.Context, tx pgx.Tx, actor, action string, a configmgmt.Assignment) error {
	if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_config','')"); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"action": action, "config_id": a.ConfigID, "version": a.Version, "layer": a.Layer})
	return s.event(ctx, tx, "config.assignment_changed", a.ID, actor, detail)
}

func (s *Store) GetAssignment(ctx context.Context, id string) (configmgmt.Assignment, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	a, err := scanAssignment(s.pool.QueryRow(ctx, "SELECT "+assignmentColumns+assignmentFrom+" WHERE a.id=$1", id))
	return a, normalize(err)
}

func (s *Store) ListAssignments(ctx context.Context, after string, limit int) (configmgmt.AssignmentPage, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	p := configmgmt.AssignmentPage{Items: []configmgmt.Assignment{}}
	rows, err := s.pool.Query(ctx, "SELECT "+assignmentColumns+assignmentFrom+" WHERE a.id>$1 ORDER BY a.id LIMIT $2", after, limit+1)
	if err != nil {
		return p, normalize(err)
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAssignment(rows)
		if err != nil {
			return p, normalize(err)
		}
		p.Items = append(p.Items, a)
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

// Targets loads every assignment with its version digest, resolved target
// and overriding rollout in one consistent read: at most MaxAssignments
// rows, each with at most fleet.MaxGroupMembers static members, and the
// released targets of running or paused rollouts, at most one per agent.
func (s *Store) Targets(ctx context.Context) ([]configmgmt.Target, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fleet.ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	targets, err := loadTargets(ctx, tx)
	if err != nil {
		return nil, normalize(err)
	}
	return targets, normalize(tx.Commit(ctx))
}

func loadTargets(ctx context.Context, tx pgx.Tx) ([]configmgmt.Target, error) {
	rows, err := tx.Query(ctx, `SELECT `+assignmentColumns+`,
		v.sha256,g.selector,CASE WHEN g.kind='static' THEN ARRAY(SELECT m.agent_id FROM fleet.group_members m WHERE m.group_id=g.id) END`+assignmentFrom+`
		LEFT JOIN fleet.config_versions v ON v.config_id=a.config_id AND v.version=a.version
		LEFT JOIN fleet.groups g ON g.id=a.group_id ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	var targets []configmgmt.Target
	index := map[string]int{}
	for rows.Next() {
		var digest []byte
		var groupSelector *string
		var members []string
		a, err := scanAssignment(rows, &digest, &groupSelector, &members)
		if err != nil {
			rows.Close()
			return nil, err
		}
		t, err := configmgmt.NewTarget(a, digest, deref(groupSelector), members)
		if err != nil {
			rows.Close()
			return nil, fleet.ErrUnavailable
		}
		index[a.ID] = len(targets)
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT r.id,r.assignment_id,COALESCE(r.to_version,0),v.sha256,
		ARRAY(SELECT t.agent_id FROM fleet.rollout_targets t WHERE t.rollout_id=r.id AND t.released_at IS NOT NULL)
		FROM fleet.rollouts r LEFT JOIN fleet.config_versions v ON v.config_id=r.config_id AND v.version=r.to_version
		WHERE r.state IN ('running','paused')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var o configmgmt.Override
		var assignment string
		var released []string
		if err := rows.Scan(&o.RolloutID, &assignment, &o.Version, &o.Digest, &released); err != nil {
			return nil, err
		}
		i, ok := index[assignment]
		if !ok {
			continue
		}
		o.Released = make(map[string]struct{}, len(released))
		for _, id := range released {
			o.Released[id] = struct{}{}
		}
		targets[i].Override = &o
	}
	return targets, rows.Err()
}

// ClearHold removes an agent's failure hold. The notification names the
// agent so its owning replica drops the hold it carries in the session.
func (s *Store) ClearHold(ctx context.Context, actor, agentID string) error {
	return s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var held *string
		if err := tx.QueryRow(ctx, "SELECT config_hold_hash FROM fleet.agents WHERE id=$1 FOR UPDATE", agentID).Scan(&held); err != nil {
			return err
		}
		if held == nil {
			return nil
		}
		if _, err := tx.Exec(ctx, "UPDATE fleet.agents SET config_hold_hash=NULL WHERE id=$1", agentID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_config',$1)", agentID); err != nil {
			return err
		}
		return s.event(ctx, tx, "config.hold_cleared", agentID, actor, mustJSON(map[string]string{"held_hash": *held}))
	})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(errors.New("unencodable event detail"))
	}
	return b
}

// ManagedCredentials returns the labels of active agent credentials that
// allow remote config. Credentials are capped at 1,000.
func (s *Store) ManagedCredentials(ctx context.Context) (map[string]map[string]string, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, "SELECT id,labels FROM fleet.credentials WHERE role='agent' AND remote_config AND revoked_at IS NULL AND expires_at>clock_timestamp()")
	if err != nil {
		return nil, normalize(err)
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var id string
		var labels map[string]string
		if err := rows.Scan(&id, &labels); err != nil {
			return nil, normalize(err)
		}
		out[id] = labels
	}
	return out, normalize(rows.Err())
}

// SetConfigHold moves an owned agent's failure hold from expected to next and
// returns the hold now stored. A hold changed meanwhile, for example by
// ClearHold, is returned unchanged. A session this replica no longer owns at
// generation gets ErrStale.
func (s *Store) SetConfigHold(ctx context.Context, id string, generation uint64, expected, next string) (string, error) {
	current := expected
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var held *string
		err := tx.QueryRow(ctx, "SELECT config_hold_hash FROM fleet.agents WHERE id=$1 AND generation=$2 AND owner_replica=$3 FOR UPDATE", id, int64(generation), s.replica).Scan(&held)
		if errors.Is(err, pgx.ErrNoRows) {
			return fleet.ErrStale
		}
		if err != nil {
			return err
		}
		current = deref(held)
		if current != expected || current == next {
			return nil
		}
		if _, err := tx.Exec(ctx, "UPDATE fleet.agents SET config_hold_hash=$2 WHERE id=$1", id, nonEmpty(next)); err != nil {
			return err
		}
		current = next
		if next == "" {
			return s.event(ctx, tx, "config.hold_cleared", id, "server", mustJSON(map[string]string{"held_hash": expected}))
		}
		return s.event(ctx, tx, "config.held", id, "server", mustJSON(map[string]string{"held_hash": next}))
	})
	return current, err
}

// ConfigHolds returns the holds of agents this replica owns, so sessions
// pick up a cleared hold even when its notification was missed.
func (s *Store) ConfigHolds(ctx context.Context) (map[string]string, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, "SELECT id,config_hold_hash FROM fleet.agents WHERE owner_replica=$1 AND config_hold_hash IS NOT NULL", s.replica)
	if err != nil {
		return nil, normalize(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, held string
		if err := rows.Scan(&id, &held); err != nil {
			return nil, normalize(err)
		}
		out[id] = held
	}
	return out, normalize(rows.Err())
}
