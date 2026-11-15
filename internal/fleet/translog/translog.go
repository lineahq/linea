// Package translog writes the fleet server's transition log: one JSON line on
// stderr for each state transition, never one per request.
//
// What can reach a line is limited by construction. Every field is an
// enumerated value (event, reason, state, listener, key) or one of four typed
// constructors: ID, Version, Count and Duration. No function takes an error,
// an arbitrary message, a map or a free-form key, so a credential, a
// configuration body, an attribute value or telemetry cannot be logged by
// accident. Enumerated types are integers whose names live only in this
// package, so a string cannot be converted into one. IDs and versions are the
// only strings, and each is checked against a strict character set and length
// before it is written; a value that fails is replaced by a placeholder.
//
// A nil *Logger is valid and discards everything, so components work
// unwired in tests. The package uses only the standard library.
package translog

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Event names one kind of state transition.
type Event uint8

const (
	eventUnknown Event = iota
	// ProcessStarted: the server began starting; carries the version.
	ProcessStarted
	// ProcessReady: both listeners are bound.
	ProcessReady
	// ProcessStopped: the server finished stopping; carries why.
	ProcessStopped
	// ReplicaRegistered: this process registered as a replica.
	ReplicaRegistered
	// ReplicaDeregistered: this replica left cleanly.
	ReplicaDeregistered
	// ReplicaReaped: the leader removed another replica that stopped renewing.
	ReplicaReaped
	// ReplicaFenced: this replica stopped itself because it cannot prove liveness.
	ReplicaFenced
	// AgentsReleased: the leader released agents held by dead replicas.
	AgentsReleased
	// LeaderGained and LeaderLost: this replica's leader lease changed.
	LeaderGained
	LeaderLost
	// DatabaseLost and DatabaseRecovered: heartbeat writes failed or resumed.
	DatabaseLost
	DatabaseRecovered
	// PluginStateChanged and PluginEpochChanged: a plugin's state or host epoch changed.
	PluginStateChanged
	PluginEpochChanged
	// RolloutCreated, RolloutStateChanged, RolloutWaveReleased: rollout transitions.
	RolloutCreated
	RolloutStateChanged
	RolloutWaveReleased
	// ConfigWithheld: a config the server would send was held back.
	ConfigWithheld
	// CredentialCreated, CredentialRevoked, CredentialUpdated: credential changes.
	CredentialCreated
	CredentialRevoked
	CredentialUpdated
	// AuthFailures: the summary of failed authentications on one listener.
	AuthFailures
	// LinesSuppressed: lines of a rate-limited event dropped in one window.
	LinesSuppressed
	numEvents
)

var eventNames = [numEvents]string{
	eventUnknown:        "unknown",
	ProcessStarted:      "process_started",
	ProcessReady:        "process_ready",
	ProcessStopped:      "process_stopped",
	ReplicaRegistered:   "replica_registered",
	ReplicaDeregistered: "replica_deregistered",
	ReplicaReaped:       "replica_reaped",
	ReplicaFenced:       "replica_fenced",
	AgentsReleased:      "agents_released",
	LeaderGained:        "leader_gained",
	LeaderLost:          "leader_lost",
	DatabaseLost:        "database_lost",
	DatabaseRecovered:   "database_recovered",
	PluginStateChanged:  "plugin_state_changed",
	PluginEpochChanged:  "plugin_epoch_changed",
	RolloutCreated:      "rollout_created",
	RolloutStateChanged: "rollout_state_changed",
	RolloutWaveReleased: "rollout_wave_released",
	ConfigWithheld:      "config_withheld",
	CredentialCreated:   "credential_created",
	CredentialRevoked:   "credential_revoked",
	CredentialUpdated:   "credential_updated",
	AuthFailures:        "auth_failures",
	LinesSuppressed:     "lines_suppressed",
}

func (e Event) String() string { return name(eventNames[:], uint8(e)) }

// Reason says why a transition happened.
type Reason uint8

