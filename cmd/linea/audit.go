package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
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
			tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "TIME\tPRINCIPAL\tCLIENT\tOPERATION\tPERMISSION\tSCOPE\tOUTCOME\tRESOURCES")
			for _, e := range page.Events {
				outcome := e.Outcome
				if e.Code != "" {
					outcome += " (" + e.Code + ")"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format(time.DateTime), e.Principal, e.Client, e.Operation,
					e.Permission, e.Scope, outcome, strings.Join(e.Resources, ","))
			}
			if err = tw.Flush(); err == nil && page.NextCursor != "" {
				fmt.Fprintf(stdout, "more: --cursor %s\n", page.NextCursor)
			}
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}
