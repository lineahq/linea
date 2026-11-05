package server

import (
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"1048576": 1 << 20,
		"512KiB":  512 << 10,
		"16MiB":   16 << 20,
		"16mib":   16 << 20,
		"1GiB":    1 << 30,
		"2TiB":    2 << 40,
		"4096B":   4096,
	} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-1MiB", "1.5GiB", "16MB", "MiB", "16 MiB", "9999999999TiB"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d, want an error", in, got)
		}
	}
	for n, want := range map[int64]string{512: "512 B", 2 << 20: "2 MiB", 1536 << 10: "1.5 MiB", 256 << 20: "256 MiB", 3 << 30: "3 GiB"} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestServeDefaultLimitsAreThePlatforms: the server config's stated defaults are the
// limits the in-memory store opens with when none are given.
func TestServeDefaultLimitsAreThePlatforms(t *testing.T) {
	store := memory.New(artifact.Limits{})
	defer store.Close()
	if l := store.Limits(); l.StorageBytes != defaultStorageLimit || l.RunBytes != defaultRunLimit {
		t.Fatalf("store defaults = %d/%d, serve states %d/%d", l.StorageBytes, l.RunBytes, defaultStorageLimit, defaultRunLimit)
	}
}

// TestServerConfigStorageLimits checks that a run limit is at least 2 MiB and at
// most the storage limit, including the defaults of a limit not given.
func TestServerConfigStorageLimits(t *testing.T) {
	for _, tc := range []struct {
		limits, want string
	}{
		{"storage_limit: 64MiB\n  run_limit: 1MiB", "run_limit 1 MiB is below the minimum 2 MiB"},
		{"storage_limit: 64MiB\n  run_limit: 65MiB", "run_limit 65 MiB exceeds storage_limit 64 MiB"},
		{"storage_limit: 8MiB", "run_limit 16 MiB exceeds storage_limit 8 MiB"},
		{"run_limit: 512MiB", "run_limit 512 MiB exceeds storage_limit 256 MiB"},
		{"storage_limit: lots", "store.storage_limit: invalid size"},
	} {
		_, err := loadConfig(writeTeam(t, strings.Replace(teamYAML(), "storage_limit: 64MiB", tc.limits, 1)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v; want %q", tc.limits, err, tc.want)
		}
	}
	tm, err := loadConfig(writeTeam(t, strings.Replace(teamYAML(), "storage_limit: 64MiB", "storage_limit: 64MiB\n  run_limit: 4MiB", 1)))
	if err != nil || tm.limits.storage != 64<<20 || tm.limits.run != 4<<20 || tm.store.Limits.RunBytes != 4<<20 {
		t.Fatalf("limits = %+v, %v", tm, err)
	}
	// Each build reserves an eighth of the storage limit, at most 128 MiB.
	if got := tm.store.Limits.BuildBytes; got != 8<<20 {
		t.Errorf("build reservation = %d; want 8 MiB", got)
	}
	tm, err = loadConfig(writeTeam(t, strings.Replace(teamYAML(), "storage_limit: 64MiB", "storage_limit: 4GiB", 1)))
	if err != nil || tm.store.Limits.BuildBytes != artifact.BuildAllowance {
		t.Errorf("build reservation at 4GiB = %+v, %v; want %d", tm.store.Limits, err, artifact.BuildAllowance)
	}
}
