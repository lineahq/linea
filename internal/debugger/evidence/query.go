package evidence

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
)

// Family names one kind of evidence query.
type Family string

const (
	// FamilyStageChanges: one stage's field changes grouped by (path, op),
	// uncapped and with their entities, then its removed, added and
	// unresolved records.
	FamilyStageChanges Family = "stage_changes"
	// FamilyRecords: the records a stage received (before) or emitted
	// (after), flattened with their resource and scope.
	FamilyRecords Family = "records"
	// FamilyJourney: one record's lineage: the record, its hops, deliveries
	// and stops.
	FamilyJourney Family = "journey"
	// FamilyStatements: the OTTL replay of one transform processor, per
	// replayed input and statement group.
	FamilyStatements Family = "statements"
	// FamilyEvents: recorded events in a sequence range, optionally of one
	// component.
	FamilyEvents Family = "events"
	// FamilyRecommendations: the run's recommendations, in catalog order,
	// optionally of one pipeline or naming one component.
	FamilyRecommendations Family = "recommendations"
)

// Sides of a records query.
const (
	SideBefore = "before"
	SideAfter  = "after"
)

// Query selects evidence of one family. Only the family's own parameters may
// be set:
//   - stage_changes: pipeline (empty for a connector) and component; signal
//     when several stages match.
//   - records: as stage_changes, plus side (before or after).
//   - journey: entity, a record ID ("<seq>:<ref>", as lineage names it).
//   - statements: component (a transform processor); pipeline optionally
//     limits it to the inputs of one pipeline.
//   - events: from_seq and to_seq (inclusive; zero to_seq is the end), and
//     optionally component.
//   - recommendations: optionally pipeline and component (a configuration
//     ID the recommendation names).
type Query struct {
	Family    Family `json:"family"`
	Pipeline  string `json:"pipeline,omitempty"`
	Component string `json:"component,omitempty"`
	Signal    string `json:"signal,omitempty"`
	Side      string `json:"side,omitempty"`
	Entity    string `json:"entity,omitempty"`
	FromSeq   int    `json:"from_seq,omitempty"`
	ToSeq     int    `json:"to_seq,omitempty"`
}

// Page bounds one answer. Items and Bytes must be positive; the platform
// applies its budgets before calling. Cursor continues a previous answer.
type Page struct {
	Cursor string
	Items  int
	Bytes  int
}

// Result is one page of evidence. Complete is true only on the last page of
// evidence the run kept in full; TruncatedReason says why not: the run's
// evidence (evidence_budget, analysis_omitted, or an execution status such as
// partial or cancelled), else the page bound (page_items or page_bytes).
// Total counts every item of the query.
type Result struct {
	RunID           string     `json:"run_id"`
	Family          Family     `json:"family"`
	Items           []Item     `json:"items"`
	Total           int        `json:"total"`
	Complete        bool       `json:"complete"`
	TruncatedReason string     `json:"truncated_reason,omitempty"`
	NextCursor      string     `json:"next_cursor,omitempty"`
	Page            PageLimits `json:"page"`
}

// PageLimits are the bounds a page was cut with. Clamped lists requested
// sizes above the policy, lowered to it.
type PageLimits struct {
	Items   int            `json:"items"`
	Bytes   int            `json:"bytes"`
	Clamped []engine.Clamp `json:"clamped,omitempty"`
}

// Item is one piece of evidence: its data, or, when the data alone exceeds
// the page's byte bound, an oversize entry with its size. An item is never
// skipped silently.
type Item struct {
	Ref      string          `json:"ref"`
	Data     json.RawMessage `json:"data,omitempty"`
	Oversize *Oversize       `json:"oversize,omitempty"`
}

// Oversize is the encoded size of an item too large for any page.
type Oversize struct {
	Bytes int `json:"bytes"`
}

// Reasons a result is not complete, besides the run's own truncation reason
// and execution status.
const (
	ReasonAnalysisOmitted = "analysis_omitted"
	ReasonPageItems       = "page_items"
	ReasonPageBytes       = "page_bytes"
)

// entry is one item before encoding.
type entry struct {
	ref   string
	value any
}

// cursor is the decoded form of an opaque cursor.
type cursor struct {
	V      int    `json:"v"`
	Run    string `json:"run"`
	Family Family `json:"family"`
	Params string `json:"params"`
	Offset int    `json:"offset"`
}

const cursorVersion = 1

