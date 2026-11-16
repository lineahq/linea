package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/translog"
)

func requireAdmin(ctx context.Context, tx pgx.Tx, id string) error {
	var ok bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.credentials WHERE id=$1 AND role='admin' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()))", id).Scan(&ok)
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
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.credentials WHERE id=$1 AND role='agent' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()))", id).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return auth.ErrUnauthorized
	}
	return nil
}

// Bootstrap issues the first admin, with plugin grants access, and returns
// its token once.
func Bootstrap(ctx context.Context, url, serverID, name string, access map[string]string) (auth.Issued, error) {
	issued, err := auth.Generate(auth.CredentialRequest{ID: randomID(), Name: name, Role: "admin", PluginAccess: access}, time.Now())
	if err != nil {
		return auth.Issued{}, err
	}
	if _, err := bootstrap(ctx, url, serverID, issued.Credential); err != nil {
		return auth.Issued{}, err
	}
	return issued, nil
}

// BootstrapToken installs the first admin with a token the deployment
// generated, so the deployment can hand the same token to linea-server. It is
// safe to repeat: when that token's admin is already active it only sets its
// plugin access to access and reports created false. It conflicts when
// another admin is active or the token's credential was revoked or is not an
// active admin.
func BootstrapToken(ctx context.Context, url, serverID, name, token string, access map[string]string) (created bool, err error) {
	issued, err := auth.Install(auth.CredentialRequest{Name: name, Role: "admin", PluginAccess: access}, token, time.Now())
	if err != nil {
		return false, err
	}
	return bootstrap(ctx, url, serverID, issued.Credential)
}

func bootstrap(ctx context.Context, url, serverID string, c auth.Credential) (created bool, err error) {
	if !auth.SafeName(serverID) {
		return false, fleet.ErrInvalid
	}
	err = adminTx(ctx, url, func(ctx context.Context, tx pgx.Tx) error {
		if err := identity(ctx, tx, serverID); err != nil {
			return err
		}
		var current bool
		err := tx.QueryRow(ctx, "SELECT token_hash=$2 AND role='admin' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) FROM fleet.credentials WHERE id=$1", c.ID, c.Hash).Scan(&current)
		switch {
		case err == nil && current:
			return syncScope(ctx, tx, c)
		case err == nil:
			return fleet.ErrConflict
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.credentials WHERE role='admin' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp())").Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fleet.ErrConflict
		}
		if err := insertCredential(ctx, tx, c); err != nil {
			return err
		}
		created = true
		return appendEvent(ctx, tx, "credential.created", c.ID, "bootstrap", nil)
	})
	return created, err
}

// InstallOperatorToken installs an operator credential with a token the
// deployment generated, so the deployment can hand that token to linea-server
// while it keeps the admin token. The credential has no expiry; access and
// delegate are its plugin grants and acting-for permission. It is safe to
// repeat: when that token's operator is already active it only sets access
// and delegate and reports created false. It conflicts when the token's
// credential exists with another role or token, was revoked or has expired.
func InstallOperatorToken(ctx context.Context, url, serverID, name, token string, access map[string]string, delegate bool) (created bool, err error) {
	issued, err := auth.Install(auth.CredentialRequest{Name: name, Role: "operator", PluginAccess: access, Delegate: delegate}, token, time.Now())
	if err != nil {
		return false, err
	}
	c := issued.Credential
	if !auth.SafeName(serverID) {
		return false, fleet.ErrInvalid
	}
	err = adminTx(ctx, url, func(ctx context.Context, tx pgx.Tx) error {
		if err := identity(ctx, tx, serverID); err != nil {
			return err
		}
		var current bool
		err := tx.QueryRow(ctx, "SELECT token_hash=$2 AND role='operator' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) FROM fleet.credentials WHERE id=$1", c.ID, c.Hash).Scan(&current)
		switch {
		case err == nil && current:
			return syncScope(ctx, tx, c)
		case err == nil:
			return fleet.ErrConflict
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if err := insertCredential(ctx, tx, c); err != nil {
			return err
		}
		created = true
		return appendEvent(ctx, tx, "credential.created", c.ID, "bootstrap", nil)
	})
	return created, err
}

// syncScope sets a bootstrap credential's plugin access and delegate flag to
// what the deployment asks for now, and records a change.
func syncScope(ctx context.Context, tx pgx.Tx, c auth.Credential) error {
	access := c.PluginAccess
	if access == nil {
		access = map[string]string{}
	}
	encoded, err := json.Marshal(access)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, "UPDATE fleet.credentials SET plugin_access=$2::jsonb, delegate=$3 WHERE id=$1 AND (plugin_access<>$2::jsonb OR delegate<>$3)", c.ID, string(encoded), c.Delegate)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	detail, err := json.Marshal(map[string]any{"plugin_access": access, "delegate": c.Delegate})
	if err != nil {
		return err
	}
	return appendEvent(ctx, tx, "credential.updated", c.ID, "bootstrap", detail)
}

const credentialColumns = "id,name,role,labels,remote_config,plugin_access,delegate,created_at,expires_at,revoked_at"

