package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
)

// access lists and changes a team server's role bindings. The server config's
// bindings are read-only; grant and revoke change the managed bindings the
// server stores. Without --revision a change applies to the revision the
// command reads first, so a concurrent change between the two still fails
// with a conflict rather than being overwritten.

const accessSynopsis = "list|grant|revoke --server URL [flags]"

func accessUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: linea access %s\n", accessSynopsis)
}

func accessCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		accessUsage(stderr)
		return exitError
	}
	rest := args[1:]
	switch args[0] {
	case "help", "-h", "-help", "--help":
		accessUsage(stdout)
		return exitOK
	case "list":
		return accessListCmd(ctx, rest, stdout, stderr)
	case "grant":
		return accessGrantCmd(ctx, rest, stdout, stderr)
	case "revoke":
		return accessRevokeCmd(ctx, rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown access command %q\n", args[0])
	accessUsage(stderr)
	return exitError
}

func accessListCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("access list", "--server URL [--roles] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	roles := fs.Bool("roles", false, "also list the built-in roles and their permissions")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" {
		fmt.Fprintln(stderr, "--server is required: role bindings are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	l, err := c.ListRoleBindings(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	return writeAccess(stdout, stderr, *format, l, *roles)
}

func accessGrantCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("access grant", "--server URL (--group NAME | --principal ID) --role ROLE [--scope *|project:ID] [--revision N] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	var req api.RoleBindingRequest
	fs.StringVar(&req.Group, "group", "", "grant to members of this identity provider group")
	fs.StringVar(&req.Principal, "principal", "", "grant to one principal, user:<issuer>/<subject> or service:<id>")
	fs.StringVar(&req.Role, "role", "", "a built-in role: viewer, engineer, fleet-operator, raw-capture or admin")
	fs.StringVar(&req.Scope, "scope", "*", "where the role applies: * or project:<id>")
	revision := revisionFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || req.Role == "" || (req.Group == "") == (req.Principal == "") {
		fmt.Fprintln(stderr, "--server, --role and exactly one of --group and --principal are required")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	if req.Revision, err = accessRevision(ctx, c, *revision); err != nil {
		return fail(stderr, err)
	}
	l, err := c.AddRoleBinding(ctx, req)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		return writeAccess(stdout, stderr, *format, l, false)
	}
	scope := req.Scope
	if scope == "" {
		scope = "*"
	}
	var added api.RoleBinding
	for _, b := range l.Bindings {
		if b.Source == "managed" && b.Group == req.Group && b.Principal == req.Principal && b.Role == req.Role && b.Scope == scope {
			added = b
		}
	}
	fmt.Fprintf(stdout, "granted %s in %s to %s as binding %s (revision %d)\n", added.Role, added.Scope, bindingSubject(api.RoleBinding{Group: req.Group, Principal: req.Principal}), added.ID, l.Revision)
	return exitOK
}

func accessRevokeCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("access revoke", "--server URL --binding ID [--revision N] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	id := fs.String("binding", "", "the managed binding's ID, as access list shows it")
	revision := revisionFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *id == "" {
		fmt.Fprintln(stderr, "--server and --binding are required")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	rev, err := accessRevision(ctx, c, *revision)
	if err != nil {
		return fail(stderr, err)
	}
	l, err := c.RemoveRoleBinding(ctx, rev, *id)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		return writeAccess(stdout, stderr, *format, l, false)
	}
	fmt.Fprintf(stdout, "binding %s revoked (revision %d)\n", *id, l.Revision)
	return exitOK
}

func revisionFlag(fs *flag.FlagSet) *int64 {
	return fs.Int64("revision", -1, "the managed bindings' revision the change applies to, as access list shows it; by default the current one")
}

// accessRevision is revision, or the server's current one when it is
// negative.
func accessRevision(ctx context.Context, c *client.Client, revision int64) (uint64, error) {
	if revision >= 0 {
		return uint64(revision), nil
	}
	l, err := c.ListRoleBindings(ctx)
	if err != nil {
		return 0, err
	}
	return l.Revision, nil
}

func bindingSubject(b api.RoleBinding) string {
	if b.Group != "" {
		return "group " + b.Group
	}
	return b.Principal
}

func writeAccess(stdout, stderr io.Writer, format string, l *api.RoleBindings, roles bool) int {
	if format == "json" {
		raw, err := encoded(l)
		if err == nil {
			_, err = stdout.Write(raw)
		}
		if err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	t := newTextOut(stdout)
	t.line(0, "managed bindings at revision %d", l.Revision)
	var rows [][]string
	for _, b := range l.Bindings {
		created := "-"
		if b.Created != nil {
			created = b.Created.Local().Format(time.DateTime) + " by " + b.CreatedBy
		}
		rows = append(rows, []string{b.ID, b.Source, bindingSubject(b), b.Role, b.Scope, created})
	}
	if len(rows) == 0 {
		t.line(1, "no bindings")
	}
	t.table(1, []string{"id", "source", "subject", "role", "scope", "created"}, rows)
	if roles {
		t.heading("roles")
		rows = nil
		for _, r := range l.Roles {
			rows = append(rows, []string{r.Name, strings.Join(r.Permissions, ", ")})
		}
		t.table(1, []string{"role", "permissions"}, rows)
	}
	if err := t.err(); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}
