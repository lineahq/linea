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

func captureManagementCmd(ctx context.Context, command string, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(
		"capture "+command,
		"--server URL [--group ID] [--session ID] [--request JSON_FILE] [--limit N] [--cursor TOKEN] [--format text|json]",
		stderr,
	)
	server := serverFlag(fs)
	id := fs.String("group", "", "fleet group ID")
	session := fs.String("session", "", "capture ID")
	file := fs.String("request", "", "JSON request file")
	limit := fs.Int("limit", 25, "page entries")
	cursor := fs.String("cursor", "", "opaque page cursor")
	format := formatFlag(fs, "text")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" {
		return fail(stderr, fmt.Errorf("--server is required"))
	}
	paging := command == "groups" || command == "group-members"
	if (command == "update") != (*file != "") || !paging && (*limit != 25 || *cursor != "") {
		return fail(stderr, fmt.Errorf("request and paging flags do not match command"))
	}
	needsGroup := command == "group" || command == "group-members"
	if needsGroup != (*id != "") || (command == "update") != (*session != "") {
		return fail(stderr, fmt.Errorf("group/session ID does not match command"))
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	decode := func(v any) error {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
		if err != nil {
			return err
		}
		if len(raw) > 128<<10 {
			return fmt.Errorf("management request exceeds 128 KiB")
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err = d.Decode(v); err != nil {
			return err
		}
		if err = d.Decode(&struct{}{}); err != io.EOF {
			return fmt.Errorf("expected one request document")
		}
		return nil
	}
	var v any
	switch command {
	case "group":
		v, err = c.GetCollectorGroup(ctx, *id)
	case "groups":
		v, err = c.ListCollectorGroups(ctx, platform.CollectorPageRequest{Limit: *limit, Cursor: *cursor})
	case "group-members":
		v, err = c.ListCollectorGroupMembers(ctx, *id, platform.CollectorPageRequest{Limit: *limit, Cursor: *cursor})
	case "update":
		var q platform.CaptureMetadataUpdate
		if err = decode(&q); err == nil {
			v, err = c.UpdateCaptureMetadata(ctx, *session, q)
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	// Management documents are compact; both formats preserve all fields for review.
	if err = platform.Encode(stdout, v); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}
