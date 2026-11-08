package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/lineahq/linea/internal/debugger/client"
)

// One retained run on a team server: show its
// state or report, cancel it, evaluate assertions against it, and read its
// stage changes, a snapshot or its inputs. --format json prints the server's
// JSON as sent.

// runsSubcommands are the runs subcommands besides the list.
var runsSubcommands = map[string]func(context.Context, []string, io.Writer, io.Writer) int{
	"log":      runsLogCmd,
	"show":     runsShowCmd,
	"cancel":   runsCancelCmd,
	"assert":   runsAssertCmd,
	"diffs":    runsDiffsCmd,
	"snapshot": runsSnapshotCmd,
	"inputs":   runsInputsCmd,
	// Run queries.
	"summary":     runsSummaryCmd,
	"evidence":    runsEvidenceCmd,
	"evaluations": runsEvaluationsCmd,
	// Deletion.
	"delete": runsDeleteCmd,
	// Run retention.
	"keep":    runsKeepCmd,
	"unkeep":  runsUnkeepCmd,
	"prune":   runsPruneCmd,
	"storage": runsStorageCmd,
}

// runRead parses a subcommand about one run, connects and prints the reply:
// the JSON as sent, or text.
func runRead(ctx context.Context, name, synopsis string, args []string, stdout, stderr io.Writer, addFlags func(*flag.FlagSet),
	read func(ctx context.Context, c *client.Client, id string) ([]byte, error), text func(w io.Writer, raw []byte) error) int {
	fs := newFlagSet("runs "+name, synopsis, stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	id := fs.String("run", "", "the run's ID")
	if addFlags != nil {
		addFlags(fs)
	}
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *id == "" {
		fmt.Fprintln(stderr, "--server and --run are required: runs are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := read(ctx, c, *id)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		err = text(stdout, raw)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// runState is what the text line says about a run.
type runState struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Error  string `json:"error"`
	Report *struct {
		Result *struct {
			Status string `json:"status"`
		} `json:"result"`
		Assertions *struct {
			Outcome string `json:"outcome"`
		} `json:"assertions"`
	} `json:"report"`
}

// writeRunState prints a run's title and its state, and what its report and
// error say when it has them.
func writeRunState(w io.Writer, raw []byte) error {
	var r runState
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	t := newTextOut(w)
	t.line(0, "run %s", r.ID)
	t.field(1, "state", r.State)
	if r.Report != nil && r.Report.Result != nil {
		t.field(1, "execution", r.Report.Result.Status)
	}
	if r.Report != nil && r.Report.Assertions != nil {
		t.field(1, "assertions", r.Report.Assertions.Outcome)
	}
	t.field(1, "error", r.Error)
	return t.err()
}

func runsShowCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var report, wait bool
	return runRead(ctx, "show", "--server URL --run ID [--report [--wait]] [--format text|json]", args, stdout, stderr,
		func(fs *flag.FlagSet) {
			fs.BoolVar(&report, "report", false, "print the complete report as JSON instead of the run's state")
			fs.BoolVar(&wait, "wait", false, "with --report, wait for the run to finish")
		},
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) {
			if report {
				return c.RunReport(ctx, id, wait)
			}
			return c.GetRun(ctx, id)
		},
		func(w io.Writer, raw []byte) error {
			if report {
				_, err := w.Write(raw)
				return err
			}
			return writeRunState(w, raw)
		})
}

func runsCancelCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runRead(ctx, "cancel", "--server URL --run ID [--format text|json]", args, stdout, stderr, nil,
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) { return c.CancelRun(ctx, id) },
		writeRunState)
}

func runsAssertCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var scenario string
	return runRead(ctx, "assert", "--server URL --run ID --scenario <scenario.yaml> [--format text|json]", args, stdout, stderr,
		func(fs *flag.FlagSet) {
			fs.StringVar(&scenario, "scenario", "", "scenario whose assertions to evaluate; its inputs are not used")
		},
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) {
			if scenario == "" {
				return nil, errors.New("--scenario is required")
			}
			text, err := readArg("scenario", scenario)
			if err != nil {
				return nil, err
			}
			return c.EvaluateAssertions(ctx, id, text)
		},
		func(w io.Writer, raw []byte) error {
			var rep struct {
				Outcome string `json:"outcome"`
				Results []struct {
					Name    string `json:"name"`
					Outcome string `json:"outcome"`
				} `json:"results"`
			}
			if err := json.Unmarshal(raw, &rep); err != nil {
				return err
			}
			t := newTextOut(w)
			t.line(0, "%s  assertions", t.status(rep.Outcome, 0))
			width := 0
			for _, r := range rep.Results {
				width = max(width, len(r.Outcome))
			}
			for _, r := range rep.Results {
				t.line(1, "%s  %s", t.status(r.Outcome, width), r.Name)
			}
			return t.err()
		})
}

func runsDiffsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runRead(ctx, "diffs", "--server URL --run ID [--format text|json]", args, stdout, stderr, nil,
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) { return c.RunDiffs(ctx, id) },
		func(w io.Writer, raw []byte) error {
			var d struct {
				Stages []struct {
					Component string `json:"component"`
					Pipeline  string `json:"pipeline"`
					Signal    string `json:"signal"`
				} `json:"stages"`
				OTTL []json.RawMessage `json:"ottl"`
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				return err
			}
			t := newTextOut(w)
			t.line(0, "diffs: %s, %s", plural(len(d.Stages), "stage"), plural(len(d.OTTL), "OTTL report"))
			t.field(1, "more", "--format json prints the changes")
			if len(d.Stages) > 0 {
				t.heading("stages")
				rows := make([][]string, 0, len(d.Stages))
				for _, s := range d.Stages {
					rows = append(rows, []string{s.Component, s.Pipeline, s.Signal})
				}
				t.table(1, []string{"component", "pipeline", "signal"}, rows)
			}
			return t.err()
		})
}

func runsSnapshotCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runs snapshot", "--server URL --run ID --ref REF --out FILE", stderr)
	server := serverFlag(fs)
	id := fs.String("run", "", "the run's ID")
	ref := fs.String("ref", "", "snapshot reference from the report")
	out := fs.String("out", "", "file to create; an existing file is never overwritten")
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if *server == "" || *id == "" || *ref == "" || *out == "" {
		fmt.Fprintln(stderr, "--server, --run, --ref and --out are required")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.Snapshot(ctx, *id, *ref)
	if err != nil {
		return fail(stderr, err)
	}
	if err := writeNew(*out, string(raw)); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "wrote %s (%d bytes)\n", *out, len(raw))
	return exitOK
}

type inputSource struct {
	Label  string `json:"label"`
	SHA256 string `json:"sha256"`
}

func runsInputsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runRead(ctx, "inputs", "--server URL --run ID [--format text|json]", args, stdout, stderr, nil,
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) { return c.RunInputs(ctx, id) },
		func(w io.Writer, raw []byte) error {
			var in struct {
				Mode                string       `json:"mode"`
				Config              inputSource  `json:"config"`
				Fixture             *inputSource `json:"fixture"`
				Scenario            *inputSource `json:"scenario"`
				Setup               *inputSource `json:"setup"`
				Assertions          *inputSource `json:"assertions"`
				Files               []string     `json:"files"`
				RequiredEnvironment []string     `json:"required_environment"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return err
			}
			t := newTextOut(w)
			t.line(0, "inputs of a %s run", in.Mode)
			t.field(1, "more", "--format json prints the texts")
			var rows [][]string
			for _, s := range []struct {
				name string
				src  *inputSource
			}{{"config", &in.Config}, {"fixture", in.Fixture}, {"scenario", in.Scenario}, {"setup", in.Setup}, {"assertions", in.Assertions}} {
				if s.src != nil {
					label := s.src.Label
					if label == "" {
						label = "(inline)"
					}
					rows = append(rows, []string{s.name, label, s.src.SHA256})
				}
			}
			if len(rows) > 0 {
				t.heading("sources")
				t.table(1, []string{"source", "label", "sha256"}, rows)
			}
			if len(in.Files) > 0 || len(in.RequiredEnvironment) > 0 {
				t.heading("needs")
				if len(in.Files) > 0 {
					t.field(1, "files", strings.Join(in.Files, ", "))
				}
				if len(in.RequiredEnvironment) > 0 {
					t.field(1, "environment", strings.Join(in.RequiredEnvironment, ", "))
				}
			}
			return t.err()
		})
}
