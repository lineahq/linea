package platform

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// compressedRun runs one quick run in a workspace whose report carries the
// given analysis component name and snapshot.
func compressedRun(t *testing.T, data *memory.Data, limits artifact.Limits, component string) Run {
	t.Helper()
	s := openPlatformLimits(t, data, limits, Options{Prepare: unprepared, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		rep := report(req, runresult.StatusCompleted)
		rep.Result.Snapshots = instrument.Snapshots{"snapshot": []byte("{ \n \"resourceLogs\" : [] }")}
		rep.Stages = []analysis.Stage{{Component: component}}
		return rep, nil
	}})
	v := waitRun(t, submit(t, s, request("quick")))
	s.Close()
	return v
}

func storedEvidence(t *testing.T, data *memory.Data, id string) []byte {
	t.Helper()
	store := memory.Open(data, artifact.Limits{})
	defer store.Close()
	if err := store.EnableRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := store.RunEvidence(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestRunEvidenceIsStoredCompressed checks that the archive is stored as
// gzip, decodes to its declared length, and the report and snapshot bytes
// are unchanged after a restart.
func TestRunEvidenceIsStoredCompressed(t *testing.T) {
	data := memory.NewData()
	v := compressedRun(t, data, artifact.Limits{}, strings.Repeat("stage ", 1000))
	original := encodedReport(t, v.Report)

	stored := storedEvidence(t, data, v.ID)
	if len(stored) < 2 || stored[0] != 0x1f || stored[1] != 0x8b {
		t.Fatalf("archive is not gzip: %q", stored[:min(len(stored), 16)])
	}
	a, err := decodeEvidence(stored)
	if err != nil || a.Schema != archiveSchema {
		t.Fatalf("decode = %+v, %v", a, err)
	}
	if len(stored) > 6000/2 {
		t.Fatalf("stored %d bytes; the repeated analysis did not compress", len(stored))
	}

	s := openPlatform(t, data, Options{})
	h, err := s.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := h.View(true)
	if !bytes.Equal(encodedReport(t, got.Report), original) || len(got.Report.Stages) != 1 {
		t.Fatal("report changed through the compressed archive")
	}
	if string(got.Report.Result.Snapshots["snapshot"]) != "{ \n \"resourceLogs\" : [] }" {
		t.Fatal("snapshot bytes changed through the compressed archive")
	}
}

// TestSchemaOneArchivesStayReadable: an uncompressed schema-1 archive, as
// written before compression, opens with the same report.
func TestSchemaOneArchivesStayReadable(t *testing.T) {
	data := memory.NewData()
	v := compressedRun(t, data, artifact.Limits{}, "legacy")
	original := encodedReport(t, v.Report)

	store := memory.Open(data, artifact.Limits{})
	ctx := context.Background()
	if err := store.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	records, err := store.Runs(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("runs = %v, %v", records, err)
	}
	stored, err := store.RunEvidence(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := decodeEvidence(stored)
	if err != nil {
		t.Fatal(err)
	}
	a.Schema = 1
	legacy, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(records[0].Metadata, &m); err != nil {
		t.Fatal(err)
	}
	id := "run-00000000000000aa"
	m["id"] = id
	metadata, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveRun(ctx, id, artifact.DefaultProject, 0, true, artifact.RunIndex{}, metadata, legacy); err != nil {
		t.Fatal(err)
	}
	store.Close()

	s := openPlatform(t, data, Options{})
	h, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encodedReport(t, h.View(true).Report), original) {
		t.Fatal("schema-1 archive read differently")
	}
}

// TestCompressedArchiveBounds: the analysis is omitted when the compressed
// archive exceeds the run allowance or its decoded size exceeds the cap, and
// a run within both keeps it.
func TestCompressedArchiveBounds(t *testing.T) {
	limits := artifact.Limits{RunBytes: 64 << 10}
	random := make([]byte, 64<<10)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		component string
		omitted   bool
	}{
		{"compressible within the cap", strings.Repeat("x", 128<<10), false},
		{"stored size over the allowance", hex.EncodeToString(random), true},
		{"decoded size over the cap", strings.Repeat("x", decodedArchiveFactor*64<<10), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := memory.NewData()
			v := compressedRun(t, data, limits, tc.component)
			s := openPlatformLimits(t, data, limits, Options{})
			h, err := s.Get(v.ID)
			if err != nil {
				t.Fatal(err)
			}
			for when, got := range map[string]Run{"before restart": v, "after restart": h.View(true)} {
				if got.State != StateDone || got.Report == nil {
					t.Fatalf("%s: state %s, error %q", when, got.State, got.Error)
				}
				cut := got.Report.Result.Truncation
				if omitted := cut != nil && slices.Equal(cut.Omitted, omittedSections); omitted != tc.omitted || (len(got.Report.Stages) == 0) != tc.omitted {
					t.Fatalf("%s: omitted = %v (stages %d), want %v", when, omitted, len(got.Report.Stages), tc.omitted)
				}
			}
		})
	}
}

// gzipArchive compresses raw as an evidence archive would, declaring the
// given decoded length unless declare is false.
func gzipArchive(t *testing.T, raw []byte, declared uint64, declare bool) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if declare {
		w.Extra = append(archiveSizeField[:], 8, 0)
		w.Extra = binary.LittleEndian.AppendUint64(w.Extra, declared)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestCorruptCompressedArchivesAreRefused: a compressed archive must decode
// to exactly its declared length in one gzip member, and each schema keeps
// its encoding.
func TestCorruptCompressedArchivesAreRefused(t *testing.T) {
	good, _, err := encodeEvidence(nil, nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeEvidence(good); err != nil {
		t.Fatalf("good archive = %v", err)
	}
	if _, err := decodeEvidence(gzipArchive(t, []byte(`{"schema":2}`), 12, true)); err != nil {
		t.Fatalf("hand-built archive = %v", err)
	}
	v2 := []byte(`{"schema":2}`)
	v1 := []byte(`{"schema":1}`)
	for _, tc := range []struct {
		name        string
		raw         []byte
		unsupported bool
	}{
		{"truncated", good[:len(good)-4], false},
		{"trailing data", append(slices.Clone(good), good...), false},
		{"decodes past its declared length", gzipArchive(t, v2, 11, true), false},
		{"decodes short of its declared length", gzipArchive(t, v2, 13, true), false},
		{"declared length beyond deflate's ratio", gzipArchive(t, v2, 1<<40, true), false},
		{"no declared length", gzipArchive(t, v2, 0, false), false},
		{"uncompressed schema 2", v2, true},
		{"compressed schema 1", gzipArchive(t, v1, 12, true), true},
	} {
		_, err := decodeEvidence(tc.raw)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
		} else if errors.Is(err, artifact.ErrUnsupportedSchema) != tc.unsupported {
			t.Errorf("%s: %v, unsupported schema want %v", tc.name, err, tc.unsupported)
		}
	}
}
