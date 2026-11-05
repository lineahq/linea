package fleetcapture

import (
	"context"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Fleet's acting-for headers and their bounds. Fleet records the value on
// the events a request writes and the livecapture plugin records it on
// captures; neither authorizes on it.
const (
	actingIDHeader      = "X-Fleet-Acting-ID"
	actingNameHeader    = "X-Fleet-Acting-Name"
	actingRequestHeader = "X-Fleet-Acting-Request"
	maxActingID         = 256
	maxActingName       = 128
	maxActingRequest    = 128
)

// Acting names the debugger user a fleet call is made for. Fleet returns it
// as acting_for on the events and captures such a call wrote.
type Acting struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

type actingKey struct{}

// WithActing makes the client send a as fleet's acting-for headers on calls
// made with the returned context.
func WithActing(ctx context.Context, a Acting) context.Context {
	return context.WithValue(ctx, actingKey{}, a)
}

// ActingFrom returns the acting-for value WithActing put into ctx.
func ActingFrom(ctx context.Context) (Acting, bool) {
	a, ok := ctx.Value(actingKey{}).(Acting)
	return a, ok
}

// setActing writes the context's acting-for headers. A value fleet would
// refuse is fitted to its bounds: the name and request ID are shortened or
// dropped, and an unusable ID drops all three, so attribution never fails a
// call.
func setActing(ctx context.Context, h http.Header) {
	a, ok := ActingFrom(ctx)
	if !ok || !actingValue(a.ID) || len(a.ID) > maxActingID {
		return
	}
	h.Set(actingIDHeader, a.ID)
	if name := fitActing(a.Name, maxActingName); name != "" {
		h.Set(actingNameHeader, name)
	}
	if request := fitActing(a.RequestID, maxActingRequest); request != "" {
		h.Set(actingRequestHeader, request)
	}
}

func fitActing(v string, limit int) string {
	v = strings.TrimSpace(strings.ToValidUTF8(v, ""))
	for len(v) > limit {
		_, size := utf8.DecodeLastRuneInString(v)
		v = strings.TrimSpace(v[:len(v)-size])
	}
	if !actingValue(v) {
		return ""
	}
	return v
}

func actingValue(v string) bool {
	return v != "" && utf8.ValidString(v) && strings.TrimSpace(v) == v && strings.IndexFunc(v, unicode.IsControl) < 0
}
