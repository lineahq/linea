package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// revokeAdmin is linea-server revoke-admin: after the operator rotates the
// built-in admin account's password, it revokes the account's API tokens and
// ends its web sessions in the server's storage, which every running server
// checks on each request. It reads the same server config as the server and
// works whether or not the account is enabled. It prints counts only.
func revokeAdmin(ctx context.Context, args []string, stdout, stderr io.Writer, env environment) int {
	fs := flag.NewFlagSet("linea-server revoke-admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "the server config file, as for linea-server; optional, and its LINEA_* environment variables apply")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "linea-server revoke-admin: unexpected argument %q\n", fs.Arg(0))
		return exitError
	}
	t, err := loadConfig(*configPath, env)
	if errors.Is(err, errNoConfig) {
		fmt.Fprintln(stderr, noConfig)
		return exitError
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	repo, err := OpenStore(ctx, t.store)
	if err != nil {
		return fail(stderr, err)
	}
	defer repo.Close()
	tokens, sessions, err := platform.RevokeAdminAccess(ctx, repo, time.Now())
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "revoked %d API tokens and ended %d web sessions of the admin account\n", tokens, sessions)
	return exitOK
}
