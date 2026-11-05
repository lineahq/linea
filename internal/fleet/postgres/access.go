package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

func requireAdmin(ctx context.Context, tx pgx.Tx, id string) error {
	var ok bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.credentials WHERE id=$1 AND role='admin' AND revoked_at IS NULL AND expires_at>clock_timestamp())", id).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return auth.ErrUnauthorized
	}
	return nil
}
func requireAgent(ctx context.Context, tx pgx.Tx, id string) error {
	var ok bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.credentials WHERE id=$1 AND role='agent' AND revoked_at IS NULL AND expires_at>clock_timestamp())", id).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return auth.ErrUnauthorized
	}
	return nil
}

func Bootstrap(ctx context.Context, url, serverID, name string) (auth.Issued, error) {
	if !auth.SafeName(serverID) {
		return auth.Issued{}, fleet.ErrInvalid
	}
	now := time.Now()
	issued, err := auth.Generate(auth.CredentialRequest{ID: randomID(), Name: name, Role: "admin", ExpiresAt: now.Add(90 * 24 * time.Hour)}, now)
	if err != nil {
		return auth.Issued{}, err
	}
	err = adminTx(ctx, url, func(ctx context.Context, tx pgx.Tx) error {
		if err := identity(ctx, tx, serverID); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.credentials WHERE role='admin' AND revoked_at IS NULL AND expires_at>clock_timestamp()").Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fleet.ErrConflict
		}
		if err := insertCredential(ctx, tx, issued.Credential); err != nil {
			return err
		}
		return appendEvent(ctx, tx, "credential.created", issued.Credential.ID, "bootstrap", nil)
	})
	if err != nil {
		return auth.Issued{}, err
	}
	return issued, nil
}

const credentialColumns = "id,name,role,labels,remote_config,created_at,expires_at,revoked_at"

func scanCredential(row scanner, withHash bool) (auth.Credential, error) {
	var c auth.Credential
	var labels []byte
	dest := []any{&c.ID, &c.Name, &c.Role, &labels, &c.RemoteConfig, &c.CreatedAt, &c.ExpiresAt, &c.RevokedAt}
	if withHash {
		dest = append(dest, &c.Hash)
	}
	if err := row.Scan(dest...); err != nil {
		return c, err
	}
	if err := json.Unmarshal(labels, &c.Labels); err != nil {
		return c, fleet.ErrUnavailable
	}
	if len(c.Labels) == 0 {
		c.Labels = nil
	}
	return c, nil
}

// CredentialByHash serves plugin introspection through the unique hash index.
func (s *Store) CredentialByHash(ctx context.Context, hash []byte) (auth.Credential, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	c, err := scanCredential(s.pool.QueryRow(ctx, "SELECT "+credentialColumns+",token_hash FROM fleet.credentials WHERE token_hash=$1", hash), true)
	return c, normalize(err)
}
func (s *Store) Credential(ctx context.Context, id string) (auth.Credential, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	c, err := scanCredential(s.pool.QueryRow(ctx, "SELECT "+credentialColumns+",token_hash FROM fleet.credentials WHERE id=$1", id), true)
	return c, normalize(err)
}
func (s *Store) ListCredentials(ctx context.Context, after string, limit int) (auth.CredentialPage, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, "SELECT "+credentialColumns+" FROM fleet.credentials WHERE id>$1 ORDER BY id LIMIT $2", after, limit+1)
	if err != nil {
		return auth.CredentialPage{}, fleet.ErrUnavailable
	}
	defer rows.Close()
	p := auth.CredentialPage{Items: []auth.Credential{}}
	for rows.Next() {
		c, err := scanCredential(rows, false)
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
func insertCredential(ctx context.Context, tx pgx.Tx, c auth.Credential) error {
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.credentials").Scan(&count); err != nil {
		return err
	}
	if count >= 1000 {
		return fleet.ErrCapacity
	}
	labels, err := json.Marshal(c.Labels)
	if err != nil || c.Labels == nil {
		labels = []byte("{}")
	}
	_, err = tx.Exec(ctx, "INSERT INTO fleet.credentials(id,name,role,labels,remote_config,token_hash,created_at,expires_at) VALUES($1,$2,$3,$4::jsonb,$5,$6,$7,$8)", c.ID, c.Name, c.Role, string(labels), c.RemoteConfig, c.Hash, c.CreatedAt, c.ExpiresAt)
	return err
}
func (s *Store) InsertCredential(ctx context.Context, actor string, c auth.Credential) error {
	return s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		if err := insertCredential(ctx, tx, c); err != nil {
			return err
		}
		if c.RemoteConfig {
			if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_config','')"); err != nil {
				return err
			}
		}
		return s.event(ctx, tx, "credential.created", c.ID, actor, nil)
	})
}

// SetCredentialRemoteConfig changes whether config management may send
// configuration to agents using an active agent credential. Every replica
// refreshes its policy cache on the fleet_config notification.
func (s *Store) SetCredentialRemoteConfig(ctx context.Context, actor, id string, enabled bool) (auth.Credential, error) {
	var c auth.Credential
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var err error
		c, err = scanCredential(tx.QueryRow(ctx, "SELECT "+credentialColumns+" FROM fleet.credentials WHERE id=$1 FOR UPDATE", id), false)
		if err != nil {
			return err
		}
		if c.Role != "agent" || c.RevokedAt != nil {
			return fleet.ErrInvalid
		}
		if c.RemoteConfig == enabled {
			return nil
		}
		if _, err := tx.Exec(ctx, "UPDATE fleet.credentials SET remote_config=$2 WHERE id=$1", id, enabled); err != nil {
			return err
		}
		c.RemoteConfig = enabled
		if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_config','')"); err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]bool{"remote_config": enabled})
		return s.event(ctx, tx, "credential.updated", id, actor, detail)
	})
	return c, err
}
func (s *Store) RevokeCredential(ctx context.Context, actor, id string) error {
	return s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		c, err := scanCredential(tx.QueryRow(ctx, "SELECT "+credentialColumns+" FROM fleet.credentials WHERE id=$1", id), false)
		if err != nil {
			return err
		}
		if c.RevokedAt != nil {
			return nil
		}
		if c.Role == "admin" {
			var count int
			var targetActive bool
			if err := tx.QueryRow(ctx, "SELECT count(*),COALESCE(bool_or(id=$1),false) FROM fleet.credentials WHERE role='admin' AND revoked_at IS NULL AND expires_at>clock_timestamp()", id).Scan(&count, &targetActive); err != nil {
				return err
			}
			if targetActive && count <= 1 {
				return fleet.ErrConflict
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE fleet.credentials SET revoked_at=clock_timestamp() WHERE id=$1", id); err != nil {
			return err
		}
		// Every replica closes sessions using the credential on commit.
		if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_credential_revoked', $1)", id); err != nil {
			return err
		}
		return s.event(ctx, tx, "credential.revoked", id, actor, nil)
	})
}
func (s *Store) ActiveCredentialIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, "SELECT id FROM fleet.credentials WHERE id=ANY($1::text[]) AND revoked_at IS NULL AND expires_at>clock_timestamp()", ids)
	if err != nil {
		return nil, fleet.ErrUnavailable
	}
	defer rows.Close()
	active := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, normalize(err)
		}
		active[id] = true
	}
	return active, normalize(rows.Err())
}
