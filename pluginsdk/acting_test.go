package pluginsdk

import (
	"net/http"
	"strings"
	"testing"
)

func TestReadActing(t *testing.T) {
	header := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(pairs); i += 2 {
			h.Add(pairs[i], pairs[i+1])
		}
		return h
	}
	if a, err := ReadActing(header()); a != nil || err != nil {
		t.Fatalf("absent = %v, %v", a, err)
	}
	a, err := ReadActing(header(ActingIDHeader, "user:alice", ActingNameHeader, "Alice Ü", ActingRequestHeader, "req-1"))
	if err != nil || *a != (Acting{ID: "user:alice", Name: "Alice Ü", RequestID: "req-1"}) {
		t.Fatalf("full = %+v, %v", a, err)
	}
	for name, h := range map[string]http.Header{
		"name without id": header(ActingNameHeader, "Alice"),
		"repeated id":     header(ActingIDHeader, "a", ActingIDHeader, "b"),
		"empty id":        header(ActingIDHeader, ""),
		"long id":         header(ActingIDHeader, strings.Repeat("a", MaxActingIDBytes+1)),
		"long name":       header(ActingIDHeader, "a", ActingNameHeader, strings.Repeat("n", MaxActingNameBytes+1)),
		"long request":    header(ActingIDHeader, "a", ActingRequestHeader, strings.Repeat("r", MaxActingRequestBytes+1)),
		"control":         header(ActingIDHeader, "a\tb"),
		"padded":          header(ActingIDHeader, " a"),
		"invalid utf-8":   header(ActingIDHeader, "a\xff"),
	} {
		if a, err := ReadActing(h); a != nil || err != ErrActing {
			t.Errorf("%s = %v, %v", name, a, err)
		}
	}

	h := header(ActingNameHeader, "forged")
	(&Acting{ID: "user:bob"}).Set(h)
	if h.Get(ActingIDHeader) != "user:bob" || h.Values(ActingNameHeader) != nil || h.Values(ActingRequestHeader) != nil {
		t.Fatalf("set = %v", h)
	}
	(*Acting)(nil).Set(h)
	if len(h) != 0 {
		t.Fatalf("nil set left %v", h)
	}
}
