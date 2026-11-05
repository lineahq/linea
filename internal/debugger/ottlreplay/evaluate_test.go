package ottlreplay

import (
	"bytes"
	"context"
	"testing"
)

// TestEvaluateMatchesTheProcessor checks that an evaluation, which has no
// observed output, produces what the real processor would and is never
// reported as verified.
func TestEvaluateMatchesTheProcessor(t *testing.T) {
	cases := []struct {
		name    string
		block   string
		dropped bool
	}{
		{"edits", "    trace_statements:\n      - context: span\n        statements:\n          - set(span.attributes[\"edited\"], true)\n", false},
		{"propagate error", "    error_mode: propagate\n    trace_statements:\n      - context: span\n        statements:\n          - merge_maps(span.attributes, span.attributes[\"http.status_code\"], \"insert\") where span.attributes[\"http.status_code\"] != nil\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := transformConfig(t, tc.block)
			in := fixture(t)
			rep, out, err := Evaluate(context.Background(), "transform", cfg, in)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Verification != VerificationNotVerifiable {
				t.Fatalf("verification = %s, want not_verifiable", rep.Verification)
			}
			want := runProcessor(t, cfg, in)
			if tc.dropped {
				if out != nil || want != nil {
					t.Fatalf("output = %v, processor output = %v; want both dropped", out, want)
				}
				return
			}
			got, _ := out.JSON()
			exp, _ := want.JSON()
			if !bytes.Equal(got, exp) {
				t.Fatalf("evaluation output differs from the processor:\n%s\n%s", got, exp)
			}
			if rep.Groups[0].Statements[0].Matched == 0 {
				t.Fatalf("statement not matched: %+v", rep.Groups[0].Statements[0])
			}
		})
	}
}
