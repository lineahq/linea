// Package translog writes the live-capture plugin's transition log: one JSON
// line on stderr per state change, never per request.
//
// The API is closed on purpose. An event is a constant of type Event. An
// attribute comes only from a typed constructor: an ID (checked against a
// narrow character set), an enum (Reason, State, Link, Listener), a count, a
// duration or a version string. No function accepts an error, a free message,
// a map or an arbitrary value, so a token, a filter, an attribute value or a
// payload cannot reach a log line by accident. Strings that come from outside
// an enum map through a closed lookup (ParseReason, ParseState, ParseLink)
// that answers unknown for anything it does not list.
//
// Logging never waits: lines go through a bounded queue to a writer goroutine
// and are dropped, and counted, when the queue is full. The package imports
// only the standard library; it is the plugin's own copy and is deliberately
// not part of pluginsdk.
package translog
