package main

import (
	"context"
	"fmt"
	"io"

	"github.com/lineahq/linea/internal/debugger/platform"
)

func captureCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(
			stderr,
			"usage: linea capture <collectors|collector|points|groups|group|group-members|validate|start|list|status|wait|stop|records|fixture|scenario|export|delete|update> --server URL [flags]",
		)
		return exitError
	}
	command := args[0]
	switch command {
	case "groups", "group", "group-members", "update":
		return captureManagementCmd(ctx, command, args[1:], stdout, stderr)
	}
	if command == "records" || command == "fixture" || command == "scenario" || command == "export" ||
		command == "delete" {
		return captureDataCmd(ctx, command, args[1:], stdout, stderr)
	}
	if command == "validate" || command == "start" || command == "list" || command == "status" || command == "wait" ||
		command == "stop" {
		return captureControlCmd(ctx, command, args[1:], stdout, stderr)
	}
	if command == "-h" || command == "--help" || command == "help" {
		fmt.Fprintln(
			stdout,
			"usage: linea capture <collectors|collector|points|groups|group|group-members|validate|start|list|status|wait|stop|records|fixture|scenario|export|delete|update> --server URL [--collector ID] [--request request.json] [--session ID] [--limit N] [--cursor CURSOR] [--format text|json]",
		)
		return exitOK
	}
	if command != "collectors" && command != "collector" && command != "points" {
		return fail(stderr, fmt.Errorf("unknown capture command %q", command))
	}
	fs := newFlagSet(
		"capture "+command,
		"--server URL [--collector ID] [--limit N] [--cursor CURSOR] [--format text|json]",
		stderr,
	)
	server := serverFlag(fs)
	id := fs.String("collector", "", "Collector ID returned by capture collectors")
	limit := fs.Int("limit", 25, "maximum entries per page, up to 100 and bounded by the server; zero uses its default")
	cursor := fs.String("cursor", "", "opaque cursor from the preceding page")
	format := formatFlag(fs, "text")
	if code, ok := parse(fs, args[1:], format); !ok {
		return code
	}
	if *server == "" {
		return fail(stderr, fmt.Errorf("capture discovery requires --server for a team server connected to fleet"))
	}
	if command != "collectors" && *id == "" {
		return fail(stderr, fmt.Errorf("--collector is required"))
	}
	if command == "collectors" && *id != "" {
		return fail(stderr, fmt.Errorf("--collector is only for collector and points"))
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	req := platform.CollectorPageRequest{Limit: *limit, Cursor: *cursor}
	var result any
	next := ""
	switch command {
	case "collectors":
		page, err := c.ListCollectors(ctx, req)
		if err != nil {
			return fail(stderr, err)
		}
		result = page
		next = page.NextCursor
		if *format == "text" {
			if !page.Enabled {
				fmt.Fprintln(stdout, "Live capture is off; start linea-server with --fleet-url and --fleet-token-file.")
			}
			for _, item := range page.Items {
				fmt.Fprintf(
					stdout,
					"%s\t%s/%s/%s\tgroup=%s\tenabled=%t\t%d points\t%d raw\n",
					item.ID,
					item.Cluster,
					item.Namespace,
					item.Pod,
					item.CollectorGroup,
					item.Enabled,
					item.PointCount,
					item.RawPointCount,
				)
			}
		}
	case "collector":
		item, err := c.GetCollector(ctx, *id)
		if err != nil {
			return fail(stderr, err)
		}
		result = item
		if *format == "text" {
			fmt.Fprintf(
				stdout,
				"%s\nCollector: %s/%s/%s\nGroup: %s\nFleet agent: %s\nGeneration: %d\nCollector version: %s\nDistribution version: %s\nCapture enabled: %t\nPoints: %d (%d raw)\n",
				item.ID,
				item.Cluster,
				item.Namespace,
				item.Pod,
				item.CollectorGroup,
				item.AgentID,
				item.Generation,
				item.CollectorVersion,
				item.DistributionVersion,
				item.Enabled,
				item.PointCount,
				item.RawPointCount,
			)
		}
	case "points":
		page, err := c.ListCapturePoints(ctx, *id, req)
		if err != nil {
			return fail(stderr, err)
		}
		result = page
		next = page.NextCursor
		if *format == "text" {
			for _, point := range page.Items {
				fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", point.Pipeline, point.Point, point.Signal, point.Sensitivity)
			}
		}
	}
	if *format == "json" {
		if err := platform.Encode(stdout, result); err != nil {
			return fail(stderr, err)
		}
	} else if next != "" {
		fmt.Fprintf(stdout, "Next page: --cursor %s\n", next)
	}
	return exitOK
}
