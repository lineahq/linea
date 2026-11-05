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

func captureDataCmd(ctx context.Context, command string, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(
		"capture "+command,
		"--server URL --session ID [--request selection.json] [--collector ID --pipeline NAME --point NAME --signal traces|logs|metrics] [--limit N --cursor CURSOR] [--format text|json]",
		stderr,
	)
	server := serverFlag(fs)
	id := fs.String("session", "", "capture session ID")
	file := fs.String("request", "", "fixture/scenario/export request JSON file (explicit destination config/receiver)")
	collector := fs.String("collector", "", "source Collector ID for record filtering")
	pipeline := fs.String("pipeline", "", "source pipeline")
	point := fs.String("point", "", "source capture point")
	signal := fs.String("signal", "", "source signal")
	limit := fs.Int("limit", 25, "record page entries")
	cursor := fs.String("cursor", "", "opaque record cursor")
	format := formatFlag(fs, "text")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *id == "" {
		return fail(stderr, fmt.Errorf("capture data commands require --server and --session"))
	}
	deriving := command == "fixture" || command == "scenario" || command == "export"
	if deriving != (*file != "") {
		return fail(stderr, fmt.Errorf("--request is required only for fixture/scenario/export"))
	}
	if command != "records" &&
		(*collector != "" || *pipeline != "" || *point != "" || *signal != "" || *limit != 25 || *cursor != "") {
		return fail(stderr, fmt.Errorf("record filters and paging are only for records"))
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	var raw []byte
	if deriving {
		f, err := os.Open(*file)
		if err != nil {
			return fail(stderr, err)
		}
		defer f.Close()
		raw, err = io.ReadAll(io.LimitReader(f, (2<<20)+1))
		if err != nil {
			return fail(stderr, err)
		}
		if len(raw) > 2<<20 {
			return fail(stderr, fmt.Errorf("capture derivation request exceeds 2 MiB"))
		}
	}
	decode := func(v any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(v); err != nil {
			return err
		}
		if err := d.Decode(&struct{}{}); err != io.EOF {
			return fmt.Errorf("expected one derivation request document")
		}
		return nil
	}
	var result any
	switch command {
	case "records":
		result, err = c.GetCaptureRecords(
			ctx,
			*id,
			platform.CaptureRecordsRequest{
				Limit:  *limit,
				Cursor: *cursor,
				Stream: platform.CaptureStream{
					CollectorID: *collector,
					Pipeline:    *pipeline,
					Point:       *point,
					Signal:      *signal,
				},
			},
		)
	case "fixture":
		var req platform.CaptureDeriveRequest
		if err = decode(&req); err == nil {
			result, err = c.CreateFixtureFromCapture(ctx, *id, req)
		}
	case "scenario":
		var req platform.CaptureScenarioRequest
		if err = decode(&req); err == nil {
			result, err = c.CreateScenarioFromCapture(ctx, *id, req)
		}
	case "export":
		var req platform.CaptureExportRequest
		if err = decode(&req); err == nil {
			result, err = c.ExportCaptureSelection(ctx, *id, req)
		}
	case "delete":
		err = c.DeleteCaptureSession(ctx, *id)
		result = struct {
			Deleted bool `json:"deleted"`
		}{true}
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
	case *platform.CaptureFixture:
		fmt.Fprintln(stdout, v.FixtureJSON)
	case *platform.CaptureScenario:
		fmt.Fprint(stdout, v.Export)
	case *platform.CaptureExport:
		fmt.Fprint(stdout, v.Content)
	case *platform.CaptureRecordsPage:
		fmt.Fprintf(stdout, "Receipt watermark: %d\n", v.Watermark)
		for _, item := range v.Items {
			fmt.Fprintf(stdout, "%d\t%s\t%s/%s\t%s\toversize=%t\n", item.Sequence, item.Stream.CollectorID, item.Stream.Pipeline, item.Stream.Point, item.Stream.Signal, item.Oversize)
		}
		if v.NextCursor != "" {
			fmt.Fprintf(stdout, "Next page: --cursor %s\n", v.NextCursor)
		}
	default:
		fmt.Fprintln(stdout, "Capture deleted.")
	}
	return exitOK
}
