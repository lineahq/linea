package main

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// captureCommands are the capture subcommands.
var captureCommands = []string{"collectors", "collector", "points", "groups", "group", "group-members", "validate", "start", "list", "status", "wait", "stop", "records", "fixture", "scenario", "export", "delete", "update"}

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
	switch command {
	case "collectors":
		page, err := c.ListCollectors(ctx, req)
		if err != nil {
			return fail(stderr, err)
		}
		result = page
		if *format == "text" {
			if err := writeCollectors(stdout, page); err != nil {
				return fail(stderr, err)
			}
		}
	case "collector":
		item, err := c.GetCollector(ctx, *id)
		if err != nil {
			return fail(stderr, err)
		}
		result = item
		if *format == "text" {
			if err := writeCollector(stdout, item); err != nil {
				return fail(stderr, err)
			}
		}
	case "points":
		page, err := c.ListCapturePoints(ctx, *id, req)
		if err != nil {
			return fail(stderr, err)
		}
		result = page
		if *format == "text" {
			if err := writeCapturePoints(stdout, page); err != nil {
				return fail(stderr, err)
			}
		}
	}
	if *format == "json" {
		if err := platform.Encode(stdout, result); err != nil {
			return fail(stderr, err)
		}
	}
	return exitOK
}

// writeCollectors prints the page of Collectors; with live capture off it
// says how to turn it on.
func writeCollectors(w io.Writer, page *platform.CollectorPage) error {
	t := newTextOut(w)
	if !page.Enabled {
		t.line(0, "live capture is off; start linea-server with --fleet-url and --fleet-token-file")
	} else if len(page.Items) == 0 {
		t.none("collectors")
	}
	var rows [][]string
	for _, item := range page.Items {
		rows = append(rows, []string{item.ID, item.Cluster + "/" + item.Namespace + "/" + item.Pod, item.CollectorGroup,
			strconv.FormatBool(item.Enabled), strconv.Itoa(item.PointCount), strconv.Itoa(item.RawPointCount)})
	}
	t.table(0, []string{"id", "cluster/namespace/pod", "group", "enabled", "points", "raw points"}, rows)
	t.nextPage(page.NextCursor)
	return t.err()
}

// writeCollector prints one Collector and what live capture knows of it.
func writeCollector(w io.Writer, item *platform.Collector) error {
	t := newTextOut(w)
	t.line(0, "collector %s", item.ID)
	t.field(1, "cluster", item.Cluster)
	t.field(1, "namespace", item.Namespace)
	t.field(1, "pod", item.Pod)
	t.field(1, "group", item.CollectorGroup)
	t.field(1, "fleet agent", item.AgentID)
	t.field(1, "generation", strconv.FormatUint(item.Generation, 10))
	t.field(1, "collector version", item.CollectorVersion)
	t.field(1, "distribution version", item.DistributionVersion)
	t.field(1, "capture enabled", strconv.FormatBool(item.Enabled))
	t.field(1, "points", fmt.Sprintf("%d (%d raw)", item.PointCount, item.RawPointCount))
	return t.err()
}

// writeCapturePoints prints a Collector's capture points.
func writeCapturePoints(w io.Writer, page *platform.CapturePointPage) error {
	t := newTextOut(w)
	if len(page.Items) == 0 {
		t.none("capture points")
	}
	var rows [][]string
	for _, p := range page.Items {
		rows = append(rows, []string{p.Pipeline, p.Point, p.Signal, p.Sensitivity})
	}
	t.table(0, []string{"pipeline", "point", "signal", "sensitivity"}, rows)
	t.nextPage(page.NextCursor)
	return t.err()
}
