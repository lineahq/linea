package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// CaptureSelection is what a capture captures. Exactly one of CollectorIDs,
// AgentIDs, GroupID or Selector chooses the Collectors; fleet resolves
// groups and selectors when it admits the capture. Identity, authorization,
// delivery, IDs and absolute expiry are supplied by the platform and fleet.
type CaptureSelection struct {
	CollectorIDs    []string                `json:"collector_ids,omitempty"`
	AgentIDs        []string                `json:"agent_ids,omitempty"`
	GroupID         string                  `json:"group_id,omitempty"`
	Selector        string                  `json:"selector,omitempty"`
	CapturePoints   []CapturePointSelection `json:"capture_points"`
	Signals         []string                `json:"signals"`
	Filter          CaptureFilter           `json:"filter"`
	Sampling        *CaptureSampling        `json:"sampling,omitempty"`
	Limits          CaptureLimits           `json:"limits"`
	DurationSeconds int                     `json:"duration_seconds,omitempty"`
}

// CapturePointSelection names a capture point of a pipeline.
type CapturePointSelection struct {
	Pipeline string `json:"pipeline"`
	Point    string `json:"point"`
}

// CaptureFilter holds OTTL conditions the Collector applies before
// sending records.
type CaptureFilter struct {
	Resource []string            `json:"resource,omitempty"`
	Traces   CaptureTraceFilter  `json:"traces,omitempty"`
	Logs     CaptureLogFilter    `json:"logs,omitempty"`
	Metrics  CaptureMetricFilter `json:"metrics,omitempty"`
}

// CaptureTraceFilter holds span conditions.
type CaptureTraceFilter struct {
	Span []string `json:"span,omitempty"`
}

// CaptureLogFilter holds log record conditions.
type CaptureLogFilter struct {
	Log []string `json:"log,omitempty"`
}

// CaptureMetricFilter holds metric and data point conditions.
type CaptureMetricFilter struct {
	Metric    []string `json:"metric,omitempty"`
	Datapoint []string `json:"datapoint,omitempty"`
}

// CaptureSampling samples selected records at a rate.
type CaptureSampling struct {
	Rate float64 `json:"rate"`
	Mode string  `json:"mode,omitempty"`
}

// CaptureLimits bounds what each Collector sends for a capture.
type CaptureLimits struct {
	BytesPerSecond   int64 `json:"bytes_per_second"`
	RecordsPerSecond int64 `json:"records_per_second"`
	TotalBytes       int64 `json:"total_bytes"`
	TotalRecords     int64 `json:"total_records"`
	RecordBytes      int64 `json:"record_bytes"`
}

// CaptureValidation is a checked capture selection and the Collectors it
// resolves to now. Fleet resolves groups and selectors again on admission.
type CaptureValidation struct {
	RequestedLimits  CaptureLimits    `json:"requested_limits"`
	Selection        CaptureSelection `json:"selection"`
	Collectors       []Collector      `json:"collectors"`
	FilterValidation string           `json:"filter_validation"`
	LimitsScope      string           `json:"limits_scope"`
}

// CaptureTargetStats are a target Collector's counters as fleet last
// reported them.
type CaptureTargetStats struct {
	Selected        int64            `json:"selected"`
	EnqueuedRecords int64            `json:"enqueued_records"`
	EnqueuedBytes   int64            `json:"enqueued_bytes"`
	FilterErrors    int64            `json:"filter_errors"`
	Dropped         map[string]int64 `json:"dropped,omitempty"`
}

// CaptureTargetStatus is a capture's state on one Collector, as fleet last
// reported it. CollectorID is fleet's peer ID.
type CaptureTargetStatus struct {
	Source      *CaptureSource      `json:"source,omitempty"`
	CollectorID string              `json:"collector_id"`
	Generation  uint64              `json:"generation"`
	AgentID     string              `json:"agent_id,omitempty"`
	State       string              `json:"state"`
	Terminal    bool                `json:"terminal"`
	ObservedAt  *time.Time          `json:"observed_at,omitempty"`
	Stats       *CaptureTargetStats `json:"stats,omitempty"`
}

// CaptureSession is a capture across its target Collectors. State is
// fleet's operation state, or uncertain when the debugger lost track of it.
type CaptureSession struct {
	Metadata        CaptureMetadata       `json:"metadata,omitempty"`
	Data            CaptureDataStatus     `json:"data"`
	RequestedLimits CaptureLimits         `json:"requested_limits"`
	ID              string                `json:"id"`
	Project         string                `json:"project_id"`
	Caller          Actor                 `json:"caller"`
	CreatedAt       time.Time             `json:"created_at"`
	ExpiresAt       time.Time             `json:"expires_at"`
	Selection       CaptureSelection      `json:"selection"`
	State           string                `json:"state"`
	Reason          string                `json:"reason,omitempty"`
	StopReason      string                `json:"stop_reason,omitempty"`
	Terminal        bool                  `json:"terminal"`
	Targets         []CaptureTargetStatus `json:"targets"`
}

