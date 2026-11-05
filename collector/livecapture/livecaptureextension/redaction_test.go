package livecaptureextension

import (
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

func TestRedactorRewritesOnlyChangedStrings(t *testing.T) {
	r, err := newRedactor(Redaction{MaxStringLength: 12, MaxLogBodyLength: 12, Masks: []string{`token=\w+`}, DenyAttributes: []string{"db.query.text"}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"unchanged", "checkout", "checkout"},
		{"masked", "token=abc", "[REDACTED]"},
		{"masked inside", "a token=abc", "a [REDACTED]"},
		{"truncated", "0123456789abcdef", "0123456789ab"},
		{"masked then truncated", "xx token=abc yy", "xx [REDACTED"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attributes := pcommon.NewMap()
			attributes.PutStr("value", tt.input)
			attributes.PutEmptyMap("nested").PutStr("value", tt.input)
			attributes.PutEmptySlice("list").AppendEmpty().SetStr(tt.input)
			attributes.PutStr("db.query.text", "secret")
			r.attributes(attributes)
			if _, ok := attributes.Get("db.query.text"); ok {
				t.Fatal("denied attribute kept")
			}
			value, _ := attributes.Get("value")
			nested, _ := attributes.Get("nested")
			inner, _ := nested.Map().Get("value")
			list, _ := attributes.Get("list")
			for _, got := range []string{value.Str(), inner.Str(), list.Slice().At(0).Str()} {
				if got != tt.want {
					t.Fatalf("redacted %q to %q, want %q", tt.input, got, tt.want)
				}
			}
		})
	}
}

// Captures redact every string of every copied record, so a string that
// needs no change must not be rewritten.
func TestRedactorUnchangedStringsAllocateNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector's instrumentation allocates")
	}
	r, err := newRedactor(Redaction{MaxStringLength: 64, MaxLogBodyLength: 64, Masks: []string{`token=\w+`}})
	if err != nil {
		t.Fatal(err)
	}
	attributes := pcommon.NewMap()
	attributes.PutStr("service.name", "checkout")
	attributes.PutStr("http.route", "/cart/{id}")
	attributes.PutInt("count", 3)
	if allocs := testing.AllocsPerRun(100, func() { r.attributes(attributes) }); allocs != 0 {
		t.Fatalf("redacting unchanged attributes allocated %.1f times", allocs)
	}
}
