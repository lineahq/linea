package mcp

import (
	"context"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type traceInput struct {
	RunID     string `json:"run_id" jsonschema:"Plain run ID of a finished run."`
	Family    string `json:"family" jsonschema:"Evidence family: stage_changes, records, journey, statements or events."`
	Pipeline  string `json:"pipeline,omitempty" jsonschema:"Pipeline ID for stage_changes and records (empty for a connector); optional filter for statements."`
	Component string `json:"component,omitempty" jsonschema:"Component ID: the stage for stage_changes and records, the transform processor for statements, an optional filter for events."`
	Signal    string `json:"signal,omitempty" jsonschema:"Signal of a connector stage that has several, for stage_changes and records."`
	Side      string `json:"side,omitempty" jsonschema:"For records: before (received) or after (emitted)."`
	Entity    string `json:"entity,omitempty" jsonschema:"For journey: a record ID from lineage or a records item ref."`
	FromSeq   int    `json:"from_seq,omitempty" jsonschema:"For events: first event sequence number."`
	ToSeq     int    `json:"to_seq,omitempty" jsonschema:"For events: last event sequence number, inclusive; 0 is the end."`
	Cursor    string `json:"cursor,omitempty" jsonschema:"next_cursor of a previous answer with the same run, family and parameters."`
	PageItems int    `json:"page_items,omitempty" jsonschema:"Maximum items in this page; the server's budget applies when larger or unset."`
	PageBytes int    `json:"page_bytes,omitempty" jsonschema:"Maximum encoded item bytes in this page; the server's budget applies when larger or unset."`
}

func (in traceInput) query() platform.EvidenceQuery {
	return platform.EvidenceQuery{
		Family: platform.EvidenceFamily(in.Family), Pipeline: in.Pipeline, Component: in.Component,
		Signal: in.Signal, Side: in.Side, Entity: in.Entity, FromSeq: in.FromSeq, ToSeq: in.ToSeq,
	}
}

// traceResult is one page of evidence, the same JSON as the matching resource.
type traceResult struct {
	*platform.EvidenceResult
}

// summary counts what the page holds; refs and data stay structured.
func (r traceResult) summary() string {
	oversize := 0
	for _, it := range r.Items {
		if it.Oversize != nil {
			oversize++
		}
	}
	text := fmt.Sprintf("Execution trace: %s of %d", plural(len(r.Items), "item"), r.Total)
	if oversize != 0 {
		text += fmt.Sprintf("; %d oversize", oversize)
	}
	if r.Complete {
		text += "; complete"
	} else {
		text += fmt.Sprintf("; incomplete (%s)", r.TruncatedReason)
	}
	if r.NextCursor != "" {
		text += "; more pages"
	}
	return text + "."
}

func queryEvidence(ctx context.Context, service *platform.Service, id string, q platform.EvidenceQuery, page platform.EvidencePage) (traceResult, error) {
	result, err := service.QueryEvidence(ctx, callerOf(ctx), id, q, page)
	if err != nil {
		return traceResult{}, err
	}
	return traceResult{result}, nil
}

func addTraceTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_get_execution_trace", "Read one bounded page of a finished run's recorded evidence: stage changes, records, a record's journey, OTTL statement replays or events. Never executes anything.",
		func(ctx context.Context, in traceInput) (summarized, error) {
			return queryEvidence(ctx, service, in.RunID, in.query(),
				platform.EvidencePage{Cursor: in.Cursor, Items: in.PageItems, Bytes: in.PageBytes})
		})
}
