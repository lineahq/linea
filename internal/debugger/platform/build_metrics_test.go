package platform

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lineahq/linea/internal/debugger/worker"
)

// waitBuildMetrics waits until the metrics are want.
func waitBuildMetrics(t *testing.T, s *Service, want BuildMetrics) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		got, err := s.BuildMetrics(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("build metrics = %+v, want %+v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBuildMetricsCountTheQueueAndBuilders: the metrics count the
// connected builders and their slots, a running build and one queued
// behind it, and nothing once both ended.
func TestBuildMetricsCountTheQueueAndBuilders(t *testing.T) {
	keep := buildCountsFor
	t.Cleanup(func() { buildCountsFor = keep })
	buildCountsFor = 0
	s, _, url := builderPlatform(t)
	waitBuildMetrics(t, s, BuildMetrics{})

	identity := "sha256:" + strings.Repeat("e", 64)
	started, release := make(chan struct{}), make(chan struct{})
	serveTestBuilds(t, url, func(ctx context.Context, job worker.BuildJob, log func(string)) worker.BuildResult {
		if job.Name == "slow" {
			close(started)
			<-release
		}
		return fakeBuild([]byte("binary "+job.Name), identity)(ctx, job, log)
	})
	waitBuildMetrics(t, s, BuildMetrics{Builders: 1, BuilderSlots: 1})

	slow, err := s.BuildRuntime(context.Background(), buildAlice, serverBuildRequest("slow", procArchive(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	edge, err := s.BuildRuntime(context.Background(), buildAlice, serverBuildRequest("edge", procArchive(t, "// edge\n")))
	if err != nil {
		t.Fatal(err)
	}
	waitBuildMetrics(t, s, BuildMetrics{Queued: 1, Running: 1, Builders: 1, BuilderSlots: 1})
	close(release)
	for _, id := range []string{slow.ID, edge.ID} {
		if b := waitServerBuild(t, s, buildAlice, id); b.State != BuildDone {
			t.Fatalf("build %s = %+v", id, b)
		}
	}
	waitBuildMetrics(t, s, BuildMetrics{Builders: 1, BuilderSlots: 1})
}

// TestBuildMetricsReuseRecentCounts: within buildCountsFor the queue's
// counts are not read again.
func TestBuildMetricsReuseRecentCounts(t *testing.T) {
	keep := buildCountsFor
	t.Cleanup(func() { buildCountsFor = keep })
	buildCountsFor = time.Hour
	s, _, _ := builderPlatform(t)
	waitBuildMetrics(t, s, BuildMetrics{})
	if _, err := s.BuildRuntime(context.Background(), buildAlice, serverBuildRequest("edge", procArchive(t, ""))); err != nil {
		t.Fatal(err)
	}
	waitBuildMetrics(t, s, BuildMetrics{})
	buildCountsFor = 0
	waitBuildMetrics(t, s, BuildMetrics{Queued: 1})
}

// TestBuildMetricsWithoutBuildsAreZero: a service that takes no builds
// reports zeros.
func TestBuildMetricsWithoutBuildsAreZero(t *testing.T) {
	s := New(Options{})
	t.Cleanup(s.Close)
	if m, err := s.BuildMetrics(context.Background()); err != nil || m != (BuildMetrics{}) {
		t.Fatalf("BuildMetrics = %+v, %v", m, err)
	}
}

// TestADrainingBuilderFinishesItsBuildAndClaimsNoMore: a
// builder that drains finishes the build it runs and stops; the build
// queued behind it stays queued for the next builder, which builds it.
func TestADrainingBuilderFinishesItsBuildAndClaimsNoMore(t *testing.T) {
	keep := buildCountsFor
	t.Cleanup(func() { buildCountsFor = keep })
	buildCountsFor = 0
	core, logs := observer.New(zap.InfoLevel)
	s, _, url := builderPlatformWith(t, Options{Logger: zap.New(core)})
	identity := "sha256:" + strings.Repeat("f", 64)
	started, release := make(chan struct{}), make(chan struct{})
	build := func(ctx context.Context, job worker.BuildJob, log func(string)) worker.BuildResult {
		if job.Name == "slow" {
			close(started)
			<-release
		}
		return fakeBuild([]byte("binary "+job.Name), identity)(ctx, job, log)
	}
	drain := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		served <- worker.ServeBuilds(context.Background(), worker.BuilderOptions{URL: url, Token: testBuildToken, Drain: drain,
			Hello: worker.BuilderHello{Name: "builder-a", Capacity: 1, OS: "linux", Arch: "amd64"}}, build)
	}()
	slow, err := s.BuildRuntime(context.Background(), buildAlice, serverBuildRequest("slow", procArchive(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	close(drain)
	edge, err := s.BuildRuntime(context.Background(), buildAlice, serverBuildRequest("edge", procArchive(t, "// edge\n")))
	if err != nil {
		t.Fatal(err)
	}
	// Release the build only once the server knows: a drain it has not
	// read yet would let it claim the queued build.
	for deadline := time.Now().Add(time.Minute); logs.FilterMessage("builder draining").Len() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the server did not see the drain")
		}
	}
	close(release)
	if b := waitServerBuild(t, s, buildAlice, slow.ID); b.State != BuildDone {
		t.Fatalf("the build in progress = %+v", b)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ServeBuilds = %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the drained builder did not stop")
	}
	waitBuildMetrics(t, s, BuildMetrics{Queued: 1})
	if b, err := s.GetRuntimeBuild(context.Background(), buildAlice, edge.ID, 0); err != nil || b.State != BuildQueued {
		t.Fatalf("the queued build = %+v, %v", b, err)
	}
	serveTestBuilds(t, url, build)
	if b := waitServerBuild(t, s, buildAlice, edge.ID); b.State != BuildDone {
		t.Fatalf("the queued build on the next builder = %+v", b)
	}
}
