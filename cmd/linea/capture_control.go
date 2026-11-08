package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

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
		err = writeCaptureSession(stdout, v)
	case *platform.CaptureSessionPage:
		err = writeCaptureSessions(stdout, v)
	case *platform.CaptureValidation:
		err = writeCaptureValidation(stdout, v)
	}
	if err != nil {
		return fail(stderr, err)
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

// writeCaptureSession prints the capture's state, then each target
// Collector's counters.
func writeCaptureSession(w io.Writer, v *platform.CaptureSession) error {
	t := newTextOut(w)
	t.line(0, "%s  capture %s", t.status(v.State, 0), v.ID)
	t.line(1, "terminal: %t", v.Terminal)
	t.field(1, "expires", v.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"))
	t.field(1, "stop reason", v.StopReason)
	if len(v.Targets) > 0 {
		t.heading("targets")
		var rows [][]string
		for _, target := range v.Targets {
			stats := platform.CaptureTargetStats{}
			if target.Stats != nil {
				stats = *target.Stats
			}
			rows = append(rows, []string{target.CollectorID, target.State, strconv.FormatInt(stats.Selected, 10), strconv.FormatInt(stats.EnqueuedRecords, 10),
				strconv.FormatInt(stats.EnqueuedBytes, 10), strconv.FormatInt(stats.FilterErrors, 10)})
		}
		t.table(1, []string{"collector", "state", "selected", "enqueued records", "enqueued bytes", "filter errors"}, rows)
	}
	return t.err()
}

// writeCaptureSessions prints a page of capture summaries.
func writeCaptureSessions(w io.Writer, v *platform.CaptureSessionPage) error {
	t := newTextOut(w)
	if len(v.Items) == 0 {
		t.none("captures")
	}
	var rows [][]string
	for _, s := range v.Items {
		rows = append(rows, []string{s.ID, s.State, strconv.FormatBool(s.Terminal), strconv.Itoa(s.TargetCount), strconv.Itoa(s.ActiveCount),
			strconv.Itoa(s.UncertainCount), strconv.Itoa(s.FailedCount)})
	}
	t.table(0, []string{"id", "state", "terminal", "targets", "active", "uncertain", "failed"}, rows)
	t.nextPage(v.NextCursor)
	return t.err()
}

// writeCaptureValidation prints the verdict of a preflight check.
func writeCaptureValidation(w io.Writer, v *platform.CaptureValidation) error {
	t := newTextOut(w)
	t.line(0, "%s  preflight accepted: %s, %ds duration", t.status("ok", 0), plural(len(v.Collectors), "Collector"), v.Selection.DurationSeconds)
	t.line(1, "Fleet admits the capture on start; budgets apply per Collector.")
	return t.err()
}
