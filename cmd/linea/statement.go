package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// statementCmd develops transform statements: candidate
// validates a proposed configuration or one statement edit without running
// it, and test runs one on a retained run's recorded inputs.
func statementCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "candidate":
			return statementCandidateCmd(ctx, args[1:], stdout, stderr)
		case "test":
			return statementTestCmd(ctx, args[1:], stdout, stderr)
		case "help", "-h", "-help", "--help":
			statementUsage(stdout)
			return exitOK
		}
		fmt.Fprintf(stderr, "unknown statement command %q\n", args[0])
	}
	statementUsage(stderr)
	return exitError
}

const statementSynopsis = "candidate --config <collector.yaml> (--proposed <collector.yaml> | --processor ID --signal SIGNAL --op OP [--group N] [--index N] [--text T] [--expected T]) [--save [--label L]] [--runtime NAME] [--format text|json] [--server URL]"

const statementTestSynopsis = "test --server URL --run ID (--proposed <collector.yaml> | --processor ID --signal SIGNAL --op OP [--group N] [--index N] [--text T] [--expected T]) [--assertions <assertions.yaml>] [--runtime NAME] [--format text|json]"

func statementUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: linea statement %s\n       linea statement %s\n", statementSynopsis, statementTestSynopsis)
}

// editFlags are one addressed statement edit, or a complete proposal.
type editFlags struct {
	proposed string
	edit     platform.StatementEdit
	op       string
}

func (e *editFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&e.proposed, "proposed", "", "complete proposed configuration YAML, in place of an addressed edit")
	fs.StringVar(&e.edit.Processor, "processor", "", "transform processor ID, for example transform/redact")
	fs.StringVar(&e.edit.Signal, "signal", "", "statement signal: traces, metrics or logs")
	fs.IntVar(&e.edit.Group, "group", 0, "zero-based statement group index in the processor's list")
	fs.IntVar(&e.edit.Index, "index", 0, "zero-based statement index in the group")
	fs.StringVar(&e.op, "op", "", "edit operation: replace, insert, append or delete")
	fs.StringVar(&e.edit.Text, "text", "", "new statement text for replace, insert or append")
	fs.StringVar(&e.edit.Expected, "expected", "", "current statement text; required for replace and delete to detect stale edits")
}

// resolve returns the proposal text or the edit; exactly one is set.
func (e *editFlags) resolve() (string, *platform.StatementEdit, error) {
	switch {
	case e.proposed != "" && e.op != "":
		return "", nil, fmt.Errorf("supply exactly one of --proposed or --op")
	case e.proposed != "":
		text, err := readArg("proposed", e.proposed)
		return text, nil, err
	case e.op != "":
		edit := e.edit
		switch e.op {
		case "replace":
			edit.Op = "replace"
		case "insert":
			edit.Op = "insert"
		case "append":
			edit.Op = "append"
		case "delete":
			edit.Op = "delete"
		default:
			return "", nil, fmt.Errorf("--op %q: use replace, insert, append or delete", e.op)
		}
		return "", &edit, nil
	}
	return "", nil, fmt.Errorf("supply exactly one of --proposed or --op")
}

func statementCandidateCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("statement candidate", statementSynopsis[len("candidate "):], stderr)
	var config, label, runtimeName string
	var save bool
	var edit editFlags
	fs.StringVar(&config, "config", "", "base Collector configuration YAML; never written")
	edit.add(fs)
	fs.BoolVar(&save, "save", false, "save the valid proposal as a new artifact in the server's saved work")
	fs.StringVar(&label, "label", "", "label of the saved artifact")
	fs.StringVar(&runtimeName, "runtime", "", "built runtime to validate on, or a runtime image reference; empty is the built-in registry")
	runtimeCacheFlag(fs)
	format := formatFlag(fs, "text")
	imports := importEnvFlag(fs)
	server := serverFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if config == "" {
		fmt.Fprintln(stderr, "--config is required")
		fs.Usage()
		return exitError
	}
	proposed, statement, err := edit.resolve()
	if err != nil {
		fmt.Fprintln(stderr, err)
		fs.Usage()
		return exitError
	}
	var raw []byte
	if *server != "" {
		c, err := connect(*server, false)
		if err != nil {
			return fail(stderr, err)
		}
		req := api.CandidateRequest{ConfigLabel: config, Edit: statement, ProposedYAML: proposed, Save: save, Label: label, Runtime: runtimeName}
		if req.ConfigYAML, err = readArg("config", config); err != nil {
			return fail(stderr, err)
		}
		if raw, err = c.CreateCandidate(ctx, req); err != nil {
			return failRuntime(stderr, err)
		}
	} else {
		env, err := localEnvironment(*imports)
		if err != nil {
			return fail(stderr, err)
		}
		service := platform.New(platform.Options{Environment: env, Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: dockerRuntimes()})
		defer service.Close()
		cand, err := service.CreateCandidate(ctx, cliCaller, platform.CandidateRequest{Base: platform.Source{Path: config}, YAML: proposed,
			Statement: statement, Save: save, Label: label, Runtime: runtimeName})
		if err != nil {
			return failRuntime(stderr, err)
		}
		out := api.Candidate{ProposedYAML: cand.Proposed, Patch: cand.Patch, AffectedPipelines: cand.AffectedPipelines, Valid: cand.Valid,
			Diagnostics: cand.Diagnostics, Saved: cand.Saved}
		if out.AffectedPipelines == nil {
			out.AffectedPipelines = []string{}
		}
		if out.Diagnostics == nil {
			out.Diagnostics = []platform.Diagnostic{}
		}
		if raw, err = encoded(out); err != nil {
			return fail(stderr, err)
		}
	}
	var cand api.Candidate
	if err := json.Unmarshal(raw, &cand); err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		err = writeCandidate(stdout, &cand)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if !cand.Valid {
		return exitError
	}
	return exitOK
}

