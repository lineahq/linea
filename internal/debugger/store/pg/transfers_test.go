package pg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// memObjects is an in-memory ObjectStore for tests that need no containers.
type memObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	modTime map[string]time.Time
	deleted []string
}

func newMemObjects() *memObjects {
	return &memObjects{objects: map[string][]byte{}, modTime: map[string]time.Time{}}
}

func (m *memObjects) put(key string, data []byte, modified time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key], m.modTime[key] = data, modified
}

func (m *memObjects) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

func (m *memObjects) Put(_ context.Context, key string, data []byte) error {
	m.put(key, data, time.Now())
	return nil
}

func (m *memObjects) Get(_ context.Context, key string, limit int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.objects[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("object is longer than the limit")
	}
	return raw, nil
}

func (m *memObjects) Open(_ context.Context, key string) (ObjectReader, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.objects[key]
	if !ok {
		return ObjectReader{}, ErrObjectNotFound
	}
	return ObjectReader{Body: io.NopCloser(bytes.NewReader(raw)), Size: int64(len(raw)), ETag: digestOf(raw)}, nil
}

func (m *memObjects) Copy(_ context.Context, src, dst, ifMatch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.objects[src]
	switch {
	case !ok:
		return ErrObjectNotFound
	case ifMatch != "" && ifMatch != digestOf(raw):
		return ErrObjectChanged
	}
	m.objects[dst], m.modTime[dst] = raw, time.Now()
	return nil
}

func (m *memObjects) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	m.deleted = append(m.deleted, key)
	return nil
}

func (m *memObjects) List(_ context.Context, prefix string, fn func(ObjectInfo) error) error {
	m.mu.Lock()
	var infos []ObjectInfo
	for k, v := range m.objects {
		if strings.HasPrefix(k, prefix) {
			infos = append(infos, ObjectInfo{Key: k, Size: int64(len(v)), Modified: m.modTime[k]})
		}
	}
	m.mu.Unlock()
	for _, o := range infos {
		if err := fn(o); err != nil {
			return err
		}
	}
	return nil
}

func (m *memObjects) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://objects.test/get/" + key, nil
}

func (m *memObjects) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://objects.test/put/" + key, nil
}

// transferStore is a Store with no database: the staged-object operations
// touch object storage only.
func transferStore(prefix string) (*Store, *memObjects) {
	objects := newMemObjects()
	return &Store{objects: objects, prefix: prefix}, objects
}

func TestStagingKey(t *testing.T) {
	for _, c := range []struct {
		prefix, name, want string
		ok                 bool
	}{
		{"", "abc-123", "staging/abc-123", true},
		{"linea/team", "abc-123", "linea/team/staging/abc-123", true},
		{"p", strings.Repeat("a", 128), "p/staging/" + strings.Repeat("a", 128), true},
		{"p", strings.Repeat("a", 129), "", false},
		{"p", "", "", false},
		{"p", "Upper", "", false},
		{"p", "a/b", "", false},
		{"p", "..", "", false},
		{"p", "a_b", "", false},
		{"p", "a b", "", false},
	} {
		got, err := stagingKey(c.prefix, c.name)
		if c.ok != (err == nil) || got != c.want || err != nil && !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("stagingKey(%q, %q) = %q, %v; want %q (ok=%v)", c.prefix, c.name, got, err, c.want, c.ok)
		}
	}
	for _, prefix := range []string{"", "linea/team"} {
		if strings.HasPrefix(stagingPrefix(prefix), objectPrefix(prefix)) || strings.HasPrefix(objectPrefix(prefix), stagingPrefix(prefix)) {
			t.Errorf("staging and digest key spaces overlap for prefix %q", prefix)
		}
	}
}

