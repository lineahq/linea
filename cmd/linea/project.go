package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
)

// project lists and manages a team server's projects; library lists the
// library and shares, publishes and copies its artifacts. Without
// --revision a change applies to the revision the command reads first, so a
// concurrent change between the two still fails with a conflict.

const (
	projectSynopsis = "list|create|update|archive|unarchive --server URL [flags]"
	librarySynopsis = "list|share|publish|copy --server URL [flags]"
	// libraryProject is the built-in project that holds the library.
	libraryProject = "library"
)

func projectUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: linea project %s\n", projectSynopsis)
}

func libraryUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: linea library %s\n", librarySynopsis)
}

func projectCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		projectUsage(stderr)
		return exitError
	}
	rest := args[1:]
	switch args[0] {
	case "help", "-h", "-help", "--help":
		projectUsage(stdout)
		return exitOK
	case "list":
		return projectListCmd(ctx, rest, stdout, stderr)
	case "create", "update":
		return projectSaveCmd(ctx, args[0], rest, stdout, stderr)
	case "archive", "unarchive":
		return projectArchiveCmd(ctx, args[0], rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown project command %q\n", args[0])
	projectUsage(stderr)
	return exitError
}

// projectServer parses a project or library command whose positional
// arguments are want IDs and connects to its server.
func projectServer(fs *flag.FlagSet, server *string, format *string, args []string, want int, stderr io.Writer) ([]string, *client.Client, int, bool) {
	pos, code, ok := parseMixed(fs, args, -1)
	if !ok {
		return nil, nil, code, false
	}
	switch {
	case len(pos) != want:
		fmt.Fprintf(stderr, "%s takes %d argument(s)\n", fs.Name(), want)
		fs.Usage()
		return nil, nil, exitError, false
	case *format != "text" && *format != "json":
		fmt.Fprintf(stderr, "unknown format %q\n", *format)
		fs.Usage()
		return nil, nil, exitError, false
	case *server == "":
		fmt.Fprintln(stderr, "--server is required: projects are kept by linea-server")
		fs.Usage()
		return nil, nil, exitError, false
	}
	c, err := connect(*server, false)
	if err != nil {
		return nil, nil, fail(stderr, err), false
	}
	return pos, c, exitOK, true
}

func projectListCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project list", "--server URL [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	_, c, code, ok := projectServer(fs, server, format, args, 0, stderr)
	if !ok {
		return code
	}
	l, err := c.ListProjects(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	return writeProjects(stdout, stderr, *format, l)
}

func projectSaveCmd(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project "+verb, "ID --server URL [--name NAME] [--default-runtime R] [--storage-limit BYTES] [--retention-runs N] [--retention-age DURATION] [--revision N] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	name := fs.String("name", "", "the project's display name (default: its ID on create)")
	runtime := fs.String("default-runtime", "", "the runtime of the project's requests that name none; empty uses the server's")
	limit := fs.Int64("storage-limit", 0, "the bytes of run evidence and bundles the project keeps; retention evicts its oldest runs beyond it (0: no project limit)")
	runs := fs.Int("retention-runs", 0, "the most runs the project keeps (0: the server's policy)")
	age := fs.Duration("retention-age", 0, "how long the project keeps a run, such as 720h (0: the server's policy)")
	revision := fs.Int64("revision", -1, "the project set's revision the change applies to, as project list --format json shows it; by default the current one")
	pos, c, code, ok := projectServer(fs, server, format, args, 1, stderr)
	if !ok {
		return code
	}
	l, err := c.ListProjects(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	req := api.ProjectRequest{Revision: l.Revision, ID: pos[0], Name: pos[0]}
	if *revision >= 0 {
		req.Revision = uint64(*revision)
	}
	if verb == "update" {
		// An update replaces the settings: the flags not given keep
		// the project's current values.
		found := false
		for _, p := range l.Projects {
			if p.ID == pos[0] {
				req.Name, req.ProjectSettings, found = p.Name, p.ProjectSettings, true
			}
		}
		if !found {
			return fail(stderr, fmt.Errorf("no project %s on the server, or you may not see it", pos[0]))
		}
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			req.Name = *name
		case "default-runtime":
			req.DefaultRuntime = *runtime
		case "storage-limit":
			req.StorageLimit = *limit
		case "retention-runs":
			req.RetentionRuns = *runs
		case "retention-age":
			req.RetentionAgeSeconds = int64(*age / time.Second)
		}
	})
	if verb == "create" {
		l, err = c.CreateProject(ctx, req)
	} else {
		l, err = c.UpdateProject(ctx, req.ID, req)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		return writeProjects(stdout, stderr, *format, l)
	}
	fmt.Fprintf(stdout, "project %s %sd (revision %d)\n", req.ID, verb, l.Revision)
	return exitOK
}

func projectArchiveCmd(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project "+verb, "ID --server URL [--revision N] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	revision := fs.Int64("revision", -1, "the project set's revision the change applies to; by default the current one")
	pos, c, code, ok := projectServer(fs, server, format, args, 1, stderr)
	if !ok {
		return code
	}
	rev, err := projectRevision(ctx, c, *revision)
	if err != nil {
		return fail(stderr, err)
	}
	l, err := c.SetProjectArchived(ctx, rev, pos[0], verb == "archive")
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		return writeProjects(stdout, stderr, *format, l)
	}
	fmt.Fprintf(stdout, "project %s %sd (revision %d)\n", pos[0], verb, l.Revision)
	return exitOK
}

