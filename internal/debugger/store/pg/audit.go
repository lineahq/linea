package pg

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// Bounds of one event's fields; AppendAudit refuses anything larger.
const (
	maxAuditField     = 256
	maxAuditResources = 16
	// ageEvery is how often, in events, an append also deletes events past
	// Limits.AuditAge; reads apply the age bound regardless.
	ageEvery = 256
)

func validAudit(e artifact.AuditEvent) error {
	fields := []string{e.Principal, e.PrincipalKind, e.Client, e.Operation, e.Outcome}
	for _, f := range fields {
		if f == "" {
			return invalid("audit event needs a principal, kind, client, operation and outcome")
		}
	}
	if e.Outcome != artifact.AuditAllowed && e.Outcome != artifact.AuditDenied {
		return invalid("audit outcome must be allowed or denied")
	}
	// Unix nanoseconds must hold the time (years 1678 to 2262).
	if !validText(append(append(fields, e.RequestID, e.Permission, e.Scope, e.Code), e.Resources...)...) {
		return invalid("audit field is not valid text")
	}
	if e.Time.IsZero() || len(e.Resources) > maxAuditResources || e.Time.Year() < 1700 || e.Time.Year() > 2200 {
		return invalid("audit event needs a time and at most 16 resources")
	}
	for _, f := range append(append(fields, e.RequestID, e.Permission, e.Scope, e.Code), e.Resources...) {
		if len(f) > maxAuditField || strings.ContainsFunc(f, unicode.IsControl) {
			return invalid("audit field too long or not printable")
		}
	}
	return nil
}

// AppendAudit records an event and returns it with its sequence, one higher
// than the previous event's. The head row's lock, held until commit, makes
// the sequence gapless and equal to commit order across instances.
func (s *Store) AppendAudit(ctx context.Context, e artifact.AuditEvent) (artifact.AuditEvent, error) {
	if err := validAudit(e); err != nil {
		return artifact.AuditEvent{}, err
	}
	if err := s.ready(ctx); err != nil {
		return artifact.AuditEvent{}, err
	}
	e.Time, e.Resources = e.Time.UTC(), slices.Clone(e.Resources)
	err := s.inTx(ctx, "audit", func(tx pgx.Tx) error {
		var seq int64
		if err := tx.QueryRow(ctx, "UPDATE audit_head SET seq = seq + 1 RETURNING seq").Scan(&seq); err != nil {
			return err
		}
		resources := e.Resources
		if resources == nil {
			resources = []string{}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_events (seq, at_ns, principal, principal_kind, client, request_id, operation, permission, scope, resources, outcome, code)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`, seq, e.Time.UnixNano(), e.Principal, e.PrincipalKind, e.Client, e.RequestID,
			e.Operation, e.Permission, e.Scope, resources, e.Outcome, e.Code); err != nil {
			return err
		}
		// Retention: events beyond the newest AuditEvents are gone, and past
		// AuditAge now and then. Reads apply both bounds themselves.
		if _, err := tx.Exec(ctx, "DELETE FROM audit_events WHERE seq <= $1", seq-int64(s.limits.AuditEvents)); err != nil {
			return err
		}
		if seq%ageEvery == 0 {
			if _, err := tx.Exec(ctx, "DELETE FROM audit_events WHERE at_ns < $1", time.Now().Add(-s.limits.AuditAge).UnixNano()); err != nil {
				return err
			}
		}
		e.Seq = uint64(seq)
		return nil
	})
	if err != nil {
		return artifact.AuditEvent{}, err
	}
	return e, nil
}

// AuditEvents returns the events q selects, newest first, within retention.
func (s *Store) AuditEvents(ctx context.Context, q artifact.AuditQuery) ([]artifact.AuditEvent, error) {
	if q.Limit <= 0 {
		return nil, invalid("audit query needs a positive limit")
	}
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	since := time.Now().Add(-s.limits.AuditAge)
	if q.Since.After(since) {
		since = q.Since
	}
	rows, err := s.pool.Query(ctx, `SELECT e.seq, e.at_ns, e.principal, e.principal_kind, e.client, e.request_id, e.operation, e.permission, e.scope, e.resources, e.outcome, e.code
		FROM audit_events e, audit_head h
		WHERE e.seq > h.seq - $1 AND ($2 = 0 OR e.seq < $2) AND ($3 = '' OR e.principal = $3) AND ($4 = '' OR e.operation = $4)
			AND ($5 = '' OR e.outcome = $5) AND e.at_ns >= $6 AND ($8 = '' OR e.scope = $8)
		ORDER BY e.seq DESC LIMIT $7`,
		int64(s.limits.AuditEvents), int64(q.Before), q.Principal, q.Operation, q.Outcome, since.UnixNano(), q.Limit, q.Scope)
	if err != nil {
		return nil, storageErr("audit", err)
	}
	defer rows.Close()
	out := []artifact.AuditEvent{}
	for rows.Next() {
		var e artifact.AuditEvent
		var seq, ns int64
		if err := rows.Scan(&seq, &ns, &e.Principal, &e.PrincipalKind, &e.Client, &e.RequestID, &e.Operation, &e.Permission, &e.Scope, &e.Resources, &e.Outcome, &e.Code); err != nil {
			return nil, storageErr("audit", err)
		}
		e.Seq, e.Time = uint64(seq), time.Unix(0, ns).UTC()
		if len(e.Resources) == 0 {
			e.Resources = nil
		}
		out = append(out, e)
	}
	return out, storageErr("audit", rows.Err())
}
