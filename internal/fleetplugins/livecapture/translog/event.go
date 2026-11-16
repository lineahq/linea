package translog

import "log/slog"

// Event names one kind of state transition. The values are constants; the
// name is the log line's msg field.
type Event uint8

// Events. Each is logged once per transition.
const (
	eventNone Event = iota
	// EventPluginStarted and EventPluginStopped bracket a serve process.
	EventPluginStarted
	EventPluginStopped
	// EventCommandFailed is a non-serve command (migrate, purge, probe) that failed.
	EventCommandFailed
	// EventAuthorityAcquired, EventAuthorityStandby and EventAuthorityReleased
	// follow the single database authority.
	EventAuthorityAcquired
	EventAuthorityStandby
	EventAuthorityReleased
	// EventHostSessionEstablished and EventHostSessionLost follow the plugin
	// host session; a new epoch is a lost session followed by an established one.
	EventHostSessionEstablished
	EventHostSessionLost
	EventDrainStarted
	// EventDatabaseDegraded is the first failed ping after healthy ones,
	// EventDatabaseRecovered the next good ping, EventDatabaseLost the end.
	EventDatabaseDegraded
	EventDatabaseRecovered
	EventDatabaseLost
	// EventPluginFatal carries the cause of every fatal exit.
	EventPluginFatal
	EventOperationState
	EventPeerConnected
	EventPeerDisconnected
	EventPeerLink
	// EventAuthFailures is the summary of rejected credentials on one listener.
	EventAuthFailures
	eventCount
)

var eventNames = [eventCount]string{
	eventNone:                   "unknown",
	EventPluginStarted:          "plugin_started",
	EventPluginStopped:          "plugin_stopped",
	EventCommandFailed:          "command_failed",
	EventAuthorityAcquired:      "authority_acquired",
	EventAuthorityStandby:       "authority_standby",
	EventAuthorityReleased:      "authority_released",
	EventHostSessionEstablished: "host_session_established",
	EventHostSessionLost:        "host_session_lost",
	EventDrainStarted:           "drain_started",
	EventDatabaseDegraded:       "database_degraded",
	EventDatabaseRecovered:      "database_recovered",
	EventDatabaseLost:           "database_lost",
	EventPluginFatal:            "plugin_fatal",
	EventOperationState:         "operation_state",
	EventPeerConnected:          "peer_connected",
	EventPeerDisconnected:       "peer_disconnected",
	EventPeerLink:               "peer_link",
	EventAuthFailures:           "auth_failures",
}

func (e Event) String() string {
	if e >= eventCount {
		return eventNames[eventNone]
	}
	return eventNames[e]
}

