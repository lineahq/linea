package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// passwordServer implements the two sign-in endpoints for the admin
// password "right" and records the last sign-in body.
func passwordServer(t *testing.T, info platform.CLISignIn, throttle bool, body *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/auth/cli":
			_ = json.NewEncoder(w).Encode(info)
		case r.Method == http.MethodPost && r.URL.Path == "/auth/password" && info.Password:
			raw, _ := io.ReadAll(r.Body)
			if body != nil {
				*body = string(raw)
			}
			var in platform.PasswordSignIn
			if r.Header.Get("Content-Type") != "application/json" || json.Unmarshal(raw, &in) != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			switch {
			case throttle:
				w.Header().Set("Retry-After", "60")
				http.Error(w, "too many failed sign-ins", http.StatusTooManyRequests)
			case in.User != "admin" || in.Password != "right" || in.Client != "cli":
				http.Error(w, "wrong user name or password", http.StatusUnauthorized)
			default:
				_ = json.NewEncoder(w).Encode(platform.PasswordToken{Token: "odt_secret", Expires: time.Now().Add(24 * time.Hour).UTC()})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestPasswordLoginKeepsTheAPIToken: the sign-in posts the admin user, the
// password and the cli client as JSON, and the credential is the returned
// token with its expiry, without an identity provider.
func TestPasswordLoginKeepsTheAPIToken(t *testing.T) {
	var body string
	server := passwordServer(t, platform.CLISignIn{Password: true}, false, &body)
	_, err := PasswordLogin(context.Background(), server, "right", []byte("ca"))
	if err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("a CA without certificates = %v", err)
	}
	cred, err := PasswordLogin(context.Background(), server, "right", nil)
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"user":"admin","password":"right","client":"cli"}` {
		t.Fatalf("body = %s", body)
	}
	if cred.IDToken != "odt_secret" || cred.Issuer != "" || cred.RefreshToken != "" || time.Until(cred.Expiry) < 23*time.Hour {
		t.Fatalf("credential = %+v", cred)
	}
}

// TestPasswordLoginMapsTheServersRefusals: each refusal is a distinct error
// that never carries the password.
func TestPasswordLoginMapsTheServersRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		info     platform.CLISignIn
		throttle bool
		password string
		check    func(error) bool
	}{
		{name: "wrong password", info: platform.CLISignIn{Password: true}, password: "hunter2-nope", check: func(err error) bool { return errors.Is(err, ErrPasswordRefused) }},
		{name: "throttled", info: platform.CLISignIn{Password: true}, throttle: true, password: "right", check: func(err error) bool {
			var te *ThrottledError
			return errors.As(err, &te) && te.RetryAfter == time.Minute && strings.Contains(te.Error(), "retry after 60 seconds")
		}},
		{name: "no password sign-in", info: platform.CLISignIn{Issuer: "https://idp", ClientID: "cli"}, password: "right", check: func(err error) bool { return errors.Is(err, ErrNoPasswordSignIn) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := passwordServer(t, tc.info, tc.throttle, nil)
			_, err := PasswordLogin(context.Background(), server, tc.password, nil)
			if err == nil || !tc.check(err) || strings.Contains(err.Error(), tc.password) {
				t.Fatalf("PasswordLogin = %v", err)
			}
		})
	}
}

// TestSignInMethodsDescribesTheServer: the methods are what GET /auth/cli
// says, a password-only server is not an incomplete description, and Login
// (the device flow) refuses a server without a provider.
func TestSignInMethodsDescribesTheServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *platform.CLISignIn
		want platform.CLISignIn
		none bool
	}{
		{name: "password only", info: &platform.CLISignIn{Password: true}, want: platform.CLISignIn{Password: true}},
		{name: "both", info: &platform.CLISignIn{Issuer: "https://idp", ClientID: "cli", Password: true}, want: platform.CLISignIn{Issuer: "https://idp", ClientID: "cli", Password: true}},
		{name: "device flow only", info: &platform.CLISignIn{Issuer: "https://idp", ClientID: "cli"}, want: platform.CLISignIn{Issuer: "https://idp", ClientID: "cli"}},
		{name: "neither", none: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := cliServer(t, tc.info)
			got, err := SignInMethods(context.Background(), server, nil)
			if tc.none {
				if !errors.Is(err, ErrNoCLISignIn) {
					t.Fatalf("SignInMethods = %v", err)
				}
				return
			}
			if err != nil || got.Issuer != tc.want.Issuer || got.ClientID != tc.want.ClientID || got.Password != tc.want.Password {
				t.Fatalf("SignInMethods = %+v, %v", got, err)
			}
		})
	}
	if _, err := Login(context.Background(), cliServer(t, &platform.CLISignIn{Password: true}), LoginOptions{}); err == nil || !strings.Contains(err.Error(), "admin password") {
		t.Fatalf("device flow against a password-only server = %v", err)
	}
}
