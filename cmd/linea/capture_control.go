package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lineahq/linea/internal/debugger/platform"
)

func captureControlCmd(ctx context.Context, command string, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("capture "+command, "--server URL [--request selection.json] [--session ID] [--limit N] [--cursor CURSOR] [--format text|json]", stderr)
	server := serverFlag(fs)
	request := fs.String("request", "", "JSON capture selection file for start/validate")
	id := fs.String("session", "", "capture session ID for status/wait/stop")
	limit := fs.Int("limit", 25, "page size for list")
	cursor := fs.String("cursor", "", "opaque list cursor")
	format := formatFlag(fs, "text")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" {
		return fail(stderr, fmt.Errorf("capture controls require --server"))
	}
	needsRequest := command == "start" || command == "validate"
	needsID := command == "status" || command == "wait" || command == "stop"
	if needsRequest != (*request != "") {
		return fail(stderr, fmt.Errorf("--request is required only for start/validate"))
	}
	if needsID != (*id != "") {
		return fail(stderr, fmt.Errorf("--session is required only for status/wait/stop"))
	}
	if command != "list" && (*limit != 25 || *cursor != "") {
		return fail(stderr, fmt.Errorf("paging flags are only for list"))
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	var result any
	if needsRequest {
		var req platform.CaptureSelection
		if req, err = readCaptureSelection(*request); err != nil {
			return fail(stderr, err)
		}
		if command == "start" {
			result, err = c.CreateCaptureSession(ctx, req)
		} else {
			result, err = c.ValidateCaptureRequest(ctx, req)
		}
	} else {
		switch command {
		case "list":
			result, err = c.ListCaptureSessions(ctx, platform.CollectorPageRequest{Limit: *limit, Cursor: *cursor})
		case "status", "wait":
			result, err = c.GetCaptureSession(ctx, *id, command == "wait")
		case "stop":
			result, err = c.StopCaptureSession(ctx, *id)
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		if err := platform.Encode(stdout, result); err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	switch v := result.(type) {
	case *platform.CaptureSession:
		printCaptureSession(stdout, *v)
	case *platform.CaptureSessionPage:
		for _, session := range v.Items {
			fmt.Fprintf(stdout, "%s\t%s\tterminal=%t\ttargets=%d\tactive=%d\tuncertain=%d\tfailed=%d\n", session.ID, session.State, session.Terminal, session.TargetCount, session.ActiveCount, session.UncertainCount, session.FailedCount)
		}
		if v.NextCursor != "" {
			fmt.Fprintf(stdout, "Next page: --cursor %s\n", v.NextCursor)
		}
	case *platform.CaptureValidation:
		fmt.Fprintf(stdout, "Preflight accepted: %d Collectors, %ds duration. Fleet admits the capture on start; budgets apply per Collector.\n", len(v.Collectors), v.Selection.DurationSeconds)
	}
	return exitOK
}

// readCaptureSelection reads one capture selection document of at most 64 KiB.
func readCaptureSelection(path string) (platform.CaptureSelection, error) {
	var req platform.CaptureSelection
	file, err := os.Open(path)
	if err != nil {
		return req, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return req, err
	}
	if len(raw) > 64<<10 {
		return req, fmt.Errorf("capture selection file exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return req, fmt.Errorf("expected one capture selection document")
	}
	return req, nil
}

func printCaptureSession(out io.Writer, v platform.CaptureSession) {
	fmt.Fprintf(out, "%s\t%s\tterminal=%t\texpires=%s\n", v.ID, v.State, v.Terminal, v.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"))
	if v.StopReason != "" {
		fmt.Fprintf(out, "  stop reason: %s\n", v.StopReason)
	}
	for _, target := range v.Targets {
		stats := platform.CaptureTargetStats{}
		if target.Stats != nil {
			stats = *target.Stats
		}
		fmt.Fprintf(out, "  %s\t%s\tselected=%d\tenqueued_records=%d\tenqueued_bytes=%d\tfilter_errors=%d\n", target.CollectorID, target.State, stats.Selected, stats.EnqueuedRecords, stats.EnqueuedBytes, stats.FilterErrors)
	}
}
