package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

const artifactsSynopsis = "list --server URL [--kind KIND] [--limit N] [--cursor C] | show --server URL --artifact ID [--revision ID|head [--out FILE]] | save --server URL --file FILE --kind KIND [--label L] [--artifact ID --generation N --head REVISION] | revisions --server URL --artifact ID [--limit N] [--cursor C] [--format text|json] | export --server URL --config ID --scenario ID... --out <file.zip> | import --server URL --archive <file.zip> [--format text|json]"

func artifactsUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: linea artifacts %s\n", artifactsSynopsis)
}

// artifactsCmd dispatches the subcommands for a team server's saved
// artifacts: list, show and save artifacts, list an artifact's revisions, export a configuration and
// scenarios for CI, or import such an archive back.
func artifactsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		artifactsUsage(stderr)
		return exitError
	}
	rest := args[1:]
	switch args[0] {
	case "help", "-h", "-help", "--help":
		artifactsUsage(stdout)
		return exitOK
	case "list":
		return artifactsListCmd(ctx, rest, stdout, stderr)
	case "show":
		return artifactsShowCmd(ctx, rest, stdout, stderr)
	case "save":
		return artifactsSaveCmd(ctx, rest, stdout, stderr)
	case "revisions":
		return artifactsRevisionsCmd(ctx, rest, stdout, stderr)
	case "export":
		return artifactsExportCmd(ctx, rest, stdout, stderr)
	case "import":
		return artifactsImportCmd(ctx, rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown artifacts command %q\n", args[0])
	artifactsUsage(stderr)
	return exitError
}

// artifactsRevisionsCmd prints one page of an artifact's revision metadata,
// newest first, and the cursor of the next page.
func artifactsRevisionsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("artifacts revisions", "--server URL --artifact ID [--limit N] [--cursor C] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	var req platform.ListArtifactRevisionsRequest
	fs.StringVar(&req.ArtifactID, "artifact", "", "saved artifact ID")
	fs.IntVar(&req.Limit, "limit", 0, "revisions per page; 0 uses the server default")
	fs.StringVar(&req.Cursor, "cursor", "", "next cursor printed by the previous page")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || req.ArtifactID == "" {
		fmt.Fprintln(stderr, "--server and --artifact are required: artifacts are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	page, err := c.ListArtifactRevisions(ctx, req)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		err = writeIndented(stdout, page)
	} else {
		err = writeRevisions(stdout, page)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// writeRevisions prints one row per revision and, when there is one, the
// cursor of the next page.
func writeRevisions(w io.Writer, page *platform.ArtifactRevisionPage) error {
	t := newTextOut(w)
	t.line(0, "artifact %s", page.Artifact.ID)
	t.field(1, "kind", string(page.Artifact.Kind))
	t.field(1, "label", page.Artifact.Label)
	t.heading("revisions")
	if len(page.Items) == 0 {
		t.none("revisions")
	}
	rows := make([][]string, 0, len(page.Items))
	for _, r := range page.Items {
		origin := "-"
		if r.Origin != nil {
			origin = r.Origin.Kind
		}
		head := ""
		if r.Ref.RevisionID == page.Artifact.Head {
			head = " (head)"
		}
		rows = append(rows, []string{r.Ref.RevisionID + head, fmt.Sprint(r.Generation), r.Created.Local().Format(time.DateTime), fmt.Sprint(r.Bytes), origin})
	}
	t.table(1, []string{"revision", "generation", "created", "bytes", "origin"}, rows)
	t.nextPage(page.NextCursor)
	return t.err()
}

// writeImport prints one row per archive file with what the import did to it.
func writeImport(w io.Writer, items []platform.ImportedArtifact) error {
	t := newTextOut(w)
	t.line(0, "imported %s", plural(len(items), "file"))
	rows := make([][]string, 0, len(items))
	for _, item := range items {
		rows = append(rows, []string{item.Path, item.Status, item.Artifact.ID})
	}
	t.table(1, []string{"file", "status", "artifact"}, rows)
	return t.err()
}

// artifactsExportCmd writes the CI archive of a saved configuration and
// scenarios, each at its head revision, to a new file.
func artifactsExportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("artifacts export", "--server URL --config ID --scenario ID... --out <file.zip>", stderr)
	server := serverFlag(fs)
	var req api.ExportForCIRequest
	var scenarios stringsFlag
	fs.StringVar(&req.ConfigurationID, "config", "", "saved configuration artifact ID")
	fs.Var(&scenarios, "scenario", "saved scenario artifact ID (repeatable, 1 to 32)")
	out := fs.String("out", "", "zip file to create; an existing file is never overwritten")
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if *server == "" || req.ConfigurationID == "" || len(scenarios) == 0 || *out == "" {
		fmt.Fprintln(stderr, "--server, --config, --scenario and --out are required")
		fs.Usage()
		return exitError
	}
	req.ScenarioIDs = scenarios
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.ExportArtifactsForCI(ctx, req)
	if err != nil {
		return fail(stderr, err)
	}
	if err := writeNew(*out, string(raw)); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "wrote %s: collector/config.yaml, %d scenarios under collector/tests and a GitHub Actions workflow\n", *out, len(scenarios))
	return exitOK
}

// artifactsImportCmd imports a CI archive, possibly edited, into the server's
// store and prints what it did with each file.
func artifactsImportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("artifacts import", "--server URL --archive <file.zip> [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	path := fs.String("archive", "", "CI archive written by artifacts export")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *path == "" {
		fmt.Fprintln(stderr, "--server and --archive are required")
		fs.Usage()
		return exitError
	}
	info, err := os.Stat(*path)
	if err != nil {
		return fail(stderr, err)
	}
	if info.Size() > platform.MaxCIArchiveBytes {
		return fail(stderr, fmt.Errorf("--archive %s exceeds %d bytes", *path, platform.MaxCIArchiveBytes))
	}
	archive, err := os.ReadFile(*path)
	if err != nil {
		return fail(stderr, err)
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	imp, err := c.ImportArtifacts(ctx, archive)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		err = writeIndented(stdout, imp)
	} else {
		err = writeImport(stdout, imp.Items)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}
