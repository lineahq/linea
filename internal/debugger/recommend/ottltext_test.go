package recommend

import "testing"

// TestStatementTextProvesNoChange: a self-assignment and a repeat of a
// statement that sets a literal or deletes or keeps literal keys change
// nothing on any input; anything else depends on the input.
func TestStatementTextProvesNoChange(t *testing.T) {
	cases := []struct {
		text             string
		self, idempotent bool
	}{
		{`set(log.severity_number, log.severity_number)`, true, true},
		{`set(log.attributes["team"], "core")`, false, true},
		{`set(log.attributes["n"], 3) where log.severity_text == "FATAL"`, false, true},
		{`set(log.attributes["a, b"], "x(y)")`, false, true},
		{`set(log.attributes["q"], "say \"hi\"")`, false, true},
		{`delete_key(log.attributes, "env")`, false, true},
		{`keep_keys(log.attributes, ["a", "b"])`, false, true},
		{`set(log.attributes["n"], log.attributes["n"] + 1)`, false, false},
		{`set(log.attributes["id"], UUID())`, false, false},
		{`set(log.body, Concat([log.body, "x"], ""))`, false, false},
		{`delete_key(log.attributes, log.attributes["k"])`, false, false},
		{`merge_maps(log.attributes, log.cache, "upsert")`, false, false},
		{`set(log.attributes["x"], "y") wher z`, false, false},
		{`not a statement`, false, false},
	}
	for _, c := range cases {
		if got := selfAssignment(c.text); got != c.self {
			t.Errorf("selfAssignment(%s) = %v, want %v", c.text, got, c.self)
		}
		if got := idempotent(c.text); got != c.idempotent {
			t.Errorf("idempotent(%s) = %v, want %v", c.text, got, c.idempotent)
		}
	}
}