// QueryRun answers q for run, one page at a time. The run must have a report.
func QueryRun(run Run, q Query, page Page) (*Result, error) {
	if run.Report == nil || run.Report.Result == nil {
		return nil, fmt.Errorf("%w: the run has no report", ErrInvalidQuery)
	}
	if page.Items <= 0 || page.Bytes <= 0 {
		return nil, fmt.Errorf("%w: page bounds must be positive", ErrInvalidQuery)
	}
	if err := q.validate(); err != nil {
		return nil, err
	}
	digest := q.digest()
	offset := 0
	if page.Cursor != "" {
		c, err := decodeCursor(page.Cursor)
		if err != nil || c.V != cursorVersion || c.Run != run.ID || c.Family != q.Family || c.Params != digest || c.Offset < 0 {
			return nil, fmt.Errorf("%w: the cursor belongs to another run, family or parameters", ErrInvalidQuery)
		}
		offset = c.Offset
	}
	entries, err := project(run.Report, q)
	if err != nil {
		return nil, err
	}
	if offset > len(entries) {
		return nil, fmt.Errorf("%w: the cursor is past the end", ErrInvalidQuery)
	}
	out := &Result{RunID: run.ID, Family: q.Family, Items: []Item{}, Total: len(entries), Page: PageLimits{Items: page.Items, Bytes: page.Bytes}}
	used, stop := 0, ""
	i := offset
	for ; i < len(entries); i++ {
		if len(out.Items) == page.Items {
			stop = ReasonPageItems
			break
		}
		data, err := json.Marshal(entries[i].value)
		if err != nil {
			return nil, err
		}
		item := Item{Ref: entries[i].ref, Data: data}
		size := len(data)
		if size > page.Bytes {
			item = Item{Ref: entries[i].ref, Oversize: &Oversize{Bytes: size}}
			size = 0
		}
		if used+size > page.Bytes && len(out.Items) > 0 {
			stop = ReasonPageBytes
			break
		}
		used += size
		out.Items = append(out.Items, item)
	}
	if i < len(entries) {
		out.NextCursor = encodeCursor(cursor{V: cursorVersion, Run: run.ID, Family: q.Family, Params: digest, Offset: i})
	}
	out.TruncatedReason = evidenceReason(run.Report, q.Family)
	if out.TruncatedReason == "" {
		out.TruncatedReason = stop
	}
	out.Complete = out.TruncatedReason == ""
	return out, nil
}

func (q Query) validate() error {
	invalid := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidQuery, msg) }
	type set struct{ pipeline, component, signal, side, entity, seqs bool }
	var allowed set
	switch q.Family {
	case FamilyStageChanges:
		allowed = set{pipeline: true, component: true, signal: true}
	case FamilyRecords:
		allowed = set{pipeline: true, component: true, signal: true, side: true}
		if q.Side != SideBefore && q.Side != SideAfter {
			return invalid("side must be before or after")
		}
	case FamilyJourney:
		allowed = set{entity: true}
		if q.Entity == "" {
			return invalid("entity is required")
		}
	case FamilyStatements:
		allowed = set{pipeline: true, component: true}
	case FamilyRecommendations:
		allowed = set{pipeline: true, component: true}
	case FamilyEvents:
		allowed = set{component: true, seqs: true}
		if q.FromSeq < 0 || q.ToSeq < 0 || q.ToSeq != 0 && q.ToSeq < q.FromSeq {
			return invalid("from_seq and to_seq must be a non-negative range")
		}
	default:
		return invalid(fmt.Sprintf("unknown family %q", q.Family))
	}
	if (q.Family == FamilyStageChanges || q.Family == FamilyRecords || q.Family == FamilyStatements) && q.Component == "" {
		return invalid("component is required")
	}
	for _, f := range []struct {
		name    string
		set     bool
		allowed bool
	}{
		{"pipeline", q.Pipeline != "", allowed.pipeline},
		{"component", q.Component != "", allowed.component},
		{"signal", q.Signal != "", allowed.signal},
		{"side", q.Side != "", allowed.side},
		{"entity", q.Entity != "", allowed.entity},
		{"from_seq and to_seq", q.FromSeq != 0 || q.ToSeq != 0, allowed.seqs},
	} {
		if f.set && !f.allowed {
			return invalid(fmt.Sprintf("%s does not apply to %s", f.name, q.Family))
		}
	}
	return nil
}

// digest identifies the query's parameters: SHA-256 over their canonical
// JSON (fixed field order, empty fields omitted).
func (q Query) digest() string {
	data, _ := json.Marshal(q)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func encodeCursor(c cursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(s string) (cursor, error) {
	var c cursor
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(data, &c)
}

// analysisFamilies read the report's derived analysis.
var analysisFamilies = []Family{FamilyStageChanges, FamilyRecords, FamilyJourney, FamilyStatements, FamilyRecommendations}

// evidenceReason says why the run's evidence for family is not complete, or
// "" when it is.
func evidenceReason(rep *engine.Report, family Family) string {
	r := rep.Result
	if cut := r.Truncation; cut != nil {
		if len(cut.Omitted) > 0 && slices.Contains(analysisFamilies, family) {
			return ReasonAnalysisOmitted
		}
		if cut.DroppedEvents > 0 || cut.DroppedBytes > 0 || len(cut.IncompleteOutputs) > 0 {
			return cut.Reason
		}
	}
	if r.Status != runresult.StatusCompleted {
		return string(r.Status)
	}
	return ""
}