const (
	reasonUnknown Reason = iota
	// Process stop.
	ReasonSignal
	ReasonConfigInvalid
	ReasonDatabaseUnavailable
	ReasonSchemaConflict
	ReasonNoAdminCredential
	ReasonTLSIdentity
	ReasonListenFailed
	ReasonPluginsInvalid
	ReasonListenerFailed
	ReasonReplicaFenced
	ReasonShutdownIncomplete
	ReasonInternal
	// Replica and leader.
	ReasonRegistrationReaped
	ReasonHeartbeatLapsed
	ReasonLeaseUnavailable
	ReasonShutdown
	// Database failure classes.
	ReasonTimeout
	ReasonSQLState
	ReasonConnection
	// Plugins.
	ReasonRestored
	ReasonProbeOK
	ReasonProbeFailed
	ReasonProbeIncompatible
	ReasonInstanceChanged
	ReasonProbeLost
	ReasonLifecycleEnable
	ReasonLifecycleDrain
	ReasonLifecycleDisable
	// Rollouts.
	ReasonOperator
	ReasonThreshold
	ReasonController
	// Config withheld.
	ReasonIncompatibleComponents
	ReasonAgentReportedFailure
	// Credentials.
	ReasonRemoteConfigEnabled
	ReasonRemoteConfigDisabled
	numReasons
)

var reasonNames = [numReasons]string{
	reasonUnknown:                "unknown",
	ReasonSignal:                 "signal",
	ReasonConfigInvalid:          "config_invalid",
	ReasonDatabaseUnavailable:    "database_unavailable",
	ReasonSchemaConflict:         "schema_or_identity_conflict",
	ReasonNoAdminCredential:      "no_admin_credential",
	ReasonTLSIdentity:            "tls_identity_failed",
	ReasonListenFailed:           "listen_failed",
	ReasonPluginsInvalid:         "plugins_invalid",
	ReasonListenerFailed:         "listener_failed",
	ReasonReplicaFenced:          "replica_fenced",
	ReasonShutdownIncomplete:     "shutdown_incomplete",
	ReasonInternal:               "internal",
	ReasonRegistrationReaped:     "registration_reaped",
	ReasonHeartbeatLapsed:        "heartbeat_lapsed",
	ReasonLeaseUnavailable:       "lease_unavailable",
	ReasonShutdown:               "shutdown",
	ReasonTimeout:                "timeout",
	ReasonSQLState:               "sqlstate",
	ReasonConnection:             "connection",
	ReasonRestored:               "restored",
	ReasonProbeOK:                "probe_ok",
	ReasonProbeFailed:            "probe_failed",
	ReasonProbeIncompatible:      "probe_incompatible",
	ReasonInstanceChanged:        "instance_changed",
	ReasonProbeLost:              "probe_lost",
	ReasonLifecycleEnable:        "lifecycle_enable",
	ReasonLifecycleDrain:         "lifecycle_drain",
	ReasonLifecycleDisable:       "lifecycle_disable",
	ReasonOperator:               "operator",
	ReasonThreshold:              "threshold",
	ReasonController:             "controller",
	ReasonIncompatibleComponents: "incompatible_components",
	ReasonAgentReportedFailure:   "agent_reported_failure",
	ReasonRemoteConfigEnabled:    "remote_config_enabled",
	ReasonRemoteConfigDisabled:   "remote_config_disabled",
}

func (r Reason) String() string { return name(reasonNames[:], uint8(r)) }

// State is the state a plugin or rollout moved to.
type State uint8

const (
	stateUnknown State = iota
	StateReady
	StateUnavailable
	StateIncompatible
	StateEnabled
	StateDraining
	StateDisabled
	StateRunning
	StatePaused
	StateRollingBack
	StateCompleted
	StateRolledBack
	StateCancelled
	numStates
)

var stateNames = [numStates]string{
	stateUnknown:      "unknown",
	StateReady:        "ready",
	StateUnavailable:  "unavailable",
	StateIncompatible: "incompatible",
	StateEnabled:      "enabled",
	StateDraining:     "draining",
	StateDisabled:     "disabled",
	StateRunning:      "running",
	StatePaused:       "paused",
	StateRollingBack:  "rolling_back",
	StateCompleted:    "completed",
	StateRolledBack:   "rolled_back",
	StateCancelled:    "cancelled",
}

func (s State) String() string { return name(stateNames[:], uint8(s)) }

// Listener names one of the server's listeners.
type Listener uint8

const (
	listenerUnknown Listener = iota
	ListenerManagement
	ListenerAgent
	ListenerPluginHost
	numListeners
)

