package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// FleetOptions connect the debugger to an linea-fleet server. Fleet's
// livecapture plugin owns Collector connections, authorization and delivery;
// the debugger is one of its clients.
type FleetOptions struct {
	// Endpoint is fleet's management API base URL, such as https://fleet:8080.
	Endpoint string
	// Token is a fleet admin credential, read from a file by the composition.
	// Every debugger caller shares the captures it owns.
	Token string
	// AllowHTTP permits plaintext to a non-loopback fleet on a protected network.
	AllowHTTP bool
}

// CaptureOptions is trusted composition policy, never decoded from user
// requests. A zero Fleet disables capture.
type CaptureOptions struct {
	Fleet FleetOptions
	// Now is a deterministic clock boundary for lifecycle tests; nil uses time.Now.
	Now func() time.Time
	// fleet substitutes an in-memory fleet in tests.
	fleet fleetAPI
}

// Validate checks the fleet settings without contacting fleet.
func (o CaptureOptions) Validate() error {
	if o.Fleet.Endpoint == "" {
		if o.Fleet.Token != "" {
			return errors.New("a fleet token needs a fleet endpoint")
		}
		return nil
	}
	_, err := o.client()
	return err
}

func (o CaptureOptions) client() (*fleetcapture.Client, error) {
	return fleetcapture.New(fleetcapture.Config{Endpoint: o.Fleet.Endpoint, Token: o.Fleet.Token, AllowHTTP: o.Fleet.AllowHTTP})
}

// Validate checks the options without contacting fleet, so a composition can
// refuse them at startup. Errors never contain the token.
func (o FleetOptions) Validate() error {
	_, err := CaptureOptions{Fleet: o}.client()
	return err
}

// fleetAPI is the part of fleetcapture.Client the platform calls; tests
// substitute an in-memory fleet.
type fleetAPI interface {
	Peers(ctx context.Context, cursor string, limit int) (*fleetcapture.PeerPage, error)
	CreateChannel(ctx context.Context, expiresAt time.Time) (*fleetcapture.Channel, error)
	DeleteChannel(ctx context.Context, id string) error
	Next(ctx context.Context, channel string) (*fleetcapture.Envelope, error)
	Acknowledge(ctx context.Context, channel string, ack fleetcapture.Ack) error
	CreateCapture(ctx context.Context, req fleetcapture.CaptureRequest) (*fleetcapture.Operation, error)
	GetCapture(ctx context.Context, id string) (*fleetcapture.Operation, error)
	StopCapture(ctx context.Context, id string, revision int64) (*fleetcapture.Operation, error)
	Groups(ctx context.Context, after string, limit int) (*fleetcapture.GroupPage, error)
	Group(ctx context.Context, id string) (*fleetcapture.Group, error)
	GroupAgents(ctx context.Context, id, after string, limit int) (*fleetcapture.AgentPage, error)
	Agents(ctx context.Context, selector, after string, limit int) (*fleetcapture.AgentPage, error)
}

// CapturePoint is one reported pipeline/point/signal tuple. Raw sensitivity is
// a source label; fleet decides who may capture it.
type CapturePoint struct {
	Pipeline    string `json:"pipeline"`
	Point       string `json:"point"`
	Signal      string `json:"signal"`
	Sensitivity string `json:"sensitivity"`
}

// Collector is a Collector connected to fleet's livecapture plugin. Its ID is
// fleet's stable peer ID; the generation advances with each reconnect. Points
// have their own paged operation.
type Collector struct {
	ID                  string `json:"id"`
	Generation          uint64 `json:"generation"`
	AgentID             string `json:"agent_id,omitempty"`
	CredentialID        string `json:"credential_id"`
	Cluster             string `json:"cluster,omitempty"`
	Namespace           string `json:"namespace,omitempty"`
	Pod                 string `json:"pod"`
	CollectorGroup      string `json:"collector_group"`
	CollectorVersion    string `json:"collector_version,omitempty"`
	DistributionVersion string `json:"distribution_version,omitempty"`
	Enabled             bool   `json:"enabled"`
	PointCount          int    `json:"point_count"`
	RawPointCount       int    `json:"raw_point_count"`
	// FilterLimits is the largest filter the Collector compiles.
	FilterLimits FilterLimits `json:"filter_limits"`
}

