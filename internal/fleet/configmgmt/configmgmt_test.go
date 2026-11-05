package configmgmt

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func TestValidateAcceptsCollectorConfig(t *testing.T) {
	body := `
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
exporters:
  otlphttp/backend:
    endpoint: https://backend.example:4318
    headers:
      Authorization: "Bearer ${env:BACKEND_TOKEN}"
      X-Tenant: ${env:TENANT}
  otlp:
    endpoint: https://${env:USER}:${env:PASS}@backend.example
    tls:
      ca_file: /etc/ca.pem
processors:
  batch:
extensions:
  basicauth/client:
    client_auth:
      username: fleet
      password: ${file:/run/secrets/pass}
service:
  extensions: [basicauth/client]
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlphttp/backend]
`
	if err := Validate([]byte(body), nil); err != nil {
		t.Fatal(err)
	}
	if err := Validate([]byte("{}"), nil); err != nil {
		t.Fatalf("empty layer: %v", err)
	}
	allowed := []string{"receiver:otlp", "exporter:otlphttp", "exporter:otlp", "processor:batch", "extension:basicauth"}
	if err := Validate([]byte(body), allowed); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	big := "receivers:\n  otlp:\n    x: \"" + strings.Repeat("a", MaxBodyBytes) + "\"\n"
	var deep strings.Builder
	deep.WriteString("service:\n")
	for i := range 40 {
		deep.WriteString(strings.Repeat("  ", i+1) + "a:\n")
	}
	for name, body := range map[string]string{
		"too large":      big,
		"not yaml":       "receivers: [",
		"empty":          "",
		"comment only":   "# nothing",
		"scalar":         "hello",
		"sequence":       "- a",
		"unknown key":    "pipelines: {}",
		"section scalar": "receivers: otlp",
		"service list":   "service: [a]",
		"bad id":         "receivers:\n  1otlp: {}",
		"bad name":       "receivers:\n  otlp/: {}",
		"name space":     "receivers:\n  otlp/a b: {}",
		"alias":          "receivers:\n  otlp: &x {}\n  otlp/2: *x",
		"deep":           deep.String(),
		"non-utf8":       "receivers:\n  otlp: {x: \"\xff\"}",
	} {
		var verr *ValidationError
		if err := Validate([]byte(body), nil); !errors.As(err, &verr) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if err := Validate([]byte("receivers:\n  kafka: {}"), []string{"receiver:otlp"}); err == nil {
		t.Error("allowlist not enforced")
	}
}

func TestValidateRejectsInlineSecrets(t *testing.T) {
	for name, body := range map[string]string{
		"password":       "extensions:\n  basicauth:\n    client_auth:\n      password: hunter2",
		"api key":        "exporters:\n  datadog:\n    api:\n      key: x\n      api_key: abc",
		"suffix":         "exporters:\n  x:\n    write_token: abc",
		"authorization":  "exporters:\n  x:\n    Authorization: Bearer abc",
		"bearer literal": "exporters:\n  x:\n    headers:\n      Authorization: \"Bearer abc ${env:X}\"",
		"header":         "exporters:\n  x:\n    headers:\n      X-Key: abc",
		"userinfo":       "exporters:\n  x:\n    endpoint: https://u:p@host",
		"half reference": "exporters:\n  x:\n    endpoint: https://${env:U}:p@host",
		"list userinfo":  "receivers:\n  x:\n    brokers: [\"kafka://u:p@host\"]",
		"mixed":          "exporters:\n  x:\n    token: abc${env:X}",
	} {
		err := Validate([]byte(body), nil)
		if !errors.Is(err, ErrInlineSecret) {
			t.Errorf("%s: got %v", name, err)
			continue
		}
		for _, secret := range []string{"hunter2", "abc", ":p@"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error leaks value: %v", name, err)
			}
		}
	}
}

