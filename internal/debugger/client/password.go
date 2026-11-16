package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Password sign-in: linea login can sign in to a team server with the
// built-in admin account's password. The server answers an API token, which
// the CLI keeps as a Credential that has no identity provider: the token is
// the bearer token (Credential.IDToken), it lasts until its expiry and is
// not renewed, so signing in again replaces it.

// AdminUser is the built-in admin account's user name.
const AdminUser = "admin"

// maxPasswordReply bounds the server's answer to a password sign-in.
const maxPasswordReply = 1 << 16

var (
	// ErrPasswordRefused is PasswordLogin's error for a wrong password.
	ErrPasswordRefused = errors.New("wrong admin password")
	// ErrNoPasswordSignIn is PasswordLogin's error for a server without the
	// built-in admin account's password sign-in.
	ErrNoPasswordSignIn = errors.New("the server has no admin password sign-in")
)

// ThrottledError is PasswordLogin's error when the server has seen too many
// failed sign-ins; RetryAfter is how long it asks to wait, zero when it
// named no time.
type ThrottledError struct {
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("too many failed sign-ins; retry after %d seconds", int64(e.RetryAfter.Round(time.Second)/time.Second))
	}
	return "too many failed sign-ins; retry later"
}

// SignInMethods asks the server how the CLI can sign in: GET /auth/cli. It
// returns ErrNoCLISignIn (wrapped) for a server that offers none. Issuer is
// set when the server offers the identity provider's device flow, Password
// when it accepts the admin password.
func SignInMethods(ctx context.Context, server string, ca []byte) (*platform.CLISignIn, error) {
	base, err := serverBase(server, true)
	if err != nil {
		return nil, err
	}
	return cliSignIn(ctx, base, ca)
}

// PasswordLogin signs in to the team server as the built-in admin account
// and returns the API token the server issues as a credential that keeps ca.
// The password is sent only in the request body and is never part of an
// error.
func PasswordLogin(ctx context.Context, server, password string, ca []byte) (*Credential, error) {
	base, err := serverBase(server, true)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(platform.PasswordSignIn{User: AdminUser, Password: password, Client: "cli"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/auth/password", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc, err := serverHTTP(ca)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxPasswordReply))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, ErrPasswordRefused
	case http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", base, ErrNoPasswordSignIn)
	case http.StatusTooManyRequests:
		e := &ThrottledError{}
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
		return nil, e
	default:
		return nil, fmt.Errorf("%s refused the sign-in (%s): %s", base, resp.Status, strings.TrimSpace(string(raw)))
	}
	var tok platform.PasswordToken
	if err := json.Unmarshal(raw, &tok); err != nil || tok.Token == "" || tok.Expires.IsZero() {
		return nil, fmt.Errorf("%s answered the sign-in without an API token and its expiry", base)
	}
	return &Credential{IDToken: tok.Token, Expiry: tok.Expires, CA: string(ca)}, nil
}
