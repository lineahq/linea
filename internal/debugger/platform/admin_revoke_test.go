package platform

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// TestRevokeAdminAccessEndsOnlyTheAdminsCredentials: it revokes the admin's
// unrevoked tokens and ends the admin's sessions, leaves other principals'
// tokens and sessions, records one audit event with counts only, and counts
// nothing the second time.
func TestRevokeAdminAccessEndsOnlyTheAdminsCredentials(t *testing.T) {
	ctx := context.Background()
	repo := memory.Open(memory.NewData(), artifact.Limits{})
	t.Cleanup(repo.Close)
	now := time.Now().UTC().Truncate(time.Second)
	alice := UserID("idp", "alice")
	hash := func(n int) string { return fmt.Sprintf("%064x", n) }
	for n, owner := range []string{AdminPrincipalID, AdminPrincipalID, alice, AdminPrincipalID} {
		err := repo.SaveToken(ctx, artifact.TokenRecord{ID: fmt.Sprintf("%032x", n+1), Hash: hash(n + 1), Principal: owner, Kind: artifact.TokenUser,
			Label: "token", CreatedBy: owner, Created: now.Add(-time.Hour), Expires: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Token 4 is the admin's, already revoked: it is not counted.
	earlier := now.Add(-time.Minute)
	if _, err := repo.RevokeToken(ctx, fmt.Sprintf("%032x", 4), earlier); err != nil {
		t.Fatal(err)
	}
	for n, owner := range []string{AdminPrincipalID, AdminPrincipalID, alice} {
		err := repo.SaveSession(ctx, artifact.SessionRecord{Hash: hash(100 + n), Principal: owner, Name: "who", Created: now.Add(-time.Hour), Expires: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}

	tokens, sessions, err := RevokeAdminAccess(ctx, repo, now)
	if err != nil || tokens != 2 || sessions != 2 {
		t.Fatalf("RevokeAdminAccess = %d tokens, %d sessions, %v; want 2, 2", tokens, sessions, err)
	}
	all, err := repo.Tokens(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		wantRevoked := r.Principal == AdminPrincipalID
		if (r.Revoked != nil) != wantRevoked {
			t.Errorf("token %s of %s revoked = %v, want %v", r.ID, r.Principal, r.Revoked != nil, wantRevoked)
		}
		if r.ID == fmt.Sprintf("%032x", 4) && !r.Revoked.Equal(earlier) {
			t.Errorf("an already revoked token was revoked again at %v", r.Revoked)
		}
	}
	for n, wantErr := range map[int]bool{100: true, 101: true, 102: false} {
		_, err := repo.SessionByHash(ctx, hash(n))
		if (err != nil) != wantErr {
			t.Errorf("session %d lookup = %v, want not found = %v", n, err, wantErr)
		}
	}
	events, err := repo.AuditEvents(ctx, artifact.AuditQuery{Operation: "RevokeAdminAccess", Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("audit events = %+v, %v; want one", events, err)
	}
	e := events[0]
	if e.Principal != string(PrincipalSystem) || e.Outcome != artifact.AuditAllowed || strings.Join(e.Resources, ",") != "tokens:2,sessions:2" {
		t.Errorf("audit event = %+v", e)
	}
	if strings.Contains(fmt.Sprint(e), hash(1)) {
		t.Errorf("audit event holds a hash: %+v", e)
	}

	tokens, sessions, err = RevokeAdminAccess(ctx, repo, now)
	if err != nil || tokens != 0 || sessions != 0 {
		t.Errorf("second run = %d, %d, %v; want 0, 0", tokens, sessions, err)
	}
}
