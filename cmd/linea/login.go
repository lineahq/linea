package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lineahq/linea/internal/debugger/client"
)

// login signs the CLI in to a team server's identity provider with the
// device flow and keeps the sign-in in the user's
// config directory; later --server commands to that server present it, and
// renew it when it expires. $LINEA_TOKEN still takes precedence.
// --ca trusts a private CA for the server and its identity provider; the
// sign-in keeps it for every later --server command to that server.

// loginWait waits between device-flow polls; tests replace it.
var loginWait func(ctx context.Context, d time.Duration) error

// renewBefore renews a stored sign-in this long before it expires.
const renewBefore = time.Minute

func loginCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("login", "--server URL [--ca FILE]", stderr)
	server := serverFlag(fs)
	caFile := fs.String("ca", "", "PEM file of a private CA verifying the server and its identity provider, kept with the sign-in (default: the system roots, or the CA of the previous sign-in)")
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if *server == "" {
		fmt.Fprintln(stderr, "--server is required: name the team server to sign in to")
		fs.Usage()
		return exitError
	}
	path, err := client.CredentialsPath()
	if err != nil {
		return fail(stderr, err)
	}
	var ca []byte
	if *caFile != "" {
		if ca, err = os.ReadFile(*caFile); err != nil {
			return fail(stderr, fmt.Errorf("--ca: %w", err))
		}
	} else if prev, err := client.LoadCredential(path, *server); err == nil && prev != nil {
		ca = []byte(prev.CA)
	}
	cred, err := client.Login(ctx, *server, client.LoginOptions{
		Prompt: func(where, code string) {
			fmt.Fprintf(stderr, "To sign in, open %s and enter the code %s\n", where, code)
		},
		Wait: loginWait,
		CA:   ca,
	})
	if err != nil {
		return fail(stderr, err)
	}
	c, err := client.NewWithCA(*server, cred.IDToken, ca)
	if err != nil {
		return fail(stderr, err)
	}
	s, err := c.Session(ctx)
	if err != nil {
		return fail(stderr, fmt.Errorf("the server refused the sign-in: %w", err))
	}
	if err := client.SaveCredential(path, *server, cred); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "signed in to %s as %s (%s)\n", *server, s.Principal.Name, s.Principal.ID)
	return exitOK
}

func logoutCmd(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("logout", "--server URL", stderr)
	server := serverFlag(fs)
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if *server == "" {
		fmt.Fprintln(stderr, "--server is required: name the team server to sign out of")
		fs.Usage()
		return exitError
	}
	path, err := client.CredentialsPath()
	if err != nil {
		return fail(stderr, err)
	}
	cred, err := client.LoadCredential(path, *server)
	if err != nil {
		return fail(stderr, err)
	}
	if cred == nil {
		fmt.Fprintf(stdout, "not signed in to %s\n", *server)
		return exitOK
	}
	if err := client.SaveCredential(path, *server, nil); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "signed out of %s\n", *server)
	return exitOK
}

// signedIn is the stored sign-in to server, its ID token renewed when it is
// about to expire, unless only its CA is wanted; nil when the CLI has not
// signed in to it.
func signedIn(server string, renew bool) (*client.Credential, error) {
	path, err := client.CredentialsPath()
	if err != nil {
		return nil, nil
	}
	cred, err := client.LoadCredential(path, server)
	if err != nil || cred == nil || !renew || time.Until(cred.Expiry) > renewBefore {
		return cred, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	renewed, err := client.Refresh(ctx, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("the sign-in to %s has expired and could not be renewed (%v); run linea login --server %s", server, err, server)
	}
	if err := client.SaveCredential(path, server, renewed); err != nil {
		return nil, err
	}
	return renewed, nil
}
