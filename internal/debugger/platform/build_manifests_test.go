package platform

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// manifestBuilds serves manifests by build ID and counts the reads of each.
type manifestBuilds struct {
	artifact.Builds
	manifests map[string][]byte
	err       error
	reads     map[string]int
}

func (b *manifestBuilds) BuildManifest(_ context.Context, id string) ([]byte, error) {
	if b.reads == nil {
		b.reads = map[string]int{}
	}
	b.reads[id]++
	if b.err != nil {
		return nil, b.err
	}
	m, ok := b.manifests[id]
	if !ok {
		return nil, artifact.ErrNotFound
	}
	return m, nil
}

func TestManifestCacheReadsEachBuildOnce(t *testing.T) {
	const big = maxCachedManifestBytes * 3 / 10 // three fit, four do not
	manifest := func(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

	tests := []struct {
		name string
		// ids are fetched in order; reads is then the expected read count
		// per build.
		manifests map[string][]byte
		ids       []string
		reads     map[string]int
	}{
		{
			name:      "repeated gets read once",
			manifests: map[string][]byte{"a": manifest('a', 10)},
			ids:       []string{"a", "a", "a"},
			reads:     map[string]int{"a": 1},
		},
		{
			name:      "distinct builds are cached apart",
			manifests: map[string][]byte{"a": manifest('a', 10), "b": manifest('b', 10)},
			ids:       []string{"a", "b", "a", "b"},
			reads:     map[string]int{"a": 1, "b": 1},
		},
		{
			name: "the oldest is evicted once the bytes bound is exceeded",
			manifests: map[string][]byte{
				"a": manifest('a', big), "b": manifest('b', big),
				"c": manifest('c', big), "d": manifest('d', big),
			},
			// d evicts a only; b and c stay cached, a is read again.
			ids:   []string{"a", "b", "c", "d", "b", "c", "a"},
			reads: map[string]int{"a": 2, "b": 1, "c": 1, "d": 1},
		},
		{
			name: "an over-bound manifest is returned but not kept",
			manifests: map[string][]byte{
				"huge": manifest('h', maxCachedManifestBytes+1),
				"a":    manifest('a', 10),
			},
			ids:   []string{"a", "huge", "huge", "a"},
			reads: map[string]int{"a": 1, "huge": 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builds := &manifestBuilds{manifests: tt.manifests}
			var c manifestCache
			for _, id := range tt.ids {
				got, err := c.get(t.Context(), builds, id)
				if err != nil {
					t.Fatalf("get(%q): %v", id, err)
				}
				if !bytes.Equal(got, tt.manifests[id]) {
					t.Fatalf("get(%q) returned %d bytes, want %d", id, len(got), len(tt.manifests[id]))
				}
			}
			for id, want := range tt.reads {
				if got := builds.reads[id]; got != want {
					t.Errorf("reads of %q = %d, want %d", id, got, want)
				}
			}
			if c.bytes > maxCachedManifestBytes {
				t.Errorf("cache holds %d bytes, bound is %d", c.bytes, maxCachedManifestBytes)
			}
		})
	}

	t.Run("errors are not cached", func(t *testing.T) {
		boom := errors.New("storage unavailable")
		builds := &manifestBuilds{manifests: map[string][]byte{"a": manifest('a', 10)}, err: boom}
		var c manifestCache
		if _, err := c.get(t.Context(), builds, "a"); !errors.Is(err, boom) {
			t.Fatalf("get error = %v, want %v", err, boom)
		}
		builds.err = nil
		got, err := c.get(t.Context(), builds, "a")
		if err != nil || !bytes.Equal(got, builds.manifests["a"]) {
			t.Fatalf("get after recovery = %d bytes, %v", len(got), err)
		}
		if builds.reads["a"] != 2 {
			t.Errorf("reads = %d, want 2", builds.reads["a"])
		}
	})
}

func TestNewBuildIDHasItsOwnPrefix(t *testing.T) {
	re := regexp.MustCompile(`^build-[0-9a-f]{16}$`)
	first, second := newBuildID(), newBuildID()
	for _, id := range []string{first, second} {
		if !re.MatchString(id) {
			t.Errorf("newBuildID() = %q, want build- and 16 lowercase hex digits", id)
		}
	}
	if first == second {
		t.Errorf("newBuildID() returned %q twice", first)
	}
}
