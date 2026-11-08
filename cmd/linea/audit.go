package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// audit pages a server's audit log, newest first. It needs the
// audit.read permission.

type auditListing struct {
	Events []struct {
		Seq        uint64    `json:"seq"`
		Time       time.Time `json:"time"`
		Principal  string    `json:"principal"`
		Client     string    `json:"client"`
		Operation  string    `json:"operation"`
		Permission string    `json:"permission"`
		Scope      string    `json:"scope"`
		Resources  []string  `json:"resources"`
		Outcome    string    `json:"outcome"`
		Code       string    `json:"code"`
	} `json:"events"`
	NextCursor string `json:"next_cursor"`
}

func auditCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("audit", "--server URL [--principal ID] [--operation NAME] [--outcome allowed|denied] [--since RFC3339] [--limit N] [--cursor C] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	var f platform.AuditFilter
	fs.StringVar(&f.Principal, "principal", "", "only this principal's events, such as user:alice")
	fs.StringVar(&f.Operation, "operation", "", "only this operation's events, such as SubmitRun")
	outcome := fs.String("outcome", "", "only allowed or only denied operations")
	since := fs.String("since", "", "only events at or after this RFC 3339 time")
	fs.IntVar(&f.Limit, "limit", 0, "events per page, up to 500; 0 uses the server default")
	fs.StringVar(&f.Cursor, "cursor", "", "next cursor printed by the previous page")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" {
		fmt.Fprintln(stderr, "--server is required: the audit log is kept by linea-server")
		fs.Usage()
		return exitError
	}
	f.Outcome = platform.AuditOutcome(*outcome)
	if *since != "" {
		t, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			fmt.Fprintln(stderr, "--since must be an RFC 3339 time, such as 2026-10-28T09:00:00Z")
			return exitError
		}
		f.Since = t
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.ListAuditEvents(ctx, f)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		var page auditListing
		if err = json.Unmarshal(raw, &page); err == nil {
			err = writeAudit(stdout, &page)
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// writeAudit prints one page of events, newest first. The outcome is the
// last column, so its color never shifts the others.
func writeAudit(w io.Writer, page *auditListing) error {
	t := newTextOut(w)
	if len(page.Events) == 0 {
		t.none("audit events")
		return t.err()
	}
	var rows [][]string
	for _, e := range page.Events {
		outcome := t.status(e.Outcome, 0)
		if e.Code != "" {
			outcome += " (" + e.Code + ")"
		}
		rows = append(rows, []string{e.Time.Local().Format(time.DateTime), e.Principal, e.Client, e.Operation,
			e.Permission, e.Scope, orDash(strings.Join(e.Resources, ",")), outcome})
	}
	t.table(0, []string{"time", "principal", "client", "operation", "permission", "scope", "resources", "outcome"}, rows)
	t.nextPage(page.NextCursor)
	return t.err()
}
