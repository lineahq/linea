package scenario

import (
	"bytes"
	"path/filepath"
	"testing"
)

const identityBase = "version: 1\nname: original\ninputs: [{receiver: otlp, client_address: 10.0.0.9, data: '{\"resourceLogs\": []}'}]\nmocks: {files: [{path: /logs/app.log, content: line}]}\nenvironment: {TOKEN: declared}\nexecution: {timeout: 5s}\n" + fromRunAssertion

func identity(t *testing.T, raw string) []byte {
	t.Helper()
	id, err := InputIdentity([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestInputIdentityIgnoresOnlyWhatInjectsNothing(t *testing.T) {
	base := identity(t, identityBase)
	replaced, err := ReplaceAssertions([]byte(identityBase), []byte(fromRunExtra))
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"renamed":               "# a comment\n" + identityBase[:len("version: 1\n")] + "name: other # renamed\n" + identityBase[len("version: 1\nname: original\n"):],
		"unnamed, no asserts":   "version: 1\ninputs: [{receiver: otlp, client_address: 10.0.0.9, data: '{\"resourceLogs\": []}'}]\nmocks: {files: [{path: /logs/app.log, content: line}]}\nenvironment: {TOKEN: declared}\nexecution: {timeout: 5s}\n",
		"replaced assertions":   string(replaced),
		"anchored mock content": "version: 1\ninputs: [{receiver: otlp, client_address: 10.0.0.9, data: '{\"resourceLogs\": []}'}]\nmocks: {files: [{path: /logs/app.log, content: &c line}]}\nenvironment: {TOKEN: declared}\nexecution: {timeout: 5s}\nassertions: [{name: *c, target: {exporter: debug, signal: logs}, count: {equals: 0}}]\n",
		"merged top level":      "version: 1\n<<: {name: merged, environment: {TOKEN: declared}}\ninputs: [{receiver: otlp, client_address: 10.0.0.9, data: '{\"resourceLogs\": []}'}]\nmocks: {files: [{path: /logs/app.log, content: line}]}\nexecution: {timeout: 5s}\n",
	} {
		if got := identity(t, raw); !bytes.Equal(got, base) {
			t.Errorf("%s: identity\n%s\nwant\n%s", name, got, base)
		}
	}
	for name, raw := range map[string]string{
		"receiver":    `version: 1` + "\ninputs: [{receiver: otlp/2, client_address: 10.0.0.9, data: '{\"resourceLogs\": []}'}]\nmocks: {files: [{path: /logs/app.log, content: line}]}\nenvironment: {TOKEN: declared}\nexecution: {timeout: 5s}\n",
		"data":        "version: 1\ninputs: [{receiver: otlp, client_address: 10.0.0.9, data: '{\"resourceSpans\": []}'}]\nmocks: {files: [{path: /logs/app.log, content: line}]}\nenvironment: {TOKEN: declared}\nexecution: {timeout: 5s}\n",
		"mock":        "version: 1\ninputs: [{receiver: otlp, client_address: 10.0.0.9, data: '{\"resourceLogs\": []}'}]\nmocks: {files: [{path: /logs/app.log, content: other}]}\nenvironment: {TOKEN: declared}\nexecution: {timeout: 5s}\n",
		"environment": "version: 1\ninputs: [{receiver: otlp, client_address: 10.0.0.9, data: '{\"resourceLogs\": []}'}]\nmocks: {files: [{path: /logs/app.log, content: line}]}\nenvironment: {TOKEN: other}\nexecution: {timeout: 5s}\n",
		"execution":   "version: 1\ninputs: [{receiver: otlp, client_address: 10.0.0.9, data: '{\"resourceLogs\": []}'}]\nmocks: {files: [{path: /logs/app.log, content: line}]}\nenvironment: {TOKEN: declared}\nexecution: {timeout: 6s}\n",
	} {
		if got := identity(t, raw); bytes.Equal(got, base) {
			t.Errorf("%s: a changed input kept the identity", name)
		}
	}
}

// A statement test runs a run's inputs inlined in one scenario, possibly with
// other assertions; that scenario, captured and derived again, must have the
// identity of the run it came from.
func TestInputIdentityOfARunMatchesItsInlinedScenario(t *testing.T) {
	dir := t.TempDir()
	for name, src := range map[string]RunSources{
		"quick": {Mode: "quick", Receiver: "otlp", ClientAddress: "10.0.0.2", Fixture: []byte("{\r\n  \"resourceLogs\": []\r\n}\r\n"),
			Setup: []byte("version: 1\nexecution: {timeout: 3s}\n"), Assertions: []byte(fromRunAssertion)},
		"scenario with files": {Mode: "scenario", Dir: dir, Scenario: []byte("# kept\nversion: 1\nname: files\ninputs: [{receiver: otlp, file: in.json}]\nmocks: {files: [{path: /a.log, file: a.log}]}\n" + fromRunAssertion),
			Files: map[string][]byte{filepath.Join(dir, "in.json"): []byte(`{"resourceLogs": []}`), filepath.Join(dir, "a.log"): []byte("line\n")}},
	} {
		t.Run(name, func(t *testing.T) {
			derived, err := FromRun(src, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, assertions := range []string{"", fromRunExtra} {
				test := derived
				if assertions != "" {
					if test, err = ReplaceAssertions(derived, []byte(assertions)); err != nil {
						t.Fatal(err)
					}
				}
				again, err := FromRun(RunSources{Mode: "scenario", Scenario: test}, "", nil)
				if err != nil {
					t.Fatal(err)
				}
				if a, b := identity(t, string(derived)), identity(t, string(again)); !bytes.Equal(a, b) {
					t.Fatalf("identity\n%s\nwant\n%s", b, a)
				}
			}
		})
	}
}

func TestInputIdentityRefusesWhatIsNotAScenario(t *testing.T) {
	for _, raw := range []string{"", "[1, 2]\n", "a: [\n"} {
		if _, err := InputIdentity([]byte(raw)); err == nil {
			t.Errorf("%q: no error", raw)
		}
	}
}