func scanCredential(row scanner, withHash bool) (auth.Credential, error) {
	var c auth.Credential
	var labels, access []byte
	dest := []any{&c.ID, &c.Name, &c.Role, &labels, &c.RemoteConfig, &access, &c.Delegate, &c.CreatedAt, &c.ExpiresAt, &c.RevokedAt}
	if withHash {
		dest = append(dest, &c.Hash, &c.PluginHash)
	}
	if err := row.Scan(dest...); err != nil {
		return c, err
	}
	if json.Unmarshal(labels, &c.Labels) != nil || json.Unmarshal(access, &c.PluginAccess) != nil {
		return c, fleet.ErrUnavailable
	}
	if len(c.Labels) == 0 {
		c.Labels = nil
	}
	if len(c.PluginAccess) == 0 {
		c.PluginAccess = nil
	}
	return c, nil
}

// CredentialByPluginHash serves plugin introspection through the unique
// plugin hash index. The agent token's hash never matches.
func (s *Store) CredentialByPluginHash(ctx context.Context, hash []byte) (auth.Credential, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	c, err := scanCredential(s.pool.QueryRow(ctx, "SELECT "+credentialColumns+",token_hash,plugin_hash FROM fleet.credentials WHERE plugin_hash=$1", hash), true)
	return c, normalize(err)
}
func (s *Store) Credential(ctx context.Context, id string) (auth.Credential, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	c, err := scanCredential(s.pool.QueryRow(ctx, "SELECT "+credentialColumns+",token_hash,plugin_hash FROM fleet.credentials WHERE id=$1", id), true)
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
	if err := capped(ctx, tx, "fleet.credentials", 1000); err != nil {
		return err
	}
	var err error
	labels, access := []byte("{}"), []byte("{}")
	if c.Labels != nil {
		if labels, err = json.Marshal(c.Labels); err != nil {
			return err
		}
	}
	if c.PluginAccess != nil {
		if access, err = json.Marshal(c.PluginAccess); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, "INSERT INTO fleet.credentials(id,name,role,labels,remote_config,plugin_access,delegate,token_hash,plugin_hash,created_at,expires_at) VALUES($1,$2,$3,$4::jsonb,$5,$6::jsonb,$7,$8,$9,$10,$11)",
		c.ID, c.Name, c.Role, string(labels), c.RemoteConfig, string(access), c.Delegate, c.Hash, c.PluginHash, c.CreatedAt, c.ExpiresAt)
	return err
}
func (s *Store) InsertCredential(ctx context.Context, actor string, c auth.Credential) error {
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
	if err == nil {
		s.log.Info(translog.CredentialCreated, translog.ID(translog.KeyCredential, c.ID), translog.ID(translog.KeyActor, actor))
	}
	return err
}

// SetCredentialRemoteConfig changes whether config management may send
// configuration to agents using an active agent credential. Every replica
// refreshes its policy cache on the fleet_config notification.
func (s *Store) SetCredentialRemoteConfig(ctx context.Context, actor, id string, enabled bool) (auth.Credential, error) {
	var c auth.Credential
	changed := false
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		changed = false
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
		changed = true
		return s.event(ctx, tx, "credential.updated", id, actor, detail)
	})
	if err == nil && changed {
		why := translog.ReasonRemoteConfigDisabled
		if enabled {
			why = translog.ReasonRemoteConfigEnabled
		}
		s.log.Info(translog.CredentialUpdated, translog.ID(translog.KeyCredential, id), translog.ID(translog.KeyActor, actor), translog.Why(why))
	}
	return c, err
}
func (s *Store) RevokeCredential(ctx context.Context, actor, id string) error {
	revokedNow := false
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		revokedNow = false
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
			// Lock every active admin, in a fixed order, before counting, so
			// two requests that each revoke an admin cannot both see the
			// other as still active. A waiting request re-evaluates the
			// predicate after the first commits and sees the revocation.
			rows, err := tx.Query(ctx, "SELECT id FROM fleet.credentials WHERE role='admin' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) ORDER BY id FOR UPDATE")
			if err != nil {
				return err
			}
			count, targetActive := 0, false
			for rows.Next() {
				var active string
				if err := rows.Scan(&active); err != nil {
					rows.Close()
					return err
				}
				count++
				targetActive = targetActive || active == id
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if targetActive && count <= 1 {
				return fleet.ErrConflict
			}
		}
		// A concurrent revocation of the same credential may have committed
		// since the read above; it already announced the revocation.
		revoked, err := tx.Exec(ctx, "UPDATE fleet.credentials SET revoked_at=clock_timestamp() WHERE id=$1 AND revoked_at IS NULL", id)
		if err != nil {
			return err
		}
		if revoked.RowsAffected() == 0 {
			return nil
		}
		// Every replica closes sessions using the credential on commit.
		if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_credential_revoked', $1)", id); err != nil {
			return err
		}
		revokedNow = true
		return s.event(ctx, tx, "credential.revoked", id, actor, nil)
	})
	if err == nil && revokedNow {
		s.log.Info(translog.CredentialRevoked, translog.ID(translog.KeyCredential, id), translog.ID(translog.KeyActor, actor))
	}
	return err
}
func (s *Store) ActiveCredentialIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, "SELECT id FROM fleet.credentials WHERE id=ANY($1::text[]) AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp())", ids)
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
