package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
)

// token manages a team server's API tokens: create
// prints the secret alone on stdout, so it can be captured into
// $LINEA_TOKEN, and the token's record on stderr.

const tokenSynopsis = "create|list|revoke --server URL [flags]"

func tokenUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: linea token %s\n", tokenSynopsis)
}

func tokenCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		tokenUsage(stderr)
		return exitError
	}
	rest := args[1:]
	switch args[0] {
	case "help", "-h", "-help", "--help":
		tokenUsage(stdout)
		return exitOK
	case "create":
		return tokenCreateCmd(ctx, rest, stdout, stderr)
	case "list":
		return tokenListCmd(ctx, rest, stdout, stderr)
	case "revoke":
		return tokenRevokeCmd(ctx, rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown token command %q\n", args[0])
	tokenUsage(stderr)
	return exitError
}

func tokenCreateCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token create", "--server URL --label TEXT [--service ID] [--expires-in-days N] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	var req api.TokenRequest
	fs.StringVar(&req.Label, "label", "", "what the token is for, such as laptop or ci-pipeline")
	fs.StringVar(&req.Service, "service", "", "create a token for the service principal service:<ID> (token.manage) instead of a personal one")
	fs.IntVar(&req.ExpiresInDays, "expires-in-days", 0, "lifetime, 1 to 365 days; 0 is 90")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || req.Label == "" {
		fmt.Fprintln(stderr, "--server and --label are required: tokens are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	created, err := c.CreateToken(ctx, req)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		raw, err := encoded(created)
		if err == nil {
			_, err = stdout.Write(raw)
		}
		if err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	t := created.Token
	fmt.Fprintf(stderr, "token %s for %s, expires %s; the secret below is shown only once\n", t.ID, t.Principal, t.Expires.Local().Format(time.DateTime))
	fmt.Fprintln(stdout, created.Secret)
	return exitOK
}

func tokenListCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token list", "--server URL [--all] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	all := fs.Bool("all", false, "list every principal's tokens (token.manage)")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" {
		fmt.Fprintln(stderr, "--server is required: tokens are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	tokens, err := c.ListTokens(ctx, *all)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		raw, err := encoded(tokens)
		if err == nil {
			_, err = stdout.Write(raw)
		}
		if err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	if err := writeTokens(stdout, tokens, time.Now()); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// writeTokens lists tokens. The state is the last column, so its color never
// shifts the others.
func writeTokens(w io.Writer, tokens []api.APIToken, now time.Time) error {
	t := newTextOut(w)
	if len(tokens) == 0 {
		t.none("tokens")
		return t.err()
	}
	var rows [][]string
	for _, tok := range tokens {
		state := "active"
		switch {
		case tok.Revoked != nil:
			state = "revoked"
		case !now.Before(tok.Expires):
			state = "expired"
		}
		rows = append(rows, []string{tok.ID, tok.Principal, tok.Label, tok.Created.Local().Format(time.DateTime), tok.Expires.Local().Format(time.DateTime), t.status(state, 0)})
	}
	t.table(0, []string{"id", "principal", "label", "created", "expires", "state"}, rows)
	return t.err()
}

func tokenRevokeCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token revoke", "--server URL --token ID [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	id := fs.String("token", "", "the token's ID, as token list shows it")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *id == "" {
		fmt.Fprintln(stderr, "--server and --token are required: tokens are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	t, err := c.RevokeToken(ctx, *id)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		raw, err := encoded(t)
		if err == nil {
			_, err = stdout.Write(raw)
		}
		if err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "token %s revoked\n", t.ID)
	return exitOK
}
