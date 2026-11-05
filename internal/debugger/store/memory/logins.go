package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// SaveLogin records a sign-in in progress; expired ones are dropped.
func (s *Store) SaveLogin(ctx context.Context, r artifact.LoginRecord) error {
	if err := artifact.ValidateLogin(r); err != nil {
		return err
	}
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	d.dropLoginsLocked(time.Now())
	if _, ok := d.logins[r.Hash]; ok {
		return fmt.Errorf("%w: login exists", artifact.ErrConflict)
	}
	if len(d.logins) >= artifact.MaxLogins {
		return fmt.Errorf("%w: at most %d sign-ins in progress", artifact.ErrLimit, artifact.MaxLogins)
	}
	r.Expires = r.Expires.UTC()
	d.logins[r.Hash] = r
	return nil
}

// TakeLogin deletes a sign-in and returns it, once.
func (s *Store) TakeLogin(ctx context.Context, hash string) (artifact.LoginRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.LoginRecord{}, err
	}
	d.dropLoginsLocked(time.Now())
	r, ok := d.logins[hash]
	if !ok {
		return artifact.LoginRecord{}, artifact.ErrNotFound
	}
	delete(d.logins, hash)
	return r, nil
}

func (d *Data) dropLoginsLocked(now time.Time) {
	for h, r := range d.logins {
		if !now.Before(r.Expires) {
			delete(d.logins, h)
		}
	}
}