// statementTestCmd tests a candidate on a run's recorded configuration and
// inputs through a team server. The test run is queued, not awaited; an
// invalid candidate exits 1 as candidate does.
func statementTestCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("statement test", statementTestSynopsis[len("test "):], stderr)
	var id, assertions, runtimeName string
	var edit editFlags
	fs.StringVar(&id, "run", "", "the run whose recorded configuration and inputs to test on")
	edit.add(fs)
	fs.StringVar(&assertions, "assertions", "", "assertions-only scenario YAML replacing the recorded assertions")
	fs.StringVar(&runtimeName, "runtime", "", "runtime to test on; empty is the one the run's report names")
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || id == "" {
		fmt.Fprintln(stderr, "--server and --run are required: runs are kept by linea-server")
		fs.Usage()
		return exitError
	}
	proposed, statement, err := edit.resolve()
	if err != nil {
		fmt.Fprintln(stderr, err)
		fs.Usage()
		return exitError
	}
	req := api.StatementTestRequest{Edit: statement, ProposedYAML: proposed, Runtime: runtimeName}
	if assertions != "" {
		if req.AssertionsYAML, err = readArg("assertions", assertions); err != nil {
			return fail(stderr, err)
		}
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.TestStatementOnRun(ctx, id, req)
	if err != nil {
		return failRuntime(stderr, err)
	}
	var test api.StatementTest
	if err := json.Unmarshal(raw, &test); err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		err = writeStatementTest(stdout, &test, *server)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if !test.Candidate.Valid {
		return exitError
	}
	return exitOK
}

func writeCandidate(w io.Writer, c *api.Candidate) error {
	t := newTextOut(w)
	candidateText(t, c)
	return t.err()
}

// writeStatementTest prints the candidate and, when the server queued one,
// the test run with the command that reads it.
func writeStatementTest(w io.Writer, test *api.StatementTest, server string) error {
	t := newTextOut(w)
	candidateText(t, &test.Candidate)
	if r := test.Run; r != nil {
		t.heading("test run")
		t.field(1, "run", r.ID)
		t.field(1, "state", fmt.Sprint(r.State))
		t.field(1, "read it with", fmt.Sprintf("runs show --server %s --run %s --report --wait", server, r.ID))
	}
	return t.err()
}

// candidateText writes a candidate: the verdict, the pipelines it affects,
// its diagnostics, where it was saved and the patch against the base.
func candidateText(t *textOut, c *api.Candidate) {
	status := "valid"
	if !c.Valid {
		status = "invalid"
	}
	t.line(0, "%s  candidate: %s, %s", t.status(status, 0), plural(len(c.AffectedPipelines), "affected pipeline"), plural(len(c.Diagnostics), "diagnostic"))
	if len(c.AffectedPipelines) > 0 {
		t.field(1, "affected pipelines", strings.Join(c.AffectedPipelines, ", "))
	}
	if c.Saved != nil {
		t.field(1, "saved", fmt.Sprintf("artifact %s revision %s", c.Saved.Revision.Ref.ArtifactID, c.Saved.Revision.Ref.RevisionID))
	}
	writeDiagnostics(t, c.Diagnostics)
	if c.Patch != "" {
		t.heading("patch")
		t.printf("%s", c.Patch)
		if !strings.HasSuffix(c.Patch, "\n") {
			t.printf("\n")
		}
	}
}

// writeDiagnostics writes a DIAGNOSTICS section, nothing when there are none.
func writeDiagnostics(t *textOut, diagnostics []platform.Diagnostic) {
	if len(diagnostics) == 0 {
		return
	}
	t.heading("diagnostics")
	for _, d := range diagnostics {
		where := ""
		if d.Component != "" {
			where = " [" + d.Component + "]"
		}
		t.line(1, "%s %s%s: %s", t.status(string(d.Severity), 0), d.Code, where, d.Message)
		t.field(2, "hint", d.Hint)
	}
}

// encoded is v as the server encodes it, so local and connected JSON match.
func encoded(v any) ([]byte, error) {
	var buf bytes.Buffer
	err := platform.Encode(&buf, v)
	return buf.Bytes(), err
}
