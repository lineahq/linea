package platform

import (
	"context"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"reflect"
	"strings"
	"testing"
)

func TestCaptureMetadataUpdatesPreserveEvidenceAndRecover(t *testing.T) {
	ctx := context.Background()
	f, data := dataFixture(t, true, false)
	session := dispatchData(t, f)
	ingestLog(t, f, session.ID, "immutable")
	before, err := f.s.GetCaptureSession(ctx, testCaller, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	records, err := f.s.GetCaptureRecords(ctx, testCaller, session.ID, CaptureRecordsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.s.UpdateCaptureMetadata(ctx, testCaller, session.ID, CaptureMetadataUpdate{Revision: 1, Name: "Investigate checkout", Description: "Observed a failure\nKeep evidence"})
	if err != nil || next.Metadata.Revision != 2 {
		t.Fatalf("%+v %v", next, err)
	}
	after, err := f.s.GetCaptureRecords(ctx, testCaller, session.ID, CaptureRecordsRequest{})
	if err != nil || !reflect.DeepEqual(records, after) {
		t.Fatal("payload changed", err)
	}
	copy := *next
	copy.Metadata = before.Metadata
	if !reflect.DeepEqual(before, &copy) {
		t.Fatal("metadata update changed immutable session fields")
	}
	if _, err = f.s.UpdateCaptureMetadata(ctx, testCaller, session.ID, CaptureMetadataUpdate{Revision: 1, Name: "stale"}); ErrorCode(err) != CodeConflict {
		t.Fatal("lost update", err)
	}
	if _, err = f.s.UpdateCaptureMetadata(ctx, Caller{Principal: Principal{ID: "other", Kind: PrincipalUser}, Client: "http"}, session.ID, CaptureMetadataUpdate{Revision: 2}); ErrorCode(err) != CodeForbidden {
		t.Fatal("owner bypass", err)
	}
	f.s.Close()
	recovered, err := openMemory(data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	got, err := recovered.GetCaptureSession(ctx, testCaller, session.ID)
	if err != nil || got.Metadata.Name != "Investigate checkout" || got.Metadata.Revision != 2 {
		t.Fatalf("recovery %+v %v", got, err)
	}
}

func TestCaptureMetadataBounds(t *testing.T) {
	s, err := openMemory(memory.NewData(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.UpdateCaptureMetadata(context.Background(), testCaller, "missing", CaptureMetadataUpdate{Revision: 1, Description: strings.Repeat("x", 2049)}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("unbounded description", err)
	}
}

func TestCaptureMetadataConcurrentUpdates(t *testing.T) {
	f := newCaptureFixture(t)
	v := f.start(t)
	results := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func(name string) {
			_, err := f.s.UpdateCaptureMetadata(context.Background(), testCaller, v.ID, CaptureMetadataUpdate{Revision: 1, Name: name})
			results <- err
		}(name)
	}
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if ErrorCode(err) == CodeConflict {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success %d conflict %d", success, conflict)
	}
}