func (e Event) level() slog.Level {
	switch e {
	case EventPluginFatal, EventDatabaseLost, EventCommandFailed:
		return slog.LevelError
	case EventHostSessionLost, EventDatabaseDegraded, EventAuthorityStandby, EventAuthFailures:
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// Reason says why a transition happened. It is also the cause of a fatal
// exit. The zero value means no reason and is not logged.
type Reason uint8

// Reasons. The strings are part of the operator-facing log format.
const (
	ReasonNone Reason = iota
	ReasonUnknown
	// ReasonShutdown is a requested, orderly stop.
	ReasonShutdown
	// Fatal causes and process-level failures.
	ReasonStorageUnavailable
	ReasonStorageLost
	ReasonStorageFailures
	ReasonSaveFailed
	ReasonTLSInvalid
	ReasonListenFailed
	ReasonListenerStopped
	ReasonSupervisorFailed
	ReasonConfigInvalid
	ReasonInvalidInput
	ReasonOwnerBusy
	ReasonUnavailable
	ReasonFailed
	// Host session.
	ReasonHostUnreachable
	ReasonPermissionsMissing
	ReasonEpochChanged
	// Why a peer left.
	ReasonCredentialRejected
	ReasonPeerStale
	ReasonPeerRetired
	ReasonStreamClosed
	ReasonStreamError
	// Why an operation ended; these are the operation reasons the plugin stores.
	ReasonHostAuthorityLost
	ReasonAbsoluteExpiry
	ReasonConsumerUnavailable
	ReasonPeerDisconnected
	ReasonCollectorRejected
	ReasonRequestedStop
	ReasonStopUnconfirmed
	ReasonAdmissionCancelled
	reasonCount
)

var reasonNames = [reasonCount]string{
	ReasonNone:                "",
	ReasonUnknown:             "unknown",
	ReasonShutdown:            "shutdown",
	ReasonStorageUnavailable:  "storage_unavailable",
	ReasonStorageLost:         "storage_lost",
	ReasonStorageFailures:     "storage_failures",
	ReasonSaveFailed:          "save_failed",
	ReasonTLSInvalid:          "tls_invalid",
	ReasonListenFailed:        "listen_failed",
	ReasonListenerStopped:     "listener_stopped",
	ReasonSupervisorFailed:    "supervisor_failed",
	ReasonConfigInvalid:       "config_invalid",
	ReasonInvalidInput:        "invalid_input",
	ReasonOwnerBusy:           "owner_busy",
	ReasonUnavailable:         "unavailable",
	ReasonFailed:              "failed",
	ReasonHostUnreachable:     "host_unreachable",
	ReasonPermissionsMissing:  "permissions_missing",
	ReasonEpochChanged:        "epoch_changed",
	ReasonCredentialRejected:  "credential_rejected",
	ReasonPeerStale:           "peer_stale",
	ReasonPeerRetired:         "peer_retired",
	ReasonStreamClosed:        "stream_closed",
	ReasonStreamError:         "stream_error",
	ReasonHostAuthorityLost:   "host_authority_lost",
	ReasonAbsoluteExpiry:      "absolute_expiry",
	ReasonConsumerUnavailable: "consumer_unavailable",
	ReasonPeerDisconnected:    "peer_disconnected",
	ReasonCollectorRejected:   "collector_rejected",
	ReasonRequestedStop:       "requested_stop",
	ReasonStopUnconfirmed:     "stop_unconfirmed",
	ReasonAdmissionCancelled:  "admission_cancelled",
}

func (r Reason) String() string {
	if r >= reasonCount {
		return reasonNames[ReasonUnknown]
	}
	return reasonNames[r]
}

// ParseReason maps a stored operation reason onto its Reason. The empty
// string is ReasonNone; a string that is not a known reason is ReasonUnknown.
func ParseReason(s string) Reason {
	if s == "" {
		return ReasonNone
	}
	for r := ReasonUnknown; r < reasonCount; r++ {
		if reasonNames[r] == s {
			return r
		}
	}
	return ReasonUnknown
}

// State is an operation's state. The zero value is none: the operation did
// not exist before the transition.
type State uint8

// States of a capture operation.
const (
	StateNone State = iota
	StateUnknown
	StateStarting
	StateActive
	StateStopping
	StateStopped
	StateCompleted
	StateExpired
	StateFailed
	StateInterrupted
	stateCount
)

var stateNames = [stateCount]string{
	StateNone: "none", StateUnknown: "unknown", StateStarting: "starting", StateActive: "active", StateStopping: "stopping",
	StateStopped: "stopped", StateCompleted: "completed", StateExpired: "expired", StateFailed: "failed", StateInterrupted: "interrupted",
}

func (s State) String() string {
	if s >= stateCount {
		return stateNames[StateUnknown]
	}
	return stateNames[s]
}

// ParseState maps an operation state string onto its State; anything not
// listed is StateUnknown.
func ParseState(s string) State {
	for v := StateStarting; v < stateCount; v++ {
		if stateNames[v] == s {
			return v
		}
	}
	return StateUnknown
}

// Link is a peer's link state to a fleet agent.
type Link uint8

// Link states.
const (
	LinkUnknown Link = iota
	LinkPending
	LinkLinked
	LinkNoMatch
	LinkAmbiguous
	linkCount
)

var linkNames = [linkCount]string{LinkUnknown: "unknown", LinkPending: "pending", LinkLinked: "linked", LinkNoMatch: "no_match", LinkAmbiguous: "ambiguous"}

func (l Link) String() string {
	if l >= linkCount {
		return linkNames[LinkUnknown]
	}
	return linkNames[l]
}

// ParseLink maps a link state string onto its Link; anything not listed is
// LinkUnknown.
func ParseLink(s string) Link {
	for v := LinkPending; v < linkCount; v++ {
		if linkNames[v] == s {
			return v
		}
	}
	return LinkUnknown
}

// Listener is one of the plugin's three authenticated listeners.
type Listener uint8

// Listeners.
const (
	ListenerHTTP Listener = iota
	ListenerControl
	ListenerOTLP
	listenerCount
)

var listenerNames = [listenerCount]string{ListenerHTTP: "http_api", ListenerControl: "control_grpc", ListenerOTLP: "otlp_grpc"}

func (l Listener) String() string {
	if l >= listenerCount {
		return "unknown"
	}
	return listenerNames[l]
}
