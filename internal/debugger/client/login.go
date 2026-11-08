package client

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// CLI sign-in: linea login signs in to a team
// server's identity provider with the device flow and keeps the refresh
// token in the user's config directory, one credential per server. The CLI
// presents the ID token as a bearer token and refreshes it when it expires.

// ErrNoCLISignIn is Login's error for a server that offers no CLI sign-in:
// it takes an API token, or signs requests in another way, such as a
// trusted proxy.
var ErrNoCLISignIn = errors.New("the server has no CLI sign-in")

const (
	credentialsSchema = 1
	maxCredentials    = 1 << 20
)

// Credential is the CLI's sign-in to one team server.
type Credential struct {
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"client_id"`
	TokenURL     string    `json:"token_url"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token"`
	Expiry       time.Time `json:"expiry"`
	// CA is the PEM certificates login --ca trusted, beside the system
	// roots, for the server and its identity provider; empty without one.
	CA string `json:"ca,omitempty"`
}

type credentialsFile struct {
	Schema  int                    `json:"schema"`
	Servers map[string]*Credential `json:"servers"`
}

// CredentialsPath is credentials.json in the user's config directory.
func CredentialsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "linea", "credentials.json"), nil
}

func readCredentials(path string) (*credentialsFile, error) {
	f := &credentialsFile{Schema: credentialsSchema, Servers: map[string]*Credential{}}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxCredentials+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCredentials || json.Unmarshal(raw, f) != nil || f.Schema != credentialsSchema {
		return nil, fmt.Errorf("%s is not a credentials file; remove it and sign in again", path)
	}
	if f.Servers == nil {
		f.Servers = map[string]*Credential{}
	}
	return f, nil
}

// LoadCredential returns the stored sign-in to server, nil without one; an
// invalid server URL has none.
func LoadCredential(path, server string) (*Credential, error) {
	base, err := serverBase(server, true)
	if err != nil {
		return nil, nil
	}
	f, err := readCredentials(path)
	if err != nil {
		return nil, err
	}
	return f.Servers[base], nil
}

// SaveCredential stores the sign-in to server, readable by the user only; a
// nil credential removes it.
func SaveCredential(path, server string, c *Credential) error {
	base, err := serverBase(server, true)
	if err != nil {
		return err
	}
	f, err := readCredentials(path)
	if err != nil {
		return err
	}
	if c == nil {
		delete(f.Servers, base)
	} else {
		f.Servers[base] = c
	}
	raw, _ := json.MarshalIndent(f, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoginOptions configure a device-flow sign-in.
type LoginOptions struct {
	// Prompt tells the user where to approve the sign-in and the code to
	// enter there.
	Prompt func(verificationURL, userCode string)
	// Wait waits between polls of the provider; nil waits on a timer.
	Wait func(ctx context.Context, d time.Duration) error
	// HTTPClient reaches the identity provider; nil is http.DefaultClient,
	// trusting CA when set.
	HTTPClient *http.Client
	// CA is PEM certificates trusted beside the system roots for the server
	// and its identity provider; the credential keeps them.
	CA []byte
}

// Login signs in to the team server's identity provider with the device
// flow. The server names the provider and client at GET /auth/cli.
func Login(ctx context.Context, server string, opts LoginOptions) (*Credential, error) {
	base, err := serverBase(server, true)
	if err != nil {
		return nil, err
	}
	info, err := cliSignIn(ctx, base, opts.CA)
	if err != nil {
		return nil, err
	}
	if u, err := url.Parse(info.Issuer); err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && loopbackName(u.Hostname()))) {
		return nil, fmt.Errorf("the server names identity provider %q, which is not an https URL", info.Issuer)
	}
	hc := opts.HTTPClient
	if hc == nil {
		if hc, err = providerHTTP(opts.CA); err != nil {
			return nil, err
		}
	}
	pctx := oidc.ClientContext(ctx, hc)
	provider, err := oidc.NewProvider(pctx, info.Issuer)
	if err != nil {
		return nil, fmt.Errorf("identity provider %s: %w", info.Issuer, err)
	}
	ep := provider.Endpoint()
	if ep.DeviceAuthURL == "" {
		return nil, fmt.Errorf("identity provider %s offers no device sign-in; use an API token", info.Issuer)
	}
	cfg := oauth2.Config{ClientID: info.ClientID, Endpoint: ep, Scopes: info.Scopes}
	da, err := cfg.DeviceAuth(pctx)
	if err != nil {
		return nil, fmt.Errorf("identity provider %s: %w", info.Issuer, err)
	}
	where := da.VerificationURIComplete
	if where == "" {
		where = da.VerificationURI
	}
	if opts.Prompt != nil {
		opts.Prompt(where, da.UserCode)
	}
	if !da.Expiry.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, da.Expiry)
		defer cancel()
	}
	wait := opts.Wait
	if wait == nil {
		wait = waitTimer
	}
	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {da.DeviceCode}, "client_id": {info.ClientID}}
	for {
		if err := wait(ctx, interval); err != nil {
			return nil, fmt.Errorf("sign-in was not approved in time: %w", err)
		}
		tok, code, err := deviceToken(ctx, hc, ep.TokenURL, form)
		switch {
		case err != nil:
			return nil, err
		case code == "authorization_pending":
			continue
		case code == "slow_down":
			interval += 5 * time.Second
			continue
		case code != "":
			return nil, fmt.Errorf("sign-in failed at the identity provider: %s", code)
		}
		c, err := credential(info.Issuer, info.ClientID, ep.TokenURL, tok.IDToken, tok.RefreshToken, tok.ExpiresIn)
		if c != nil {
			c.CA = string(opts.CA)
		}
		return c, err
	}
}