// projectRevision is revision, or the server's current one when it is
// negative.
func projectRevision(ctx context.Context, c *client.Client, revision int64) (uint64, error) {
	if revision >= 0 {
		return uint64(revision), nil
	}
	l, err := c.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	return l.Revision, nil
}

func writeProjects(stdout, stderr io.Writer, format string, l *api.Projects) int {
	if format == "json" {
		return writeEncoded(stdout, stderr, l)
	}
	fmt.Fprintf(stdout, "projects at revision %d\n", l.Revision)
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tDEFAULT RUNTIME\tSTORAGE LIMIT\tRETENTION\tPERMISSIONS")
	for _, p := range l.Projects {
		state := "active"
		switch {
		case p.Archived:
			state = "archived"
		case p.BuiltIn:
			state = "built-in"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, state, orDash(p.DefaultRuntime), projectLimit(p.StorageLimit),
			projectRetention(p.ProjectSettings), orDash(strings.Join(p.Permissions, ", ")))
	}
	if err := tw.Flush(); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func projectLimit(bytes int64) string {
	if bytes == 0 {
		return "-"
	}
	return fmt.Sprintf("%d B", bytes)
}

func projectRetention(s api.ProjectSettings) string {
	var parts []string
	if s.RetentionRuns > 0 {
		parts = append(parts, fmt.Sprintf("%d runs", s.RetentionRuns))
	}
	if s.RetentionAgeSeconds > 0 {
		parts = append(parts, (time.Duration(s.RetentionAgeSeconds) * time.Second).String())
	}
	return orDash(strings.Join(parts, ", "))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeEncoded(stdout, stderr io.Writer, v any) int {
	raw, err := encoded(v)
	if err == nil {
		_, err = stdout.Write(raw)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func libraryCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		libraryUsage(stderr)
		return exitError
	}
	rest := args[1:]
	switch args[0] {
	case "help", "-h", "-help", "--help":
		libraryUsage(stdout)
		return exitOK
	case "list":
		return libraryListCmd(ctx, rest, stdout, stderr)
	case "share":
		return libraryShareCmd(ctx, rest, stdout, stderr)
	case "publish", "copy":
		return libraryCopyCmd(ctx, args[0], rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown library command %q\n", args[0])
	libraryUsage(stderr)
	return exitError
}

func libraryListCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("library list", "--server URL [--project P] [--kind configuration|fixture|scenario] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	kind := fs.String("kind", "", "only artifacts of this kind")
	_, c, code, ok := projectServer(fs, server, format, args, 0, stderr)
	if !ok {
		return code
	}
	page := &api.LibraryPage{Items: []api.LibraryItem{}}
	for cursor := ""; ; {
		p, err := c.ListLibrary(ctx, *kind, cursor, 0)
		if err != nil {
			return fail(stderr, err)
		}
		page.Revision = p.Revision
		page.Items = append(page.Items, p.Items...)
		if cursor = p.NextCursor; cursor == "" {
			break
		}
	}
	if *format == "json" {
		return writeEncoded(stdout, stderr, page)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ARTIFACT\tKIND\tLABEL\tSHARED WITH\tSHARED")
	for _, item := range page.Items {
		shared := "-"
		if item.Shared != nil {
			shared = item.Shared.Local().Format(time.DateTime)
			if item.SharedBy != "" {
				shared += " by " + item.SharedBy
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", item.Artifact.ID, item.Artifact.Kind, item.Artifact.Label, orDash(strings.Join(item.Audience, ", ")), shared)
	}
	if err := tw.Flush(); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// projectList reads a comma-separated --projects value; empty is none.
func projectList(value string) []string {
	out := []string{}
	for _, p := range strings.Split(value, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func libraryShareCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("library share", "ARTIFACT --server URL --projects P,Q|*|'' [--revision N] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	projects := fs.String("projects", "", "the projects to share with, comma-separated, or * for every project; empty shares with none")
	revision := fs.Int64("revision", -1, "the project set's revision the change applies to; by default the current one")
	pos, c, code, ok := projectServer(fs, server, format, args, 1, stderr)
	if !ok {
		return code
	}
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "projects" })
	if !set {
		return fail(stderr, errors.New("--projects is required: name the projects, * for every project, or '' for none"))
	}
	rev, err := projectRevision(ctx, c, *revision)
	if err != nil {
		return fail(stderr, err)
	}
	v, err := c.SetLibraryAudience(ctx, pos[0], api.SharingRequest{Revision: rev, Projects: projectList(*projects)})
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		return writeEncoded(stdout, stderr, v)
	}
	fmt.Fprintf(stdout, "%s shared with %s (revision %d)\n", pos[0], sharedWith(v.Projects), v.Revision)
	return exitOK
}

// libraryCopyCmd copies an artifact: publish copies one of the caller's
// into the library, copy copies a library item into --project.
func libraryCopyCmd(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) int {
	synopsis := "ARTIFACT --server URL [--project P] [--revision-id ID] [--label L] [--format text|json]"
	if verb == "copy" {
		synopsis = "ARTIFACT --server URL --project P [--revision-id ID] [--label L] [--format text|json]"
	}
	fs := newFlagSet("library "+verb, synopsis, stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	var req api.CopyRequest
	fs.StringVar(&req.RevisionID, "revision-id", "", "the revision to copy (default: the artifact's current one)")
	fs.StringVar(&req.Label, "label", "", "the copy's label (default: the source's)")
	pos, c, code, ok := projectServer(fs, server, format, args, 1, stderr)
	if !ok {
		return code
	}
	target := workProject
	if verb == "publish" {
		target = libraryProject
	} else if target == "" {
		return fail(stderr, errors.New("copy needs --project: the project the library item is copied into"))
	}
	saved, err := c.InProject(target).CopyArtifact(ctx, pos[0], req)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		return writeEncoded(stdout, stderr, saved)
	}
	fmt.Fprintf(stdout, "copied %s into project %s as %s (revision %s)\n", pos[0], saved.Artifact.Project, saved.Artifact.ID, saved.Revision.Ref.RevisionID)
	return exitOK
}

func sharedWith(projects []string) string {
	if len(projects) == 0 {
		return "no project"
	}
	return strings.Join(projects, ", ")
}

// runtimeShareCmd limits the projects a runtime runs work for.
func runtimeShareCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runtime share", "NAME --server URL --projects P,Q|*|'' [--revision N] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	projects := fs.String("projects", "", "the projects the runtime runs work for, comma-separated, or *; empty makes it available to every project")
	revision := fs.Int64("revision", -1, "the project set's revision the change applies to; by default the current one")
	pos, c, code, ok := projectServer(fs, server, format, args, 1, stderr)
	if !ok {
		return code
	}
	rev, err := projectRevision(ctx, c, *revision)
	if err != nil {
		return fail(stderr, err)
	}
	v, err := c.SetRuntimeAvailability(ctx, pos[0], api.SharingRequest{Revision: rev, Projects: projectList(*projects)})
	if err != nil {
		return failRuntime(stderr, err)
	}
	if *format == "json" {
		return writeEncoded(stdout, stderr, v)
	}
	available := "every project"
	if len(v.Projects) > 0 {
		available = strings.Join(v.Projects, ", ")
	}
	fmt.Fprintf(stdout, "runtime %s is available to %s (revision %d)\n", pos[0], available, v.Revision)
	return exitOK
}
