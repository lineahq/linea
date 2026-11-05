package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

func (s *Store) GetGroup(ctx context.Context, id string) (fleet.Group, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	var g fleet.Group
	var selector *string
	var members int
	err := s.pool.QueryRow(ctx, `SELECT g.id,g.name,g.kind,g.selector,g.revision,count(m.agent_id),
        COALESCE(array_agg(m.agent_id ORDER BY m.agent_id) FILTER(WHERE m.agent_id IS NOT NULL),'{}')
        FROM fleet.groups g LEFT JOIN fleet.group_members m ON m.group_id=g.id WHERE g.id=$1 GROUP BY g.id`, id).Scan(&g.ID, &g.Name, &g.Kind, &selector, &g.Revision, &members, &g.AgentIDs)
	if err != nil {
		return g, normalize(err)
	}
	if selector != nil {
		g.Selector = *selector
		// A detail read counts current matches; list pages do not.
		parsed, err := fleet.ParseSelector(g.Selector)
		if err != nil {
			return g, fleet.ErrUnavailable
		}
		p := &params{}
		where := selectorSQL(parsed, p)
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM fleet.agents a WHERE "+strings.Join(where, " AND "), p.args...).Scan(&members); err != nil {
			return g, normalize(err)
		}
	}
	g.MemberCount = &members
	return g, nil
}
func (s *Store) ListGroups(ctx context.Context, after string, limit int) (fleet.GroupPage, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT g.id,g.name,g.kind,g.selector,g.revision,
		CASE WHEN g.kind='static' THEN (SELECT count(*) FROM fleet.group_members m WHERE m.group_id=g.id) END
		FROM fleet.groups g WHERE g.id>$1 ORDER BY g.id LIMIT $2`, after, limit+1)
	if err != nil {
		return fleet.GroupPage{}, fleet.ErrUnavailable
	}
	defer rows.Close()
	p := fleet.GroupPage{Items: []fleet.Group{}}
	for rows.Next() {
		var g fleet.Group
		var selector *string
		if err := rows.Scan(&g.ID, &g.Name, &g.Kind, &selector, &g.Revision, &g.MemberCount); err != nil {
			return p, normalize(err)
		}
		g.Selector = deref(selector)
		p.Items = append(p.Items, g)
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

// PutGroup stores either explicit members or a selector, never both. The
// selector is stored in canonical form.
func (s *Store) PutGroup(ctx context.Context, actor string, g fleet.Group, expected int64) (fleet.Group, error) {
	g, err := fleet.NormalizeGroup(g)
	switch {
	case err != nil:
		return g, err
	case !auth.SafeName(g.Name):
		return g, fleet.Invalid(auth.NameRule)
	case expected < 0:
		return g, fleet.ErrInvalid
	}
	var selector *string
	if g.Kind == fleet.GroupSelector {
		selector = &g.Selector
	}
	err = s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var revision int64
		err := tx.QueryRow(ctx, "SELECT revision FROM fleet.groups WHERE id=$1", g.ID).Scan(&revision)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if revision != expected {
			return fleet.ErrPrecondition
		}
		if revision == 0 {
			var count int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.groups").Scan(&count); err != nil {
				return err
			}
			if count >= 1000 {
				return fleet.ErrCapacity
			}
		}
		g.Revision = revision + 1
		if _, err := tx.Exec(ctx, `INSERT INTO fleet.groups(id,name,kind,selector,revision) VALUES($1,$2,$3,$4,$5)
			ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,kind=EXCLUDED.kind,selector=EXCLUDED.selector,revision=EXCLUDED.revision`, g.ID, g.Name, g.Kind, selector, g.Revision); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM fleet.group_members WHERE group_id=$1", g.ID); err != nil {
			return err
		}
		if len(g.AgentIDs) > 0 {
			if _, err := tx.Exec(ctx, "INSERT INTO fleet.group_members(group_id,agent_id) SELECT $1,unnest($2::text[])", g.ID, g.AgentIDs); err != nil {
				return err
			}
		}
		// Assignments may target the group; replicas refresh their cache.
		if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_config','')"); err != nil {
			return err
		}
		return s.event(ctx, tx, "group.updated", g.ID, actor, nil)
	})
	if err == nil && g.Kind == fleet.GroupStatic {
		n := len(g.AgentIDs)
		g.MemberCount = &n
	}
	return g, err
}
func (s *Store) DeleteGroup(ctx context.Context, actor, id string, expected int64) error {
	return s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var revision int64
		if err := tx.QueryRow(ctx, "SELECT revision FROM fleet.groups WHERE id=$1", id).Scan(&revision); err != nil {
			return err
		}
		if revision != expected {
			return fleet.ErrPrecondition
		}
		var assigned bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.config_assignments WHERE group_id=$1)", id).Scan(&assigned); err != nil {
			return err
		}
		if assigned {
			return fleet.ErrConflict
		}
		if _, err := tx.Exec(ctx, "DELETE FROM fleet.groups WHERE id=$1", id); err != nil {
			return err
		}
		return s.event(ctx, tx, "group.deleted", id, actor, nil)
	})
}

// Cursors encode a stream position (xid, id) in the current epoch. Readers
// only see events from transactions below the snapshot xmin, so a position
// never skips an event that commits later.
func cursor(epoch string, xid uint64, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v2:" + epoch + ":" + strconv.FormatUint(xid, 10) + ":" + strconv.FormatInt(id, 10)))
}

type position struct {
	xid uint64
	id  int64
}

func (p position) less(q position) bool { return p.xid < q.xid || (p.xid == q.xid && p.id < q.id) }

// errCursor refuses an event cursor this server did not issue.
var errCursor = fleet.Invalid("cursor must be one from a previous event page")

func parseCursor(value string) (string, position, error) {
	if len(value) > 256 {
		return "", position{}, errCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", position{}, errCursor
	}
	parts := strings.Split(string(b), ":")
	if len(parts) == 2 {
		return "", position{}, fleet.ErrCursorExpired // v1 cursor
	}
	if len(parts) != 4 || parts[0] != "v2" {
		return "", position{}, errCursor
	}
	xid, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return "", position{}, errCursor
	}
	id, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || id < 0 {
		return "", position{}, errCursor
	}
	return parts[1], position{xid: xid, id: id}, nil
}

func (s *Store) Events(ctx context.Context, from string, limit int) (fleet.EventPage, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fleet.EventPage{}, fleet.ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	var epoch, floorXID, xmin string
	var floorID, lastID int64
	if err := tx.QueryRow(ctx, `SELECT event_epoch,event_floor_xid::text,event_floor_id,pg_snapshot_xmin(pg_current_snapshot())::text,
		(SELECT last_value FROM fleet.events_id_seq) FROM fleet.metadata WHERE singleton`).Scan(&epoch, &floorXID, &floorID, &xmin, &lastID); err != nil {
		return fleet.EventPage{}, normalize(err)
	}
	floorX, err1 := strconv.ParseUint(floorXID, 10, 64)
	horizon, err2 := strconv.ParseUint(xmin, 10, 64)
	if err1 != nil || err2 != nil {
		return fleet.EventPage{}, fleet.ErrUnavailable
	}
	floor := position{xid: floorX, id: floorID}
	after := floor
	if from != "" {
		clientEpoch, p, err := parseCursor(from)
		if err != nil {
			return fleet.EventPage{}, err
		}
		if clientEpoch != epoch || p.less(floor) || p.xid >= horizon || p.id > lastID {
			return fleet.EventPage{}, fleet.ErrCursorExpired
		}
		after = p
	}
	rows, err := tx.Query(ctx, `SELECT xid::text AS position_xid,id,kind,subject_id,actor_id,occurred_at,detail,acting_for FROM fleet.events
		WHERE (xid,id) > ($1::text::xid8,$2) AND xid < pg_snapshot_xmin(pg_current_snapshot()) ORDER BY xid,id LIMIT $3`,
		strconv.FormatUint(after.xid, 10), after.id, limit)
	if err != nil {
		return fleet.EventPage{}, normalize(err)
	}
	p := fleet.EventPage{Items: []fleet.Event{}}
	for rows.Next() {
		var e fleet.Event
		var xid string
		var detail, acting []byte
		if err := rows.Scan(&xid, &e.ID, &e.Kind, &e.SubjectID, &e.ActorID, &e.OccurredAt, &detail, &acting); err != nil {
			rows.Close()
			return p, normalize(err)
		}
		if after.xid, err = strconv.ParseUint(xid, 10, 64); err != nil {
			rows.Close()
			return p, fleet.ErrUnavailable
		}
		if detail != nil {
			e.Detail = json.RawMessage(detail)
		}
		if acting != nil {
			if err := json.Unmarshal(acting, &e.ActingFor); err != nil {
				rows.Close()
				return p, fleet.ErrUnavailable
			}
		}
		after.id = e.ID
		p.Items = append(p.Items, e)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return p, normalize(rowErr)
	}
	p.NextCursor = cursor(epoch, after.xid, after.id)
	return p, normalize(tx.Commit(ctx))
}
