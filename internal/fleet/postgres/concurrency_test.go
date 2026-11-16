package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
func TestPostgresConcurrentControlWrites(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_concurrency"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_concurrency?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("migration:", err)
	}
	first, err := Bootstrap(ctx, url, "concurrency-fleet", "first-admin", nil)
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	s, err := Open(ctx, url, Options{ServerID: "concurrency-fleet", MaxAgents: 10, MaxEvents: 100})
	if err != nil {
		t.Fatal("open:", err)
	}
	defer s.Close()
	admin := first.Credential.ID

	// Every request below runs at the same time; the outcomes are the contract.
	race := func(n int, work func(i int) error) []error {
		results := make([]error, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = work(i)
			}()
		}
		close(start)
		wg.Wait()
		return results
	}
	count := func(results []error, want error) (n int) {
		for _, err := range results {
			if errors.Is(err, want) || (want == nil && err == nil) {
				n++
			}
		}
		return n
	}

	const group = "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"
	created := race(4, func(i int) error {
		_, err := s.PutGroup(ctx, admin, fleet.Group{ID: group, Name: "race", Selector: "deployment.environment.name=prod"}, 0)
		return err
	})
	if count(created, nil) != 1 || count(created, fleet.ErrPrecondition) != 3 {
		t.Fatalf("concurrent creates of one group: %v", created)
	}
	updated := race(4, func(i int) error {
		_, err := s.PutGroup(ctx, admin, fleet.Group{ID: group, Name: "race", Selector: "deployment.environment.name=prod"}, 1)
		return err
	})
	if count(updated, nil) != 1 || count(updated, fleet.ErrPrecondition) != 3 {
		t.Fatalf("concurrent updates at one revision: %v", updated)
	}
	var revision int64
	if err := s.pool.QueryRow(ctx, "SELECT revision FROM fleet.groups WHERE id=$1", group).Scan(&revision); err != nil || revision != 2 {
		t.Fatalf("stored revision %d, %v", revision, err)
	}

	// Two administrators that each revoke the other must leave one active.
	second, err := auth.Generate(auth.CredentialRequest{ID: randomID(), Name: "second-admin", Role: "admin"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCredential(ctx, admin, second.Credential); err != nil {
		t.Fatal(err)
	}
	ids := []string{admin, second.Credential.ID}
	revoked := race(2, func(i int) error { return s.RevokeCredential(ctx, ids[i], ids[1-i]) })
	// The loser is refused as the last administrator, or as an actor that
	// the winner already revoked, depending on timing.
	if count(revoked, nil) != 1 {
		t.Fatalf("concurrent revocation of both administrators: %v", revoked)
	}
	var active int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM fleet.credentials WHERE role='admin' AND revoked_at IS NULL").Scan(&active); err != nil || active != 1 {
		t.Fatalf("active administrators %d, %v", active, err)
	}
}
