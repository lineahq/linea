package translog

import (
	"log/slog"
	"time"
)

// Attr is one log attribute. Only the constructors below make one.
type Attr struct{ a slog.Attr }

// maxIDLength bounds every ID value.
const maxIDLength = 128

// invalidValue replaces an ID or version that is not in the allowed set.
const invalidValue = "invalid"

// safeToken accepts letters, digits and . _ - (plus + when plus is set),
// 1 to maxLength bytes: enough for hexadecimal IDs, plugin IDs and versions,
// too narrow for prose.
func safeToken(s string, maxLength int, plus bool) bool {
	if len(s) == 0 || len(s) > maxLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		case c == '+' && plus:
		default:
			return false
		}
	}
	return true
}

func id(key, value string) Attr {
	if !safeToken(value, maxIDLength, false) {
		value = invalidValue
	}
	return Attr{slog.String(key, value)}
}

// Operation is a capture operation ID.
func Operation(value string) Attr { return id("operation", value) }

// Peer is a capture peer ID.
func Peer(value string) Attr { return id("peer", value) }

// Credential is a fleet credential ID, never its secret.
func Credential(value string) Attr { return id("credential", value) }

// Agent is a fleet agent ID.
func Agent(value string) Attr { return id("agent", value) }

// Authority is the database authority ID.
func Authority(value string) Attr { return id("authority", value) }

// Instance is the plugin instance ID.
func Instance(value string) Attr { return id("instance", value) }

// Epoch is the plugin host epoch.
func Epoch(value string) Attr { return id("epoch", value) }

// Version is a release version such as 0.5.0 or 0.0.0-dev.
func Version(value string) Attr {
	if !safeToken(value, 64, true) {
		value = invalidValue
	}
	return Attr{slog.String("version", value)}
}

// Why is the reason for a transition; ReasonNone logs nothing.
func Why(r Reason) Attr {
	if r == ReasonNone {
		return Attr{}
	}
	return Attr{slog.String("reason", r.String())}
}

// Cause is the cause of a fatal exit or of a command failure.
func Cause(r Reason) Attr { return Attr{slog.String("cause", r.String())} }

// From is the state before a transition.
func From(s State) Attr { return Attr{slog.String("from", s.String())} }

// To is the state after a transition.
func To(s State) Attr { return Attr{slog.String("to", s.String())} }

// LinkState is a peer's link state.
func LinkState(l Link) Attr { return Attr{slog.String("link", l.String())} }

// OnListener names the listener a summary line counts.
func OnListener(l Listener) Attr { return Attr{slog.String("listener", l.String())} }

// Count is a number of things.
func Count(n int) Attr { return Attr{slog.Int("count", n)} }

// Elapsed is a duration in milliseconds.
func Elapsed(d time.Duration) Attr { return Attr{slog.Int64("duration_ms", d.Milliseconds())} }