var listenerNames = [numListeners]string{
	listenerUnknown:    "unknown",
	ListenerManagement: "management",
	ListenerAgent:      "agent",
	ListenerPluginHost: "plugin_host",
}

func (l Listener) String() string { return name(listenerNames[:], uint8(l)) }

// IDKey is the field name of an ID. The set is closed.
type IDKey uint8

const (
	idUnknown IDKey = iota
	KeyReplica
	KeyPlugin
	KeyCredential
	KeyActor
	KeyRollout
	KeyAssignment
	KeyAgent
	numIDKeys
)

var idKeyNames = [numIDKeys]string{
	idUnknown:     "id",
	KeyReplica:    "replica",
	KeyPlugin:     "plugin",
	KeyCredential: "credential",
	KeyActor:      "actor",
	KeyRollout:    "rollout",
	KeyAssignment: "assignment",
	KeyAgent:      "agent",
}

func (k IDKey) String() string { return name(idKeyNames[:], uint8(k)) }

// CountKey is the field name of a count. The set is closed.
type CountKey uint8

const (
	countUnknown CountKey = iota
	CountAgents
	CountReplicas
	CountTargets
	CountIncompatible
	CountWave
	CountWaves
	CountTotal
	CountSuppressed
	numCountKeys
)

var countKeyNames = [numCountKeys]string{
	countUnknown:      "unknown",
	CountAgents:       "agents",
	CountReplicas:     "replicas",
	CountTargets:      "targets",
	CountIncompatible: "incompatible",
	CountWave:         "wave",
	CountWaves:        "waves",
	CountTotal:        "count",
	CountSuppressed:   "suppressed",
}

func (k CountKey) String() string { return name(countKeyNames[:], uint8(k)) }

// DurationKey is the field name of a duration. The set is closed.
type DurationKey uint8

const (
	durationUnknown DurationKey = iota
	DurationUptime
	DurationOutage
	DurationWindow
	numDurationKeys
)

var durationKeyNames = [numDurationKeys]string{
	durationUnknown: "unknown",
	DurationUptime:  "uptime",
	DurationOutage:  "outage",
	DurationWindow:  "duration",
}

func (k DurationKey) String() string { return name(durationKeyNames[:], uint8(k)) }

func name(names []string, i uint8) string {
	if int(i) < len(names) && names[i] != "" {
		return names[i]
	}
	return "unknown"
}

// Attr is one field of a line. Only this package can construct one.
type Attr struct{ attr slog.Attr }

const (
	maxIDLength      = 128
	maxVersionLength = 64
	invalid          = "invalid"
)

// validToken reports whether s is a plausible identifier: ASCII letters,
// digits and . _ : @ - only, within max bytes.
func validToken(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == ':', c == '@', c == '-':
		default:
			return false
		}
	}
	return true
}

// ID is an identifier such as a replica, plugin, credential or rollout ID.
// A value that is not a short token of letters, digits and . _ : @ - is
// written as "invalid", never as given.
func ID(key IDKey, value string) Attr {
	if !validToken(value, maxIDLength) {
		value = invalid
	}
	return Attr{slog.String(key.String(), value)}
}

// Version is a release string such as 1.2.3 or 0.0.0-dev.
func Version(value string) Attr {
	if !validToken(value, maxVersionLength) && !validRelease(value) {
		value = invalid
	}
	return Attr{slog.String("version", value)}
}

// validRelease allows the + of semantic version build metadata.
func validRelease(s string) bool {
	if len(s) > maxVersionLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '+' {
			return validToken(s[:i], maxVersionLength) && validToken(s[i+1:], maxVersionLength)
		}
	}
	return false
}

// Why adds the reason of a transition.
func Why(r Reason) Attr { return Attr{slog.String("reason", r.String())} }

// Now adds the state a plugin or rollout moved to.
func Now(s State) Attr { return Attr{slog.String("state", s.String())} }

// On names a listener.
func On(l Listener) Attr { return Attr{slog.String("listener", l.String())} }

// Of names the event a suppression count refers to.
func Of(e Event) Attr { return Attr{slog.String("of", e.String())} }

// Count is a non-negative count.
func Count(key CountKey, n int64) Attr {
	if n < 0 {
		n = 0
	}
	return Attr{slog.Int64(key.String(), n)}
}

