package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// A retained run's summary, evidence pages and recorded evaluations,
// as GET /runs/{id}/summary, /evidence and /evaluations send
// them.

func runsSummaryCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runRead(ctx, "summary", "--server URL --run ID [--format text|json]", args, stdout, stderr, nil,
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) { return c.RunSummary(ctx, id) },
		func(w io.Writer, raw []byte) error {
			var s platform.RunSummary
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}
			status := string(s.ExecutionStatus)
			if status == "" {
				status = "none"
			}
			fmt.Fprintf(w, "execution %s, complete %t", status, s.Complete)
			if s.Incomplete != "" {
				fmt.Fprintf(w, " (%s)", s.Incomplete)
			}
			fmt.Fprintf(w, "; %d stages, %d outputs", len(s.Stages), len(s.Outputs))
			if s.Assertions != nil {
				a := s.Assertions
				fmt.Fprintf(w, "; assertions %s (%d pass, %d fail, %d inconclusive, %d error)", a.Outcome, a.Pass, a.Fail, a.Inconclusive, a.Error)
			}
			fmt.Fprintln(w)
			for _, e := range s.Errors {
				fmt.Fprintf(w, "  error %s %s: %s\n", e.Source, e.Component, e.Message)
			}
			return nil
		})
}

func runsEvidenceCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	strs := map[string]*string{}
	ints := map[string]*int{}
	return runRead(ctx, "evidence", "--server URL --run ID --family FAMILY [--pipeline P] [--component C] [--signal S] [--side before|after] [--entity ID] [--from-seq N] [--to-seq N] [--cursor C] [--page-items N] [--page-bytes N] [--format text|json]",
		args, stdout, stderr,
		func(fs *flag.FlagSet) {
			for _, f := range []struct{ name, usage string }{
				{"family", "stage_changes, records, journey, statements or events"},
				{"pipeline", "pipeline ID for stage_changes and records; optional filter for statements"},
				{"component", "the stage, the transform processor for statements, or an events filter"},
				{"signal", "signal of a connector stage that has several"},
				{"side", "for records: before (received) or after (emitted)"},
				{"entity", "for journey: a record ID"},
				{"cursor", "next cursor printed by the previous page"},
			} {
				strs[f.name] = fs.String(f.name, "", f.usage)
			}
			for _, f := range []struct{ flag, param, usage string }{
				{"from-seq", "from_seq", "for events: first event sequence number"},
				{"to-seq", "to_seq", "for events: last event sequence number, inclusive; 0 is the end"},
				{"page-items", "page_items", "maximum items; the server's budget applies when larger or unset"},
				{"page-bytes", "page_bytes", "maximum encoded item bytes; the server's budget applies when larger or unset"},
			} {
				ints[f.param] = fs.Int(f.flag, 0, f.usage)
			}
		},
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) {
			q := url.Values{}
			for name, v := range strs {
				if *v != "" {
					q.Set(name, *v)
				}
			}
			for name, v := range ints {
				if *v != 0 {
					q.Set(name, strconv.Itoa(*v))
				}
			}
			return c.RunEvidence(ctx, id, q)
		},
		func(w io.Writer, raw []byte) error {
			var r platform.EvidenceResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			fmt.Fprintf(w, "%s: %d items of %d", r.Family, len(r.Items), r.Total)
			if r.Complete {
				fmt.Fprint(w, "; complete")
			} else {
				fmt.Fprintf(w, "; incomplete (%s)", r.TruncatedReason)
			}
			fmt.Fprintln(w, "; --format json prints the items")
			for _, it := range r.Items {
				if it.Oversize != nil {
					fmt.Fprintf(w, "  %s (oversize, %d bytes)\n", it.Ref, it.Oversize.Bytes)
				} else {
					fmt.Fprintf(w, "  %s\n", it.Ref)
				}
			}
			if r.NextCursor != "" {
				fmt.Fprintf(w, "more: --cursor %s\n", r.NextCursor)
			}
			return nil
		})
}

func runsEvaluationsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var evaluation string
	return runRead(ctx, "evaluations", "--server URL --run ID [--evaluation ID] [--format text|json]", args, stdout, stderr,
		func(fs *flag.FlagSet) {
			fs.StringVar(&evaluation, "evaluation", "", "one recorded evaluation to print with its assertion report")
		},
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) {
			if evaluation != "" {
				return c.RunEvaluation(ctx, id, evaluation)
			}
			return c.RunEvaluations(ctx, id)
		},
		func(w io.Writer, raw []byte) error {
			if evaluation != "" {
				var e api.Evaluation
				if err := json.Unmarshal(raw, &e); err != nil {
					return err
				}
				fmt.Fprintf(w, "evaluation %s by %s at %s: %s\n", e.ID, e.Client, e.Created.Local().Format(time.DateTime), e.Outcome)
				if e.Result != nil {
					for _, r := range e.Result.Results {
						fmt.Fprintf(w, "  %s: %s\n", r.Name, r.Outcome)
					}
				}
				return nil
			}
			var list []api.EvaluationInfo
			if err := json.Unmarshal(raw, &list); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "EVALUATION\tCLIENT\tCREATED\tOUTCOME")
			for _, e := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.ID, e.Client, e.Created.Local().Format(time.DateTime), e.Outcome)
			}
			return tw.Flush()
		})
}