func TestStageURL(t *testing.T) {
	s, _ := transferStore("p")
	u, err := s.StageURL(ctx, "job-1", time.Minute)
	if err != nil || u != "https://objects.test/put/p/staging/job-1" {
		t.Errorf("StageURL = %q, %v", u, err)
	}
	for name, run := range map[string]func() error{
		"bad name":      func() error { _, err := s.StageURL(ctx, "Job", time.Minute); return err },
		"zero lifetime": func() error { _, err := s.StageURL(ctx, "job-1", 0); return err },
	} {
		if err := run(); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("%s: err = %v; want ErrInvalidRequest", name, err)
		}
	}
	s.closed.Store(true)
	if _, err := s.StageURL(ctx, "job-1", time.Minute); !errors.Is(err, artifact.ErrClosed) {
		t.Errorf("closed store: err = %v", err)
	}
}

func TestTakeStaged(t *testing.T) {
	payload := []byte("staged result")
	good := digestOf(payload)
	for _, c := range []struct {
		name      string
		stored    []byte
		digest    string
		size      int64
		limit     int64
		want      error
		wantTaken bool // the object was read, so it must be gone
	}{
		{name: "match", stored: payload, digest: good, size: int64(len(payload)), limit: 100, wantTaken: true},
		{name: "empty object", stored: []byte{}, digest: digestOf(nil), size: 0, limit: 100, wantTaken: true},
		{name: "missing", digest: good, size: int64(len(payload)), limit: 100, want: artifact.ErrNotFound},
		{name: "size over limit", stored: payload, digest: good, size: int64(len(payload)), limit: 5, want: artifact.ErrInvalidRequest},
		{name: "negative size", stored: payload, digest: good, size: -1, limit: 100, want: artifact.ErrInvalidRequest},
		{name: "digest mismatch", stored: payload, digest: digestOf([]byte("other")), size: int64(len(payload)), limit: 100, want: artifact.ErrCorrupt, wantTaken: true},
		{name: "malformed digest", stored: payload, digest: "nope", size: int64(len(payload)), limit: 100, want: artifact.ErrCorrupt, wantTaken: true},
		{name: "stored shorter", stored: payload[:4], digest: good, size: int64(len(payload)), limit: 100, want: artifact.ErrCorrupt, wantTaken: true},
		{name: "stored longer", stored: append(append([]byte{}, payload...), 'x'), digest: good, size: int64(len(payload)), limit: 100, want: artifact.ErrCorrupt, wantTaken: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, objects := transferStore("p")
			key := "p/staging/job-1"
			if c.stored != nil {
				objects.put(key, c.stored, time.Now())
			}
			got, err := s.TakeStaged(ctx, "job-1", c.digest, c.size, c.limit)
			if c.want == nil && (err != nil || string(got) != string(payload[:c.size])) || c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("TakeStaged = %q, %v; want error %v", got, err, c.want)
			}
			if c.want != nil && got != nil {
				t.Errorf("TakeStaged returned %q with %v", got, err)
			}
			if c.wantTaken && objects.has(key) {
				t.Error("staged object survived being taken")
			}
			if !c.wantTaken && c.stored != nil && !objects.has(key) {
				t.Error("a refused request deleted the staged object")
			}
		})
	}
	t.Run("invalid name", func(t *testing.T) {
		s, _ := transferStore("p")
		if _, err := s.TakeStaged(ctx, "../x", good, 1, 10); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("err = %v; want ErrInvalidRequest", err)
		}
	})
}

// TestSweepDeletesOldStagedObjects checks Sweep's staged pass, which needs no
// database when no digest object is a candidate.
func TestSweepDeletesOldStagedObjects(t *testing.T) {
	s, objects := transferStore("p")
	now := time.Now()
	objects.put("p/staging/old", []byte("a"), now.Add(-2*time.Hour))
	objects.put("p/staging/fresh", []byte("b"), now.Add(-time.Minute))
	// Another installation's staged object and a digest object are not touched.
	objects.put("q/staging/old", []byte("c"), now.Add(-2*time.Hour))
	n, err := s.Sweep(ctx, 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want 1", n, err)
	}
	if objects.has("p/staging/old") || !objects.has("p/staging/fresh") || !objects.has("q/staging/old") {
		t.Errorf("objects after sweep = %v", objects.objects)
	}
}