// providerHTTP reaches an identity provider as http.DefaultClient does,
// also trusting ca when set.
func providerHTTP(ca []byte) (*http.Client, error) {
	if len(ca) == 0 {
		return http.DefaultClient, nil
	}
	pool, err := caPool(ca)
	if err != nil {
		return nil, err
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: t}, nil
}

func waitTimer(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func cliSignIn(ctx context.Context, base string, ca []byte) (*platform.CLISignIn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/auth/cli", nil)
	if err != nil {
		return nil, err
	}
	hc, err := serverHTTP(ca)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// A server without OIDC answers 404, or serves the web client's page
	// for the unknown path.
	if resp.StatusCode == http.StatusNotFound ||
		resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil, fmt.Errorf("%s: %w", base, ErrNoCLISignIn)
	}
	var info platform.CLISignIn
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&info) != nil || info.Issuer == "" || info.ClientID == "" {
		return nil, fmt.Errorf("%s did not describe its sign-in (%s)", base, resp.Status)
	}
	return &info, nil
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
}

// deviceToken polls the token endpoint once; code is the provider's error
// code, such as authorization_pending.
func deviceToken(ctx context.Context, hc *http.Client, tokenURL string, form url.Values) (*tokenResponse, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var tok tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return nil, "", fmt.Errorf("identity provider token endpoint (%s): %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK && tok.Error == "" {
		tok.Error = resp.Status
	}
	return &tok, tok.Error, nil
}

// credential keeps the tokens; the ID token's expiry is read from it
// unverified, since the server verifies it.
func credential(issuer, clientID, tokenURL, idToken, refresh string, expiresIn int64) (*Credential, error) {
	if idToken == "" {
		return nil, errors.New("the identity provider returned no ID token")
	}
	c := &Credential{Issuer: issuer, ClientID: clientID, TokenURL: tokenURL, RefreshToken: refresh, IDToken: idToken, Expiry: time.Now().Add(time.Duration(expiresIn) * time.Second)}
	if parts := strings.Split(idToken, "."); len(parts) == 3 {
		var claims struct {
			Exp int64 `json:"exp"`
		}
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil && json.Unmarshal(raw, &claims) == nil && claims.Exp > 0 {
			c.Expiry = time.Unix(claims.Exp, 0)
		}
	}
	return c, nil
}

// Refresh renews an expired credential with its refresh token, keeping its
// CA.
func Refresh(ctx context.Context, c *Credential, hc *http.Client) (*Credential, error) {
	if c.RefreshToken == "" {
		return nil, errors.New("the sign-in has no refresh token")
	}
	if hc == nil {
		var err error
		if hc, err = providerHTTP([]byte(c.CA)); err != nil {
			return nil, err
		}
	}
	cfg := oauth2.Config{ClientID: c.ClientID, Endpoint: oauth2.Endpoint{TokenURL: c.TokenURL, AuthStyle: oauth2.AuthStyleInParams}}
	tok, err := cfg.TokenSource(oidc.ClientContext(ctx, hc), &oauth2.Token{RefreshToken: c.RefreshToken}).Token()
	if err != nil {
		return nil, err
	}
	id, _ := tok.Extra("id_token").(string)
	var expiresIn int64
	if !tok.Expiry.IsZero() {
		expiresIn = int64(time.Until(tok.Expiry).Seconds())
	}
	renewed, err := credential(c.Issuer, c.ClientID, c.TokenURL, id, tok.RefreshToken, expiresIn)
	if renewed != nil {
		renewed.CA = c.CA
	}
	return renewed, err
}

// Session returns who the client's credential signs in.
func (c *Client) Session(ctx context.Context) (*api.Session, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/session", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var s api.Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, c.malformed(err)
	}
	return &s, nil
}
