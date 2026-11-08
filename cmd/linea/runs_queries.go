package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/assertion"
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
			t := newTextOut(w)
			t.line(0, "%s  run summary: %s, %s", t.status(status, 0), plural(len(s.Stages), "stage"), plural(len(s.Outputs), "output"))
			complete := fmt.Sprint(s.Complete)
			if s.Incomplete != "" {
				complete += " (" + s.Incomplete + ")"
			}
			t.field(1, "complete", complete)
			if a := s.Assertions; a != nil {
				t.field(1, "assertions", fmt.Sprintf("%s (%d pass, %d fail, %d inconclusive, %d error)", strings.ToUpper(string(a.Outcome)), a.Pass, a.Fail, a.Inconclusive, a.Error))
			}
			if len(s.Errors) > 0 {
				t.heading("errors")
				rows := make([][]string, 0, len(s.Errors))
				for _, e := range s.Errors {
					rows = append(rows, []string{e.Source, e.Component, e.Message})
				}
				t.table(1, []string{"source", "component", "message"}, rows)
			}
			return t.err()
		})
}

func runsEvidenceCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	strs := map[string]*string{}
	ints := map[string]*int{}
	return runRead(ctx, "evidence", "--server URL --run ID --family FAMILY [--pipeline P] [--component C] [--signal S] [--side before|after] [--entity ID] [--from-seq N] [--to-seq N] [--cursor C] [--page-items N] [--page-bytes N] [--format text|json]",
		args, stdout, stderr,
		func(fs *flag.FlagSet) {
			for _, f := range []struct{ name, usage string }{
				{"family", "stage_changes, records, journey, statements, events or recommendations"},
				{"pipeline", "pipeline ID for stage_changes and records; optional filter for statements and recommendations"},
				{"component", "the stage, the transform processor for statements, or an events and recommendations filter"},
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
			t := newTextOut(w)
			t.line(0, "evidence %s: %d of %d items", r.Family, len(r.Items), r.Total)
			if r.Complete {
				t.field(1, "complete", "true")
			} else {
				t.field(1, "complete", fmt.Sprintf("false (%s)", r.TruncatedReason))
			}
			t.field(1, "more", "--format json prints the items")
			t.heading("items")
			if len(r.Items) == 0 {
				t.none("items")
			}
			rows := make([][]string, 0, len(r.Items))
			for _, it := range r.Items {
				note := ""
				if it.Oversize != nil {
					note = fmt.Sprintf("oversize, %d bytes", it.Oversize.Bytes)
				}
				rows = append(rows, []string{it.Ref, note})
			}
			t.table(1, []string{"ref", "note"}, rows)
			t.nextPage(r.NextCursor)
			return t.err()
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
				t := newTextOut(w)
				t.line(0, "%s  evaluation %s", t.status(string(e.Outcome), 0), e.ID)
				t.field(1, "client", e.Client)
				t.field(1, "created", e.Created.Local().Format(time.DateTime))
				if e.Result != nil && len(e.Result.Results) > 0 {
					t.heading("results")
					width := statusWidth(e.Result.Results, func(r assertion.Result) string { return string(r.Outcome) })
					for _, r := range e.Result.Results {
						t.line(1, "%s  %s", t.status(string(r.Outcome), width), r.Name)
					}
				}
				return t.err()
			}
			var list []api.EvaluationInfo
			if err := json.Unmarshal(raw, &list); err != nil {
				return err
			}
			t := newTextOut(w)
			if len(list) == 0 {
				t.none("evaluations")
				return t.err()
			}
			rows := make([][]string, 0, len(list))
			for _, e := range list {
				rows = append(rows, []string{e.ID, e.Client, e.Created.Local().Format(time.DateTime), strings.ToUpper(string(e.Outcome))})
			}
			t.table(0, []string{"evaluation", "client", "created", "outcome"}, rows)
			return t.err()
		})
}