// CaptureSessionPage is a page of capture summaries.
type CaptureSessionPage struct {
	Items      []CaptureSessionSummary `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// List entries are compact; GetCaptureSession supplies the full selection
// and per-target evidence.
type CaptureSessionSummary struct {
	Metadata       CaptureMetadata   `json:"metadata,omitempty"`
	Data           CaptureDataStatus `json:"data"`
	ID             string            `json:"id"`
	Project        string            `json:"project_id"`
	CreatedAt      time.Time         `json:"created_at"`
	ExpiresAt      time.Time         `json:"expires_at"`
	State          string            `json:"state"`
	Terminal       bool              `json:"terminal"`
	TargetCount    int               `json:"target_count"`
	ActiveCount    int               `json:"active_count"`
	UncertainCount int               `json:"uncertain_count"`
	FailedCount    int               `json:"failed_count"`
}

func (c *captureRecord) summary() CaptureSessionSummary {
	v := CaptureSessionSummary{
		Metadata:    c.view.Metadata,
		Data:        c.view.Data,
		ID:          c.view.ID,
		Project:     c.view.Project,
		CreatedAt:   c.view.CreatedAt,
		ExpiresAt:   c.view.ExpiresAt,
		State:       c.view.State,
		Terminal:    c.view.Terminal,
		TargetCount: len(c.view.Targets),
	}
	for _, t := range c.view.Targets {
		switch t.State {
		case "active":
			v.ActiveCount++
		case "unknown":
			v.UncertainCount++
		case "failed":
			v.FailedCount++
		}
	}
	return v
}

const (
	// maxCaptureSeconds keeps the capture and its channel, which outlives it
	// by channelGrace, inside fleet's 5-minute bound.
	maxCaptureSeconds = 280
	channelGrace      = 15 * time.Second
	// captureGuard is when the debugger gives up on a capture fleet never
	// reported terminal; the channel has expired by then.
	captureGuard = channelGrace + 15*time.Second
	// captureCleanup bounds stopping and deleting a channel at shutdown.
	captureCleanup = 2 * time.Second
	// captureStatusPeriod is how often a runner reads the operation.
	captureStatusPeriod = time.Second
	// captureIdle is how long a runner waits on an empty channel.
	captureIdle = 100 * time.Millisecond
	// batchHistory bounds the batch IDs a runner remembers for deduplication.
	batchHistory = 4096
)

type captureRecord struct {
	view            CaptureSession
	changed         chan struct{}
	records         []capturedRecord
	onDisk          bool // records were released; recordsLocked reads them
	sensitivities   map[string]string
	storeGeneration uint64

	// Fleet state, set at admission and read by the runner.
	fleetID, channel string
	revision         int64
	running          bool
	stopReason       string // a stop the runner must still send
	refused          map[string]bool
	batches          map[string]int64
	batchOrder       []string
	wake             chan struct{}
}

func cloneCapture[T any](v T) T {
	raw, _ := json.Marshal(v)
	var owned T
	_ = json.Unmarshal(raw, &owned)
	return owned
}

func captureInvalid(message string) error { return &Error{Code: CodeInvalidRequest, Message: message} }

// normalizeSelection checks a selection against fleet's bounds and fills
// defaults. It reads nothing.
func normalizeSelection(req CaptureSelection) (CaptureSelection, error) {
	req = cloneCapture(req)
	modes := 0
	for _, set := range []bool{len(req.CollectorIDs) > 0, len(req.AgentIDs) > 0, req.GroupID != "", req.Selector != ""} {
		if set {
			modes++
		}
	}
	if modes != 1 {
		return req, captureInvalid("choose Collectors by exactly one of collector_ids, agent_ids, group_id or selector")
	}
	if len(req.CollectorIDs) > 16 || len(req.AgentIDs) > 16 {
		return req, captureInvalid("select at most 16 Collectors or agents")
	}
	seen := map[string]bool{}
	for _, id := range req.CollectorIDs {
		if !validPeerID(id) || seen[id] {
			return req, captureInvalid("Collector IDs must be unique fleet peer IDs")
		}
		seen[id] = true
	}
	for _, id := range req.AgentIDs {
		if !validFleetID(id) || seen[id] {
			return req, captureInvalid("agent IDs must be unique fleet agent IDs")
		}
		seen[id] = true
	}
	if !collectorText(req.GroupID, 128, false) || !collectorText(req.Selector, 1024, false) {
		return req, captureInvalid("group ID or selector is too long")
	}
	if len(req.CapturePoints) < 1 || len(req.CapturePoints) > 64 || len(req.Signals) < 1 || len(req.Signals) > 3 {
		return req, captureInvalid("select 1–64 points and 1–3 signals")
	}
	if req.DurationSeconds == 0 {
		req.DurationSeconds = 60
	}
	if req.DurationSeconds < 5 || req.DurationSeconds > maxCaptureSeconds {
		return req, captureInvalid("capture duration must be 5–280 seconds")
	}
	seen = map[string]bool{}
	for _, signal := range req.Signals {
		if !slices.Contains([]string{"logs", "traces", "metrics"}, signal) || seen[signal] {
			return req, captureInvalid("signals must be unique traces, logs or metrics")
		}
		seen[signal] = true
	}
	seen = map[string]bool{}
	for _, point := range req.CapturePoints {
		key := point.Pipeline + "\x00" + point.Point
		if !collectorText(point.Pipeline, 128, true) || !collectorText(point.Point, 128, true) ||
			strings.TrimSpace(point.Pipeline) != point.Pipeline || strings.TrimSpace(point.Point) != point.Point || seen[key] {
			return req, captureInvalid("capture points must be bounded and unique")
		}
		seen[key] = true
	}
	if req.Sampling != nil &&
		(math.IsNaN(req.Sampling.Rate) || math.IsInf(req.Sampling.Rate, 0) || req.Sampling.Rate < 0 || req.Sampling.Rate > 1 || !slices.Contains([]string{"", "hash", "random"}, req.Sampling.Mode)) {
		return req, captureInvalid("sampling requires rate in [0,1] and hash or random mode")
	}
	conditionBytes := 0
	for _, list := range [][]string{req.Filter.Resource, req.Filter.Traces.Span, req.Filter.Logs.Log, req.Filter.Metrics.Metric, req.Filter.Metrics.Datapoint} {
		if len(list) > 32 {
			return req, captureInvalid("each filter context allows at most 32 conditions")
		}
		for _, condition := range list {
			if !collectorText(condition, 4096, true) {
				return req, captureInvalid("filter conditions must be nonempty and at most 4,096 bytes")
			}
			conditionBytes += len(condition)
		}
	}
	if conditionBytes > 16<<10 {
		return req, captureInvalid("capture filters allow at most 16 KiB of conditions")
	}
	// Fleet's ceilings are sent even when the caller omits budgets. The
	// Collector may clamp them lower.
	for _, pair := range []struct {
		n   *int64
		max int64
	}{{&req.Limits.BytesPerSecond, 1 << 20}, {&req.Limits.RecordsPerSecond, 5000}, {&req.Limits.TotalBytes, 16 << 20}, {&req.Limits.TotalRecords, 10000}, {&req.Limits.RecordBytes, 64 << 10}} {
		if *pair.n < 0 {
			return req, captureInvalid("capture limits cannot be negative")
		}
		if *pair.n == 0 || *pair.n > pair.max {
			*pair.n = pair.max
		}
	}
	return req, nil
}

// capturable reports why a Collector cannot serve a selection, as fleet
// would: it must be enabled, compile the filter and offer every point and
// signal.
func capturable(c Collector, points []CapturePoint, req CaptureSelection) error {
	if !c.Enabled {
		return &Error{Code: CodeConflict, Message: "a selected Collector has capture disabled"}
	}
	if err := fitFilter(req.Filter, c.FilterLimits); err != nil {
		return err
	}
	covered := map[string]bool{}
	for _, point := range req.CapturePoints {
		found := false
		for _, known := range points {
			if known.Pipeline == point.Pipeline && known.Point == point.Point && slices.Contains(req.Signals, known.Signal) {
				found = true
				covered[known.Signal] = true
			}
		}
		if !found {
			return captureInvalid("a selected point and signal is absent from a target Collector's catalog")
		}
	}
	for _, signal := range req.Signals {
		if !covered[signal] {
			return captureInvalid("each signal needs a selected point on every target Collector")
		}
	}
	return nil
}

// fitFilter refuses a filter larger than a Collector reports it compiles,
// naming the limit. The Collector checks IsMatch patterns itself.
func fitFilter(f CaptureFilter, limits FilterLimits) error {
	total := 0
	for _, list := range [][]string{f.Resource, f.Traces.Span, f.Logs.Log, f.Metrics.Metric, f.Metrics.Datapoint} {
		total += len(list)
		for _, condition := range list {
			if n := utf8.RuneCountInString(condition); n > limits.MaxConditionChars {
				return captureInvalid(fmt.Sprintf("a filter condition has %d characters; the target Collector compiles at most %d", n, limits.MaxConditionChars))
			}
		}
	}
	if total > limits.MaxConditions {
		return captureInvalid(fmt.Sprintf("the filter has %d conditions; the target Collector compiles at most %d across all lists", total, limits.MaxConditions))
	}
	return nil
}

// resolve previews the Collectors a selection captures from. Collector and
// agent selections are strict; groups and selectors keep the Collectors that
// can serve the selection, as fleet does, and when none can only because of
// the filter's size, say so.
func (r *collectorRegistry) resolve(ctx context.Context, req CaptureSelection, snap *peerSnapshot) ([]Collector, error) {
	out := []Collector{}
	byAgent := map[string][]string{}
	for _, id := range snap.ids {
		if agent := snap.collectors[id].AgentID; agent != "" {
			byAgent[agent] = append(byAgent[agent], id)
		}
	}
	strict := func(id string) error {
		c, ok := snap.collectors[id]
		if !ok {
			return &Error{Code: CodeNotFound, Message: "Collector is not connected to fleet"}
		}
		if err := capturable(c, snap.points[id], req); err != nil {
			return err
		}
		out = append(out, c)
		return nil
	}
	switch {
	case len(req.CollectorIDs) > 0:
		for _, id := range req.CollectorIDs {
			if err := strict(id); err != nil {
				return nil, err
			}
		}
		return out, nil
	case len(req.AgentIDs) > 0:
		for _, agent := range req.AgentIDs {
			if len(byAgent[agent]) != 1 {
				return nil, &Error{Code: CodeConflict, Message: "a selected agent has no single connected Collector"}
			}
			if err := strict(byAgent[agent][0]); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	agents, err := r.members(ctx, req)
	if err != nil {
		return nil, err
	}
	var tooLarge error
	for _, agent := range agents {
		for _, id := range byAgent[agent] {
			c := snap.collectors[id]
			if capturable(c, snap.points[id], req) == nil {
				out = append(out, c)
			} else if err := fitFilter(req.Filter, c.FilterLimits); err != nil && c.Enabled {
				tooLarge = err
			}
		}
	}
	slices.SortFunc(out, func(a, b Collector) int { return strings.Compare(a.ID, b.ID) })
	if len(out) == 0 && tooLarge != nil {
		return nil, tooLarge
	}
	if len(out) == 0 {
		return nil, &Error{Code: CodeConflict, Message: "no connected Collector in the selection can serve these points and signals"}
	}
	if len(out) > 16 {
		return nil, &Error{Code: CodeResourceExhausted, Message: "the selection resolves to more than 16 Collectors"}
	}
	return out, nil
}

// members reads the connected agents of a group or selector, up to 4,096.
func (r *collectorRegistry) members(ctx context.Context, req CaptureSelection) ([]string, error) {
	out := []string{}
	after := ""
	for range 9 {
		var page *fleetcapture.AgentPage
		var err error
		if req.GroupID != "" {
			page, err = r.fleet.GroupAgents(ctx, req.GroupID, after, 500)
		} else {
			page, err = r.fleet.Agents(ctx, req.Selector, after, 500)
		}
		if err != nil {
			return nil, fleetError(ctx, err)
		}
		for _, a := range page.Items {
			if a.Connected {
				out = append(out, a.ID)
			}
		}
		if page.NextAfter == "" {
			return out, nil
		}
		after = page.NextAfter
	}
	return nil, &Error{Code: CodeResourceExhausted, Message: "the selection has more than 4,096 agents"}
}

func validationOf(req, normalized CaptureSelection, collectors []Collector) *CaptureValidation {
	return &CaptureValidation{
		RequestedLimits:  req.Limits,
		Selection:        normalized,
		Collectors:       collectors,
		FilterValidation: "bounded_preflight_only; collector_compiles_on_activation",
		LimitsScope:      "per_collector; collector_may_clamp_lower",
	}
}

// ValidateCaptureRequest checks a capture selection and previews the
// Collectors it would capture from, without starting it.
func (s *Service) ValidateCaptureRequest(ctx context.Context, caller Caller, req CaptureSelection) (_ *CaptureValidation, err error) {
	defer s.audit(&caller, "ValidateCaptureRequest", &err)()
	if err := s.begin(ctx, caller, authz.CaptureStart); err != nil {
		return nil, err
	}
	if err := s.workIn(&caller, authz.CaptureStart); err != nil {
		return nil, err
	}
	r := s.collectors
	if err := r.checkFleet(); err != nil {
		return nil, err
	}
	normalized, err := normalizeSelection(req)
	if err != nil {
		return nil, err
	}
	snap, err := r.snapshot(ctx, false)
	if err != nil {
		return nil, err
	}
	collectors, err := r.resolve(ctx, normalized, snap)
	if err != nil {
		return nil, err
	}
	if collectors, _, err = s.admissible(caller, normalized, snap, collectors); err != nil {
		return nil, err
	}
	return validationOf(req, normalized, collectors), nil
}

func fleetFilter(f CaptureFilter) fleetcapture.Filter {
	out := fleetcapture.Filter{Resource: f.Resource}
	if len(f.Traces.Span) > 0 {
		out.Traces = &fleetcapture.TraceFilter{Span: f.Traces.Span}
	}
	if len(f.Logs.Log) > 0 {
		out.Logs = &fleetcapture.LogFilter{Log: f.Logs.Log}
	}
	if len(f.Metrics.Metric) > 0 || len(f.Metrics.Datapoint) > 0 {
		out.Metrics = &fleetcapture.MetricFilter{Metric: f.Metrics.Metric, Datapoint: f.Metrics.Datapoint}
	}
	return out
}

// fleetRequest maps a selection onto fleet's request. A pinned selection
// sends its resolved Collectors as explicit targets instead of its agents,
// group or selector.
func fleetRequest(id string, ch *fleetcapture.Channel, sel CaptureSelection, targets []Collector, pinned bool, expires time.Time) fleetcapture.CaptureRequest {
	req := fleetcapture.CaptureRequest{
		AuthorityID: ch.AuthorityID, PluginInstance: ch.PluginInstance, ID: id, ChannelID: ch.ID,
		Agents: sel.AgentIDs, GroupID: sel.GroupID, Selector: sel.Selector,
		Signals: sel.Signals, Filter: fleetFilter(sel.Filter), ExpiresAt: expires.UTC(),
		Limits: fleetcapture.Limits(sel.Limits),
	}
	if pinned {
		req.Agents, req.GroupID, req.Selector = nil, "", ""
	}
	if len(sel.CollectorIDs) > 0 || pinned {
		for _, c := range targets {
			req.Targets = append(req.Targets, fleetcapture.Target{PeerID: c.ID, Generation: c.Generation})
		}
	}
	for _, p := range sel.CapturePoints {
		req.CapturePoints = append(req.CapturePoints, fleetcapture.Point(p))
	}
	if sel.Sampling != nil {
		req.Sampling = &fleetcapture.Sampling{Rate: sel.Sampling.Rate, Mode: sel.Sampling.Mode}
	}
	return req
}

// reserve claims a session slot and its memory before admission.
func (r *collectorRegistry) reserve(principal string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	if r.perPrincipal > 0 {
		open := r.pendingBy[principal]
		for _, c := range r.sessions {
			if c.view.Caller.Principal == principal && !c.view.Terminal {
				open++
			}
		}
		if open >= r.perPrincipal {
			return &Error{Code: CodeResourceExhausted, Retryable: true, Message: fmt.Sprintf(
				"%s already has %d open captures; at most %d are allowed (limits.captures_per_principal); stop one or wait for it to end", principal, open, r.perPrincipal)}
		}
	}
	if r.dataMemoryReservedLocked()+int64(r.pending+1)*captureSessionBytes > captureMemoryBytes {
		return &Error{Code: CodeResourceExhausted, Message: "capture memory reservation limit reached"}
	}
	if len(r.sessions)+r.pending >= r.history {
		return &Error{Code: CodeResourceExhausted, Message: "capture history is full; delete old captures"}
	}
	r.pending++
	r.pendingBy[principal]++
	return nil
}

// unreserveLocked releases a reservation, admitted or not.
func (r *collectorRegistry) unreserveLocked(principal string) {
	r.pending--
	if r.pendingBy[principal]--; r.pendingBy[principal] <= 0 {
		delete(r.pendingBy, principal)
	}
}

// CreateCaptureSession asks fleet to capture from the selected Collectors
// and delivers the batches into a new session. Admission is not activation:
// the session's targets become active as the Collectors confirm.
func (s *Service) CreateCaptureSession(ctx context.Context, caller Caller, req CaptureSelection) (_ *CaptureSession, err error) {
	defer s.audit(&caller, "CreateCaptureSession", &err)()
	if err := s.begin(ctx, caller, authz.CaptureStart); err != nil {
		return nil, err
	}
	if err := s.workIn(&caller, authz.CaptureStart); err != nil {
		return nil, err
	}
	ctx = actingFor(ctx, caller)
	r := s.collectors
	if err := r.checkFleet(); err != nil {
		return nil, err
	}
	sel, err := normalizeSelection(req)
	if err != nil {
		return nil, err
	}
	if err := r.reserve(caller.Principal.ID); err != nil {
		return nil, err
	}
	reserved := true
	defer func() {
		if reserved {
			r.mu.Lock()
			r.unreserveLocked(caller.Principal.ID)
			r.mu.Unlock()
		}
	}()
	snap, err := r.snapshot(ctx, false)
	if err != nil {
		return nil, err
	}
	targets, err := r.resolve(ctx, sel, snap)
	if err != nil {
		return nil, err
	}
	targets, pinned, err := s.admissible(caller, sel, snap, targets)
	if err != nil {
		return nil, err
	}
	now := r.now()
	expires := now.Add(time.Duration(sel.DurationSeconds) * time.Second)
	ch, err := r.fleet.CreateChannel(ctx, expires.Add(channelGrace))
	if err != nil {
		return nil, fleetError(ctx, err)
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	fleetID := hex.EncodeToString(b)
	// An admitted capture must be tracked even if the caller leaves, and a
	// transport failure is retried with the same ID, which fleet deduplicates.
	admit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	request := fleetRequest(fleetID, ch, sel, targets, pinned, expires)
	op, err := r.fleet.CreateCapture(admit, request)
	if err != nil && fleetcapture.StatusOf(err) == 0 && admit.Err() == nil {
		op, err = r.fleet.CreateCapture(admit, request)
	}
	if err != nil {
		_ = r.fleet.DeleteChannel(admit, ch.ID)
		if collectorBusy(err) {
			if _, held := s.holders(caller, targets); held != nil {
				return nil, held
			}
		}
		if fleetcapture.StatusOf(err) == 403 && slices.ContainsFunc(targets, func(c Collector) bool { return capturesRaw(sel, c.ID, snap) }) {
			return nil, &Error{Code: CodeForbidden, Message: "forbidden: fleet's livecapture plugin does not let this server capture raw points; select redacted points, or have the deployment grant raw capture to the server (the plugin's raw_principals)"}
		}
		return nil, fleetError(admit, err)
	}
	// Sensitivities come from the catalogs of the peers fleet chose.
	for _, t := range op.Targets {
		if c, ok := snap.collectors[t.PeerID]; !ok || c.Generation != t.Generation {
			if fresh, err := r.snapshot(admit, true); err == nil {
				snap = fresh
			}
			break
		}
	}
	c := &captureRecord{
		view: CaptureSession{
			Metadata:        CaptureMetadata{Revision: 1, UpdatedAt: now.UTC()},
			RequestedLimits: req.Limits,
			ID:              "capture-" + fleetID,
			Project:         caller.Project,
			Caller:          caller.Actor(),
			CreatedAt:       now.UTC(),
			ExpiresAt:       expires.UTC(),
			Selection:       sel,
			State:           "starting",
			Targets:         []CaptureTargetStatus{},
		},
		changed:  make(chan struct{}),
		fleetID:  fleetID,
		channel:  ch.ID,
		revision: 0,
		running:  true,
		refused:  map[string]bool{},
		batches:  map[string]int64{},
		wake:     make(chan struct{}, 1),
	}
	caller.touch(c.view.ID)
	for _, t := range op.Targets {
		target := CaptureTargetStatus{CollectorID: t.PeerID, Generation: t.Generation, AgentID: t.AgentID, State: t.State}
		if peer, ok := snap.collectors[t.PeerID]; ok && peer.Generation == t.Generation {
			target.Source = captureSource(peer)
		}
		c.view.Targets = append(c.view.Targets, target)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unreserveLocked(caller.Principal.ID)
	reserved = false
	r.initializeDataLocked(c, snap)
	r.applyOperationLocked(c, op)
	if err := r.check(); err != nil {
		go r.abandon(c)
		return nil, err
	}
	// Fleet's targets can differ from the preview; raw data from any of them
	// still needs capture.raw.
	if c.view.Data.ContainsRaw && !s.visible(caller, authz.CaptureRaw, c.view.Project) {
		go r.abandon(c)
		caller.decide(authz.CaptureRaw, "project:"+c.view.Project, false)
		return nil, &Error{Code: CodeForbidden, Message: fmt.Sprintf(
			"forbidden: fleet chose a Collector whose selected points are raw; capturing them needs the %s permission in project %s", authz.CaptureRaw, c.view.Project)}
	}
	if err := r.checkpointCaptureLocked(admit, c, nil); err != nil {
		go r.abandon(c)
		return nil, err
	}
	r.sessions[c.view.ID] = c
	r.sessionGeneration++
	r.runners.Add(1)
	go r.run(c)
	v := cloneCapture(c.view)
	return &v, nil
}

// captureSource is the provenance of a target, when its identity fits it.
func captureSource(c Collector) *CaptureSource {
	if !collectorText(c.Pod, 256, true) || !collectorText(c.CollectorGroup, 256, true) || !collectorText(c.CollectorVersion, 128, true) {
		return nil
	}
	return &CaptureSource{
		Cluster: c.Cluster, Namespace: c.Namespace, Pod: c.Pod, CollectorGroup: c.CollectorGroup,
		CollectorVersion: c.CollectorVersion, DistributionVersion: c.DistributionVersion,
	}
}

// abandon stops an admitted capture the platform cannot track.
func (r *collectorRegistry) abandon(c *captureRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), captureCleanup)
	defer cancel()
	_, _ = r.stopFleet(ctx, c)
	_ = r.fleet.DeleteChannel(ctx, c.channel)
}

func (c *captureRecord) notify() { close(c.changed); c.changed = make(chan struct{}) }

func (c *captureRecord) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func targetTerminal(state string) bool {
	return slices.Contains([]string{"completed", "expired", "limit_reached", "failed", "disabled"}, state)
}

// applyOperationLocked records fleet's view of the operation; it reports
// whether anything but counters changed.
func (r *collectorRegistry) applyOperationLocked(c *captureRecord, op *fleetcapture.Operation) bool {
	if op.ID != c.fleetID || op.Revision < c.revision || c.view.State == "uncertain" {
		return false
	}
	before, _ := json.Marshal([]any{c.view.State, c.view.Reason, c.view.Terminal, c.targetStates()})
	c.revision = op.Revision
	c.view.State, c.view.Reason, c.view.Terminal = op.State, op.Reason, op.Terminal()
	now := r.now().UTC()
	for _, t := range op.Targets {
		for i := range c.view.Targets {
			v := &c.view.Targets[i]
			if v.CollectorID != t.PeerID || v.Generation != t.Generation {
				continue
			}
			v.State, v.AgentID, v.ObservedAt = t.State, t.AgentID, &now
			v.Terminal = c.view.Terminal || targetTerminal(t.State)
			if t.Stats != nil {
				v.Stats = &CaptureTargetStats{Selected: t.Stats.Selected, EnqueuedRecords: t.Stats.EnqueuedRecords,
					EnqueuedBytes: t.Stats.EnqueuedBytes, FilterErrors: t.Stats.FilterErrors, Dropped: t.Stats.Dropped}
			}
		}
	}
	if c.view.Terminal {
		// A target fleet left unfinished in an ended operation (for example
		// interrupted) has no final state the debugger can claim.
		for i := range c.view.Targets {
			t := &c.view.Targets[i]
			if !targetTerminal(t.State) {
				t.State = "unknown"
			}
			t.Terminal = true
		}
		if c.view.Data.State == "open" {
			c.view.Data.State = "draining"
		}
	}
	after, _ := json.Marshal([]any{c.view.State, c.view.Reason, c.view.Terminal, c.targetStates()})
	return string(before) != string(after)
}

func (c *captureRecord) targetStates() []string {
	out := make([]string, 0, len(c.view.Targets))
	for _, t := range c.view.Targets {
		out = append(out, t.State)
	}
	return out
}

// lostLocked ends a capture whose fleet state the debugger can no longer
// read. Fleet still enforces the capture's expiry.
func (r *collectorRegistry) lostLocked(c *captureRecord, reason string) {
	if c.view.Terminal {
		return
	}
	c.view.State, c.view.Reason, c.view.Terminal = "uncertain", reason, true
	for i := range c.view.Targets {
		t := &c.view.Targets[i]
		if !t.Terminal {
			t.State, t.Terminal = "unknown", true
		}
	}
}

// stopFleet stops a capture at its current revision, rereading it when the
// revision moved.
func (r *collectorRegistry) stopFleet(ctx context.Context, c *captureRecord) (*fleetcapture.Operation, error) {
	r.mu.Lock()
	id, revision := c.fleetID, c.revision
	r.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if revision == 0 || attempt > 0 {
			op, err := r.fleet.GetCapture(ctx, id)
			if err != nil {
				return nil, err
			}
			if op.Terminal() {
				return op, nil
			}
			revision = op.Revision
		}
		op, err := r.fleet.StopCapture(ctx, id, revision)
		if fleetcapture.StatusOf(err) == 412 && attempt < 3 {
			continue
		}
		return op, err
	}
}

// GetCaptureSession returns a capture session.
func (s *Service) GetCaptureSession(ctx context.Context, caller Caller, id string) (_ *CaptureSession, err error) {
	defer s.audit(&caller, "GetCaptureSession", &err)()
	if err := s.begin(ctx, caller, authz.CaptureRead); err != nil {
		return nil, err
	}
	r := s.collectors
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return nil, err
	}
	c := r.sessions[id]
	if c == nil {
		return nil, &Error{Code: CodeNotFound, Message: "capture session not found"}
	}
	if err := s.authorizeIn(caller, authz.CaptureRead, c.view.Project); err != nil {
		return nil, err
	}
	v := cloneCapture(c.view)
	return &v, nil
}

// ListCaptureSessions returns a page of capture summaries.
func (s *Service) ListCaptureSessions(ctx context.Context, caller Caller, req CollectorPageRequest) (_ *CaptureSessionPage, err error) {
	defer s.audit(&caller, "ListCaptureSessions", &err)()
	if err := s.begin(ctx, caller, authz.CaptureRead); err != nil {
		return nil, err
	}
	projects, err := s.listIn(caller, authz.CaptureRead)
	if err != nil {
		return nil, err
	}
	r := s.collectors
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return nil, err
	}
	items := make([]CaptureSessionSummary, 0, len(r.sessions))
	for _, c := range r.sessions {
		if inProjects(projects, c.view.Project) {
			items = append(items, c.summary())
		}
	}
	slices.SortFunc(items, func(a, b CaptureSessionSummary) int { return strings.Compare(a.ID, b.ID) })
	page, next, err := collectorPage(items, req, r.epoch+":captures", r.sessionGeneration, s.budgets)
	if err != nil {
		return nil, err
	}
	return &CaptureSessionPage{Items: cloneCapture(page), NextCursor: next}, nil
}

// WaitCaptureSession waits until a capture session is terminal.
func (s *Service) WaitCaptureSession(ctx context.Context, caller Caller, id string) (_ *CaptureSession, err error) {
	defer s.audit(&caller, "WaitCaptureSession", &err)()
	for {
		v, err := s.GetCaptureSession(ctx, caller, id)
		if err != nil {
			return nil, err
		}
		if v.Terminal {
			return v, nil
		}
		r := s.collectors
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, &Error{Code: CodeUnavailable, Message: "platform is closed"}
		}
		c := r.sessions[id]
		if c == nil || c.view.Terminal {
			r.mu.Unlock()
			continue
		}
		changed := c.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, classify(ctx.Err(), CodeCancelled)
		case <-changed:
		}
	}
}

// requestStopLocked asks the runner to stop a capture for a local reason,
// such as a storage limit.
func (r *collectorRegistry) requestStopLocked(c *captureRecord, reason string) {
	if c.view.Terminal || c.stopReason != "" || c.view.StopReason != "" {
		return
	}
	c.stopReason, c.view.StopReason = reason, reason
	c.poke()
}

// StopCaptureSession asks fleet to stop a capture on every target. The
// targets confirm asynchronously; already delivered batches are kept.
func (s *Service) StopCaptureSession(ctx context.Context, caller Caller, id string) (_ *CaptureSession, err error) {
	defer s.audit(&caller, "StopCaptureSession", &err)()
	if err := s.begin(ctx, caller, authz.CaptureStart); err != nil {
		return nil, err
	}
	if err := s.captureIn(caller, authz.CaptureStart, id); err != nil {
		return nil, err
	}
	ctx = actingFor(ctx, caller)
	r := s.collectors
	r.mu.Lock()
	if err := r.check(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	c := r.sessions[id]
	if c == nil {
		r.mu.Unlock()
		return nil, &Error{Code: CodeNotFound, Message: "capture session not found"}
	}
	if c.view.Terminal || !c.running {
		v := cloneCapture(c.view)
		r.mu.Unlock()
		return &v, nil
	}
	if c.view.StopReason == "" {
		c.view.StopReason = "stop_requested"
	}
	r.mu.Unlock()
	op, err := r.stopFleet(ctx, c)
	if err != nil {
		return nil, fleetError(ctx, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applyOperationLocked(c, op) {
		_ = r.checkpointCaptureLocked(context.Background(), c, nil)
		c.notify()
	}
	c.poke()
	v := cloneCapture(c.view)
	return &v, nil
}
