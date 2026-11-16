package postgres

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

var auditRetention = strconv.Itoa(fleet.AuditRetentionDays) + " days"

// Audit reads the administrative record, newest first.
func (s *Store) Audit(ctx context.Context, q fleet.AuditQuery) (fleet.AuditPage, error) {
	if q.Limit < 1 || q.Limit > fleet.MaxAuditPage || q.Before < 0 {
		return fleet.AuditPage{}, fleet.ErrInvalid
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	var since, until any
	if !q.Since.IsZero() {
		since = q.Since
	}
	if !q.Until.IsZero() {
		until = q.Until
	}
	rows, err := s.pool.Query(ctx, `SELECT id,kind,subject_id,actor_id,occurred_at,detail,acting_for FROM fleet.audit
		WHERE ($1::bigint = 0 OR id < $1) AND ($2 = '' OR kind = $2) AND ($3 = '' OR actor_id = $3)
		AND ($4::timestamptz IS NULL OR occurred_at >= $4) AND ($5::timestamptz IS NULL OR occurred_at < $5)
		ORDER BY id DESC LIMIT $6`, q.Before, q.Kind, q.Actor, since, until, q.Limit+1)
	if err != nil {
		return fleet.AuditPage{}, normalize(err)
	}
	defer rows.Close()
	page := fleet.AuditPage{Items: []fleet.Event{}}
	for rows.Next() {
		var e fleet.Event
		var detail, acting []byte
		if err := rows.Scan(&e.ID, &e.Kind, &e.SubjectID, &e.ActorID, &e.OccurredAt, &detail, &acting); err != nil {
			return fleet.AuditPage{}, normalize(err)
		}
		if detail != nil {
			e.Detail = json.RawMessage(detail)
		}
		if acting != nil {
			if err := json.Unmarshal(acting, &e.ActingFor); err != nil {
				return fleet.AuditPage{}, fleet.ErrUnavailable
			}
		}
		page.Items = append(page.Items, e)
	}
	if err := rows.Err(); err != nil {
		return fleet.AuditPage{}, normalize(err)
	}
	if len(page.Items) > q.Limit {
		page.Items = page.Items[:q.Limit]
		page.NextBefore = strconv.FormatInt(page.Items[q.Limit-1].ID, 10)
	}
	return page, nil
}

// retainAudit deletes records past their retention, then the oldest above
// the row cap, one bounded batch each per leader tick.
func (s *Store) retainAudit(ctx context.Context) error {
	return s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM fleet.audit WHERE id IN (
			SELECT id FROM fleet.audit WHERE occurred_at < now() - interval '`+auditRetention+`' ORDER BY id LIMIT $1)`, trimBatch); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM fleet.audit WHERE id IN (
			SELECT id FROM fleet.audit WHERE id <= (SELECT max(id) FROM fleet.audit) - $1 ORDER BY id LIMIT $2)`, fleet.MaxAuditRows, trimBatch)
		return err
	})
}