// Duration is written in whole milliseconds, as <key>_ms.
func Duration(key DurationKey, d time.Duration) Attr {
	if d < 0 {
		d = 0
	}
	return Attr{slog.Int64(key.String()+"_ms", d.Milliseconds())}
}

// component is the value of the component field on every line, so lines of
// the fleet server and of its plugins can share one log stream.
const component = "fleet"

// limitPerWindow bounds the lines of one rate-limited event in each window.
const limitPerWindow = 20

// Logger writes transition lines. The zero value and nil discard.
type Logger struct {
	log *slog.Logger
	// authFailures counts failed authentications per listener since the
	// last flush.
	authFailures [numListeners]atomic.Uint64
	// written and suppressed count the lines of rate-limited events in
	// this window.
	mu          sync.Mutex
	written     [numEvents]int
	suppressed  [numEvents]int
	windowStart time.Time
}

// New writes JSON lines to w. Each line has time, level and msg (the event
// name), then component (always fleet) and the fields of the event.
func New(w io.Writer) *Logger {
	handler := slog.NewJSONHandler(w, nil)
	return &Logger{log: slog.New(handler).With(slog.String("component", component)), windowStart: time.Now()}
}

// Discard returns a logger that writes nothing. Counters still work.
func Discard() *Logger { return New(io.Discard) }

func (l *Logger) emit(level slog.Level, e Event, attrs []Attr) {
	if l == nil || l.log == nil {
		return
	}
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = a.attr
	}
	l.log.LogAttrs(context.Background(), level, e.String(), out...)
}

// Info logs a transition that is expected in normal operation.
func (l *Logger) Info(e Event, attrs ...Attr) { l.emit(slog.LevelInfo, e, attrs) }

// Warn logs a transition an operator should notice.
func (l *Logger) Warn(e Event, attrs ...Attr) { l.emit(slog.LevelWarn, e, attrs) }

// Error logs a transition that ends the process or its authority.
func (l *Logger) Error(e Event, attrs ...Attr) { l.emit(slog.LevelError, e, attrs) }

// Limited logs like Warn but at most limitPerWindow lines of e per window;
// the rest are counted and reported by the next Flush as lines_suppressed.
// Use it for events that can repeat once per agent.
func (l *Logger) Limited(e Event, attrs ...Attr) {
	if l == nil || e >= numEvents {
		return
	}
	l.mu.Lock()
	if l.written[e] >= limitPerWindow {
		l.suppressed[e]++
		l.mu.Unlock()
		return
	}
	l.written[e]++
	l.mu.Unlock()
	l.emit(slog.LevelWarn, e, attrs)
}

// AuthFailure counts one failed authentication on a listener. It writes
// nothing now; Flush writes one summary line per listener.
func (l *Logger) AuthFailure(on Listener) {
	if l == nil || on >= numListeners {
		return
	}
	l.authFailures[on].Add(1)
}

// Flush writes the summary lines for the window that ended: one auth_failures
// line per listener with a count above zero, and one lines_suppressed line
// per rate-limited event that dropped lines. It then starts a new window.
func (l *Logger) Flush() {
	if l == nil {
		return
	}
	l.mu.Lock()
	now := time.Now()
	window := now.Sub(l.windowStart)
	l.windowStart = now
	l.mu.Unlock()
	for on := range l.authFailures {
		if n := l.authFailures[on].Swap(0); n > 0 {
			l.emit(slog.LevelWarn, AuthFailures, []Attr{On(Listener(on)), Count(CountTotal, int64(n)), Duration(DurationWindow, window)})
		}
	}
	l.mu.Lock()
	suppressed := l.suppressed
	l.suppressed = [numEvents]int{}
	l.written = [numEvents]int{}
	l.mu.Unlock()
	for e, n := range suppressed {
		if n > 0 {
			l.emit(slog.LevelWarn, LinesSuppressed, []Attr{Of(Event(e)), Count(CountSuppressed, int64(n))})
		}
	}
}

// SummaryInterval is how often the server flushes the summaries.
const SummaryInterval = 10 * time.Second

// Summarize flushes on every tick until ctx ends, then once more so a
// shutdown does not lose the last window. The caller supplies the ticks, so
// tests drive it without sleeping.
func (l *Logger) Summarize(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			l.Flush()
			return
		case <-ticks:
			l.Flush()
		}
	}
}
