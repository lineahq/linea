package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/lineahq/linea/internal/debugger/client"
)

// login signs the CLI in to a team server with the identity provider's device
// flow, or with the built-in admin account's password, and keeps the sign-in in the user's
// config directory; later --server commands to that server present it, and
// renew it when it expires. $LINEA_TOKEN still takes precedence.
// --ca trusts a private CA for the server and its identity provider; the
// sign-in keeps it for every later --server command to that server.

// loginWait waits between device-flow polls; tests replace it.
var loginWait func(ctx context.Context, d time.Duration) error

// The password sign-in reads the terminal and standard input; tests replace
// them.
var (
	loginStdin      io.Reader = os.Stdin
	loginIsTerminal           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	loginReadSecret           = func() ([]byte, error) { return term.ReadPassword(int(os.Stdin.Fd())) }
)

// maxLoginPassword bounds the admin password read from standard input.
const maxLoginPassword = 1024

// renewBefore renews a stored sign-in this long before it expires.
const renewBefore = time.Minute

func loginCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("login", "--server URL [--ca FILE] [--admin | --password-stdin]", stderr)
	server := serverFlag(fs)
	caFile := fs.String("ca", "", "PEM file of a private CA verifying the server and its identity provider, kept with the sign-in (default: the system roots, or the CA of the previous sign-in)")
	admin := fs.Bool("admin", false, "sign in as the built-in admin account with its password, prompted for on the terminal without echo, instead of the identity provider (a server that offers only the password needs no flag)")
	passwordStdin := fs.Bool("password-stdin", false, "like --admin, reading the password from the first line of standard input, for scripts")
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
	methods, err := client.SignInMethods(ctx, *server, ca)
	if errors.Is(err, client.ErrNoCLISignIn) {
		return noCLISignIn(ctx, *server, ca, stdout, stderr)
	}
	if err != nil {
		return fail(stderr, err)
	}
	// The device flow is the default; the password is for a server that offers
	// nothing else, or when asked for with --admin or --password-stdin.
	wantPassword := *admin || *passwordStdin
	if wantPassword && !methods.Password {
		return fail(stderr, fmt.Errorf("%s does not accept the admin password; sign in without --admin and --password-stdin", *server))
	}
	var cred *client.Credential
	if wantPassword || methods.Issuer == "" {
		cred, err = passwordSignIn(ctx, *server, ca, *passwordStdin, stderr)
	} else {
		cred, err = client.Login(ctx, *server, client.LoginOptions{
			Prompt: func(where, code string) {
				fmt.Fprintf(stderr, "To sign in, open %s and enter the code %s\n", where, code)
			},
			Wait: loginWait,
			CA:   ca,
		})
	}
	if err != nil {
		return fail(stderr, loginError(err, *server))
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

// passwordSignIn signs in as the built-in admin account. The password comes
// from the first line of standard input with fromStdin, otherwise from the
// terminal without echo; it is never echoed, printed or logged.
func passwordSignIn(ctx context.Context, server string, ca []byte, fromStdin bool, stderr io.Writer) (*client.Credential, error) {
	var password string
	if fromStdin {
		line, err := bufio.NewReader(io.LimitReader(loginStdin, maxLoginPassword+3)).ReadString('\n')
		if err != nil && !(errors.Is(err, io.EOF) && line != "") {
			return nil, errors.New("--password-stdin: no password on standard input")
		}
		password = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	} else {
		if !loginIsTerminal() {
			return nil, errors.New("the admin password is prompted for on a terminal, and standard input is not one; pass --password-stdin to read it from standard input")
		}
		fmt.Fprintf(stderr, "Admin password for %s: ", server)
		secret, err := loginReadSecret()
		fmt.Fprintln(stderr)
		if err != nil {
			return nil, fmt.Errorf("reading the password: %w", err)
		}
		password = string(secret)
	}
	if password == "" {
		return nil, errors.New("the admin password is empty")
	}
	if len(password) > maxLoginPassword {
		return nil, fmt.Errorf("the admin password is longer than %d bytes", maxLoginPassword)
	}
	return client.PasswordLogin(ctx, server, password, ca)
}

// loginError words a sign-in's failure for the user.
func loginError(err error, server string) error {
	var throttled *client.ThrottledError
	switch {
	case errors.Is(err, client.ErrPasswordRefused):
		return errors.New("sign-in refused: wrong admin password")
	case errors.As(err, &throttled):
		return fmt.Errorf("sign-in refused: %w", throttled)
	case errors.Is(err, client.ErrNoPasswordSignIn):
		return fmt.Errorf("%s has no admin password sign-in", server)
	}
	return err
}

// noCLISignIn answers login for a server without CLI sign-in. When the server
// already signs requests in, such as behind an authenticating proxy, no
// sign-in is needed; otherwise it takes an API token.
func noCLISignIn(ctx context.Context, server string, ca []byte, stdout, stderr io.Writer) int {
	if c, err := client.NewWithCA(server, "", ca); err == nil {
		if s, err := c.Session(ctx); err == nil {
			fmt.Fprintf(stdout, "no sign-in needed: %s already signs requests in as %s (%s); pass --server to any command\n", server, s.Principal.Name, s.Principal.ID)
			return exitOK
		}
	}
	fmt.Fprintf(stderr, "error: %s has no CLI sign-in; ask for an API token and set %s\n", server, serverTokenEnv)
	return exitError
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
	// An API token from the admin password has no refresh token: it is used
	// until it expires, then the user signs in again.
	if cred.RefreshToken == "" && time.Now().Before(cred.Expiry) {
		return cred, nil
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
