package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Saved work on a team server: list
// artifacts, show one or a revision's content, and save a file as a new
// artifact or revision. --format json prints the server's JSON as sent.

func artifactsListCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("artifacts list", "--server URL [--kind KIND] [--limit N] [--cursor C] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	var req platform.ListArtifactsRequest
	kind := fs.String("kind", "", "only this kind: configuration, fixture, scenario, setup or manifest")
	fs.IntVar(&req.Limit, "limit", 0, "artifacts per page; 0 uses the server default")
	fs.StringVar(&req.Cursor, "cursor", "", "next cursor printed by the previous page")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" {
		fmt.Fprintln(stderr, "--server is required: artifacts are kept by linea-server")
		fs.Usage()
		return exitError
	}
	req.Kind = platform.ArtifactKind(*kind)
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.ListArtifacts(ctx, req)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		var page platform.ArtifactPage
		if err = json.Unmarshal(raw, &page); err == nil {
			err = writeArtifactList(stdout, &page)
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// writeArtifactList prints one row per saved artifact and the next page.
func writeArtifactList(w io.Writer, page *platform.ArtifactPage) error {
	t := newTextOut(w)
	if len(page.Items) == 0 {
		t.none("artifacts")
	}
	rows := make([][]string, 0, len(page.Items))
	for _, m := range page.Items {
		rows = append(rows, []string{m.ID, string(m.Kind), fmt.Sprint(m.Generation), m.Updated.Local().Format(time.DateTime), m.Label})
	}
	t.table(0, []string{"artifact", "kind", "generation", "updated", "label"}, rows)
	t.nextPage(page.NextCursor)
	return t.err()
}

// writeArtifactMetadata prints one saved artifact's metadata.
func writeArtifactMetadata(w io.Writer, m *platform.ArtifactMetadata) error {
	t := newTextOut(w)
	t.line(0, "artifact %s", m.ID)
	t.field(1, "kind", string(m.Kind))
	t.field(1, "label", m.Label)
	t.field(1, "generation", fmt.Sprint(m.Generation))
	t.field(1, "head", m.Head)
	t.field(1, "updated", m.Updated.Local().Format(time.DateTime))
	return t.err()
}

// artifactsShowCmd prints an artifact's metadata or, with --revision, that
// revision's content (to --out, which is never overwritten, when given).
func artifactsShowCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("artifacts show", "--server URL --artifact ID [--revision ID [--out FILE]] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	id := fs.String("artifact", "", "saved artifact ID")
	revision := fs.String("revision", "", "revision to read with its content; head is the artifact's head revision")
	out := fs.String("out", "", "with --revision, file to write the content to; an existing file is never overwritten")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *id == "" || (*out != "" && *revision == "") {
		fmt.Fprintln(stderr, "--server and --artifact are required, and --out needs --revision")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.GetArtifact(ctx, *id)
	if err != nil {
		return fail(stderr, err)
	}
	var m platform.ArtifactMetadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return fail(stderr, err)
	}
	if *revision == "" {
		if *format == "json" {
			_, err = stdout.Write(raw)
		} else {
			err = writeArtifactMetadata(stdout, &m)
		}
		if err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	if *revision == "head" {
		*revision = m.Head
	}
	if raw, err = c.GetArtifactRevision(ctx, *id, *revision); err != nil {
		return fail(stderr, err)
	}
	var rev platform.ArtifactRevision
	if err := json.Unmarshal(raw, &rev); err != nil {
		return fail(stderr, err)
	}
	switch {
	case *out != "":
		if err = writeNew(*out, string(rev.Content)); err == nil {
			_, err = fmt.Fprintf(stdout, "wrote %s (revision %s, %d bytes)\n", *out, rev.Revision.Ref.RevisionID, len(rev.Content))
		}
	case *format == "json":
		_, err = stdout.Write(raw)
	default:
		_, err = stdout.Write(rev.Content)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// artifactsSaveCmd saves a file's bytes unchanged in the server's store.
// A revision names the generation and head it was based on; a stale one is
// refused with the current head.
func artifactsSaveCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("artifacts save", "--server URL --file FILE --kind KIND [--label L] [--artifact ID --generation N --head REVISION] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	var req api.SaveRequest
	file := fs.String("file", "", "file whose bytes are saved unchanged")
	kind := fs.String("kind", "", "configuration, fixture, scenario, setup or manifest")
	fs.StringVar(&req.Label, "label", "", "display label; defaults to --file")
	fs.StringVar(&req.ArtifactID, "artifact", "", "artifact to revise; omit to create a new artifact")
	fs.Uint64Var(&req.ExpectedGeneration, "generation", 0, "with --artifact, the generation the edit was based on")
	fs.StringVar(&req.ExpectedHead, "head", "", "with --artifact, the head revision the edit was based on")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *file == "" || *kind == "" {
		fmt.Fprintln(stderr, "--server, --file and --kind are required: artifacts are kept by linea-server")
		fs.Usage()
		return exitError
	}
	req.Kind = platform.ArtifactKind(*kind)
	if req.Label == "" {
		req.Label = *file
	}
	var err error
	if req.Content, err = os.ReadFile(*file); err != nil {
		return fail(stderr, err)
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	// The save names the workspace the listing reports, as the web does.
	listing, err := c.ListArtifacts(ctx, platform.ListArtifactsRequest{Limit: 1})
	if err != nil {
		return fail(stderr, err)
	}
	var page platform.ArtifactPage
	if err := json.Unmarshal(listing, &page); err != nil {
		return fail(stderr, err)
	}
	req.WorkspaceID = page.Workspace.ID
	raw, err := c.SaveArtifact(ctx, req)
	var refused *client.Error
	if errors.As(err, &refused) && refused.Current != nil {
		fmt.Fprintf(stderr, "%v\ncurrent: generation %d, head %s; read it with artifacts show --revision head and save again\n", err,
			refused.Current.Generation, refused.Current.Head)
		return exitError
	}
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		var saved platform.SavedArtifact
		if err = json.Unmarshal(raw, &saved); err == nil {
			_, err = fmt.Fprintf(stdout, "saved %s %s (%s): generation %d, revision %s\n", saved.Artifact.Kind, saved.Artifact.Label, saved.Artifact.ID,
				saved.Artifact.Generation, saved.Revision.Ref.RevisionID)
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}