func TestValidPolicy(t *testing.T) {
	if !ValidPolicy([]string{"receiver:otlp", "connector:spanmetrics"}) || !ValidPolicy(nil) {
		t.Fatal("valid policy refused")
	}
	for _, bad := range []string{"otlp", "receivers:otlp", "receiver:", "receiver:o-tlp"} {
		if ValidPolicy([]string{bad}) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDesiredOrdersAndNamesLayers(t *testing.T) {
	plan, ok, err := Desired([]Layer{
		{Layer: 80, Name: "site", Body: []byte("c")},
		{Layer: 10, Name: "base", Body: []byte("a")},
		{Layer: 50, Name: "env-b", Body: []byte("b2")},
		{Layer: 50, Name: "env-a", Body: []byte("b1")},
	})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	var names []string
	for _, e := range plan.Entries {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, ","); got != "10-base,50-env-a,50-env-b,80-site" {
		t.Fatal(got)
	}
	reordered, _, _ := Desired([]Layer{
		{Layer: 50, Name: "env-a", Body: []byte("b1")},
		{Layer: 10, Name: "base", Body: []byte("a")},
		{Layer: 80, Name: "site", Body: []byte("c")},
		{Layer: 50, Name: "env-b", Body: []byte("b2")},
	})
	if !bytes.Equal(plan.Hash, reordered.Hash) {
		t.Fatal("hash depends on input order")
	}
	changed, _, _ := Desired([]Layer{{Layer: 10, Name: "base", Body: []byte("a2")}})
	if bytes.Equal(changed.Hash, plan.Hash) {
		t.Fatal("hash ignores bodies")
	}
	if p, ok, err := Desired(nil); ok || err != nil || len(p.Entries) != 0 {
		t.Fatal("no layers must not produce a plan")
	}
}

func TestDesiredBounds(t *testing.T) {
	var layers []Layer
	for i := range MaxLayers + 1 {
		layers = append(layers, Layer{Layer: i, Name: "a", Body: []byte("{}")})
	}
	if _, _, err := Desired(layers); !errors.Is(err, ErrTooManyLayers) {
		t.Fatal(err)
	}
	for _, l := range []Layer{{Layer: -1, Name: "a"}, {Layer: 100, Name: "a"}, {Layer: 1, Name: "A"}, {Layer: 1, Name: "-a"}, {Layer: 1, Name: ""}} {
		if _, _, err := Desired([]Layer{l}); err == nil {
			t.Errorf("accepted %+v", l)
		}
	}
	if _, _, err := Desired([]Layer{{Layer: 1, Name: "a"}, {Layer: 1, Name: "a"}}); err == nil {
		t.Error("accepted duplicate layer")
	}
}

func TestHashMatchesSpikeEncoding(t *testing.T) {
	a, b := sha256.Sum256([]byte("x")), sha256.Sum256([]byte("y"))
	var want []byte
	want = append(want, "10-a\x00"...)
	want = append(want, a[:]...)
	want = append(want, "\n50-b\x00"...)
	want = append(want, b[:]...)
	want = append(want, '\n')
	sum := sha256.Sum256(want)
	if got := Hash([]Entry{{"50-b", []byte("y")}, {"10-a", []byte("x")}}); !bytes.Equal(got, sum[:]) {
		t.Fatal("hash encoding changed")
	}
}

func TestDecide(t *testing.T) {
	desired, _, _ := Desired([]Layer{{Layer: 10, Name: "base", Body: []byte("{}")}})
	other, _, _ := Desired([]Layer{{Layer: 10, Name: "base", Body: []byte("receivers: {}")}})
	unmanaged := Unmanage()
	on := Agent{Enabled: true, CredentialAllowed: true, AcceptsRemoteConfig: true}
	with := func(f func(*Agent)) Agent { a := on; f(&a); return a }

	cases := []struct {
		name    string
		agent   Agent
		plan    Plan
		matched bool
		want    Decision
	}{
		{"disabled", with(func(a *Agent) { a.Enabled = false }), desired, true, Decision{}},
		{"credential", with(func(a *Agent) { a.CredentialAllowed = false }), desired, true, Decision{}},
		{"capability", with(func(a *Agent) { a.AcceptsRemoteConfig = false }), desired, true, Decision{}},
		{"first send", on, desired, true, Decision{Send: true, Plan: desired}},
		{"in sync", with(func(a *Agent) { a.Status = Status{desired.Hash, Applied} }), desired, true, Decision{}},
		{"applying", with(func(a *Agent) { a.Status = Status{desired.Hash, Applying} }), desired, true, Decision{}},
		{"drift", with(func(a *Agent) { a.Status = Status{other.Hash, Applied} }), desired, true, Decision{Send: true, Plan: desired}},
		{"held", with(func(a *Agent) { a.Status = Status{other.Hash, Applied}; a.Held = desired.Hash }), desired, true, Decision{Held: true}},
		{"hold cleared by change", with(func(a *Agent) { a.Status = Status{desired.Hash, Applied}; a.Held = desired.Hash }), other, true, Decision{Send: true, Plan: other, ClearHold: true}},
		{"never managed", on, Plan{}, false, Decision{}},
		{"never managed stale hold", with(func(a *Agent) { a.Held = desired.Hash }), Plan{}, false, Decision{ClearHold: true}},
		{"release", with(func(a *Agent) { a.Status = Status{desired.Hash, Applied} }), Plan{}, false, Decision{Send: true, Plan: unmanaged}},
		{"released", with(func(a *Agent) { a.Status = Status{unmanaged.Hash, Applied} }), Plan{}, false, Decision{}},
		{"release after failure", with(func(a *Agent) { a.Status = Status{desired.Hash, Failed}; a.Held = desired.Hash }), Plan{}, false, Decision{Send: true, Plan: unmanaged, ClearHold: true}},
	}
	for _, c := range cases {
		got := Decide(c.agent, c.plan, c.matched)
		if got.Send != c.want.Send || got.ClearHold != c.want.ClearHold || got.Held != c.want.Held || !bytes.Equal(got.Plan.Hash, c.want.Plan.Hash) {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

// TestNoEmptyConfigMap enumerates the send rule's inputs: no decision may
// send a map without entries, which would stop the Collector.
func TestNoEmptyConfigMap(t *testing.T) {
	desired, _, _ := Desired([]Layer{{Layer: 10, Name: "base", Body: []byte("{}")}})
	empty, _, _ := Desired(nil)
	hashes := [][]byte{nil, desired.Hash, Unmanage().Hash, []byte("other")}
	states := []State{Unset, Applying, Applied, Failed}
	for _, plan := range []struct {
		p  Plan
		ok bool
	}{{desired, true}, {empty, false}} {
		for _, reported := range hashes {
			for _, held := range hashes {
				for _, st := range states {
					for bits := range 8 {
						a := Agent{Enabled: bits&1 != 0, CredentialAllowed: bits&2 != 0, AcceptsRemoteConfig: bits&4 != 0, Status: Status{reported, st}, Held: held}
						d := Decide(a, plan.p, plan.ok)
						if d.Send && len(d.Plan.Entries) == 0 {
							t.Fatalf("empty config map for %+v", a)
						}
						if d.Send && bits != 7 {
							t.Fatalf("sent with a gate closed: %+v", a)
						}
					}
				}
			}
		}
	}
}

func TestNextHold(t *testing.T) {
	a, b := []byte("a"), []byte("b")
	cases := []struct {
		name       string
		prev, next Status
		held, want []byte
	}{
		{"failed", Status{a, Applied}, Status{b, Failed}, nil, b},
		{"restart reports persisted failure", Status{}, Status{b, Failed}, nil, b},
		{"rollback", Status{b, Applying}, Status{a, Applied}, nil, b},
		{"applied", Status{b, Applying}, Status{b, Applied}, nil, nil},
		{"newer send applying", Status{b, Applying}, Status{a, Applying}, nil, nil},
		{"keeps hold", Status{a, Applied}, Status{a, Applied}, b, b},
	}
	for _, c := range cases {
		if got := NextHold(c.prev, c.next, c.held); !bytes.Equal(got, c.want) {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
