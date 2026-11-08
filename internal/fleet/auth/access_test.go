package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

type hashRepository struct {
	CredentialRepository
	byHash map[[32]byte]Credential
}

func (r hashRepository) CredentialByHash(_ context.Context, hash []byte) (Credential, error) {
	c, ok := r.byHash[[32]byte(hash)]
	if !ok {
		return Credential{}, fleet.ErrNotFound
	}
	return c, nil
}

func TestCredentialLabels(t *testing.T) {
	now := time.Now()
	req := func(role string, labels map[string]string) CredentialRequest {
		return CredentialRequest{ID: strings.Repeat("a", 32), Name: "labelled", Role: role, Labels: labels, ExpiresAt: new(now.Add(time.Hour))}
	}
	labels := map[string]string{"capture": "edge,core", "team": "payments"}
	issued, err := Generate(req("agent", labels), now)
	if err != nil || issued.Credential.Labels["capture"] != "edge,core" {
		t.Fatalf("agent labels refused: %v", err)
	}
	labels["capture"] = "changed"
	if issued.Credential.Labels["capture"] != "edge,core" {
		t.Fatal("labels alias the request")
	}
	many := map[string]string{}
	for i := 0; i <= MaxLabels; i++ {
		many[string(rune('a'+i))] = "v"
	}
	for name, bad := range map[string]CredentialRequest{
		"admin labels": req("admin", map[string]string{"a": "b"}),
		"too many":     req("agent", many),
		"long key":     req("agent", map[string]string{strings.Repeat("k", MaxLabelKeyBytes+1): "v"}),
		"long value":   req("agent", map[string]string{"k": strings.Repeat("v", MaxLabelValueBytes+1)}),
		"invalid key":  req("agent", map[string]string{"bad key": "v"}),
		"control":      req("agent", map[string]string{"k": "a\nb"}),
	} {
		if _, err := Generate(bad, now); !errors.Is(err, fleet.ErrInvalid) {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestIntrospection(t *testing.T) {
	now := time.Now()
	mk := func(id, role string, expires time.Time, revoked bool) (Issued, [32]byte) {
		i, err := Generate(CredentialRequest{ID: id, Name: id[:4], Role: role, ExpiresAt: new(expires)}, now)
		if err != nil {
			t.Fatal(err)
		}
		if role == "agent" {
			i.Credential.Labels = map[string]string{"capture": "edge"}
		}
		if revoked {
			i.Credential.RevokedAt = &now
		}
		return i, sha256.Sum256([]byte(i.Token))
	}
	active, activeHash := mk(strings.Repeat("1", 32), "agent", now.Add(time.Hour), false)
	_, adminHash := mk(strings.Repeat("2", 32), "admin", now.Add(time.Hour), false)
	revoked, revokedHash := mk(strings.Repeat("3", 32), "agent", now.Add(time.Hour), true)
	repo := hashRepository{byHash: map[[32]byte]Credential{activeHash: active.Credential, adminHash: {Role: "admin", ExpiresAt: new(now.Add(time.Hour))}, revokedHash: revoked.Credential}}
	// Expiry is checked against the service clock, so store an already expired copy.
	expiredHash := sha256.Sum256([]byte("expired"))
	expired := active.Credential
	expired.ExpiresAt = new(now.Add(-time.Second))
	repo.byHash[expiredHash] = expired
	s := &Service{Repository: repo}
	got, err := s.Introspect(context.Background(), activeHash)
	if err != nil || !got.Active || got.CredentialID != active.Credential.ID || got.Role != "agent" || got.Labels["capture"] != "edge" || got.ExpiresAt == nil {
		t.Fatalf("active credential: %+v %v", got, err)
	}
	for name, h := range map[string][32]byte{"admin": adminHash, "revoked": revokedHash, "expired": expiredHash, "unknown": sha256.Sum256([]byte("x"))} {
		got, err := s.Introspect(context.Background(), h)
		if err != nil || got.Active || got.CredentialID != "" || got.Labels != nil || got.ExpiresAt != nil {
			t.Fatalf("%s: disclosed %+v %v", name, got, err)
		}
	}

	token := strings.Repeat("t", 32)
	b, _ := New(token)
	if got, _ := (Static{Bearer: b, Role: "agent"}).Introspect(context.Background(), sha256.Sum256([]byte(token))); !got.Active {
		t.Fatal("static agent token not introspected")
	}
	if got, _ := (Static{Bearer: b, Role: "admin"}).Introspect(context.Background(), sha256.Sum256([]byte(token))); got.Active {
		t.Fatal("static admin token introspected")
	}
	if got, _ := (Static{Bearer: b, Role: "agent"}).Introspect(context.Background(), sha256.Sum256([]byte("other"))); got.Active {
		t.Fatal("static mismatch introspected")
	}
}

// Each refused credential request names the field it broke.
func TestGenerateNamesTheInvalidField(t *testing.T) {
	now := time.Now()
	valid := CredentialRequest{ID: strings.Repeat("a", 32), Name: "ci", Role: "agent", ExpiresAt: new(now.Add(time.Hour))}
	for want, mutate := range map[string]func(*CredentialRequest){
		"id must be 32 lowercase hexadecimal characters": func(r *CredentialRequest) { r.ID = "ci-token" },
		"name must be":       func(r *CredentialRequest) { r.Name = " ci" },
		"expires_at must be": func(r *CredentialRequest) { r.ExpiresAt = new(now.Add(-time.Hour)) },
		"expires_at is required for agent credentials": func(r *CredentialRequest) { r.ExpiresAt = nil },
		"role must be": func(r *CredentialRequest) { r.Role = "owner" },
		"labels allow": func(r *CredentialRequest) { r.Labels = map[string]string{"a b": "c"} },
		"labels and remote_config are for agent credentials only": func(r *CredentialRequest) { r.Role, r.RemoteConfig = "reader", true },
	} {
		r := valid
		mutate(&r)
		_, err := Generate(r, now)
		var invalid *fleet.InvalidError
		if !errors.Is(err, fleet.ErrInvalid) || !errors.As(err, &invalid) || !strings.HasPrefix(invalid.Reason, want) {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	if _, err := Generate(valid, now); err != nil {
		t.Fatal(err)
	}
}

// Admin and reader credentials without an expiry stay valid until revoked.
func TestCredentialsWithoutExpiry(t *testing.T) {
	now := time.Now()
	for _, role := range []string{"admin", "reader"} {
		i, err := Generate(CredentialRequest{ID: strings.Repeat("b", 32), Name: role, Role: role}, now)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		c := i.Credential
		if c.ExpiresAt != nil {
			t.Fatalf("%s: expiry %v", role, c.ExpiresAt)
		}
		if !c.Active(now.Add(100 * 365 * 24 * time.Hour)) {
			t.Fatalf("%s: inactive without expiry", role)
		}
		c.RevokedAt = &now
		if c.Active(now) {
			t.Fatalf("%s: revoked credential active", role)
		}
	}
	expiring := Credential{ExpiresAt: new(now.Add(time.Minute))}
	if !expiring.Active(now) || expiring.Active(now.Add(time.Minute)) {
		t.Fatal("expiry not applied")
	}
}

func TestInstallTakesTheDeploymentToken(t *testing.T) {
	id, secret := strings.Repeat("ab", 16), strings.Repeat("cd", 32)
	for name, token := range map[string]string{
		"short":             "of1_" + id,
		"wrong prefix":      "of2_" + id + "_" + secret,
		"uppercase id":      "of1_" + strings.ToUpper(id) + "_" + secret,
		"uppercase secret":  "of1_" + id + "_" + strings.ToUpper(secret),
		"non-hex secret":    "of1_" + id + "_" + strings.Repeat("zz", 32),
		"missing separator": "of1_" + id + "x" + secret,
	} {
		if _, err := Install(CredentialRequest{Name: "linea-server", Role: "admin"}, token, time.Now()); !errors.Is(err, fleet.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	token := "of1_" + id + "_" + secret
	issued, err := Install(CredentialRequest{Name: "linea-server", Role: "admin"}, token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(token))
	if issued.Credential.ID != id || string(issued.Credential.Hash) != string(hash[:]) || issued.Token != token || issued.Credential.ExpiresAt != nil {
		t.Fatalf("installed credential %+v", issued.Credential)
	}
	if _, err := Install(CredentialRequest{Name: "agent", Role: "agent"}, token, time.Now()); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatalf("agent without expiry installed: %v", err)
	}
}
