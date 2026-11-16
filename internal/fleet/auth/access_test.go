package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

type hashRepository struct {
	CredentialRepository
	byHash map[[32]byte]Credential
	byID   map[string]Credential
}

func (r hashRepository) CredentialByPluginHash(_ context.Context, hash []byte) (Credential, error) {
	c, ok := r.byHash[[32]byte(hash)]
	if !ok {
		return Credential{}, fleet.ErrNotFound
	}
	return c, nil
}

func (r hashRepository) Credential(_ context.Context, id string) (Credential, error) {
	c, ok := r.byID[id]
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
		if role == "agent" {
			return i, sha256.Sum256([]byte(i.PluginToken))
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
	if got, _ := (Static{Bearer: b, Role: "agent"}).Introspect(context.Background(), sha256.Sum256([]byte(token))); got.Active {
		t.Fatal("memory mode's agent token introspected")
	}
}

// An agent credential gets a plugin token of its own. Core authenticates
// only the agent token; introspection matches only the plugin token.
func TestPluginTokenIsSeparate(t *testing.T) {
	now := time.Now()
	issued, err := Generate(CredentialRequest{ID: strings.Repeat("4", 32), Name: "edge", Role: "agent", ExpiresAt: new(now.Add(time.Hour))}, now)
	if err != nil {
		t.Fatal(err)
	}
	c := issued.Credential
	if !strings.HasPrefix(issued.PluginToken, "ofp1_"+c.ID+"_") || len(issued.PluginToken) != 102 || issued.PluginToken[5:] == issued.Token[4:] {
		t.Fatalf("plugin token %q", issued.PluginToken)
	}
	if h := sha256.Sum256([]byte(issued.PluginToken)); string(h[:]) != string(c.PluginHash) {
		t.Fatal("plugin hash does not match the plugin token")
	}
	s := &Service{Repository: hashRepository{byID: map[string]Credential{c.ID: c}, byHash: map[[32]byte]Credential{[32]byte(c.PluginHash): c}}}
	request := func(token string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
	if p, err := s.Authenticate(context.Background(), request(issued.Token)); err != nil || p.ID != c.ID {
		t.Fatalf("agent token: %+v %v", p, err)
	}
	if _, err := s.Authenticate(context.Background(), request(issued.PluginToken)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("plugin token authenticated to core: %v", err)
	}
	if got, _ := s.Introspect(context.Background(), sha256.Sum256([]byte(issued.Token))); got.Active {
		t.Fatal("the agent token introspected")
	}
	if got, _ := s.Introspect(context.Background(), sha256.Sum256([]byte(issued.PluginToken))); !got.Active || got.CredentialID != c.ID {
		t.Fatalf("plugin token: %+v", got)
	}
	reader, err := Generate(CredentialRequest{ID: strings.Repeat("5", 32), Name: "ui", Role: "reader"}, now)
	if err != nil || reader.PluginToken != "" || reader.Credential.PluginHash != nil {
		t.Fatalf("a management credential got a plugin token: %+v %v", reader, err)
	}
}

// Plugin grants and the delegate flag are bounded, kept on management
// credentials only, and carried into the authenticated principal.
func TestPluginAccessAndDelegate(t *testing.T) {
	now := time.Now()
	access := map[string]string{"livecapture": "write", "agent-summary": "read"}
	issued, err := Generate(CredentialRequest{ID: strings.Repeat("6", 32), Name: "debugger", Role: "reader", PluginAccess: access, Delegate: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	access["livecapture"] = "admin"
	c := issued.Credential
	if c.PluginAccess["livecapture"] != "write" || !c.Delegate {
		t.Fatalf("credential %+v", c)
	}
	s := &Service{Repository: hashRepository{byID: map[string]Credential{c.ID: c}}}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+issued.Token)
	p, err := s.Authenticate(context.Background(), r)
	if err != nil || p.Access("livecapture") != "write" || p.Access("agent-summary") != "read" || p.Access("other") != "" || !p.Delegate {
		t.Fatalf("principal %+v %v", p, err)
	}
	many := map[string]string{}
	for i := 0; i <= MaxPluginAccess; i++ {
		many[fmt.Sprintf("p%d", i)] = "read"
	}
	agent := func(r *CredentialRequest) { r.Role, r.ExpiresAt = "agent", new(now.Add(time.Hour)) }
	for name, mutate := range map[string]func(*CredentialRequest){
		"level":          func(r *CredentialRequest) { r.PluginAccess = map[string]string{"livecapture": "owner"} },
		"empty level":    func(r *CredentialRequest) { r.PluginAccess = map[string]string{"livecapture": ""} },
		"plugin id":      func(r *CredentialRequest) { r.PluginAccess = map[string]string{"Live_Capture": "read"} },
		"too many":       func(r *CredentialRequest) { r.PluginAccess = many },
		"agent access":   func(r *CredentialRequest) { agent(r); r.PluginAccess = map[string]string{"livecapture": "read"} },
		"agent delegate": func(r *CredentialRequest) { agent(r); r.Delegate = true },
	} {
		req := CredentialRequest{ID: strings.Repeat("7", 32), Name: "bad", Role: "reader"}
		mutate(&req)
		if _, err := Generate(req, now); !errors.Is(err, fleet.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
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

// Admin, operator and reader credentials without an expiry stay valid until revoked.
func TestCredentialsWithoutExpiry(t *testing.T) {
	now := time.Now()
	for _, role := range []string{"admin", "operator", "reader"} {
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
