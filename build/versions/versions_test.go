package versions

import (
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

func TestTheTableHoldsCompleteReleases(t *testing.T) {
	table := Embedded()
	if len(table.Versions()) == 0 {
		t.Fatal("the support table is empty")
	}
	for _, v := range table.Versions() {
		r := table.Release(v)
		f, err := modfile.Parse(v+"/dependencies.mod", r.GoMod, nil)
		if err != nil {
			t.Fatal(err)
		}
		required := map[string]string{}
		for _, req := range f.Require {
			required[req.Mod.Path] = req.Mod.Version
		}
		if got := required["go.opentelemetry.io/collector/otelcol"]; got != "v"+v {
			t.Errorf("%s: the set requires otelcol %s", v, got)
		}
		if r.Catalog.CollectorVersion != v || len(r.Catalog.Components) == 0 {
			t.Errorf("%s: catalog of %s with %d components", v, r.Catalog.CollectorVersion, len(r.Catalog.Components))
		}
		for _, c := range r.Catalog.Components {
			if c.Unsupported != "" {
				continue
			}
			mod, version, _ := strings.Cut(c.GoMod, " ")
			if required[mod] != version {
				t.Errorf("%s: %s %s is in the catalog, the set has %q", v, mod, version, required[mod])
			}
		}
		if len(r.GoSum) == 0 {
			t.Errorf("%s: empty go.sum", v)
		}
	}
}

func TestCheckRefusesOldReleases(t *testing.T) {
	table := NewTable(&Release{Version: "0.162.0"})
	for v, want := range map[string]Support{"0.162.0": Verified, "v0.162.0": Verified, "0.160.0": Unverified, "0.163.0": Unverified} {
		if got, err := table.Check(v); err != nil || got != want {
			t.Errorf("%s: %s, %v", v, got, err)
		}
	}
	for _, v := range []string{"0.159.0", "0.97.0", "latest", ""} {
		if _, err := table.Check(v); err == nil {
			t.Errorf("%s: accepted", v)
		} else if v == "0.159.0" && !strings.Contains(err.Error(), Oldest) {
			t.Errorf("%s: %v does not name %s", v, err, Oldest)
		}
	}
	if table.Newest() != "0.162.0" || NewTable().Newest() != "" {
		t.Fatal("newest")
	}
}
