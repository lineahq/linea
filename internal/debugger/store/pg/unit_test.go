package pg

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestObjectKey(t *testing.T) {
	d := digestOf([]byte("x"))
	hexSum := strings.TrimPrefix(d, "sha256:")
	for _, c := range []struct{ prefix, want string }{
		{"", "sha256/" + hexSum},
		{"linea/team", "linea/team/sha256/" + hexSum},
	} {
		if got, err := objectKey(c.prefix, d); err != nil || got != c.want {
			t.Errorf("objectKey(%q) = %q, %v; want %q", c.prefix, got, err, c.want)
		}
		if got := objectPrefix(c.prefix); !strings.HasPrefix(c.want, got) {
			t.Errorf("objectPrefix(%q) = %q is not a prefix of %q", c.prefix, got, c.want)
		}
	}
	for _, bad := range []string{"", "sha256:", "sha256:ABC", "md5:" + hexSum, d + "0"} {
		if _, err := objectKey("p", bad); err == nil {
			t.Errorf("digest %q was accepted", bad)
		}
	}
	if cleanPrefix("/a/b/") != "a/b" {
		t.Error("prefix not trimmed")
	}
}

func TestEmbeddedMigrationsAreNumbered(t *testing.T) {
	ms, err := loadMigrations(migrationFiles)
	if err != nil || len(ms) == 0 {
		t.Fatalf("migrations = %d, %v", len(ms), err)
	}
	for i, m := range ms {
		if m.version != i+1 || len(m.checksum) != 64 {
			t.Errorf("migration %+v", m)
		}
	}
	if _, err := loadMigrations(fstest.MapFS{"migrations/1_bad.sql": {Data: []byte("SELECT 1")}}); err == nil {
		t.Error("badly named migration accepted")
	}
}

func TestIDFormats(t *testing.T) {
	for id, valid := range map[string]bool{"run-0123456789abcdef": true, "run-0123456789ABCDEF": false, "run-01": false, "suite-0123456789abcdef": false} {
		if validRunID(id) != valid {
			t.Errorf("validRunID(%q) = %v", id, !valid)
		}
	}
	if !validSuiteID("suite-0123456789abcdef") || !validEvaluationID("eval-0123456789abcdef") || validID("xyz") || !validID(newID()) {
		t.Error("id validators disagree")
	}
}