// FilterLimits bound a capture filter on one Collector, as it reports them;
// the version 1 limits when it reports none. Lengths count Unicode code
// points. MaxConditions counts every list of the filter together, and
// MaxRegexChars bounds an IsMatch pattern, which the Collector checks.
type FilterLimits struct {
	MaxConditions     int `json:"max_conditions"`
	MaxConditionChars int `json:"max_condition_chars"`
	MaxRegexChars     int `json:"max_regex_chars"`
}

// CollectorPageRequest pages a list.
type CollectorPageRequest struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// CollectorPage is a page of Collectors. Enabled reports whether the
// debugger is connected to fleet.
type CollectorPage struct {
	Enabled    bool        `json:"enabled"`
	Items      []Collector `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// CapturePointPage is a page of a Collector's capture points.
type CapturePointPage struct {
	CollectorID string         `json:"collector_id"`
	Items       []CapturePoint `json:"items"`
	NextCursor  string         `json:"next_cursor,omitempty"`
}

// peerSnapshot is one complete read of fleet's peers.
type peerSnapshot struct {
	at         time.Time
	ids        []string
	collectors map[string]Collector
	points     map[string][]CapturePoint
}

const (
	// peerFreshness is how long a peer read serves lists and admissions.
	peerFreshness = 2 * time.Second
	// maxPeerPages bounds one peer read to 4,096 peers.
	maxPeerPages = 64
)

type collectorRegistry struct {
	mu      sync.Mutex
	fleet   fleetAPI
	invalid bool
	now     func() time.Time
	epoch   string
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	runners sync.WaitGroup

	refresh    sync.Mutex // serializes peer reads
	peers      *peerSnapshot
	peerDigest string
	generation uint64

	sessions map[string]*captureRecord
	pending  int
	// pendingBy counts pending by principal; perPrincipal bounds each
	// principal's captures that have not ended (0 is unlimited).
	pendingBy         map[string]int
	perPrincipal      int
	sessionGeneration uint64
	dataEnabled       bool
	repository        artifact.Repository
	memoryBytes       int64
	history           int // captures kept, until deleted
	// loaded caches the records of the offloaded capture read last.
	loaded struct {
		id      string
		records []capturedRecord
	}
	derivationGate chan struct{}
}

func newCollectorRegistry(opts CaptureOptions) *collectorRegistry {
	ctx, cancel := context.WithCancel(context.Background())
	r := &collectorRegistry{
		sessions:       map[string]*captureRecord{},
		pendingBy:      map[string]int{},
		now:            opts.Now,
		epoch:          rand.Text(),
		ctx:            ctx,
		cancel:         cancel,
		derivationGate: make(chan struct{}, 2),
		history:        artifact.CaptureHistory,
	}
	if r.now == nil {
		r.now = time.Now
	}
	if opts.fleet != nil {
		r.fleet, r.dataEnabled = opts.fleet, true
	} else if opts.Fleet.Endpoint != "" {
		client, err := opts.client()
		if err != nil {
			r.invalid = true
		} else {
			r.fleet, r.dataEnabled = client, true
		}
	}
	return r
}

func collectorText(v string, max int, required bool) bool {
	return (!required || strings.TrimSpace(v) != "") && len(v) <= max && !strings.ContainsFunc(v, unicode.IsControl)
}

func validPeerID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == id
}

func validFleetID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == id
}

func (r *collectorRegistry) check() error {
	if r.closed {
		return &Error{Code: CodeUnavailable, Message: "platform is closed"}
	}
	if r.invalid {
		return &Error{Code: CodeInternal, Message: "invalid fleet connection settings"}
	}
	return nil
}

// checkFleet also requires a fleet connection.
func (r *collectorRegistry) checkFleet() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	if r.fleet == nil {
		return &Error{Code: CodeUnavailable, Message: "live capture needs a fleet connection; start linea-server with --fleet-url"}
	}
	return nil
}

// fleetError maps a fleet refusal onto the platform's codes. Fleet's messages
// are its own sanitized errors and never carry the debugger's credential.
func fleetError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return classify(ctx.Err(), CodeCancelled)
	}
	var refused *fleetcapture.Error
	if !errors.As(err, &refused) {
		return &Error{Code: CodeUnavailable, Message: "fleet is unreachable"}
	}
	message := "fleet refused the request"
	if refused.Message != "" {
		message = "fleet: " + refused.Message
	}
	if refused.Code == "collector_busy" {
		return &Error{Code: CodeConflict, Message: "a selected Collector is already serving another capture; a Collector serves one capture at a time, so wait for it to end"}
	}
	if refused.Code == "config_management_disabled" {
		return &Error{Code: CodeUnavailable, Message: "fleet runs without config management; start fleet with --config-management"}
	}
	switch refused.Status {
	case 400, 422:
		return &Error{Code: CodeInvalidRequest, Message: message}
	case 401:
		return &Error{Code: CodeUnavailable, Message: "fleet refused the debugger's credential"}
	case 403:
		return &Error{Code: CodeForbidden, Message: message}
	case 404:
		return &Error{Code: CodeNotFound, Message: message}
	case 409, 410, 412:
		return &Error{Code: CodeConflict, Message: message}
	case 413, 429:
		return &Error{Code: CodeResourceExhausted, Message: message}
	}
	return &Error{Code: CodeUnavailable, Message: message}
}

// snapshot returns a peer read at most peerFreshness old, or a new one when
// force is set.
func (r *collectorRegistry) snapshot(ctx context.Context, force bool) (*peerSnapshot, error) {
	r.refresh.Lock()
	defer r.refresh.Unlock()
	r.mu.Lock()
	current := r.peers
	r.mu.Unlock()
	if current != nil && !force && r.now().Sub(current.at) < peerFreshness && !r.now().Before(current.at) {
		return current, nil
	}
	fresh, err := r.readPeers(ctx)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal([]any{fresh.ids, fresh.collectors, fresh.points})
	sum := sha256.Sum256(raw)
	r.mu.Lock()
	defer r.mu.Unlock()
	if digest := hex.EncodeToString(sum[:]); digest != r.peerDigest {
		r.peerDigest = digest
		r.generation++
	}
	r.peers = fresh
	return fresh, nil
}

// readPeers pages all peers. A 410 means the plugin restarted mid-read.
func (r *collectorRegistry) readPeers(ctx context.Context) (*peerSnapshot, error) {
	for restarts := 0; ; restarts++ {
		s := &peerSnapshot{at: r.now(), collectors: map[string]Collector{}, points: map[string][]CapturePoint{}}
		cursor, restart := "", false
		for page := 0; ; page++ {
			if page == maxPeerPages {
				return nil, &Error{Code: CodeResourceExhausted, Message: "fleet reports more than 4,096 capture Collectors"}
			}
			got, err := r.fleet.Peers(ctx, cursor, 64)
			if fleetcapture.StatusOf(err) == 410 && restarts < 2 {
				restart = true
				break
			}
			if err != nil {
				return nil, fleetError(ctx, err)
			}
			for _, p := range got.Items {
				if c, points, ok := peerCollector(p); ok && s.collectors[c.ID].ID == "" {
					s.ids = append(s.ids, c.ID)
					s.collectors[c.ID], s.points[c.ID] = c, points
				}
			}
			if got.NextCursor == "" {
				break
			}
			cursor = got.NextCursor
		}
		if !restart {
			slices.Sort(s.ids)
			return s, nil
		}
	}
}

// peerCollector bounds a peer's report. Fleet validated it; a peer that
// still exceeds the debugger's bounds is left out.
func peerCollector(p fleetcapture.Peer) (Collector, []CapturePoint, bool) {
	i := p.Identity
	if !validPeerID(p.ID) || p.Generation == 0 || !collectorText(p.CredentialID, 128, false) || !collectorText(p.AgentID, 128, false) ||
		!collectorText(i.Cluster, 256, false) || !collectorText(i.Namespace, 256, false) || !collectorText(i.Pod, 256, true) ||
		!collectorText(i.CollectorGroup, 256, true) || !collectorText(i.CollectorVersion, 128, false) ||
		!collectorText(i.DistributionVersion, 128, false) || len(i.CapturePoints) > 256 {
		return Collector{}, nil, false
	}
	c := Collector{
		ID: p.ID, Generation: p.Generation, AgentID: p.AgentID, CredentialID: p.CredentialID,
		Cluster: i.Cluster, Namespace: i.Namespace, Pod: i.Pod, CollectorGroup: i.CollectorGroup,
		CollectorVersion: i.CollectorVersion, DistributionVersion: i.DistributionVersion, Enabled: i.Enabled,
		FilterLimits: reportedFilterLimits(i.FilterLimits),
	}
	points := make([]CapturePoint, 0, len(i.CapturePoints))
	seen := map[string]bool{}
	for _, point := range i.CapturePoints {
		key := point.Pipeline + "\x00" + point.Point + "\x00" + point.Signal
		if !collectorText(point.Pipeline, 256, true) || !collectorText(point.Point, 128, true) ||
			!slices.Contains([]string{"traces", "logs", "metrics"}, point.Signal) ||
			!slices.Contains([]string{"raw", "redacted"}, point.Sensitivity) || seen[key] {
			return Collector{}, nil, false
		}
		seen[key] = true
		points = append(points, CapturePoint(point))
		if point.Sensitivity == "raw" {
			c.RawPointCount++
		}
	}
	slices.SortFunc(points, func(a, b CapturePoint) int {
		return strings.Compare(a.Pipeline+"\x00"+a.Point+"\x00"+a.Signal, b.Pipeline+"\x00"+b.Point+"\x00"+b.Signal)
	})
	c.PointCount = len(points)
	return c, points, true
}

// v1FilterLimits apply to a Collector that reports no filter limits, or a
// zero one: livecapture protocol version 1 compiles 16 conditions of 256
// characters.
var v1FilterLimits = FilterLimits{MaxConditions: 16, MaxConditionChars: 256, MaxRegexChars: 256}

// reportedFilterLimits fills unset limits from version 1 and caps each at
// 65,536, as fleet does.
func reportedFilterLimits(reported *fleetcapture.FilterLimits) FilterLimits {
	limits := v1FilterLimits
	if reported != nil {
		for _, f := range []struct {
			to   *int
			from int
		}{{&limits.MaxConditions, reported.MaxConditions}, {&limits.MaxConditionChars, reported.MaxConditionChars}, {&limits.MaxRegexChars, reported.MaxRegexChars}} {
			if f.from > 0 {
				*f.to = min(f.from, 1<<16)
			}
		}
	}
	return limits
}

// close stops admission and cancels every capture's runner. The runners stop
// their fleet captures and delete their channels; wait bounds that.
func (r *collectorRegistry) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for _, capture := range r.sessions {
		if capture.view.Data.Enabled && capture.running {
			capture.view.Data.State = "frozen"
			_ = r.checkpointCaptureLocked(context.Background(), capture, nil)
			capture.records = nil
		}
		capture.notify()
	}
	r.cancel()
}

// wait gives the runners up to their cleanup bound to finish.
func (r *collectorRegistry) wait() {
	done := make(chan struct{})
	go func() {
		r.runners.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(captureCleanup + time.Second):
	}
}

// ListCollectors returns a page of the Collectors connected to fleet.
func (s *Service) ListCollectors(ctx context.Context, caller Caller, req CollectorPageRequest) (_ *CollectorPage, err error) {
	defer s.audit(&caller, "ListCollectors", &err)()
	if err := s.begin(ctx, caller, authz.FleetRead); err != nil {
		return nil, err
	}
	r := s.collectors
	r.mu.Lock()
	err = r.check()
	enabled := r.fleet != nil
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !enabled {
		page, next, err := collectorPage([]Collector{}, req, r.epoch+":collectors", 0, s.budgets)
		return &CollectorPage{Items: page, NextCursor: next}, err
	}
	snap, err := r.snapshot(ctx, false)
	if err != nil {
		return nil, err
	}
	items := make([]Collector, 0, len(snap.ids))
	for _, id := range snap.ids {
		items = append(items, snap.collectors[id])
	}
	r.mu.Lock()
	generation := r.generation
	r.mu.Unlock()
	page, next, err := collectorPage(items, req, r.epoch+":collectors", generation, s.budgets)
	return &CollectorPage{Enabled: true, Items: page, NextCursor: next}, err
}

// lookupCollector reads a connected Collector and its points.
func (s *Service) lookupCollector(ctx context.Context, caller Caller, perm authz.Permission, id string) (Collector, []CapturePoint, uint64, error) {
	if err := s.begin(ctx, caller, perm); err != nil {
		return Collector{}, nil, 0, err
	}
	r := s.collectors
	if err := r.checkFleet(); err != nil {
		return Collector{}, nil, 0, err
	}
	if !validPeerID(id) {
		return Collector{}, nil, 0, &Error{Code: CodeNotFound, Message: "Collector not found"}
	}
	snap, err := r.snapshot(ctx, false)
	if err != nil {
		return Collector{}, nil, 0, err
	}
	c, ok := snap.collectors[id]
	if !ok {
		return Collector{}, nil, 0, &Error{Code: CodeNotFound, Message: "Collector is not connected to fleet"}
	}
	r.mu.Lock()
	generation := r.generation
	r.mu.Unlock()
	return c, snap.points[id], generation, nil
}

// GetCollector returns a connected Collector.
func (s *Service) GetCollector(ctx context.Context, caller Caller, id string) (_ *Collector, err error) {
	defer s.audit(&caller, "GetCollector", &err)()
	c, _, _, err := s.lookupCollector(ctx, caller, authz.FleetRead, id)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListCapturePoints returns a page of a Collector's capture points.
func (s *Service) ListCapturePoints(ctx context.Context, caller Caller, id string, req CollectorPageRequest) (_ *CapturePointPage, err error) {
	defer s.audit(&caller, "ListCapturePoints", &err)()
	_, points, generation, err := s.lookupCollector(ctx, caller, authz.FleetRead, id)
	if err != nil {
		return nil, err
	}
	page, next, err := collectorPage(points, req, s.collectors.epoch+":points:"+id, generation, s.budgets)
	return &CapturePointPage{CollectorID: id, Items: page, NextCursor: next}, err
}

// Cursors bind the inventory generation and page size, not counters, so a
// stable inventory can always be paged through.
func collectorPage[T any](items []T, req CollectorPageRequest, family string, generation uint64, budgets Budgets) ([]T, string, error) {
	limit := req.Limit
	if limit == 0 {
		limit = min(25, budgets.PageItems)
	}
	if limit < 1 || limit > min(100, budgets.PageItems) {
		return nil, "", &Error{Code: CodeInvalidRequest, Message: "invalid Collector page limit"}
	}
	type cursor struct {
		Family        string
		Generation    uint64
		Limit, Offset int
	}
	c := cursor{Family: family, Generation: generation, Limit: limit}
	if req.Cursor != "" {
		if len(req.Cursor) > 1024 {
			return nil, "", &Error{Code: CodeInvalidRequest, Message: "invalid Collector cursor"}
		}
		raw, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		if err != nil || json.Unmarshal(raw, &c) != nil || c.Offset < 0 || c.Offset > len(items) || c.Limit != limit ||
			c.Family != family {
			return nil, "", &Error{Code: CodeInvalidRequest, Message: "invalid Collector cursor"}
		}
		if c.Generation != generation {
			return nil, "", &Error{Code: CodeConflict, Message: "Collector inventory changed; restart paging"}
		}
	}
	page := make([]T, 0, limit)
	used := 512 // bounds the envelope plus a session-bound cursor and collector ID
	for c.Offset < len(items) && len(page) < limit {
		raw, _ := json.Marshal(items[c.Offset])
		if used+len(raw) > budgets.PageBytes {
			if len(page) == 0 {
				return nil, "", &Error{
					Code:    CodeResourceExhausted,
					Message: "Collector entry exceeds the query byte budget",
				}
			}
			break
		}
		page = append(page, items[c.Offset])
		c.Offset++
		used += len(raw) + 1
	}
	next := ""
	if c.Offset < len(items) {
		raw, _ := json.Marshal(c)
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, next, nil
}
