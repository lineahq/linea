package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// connectedBuilder serves builds with build from a builder in this process
// that dials a test server with the build token, and returns the server's
// connection to it.
func connectedBuilder(t *testing.T, capacity int, build BuildFunc) *BuilderConn {
	t.Helper()
	accepted := make(chan *BuilderConn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != BuilderPath {
			http.NotFound(w, r)
			return
		}
		b, err := AcceptBuilder(w, r, func(token string) bool { return ConstantTimeEqual(token, testToken) })
		if err != nil {
			t.Errorf("AcceptBuilder = %v", err)
			return
		}
		accepted <- b
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeBuilds(ctx, BuilderOptions{URL: srv.URL, Token: testToken,
			Hello: BuilderHello{Name: "builder-a", Capacity: capacity, OS: "linux", Arch: "amd64", GoVersion: "go1.27.2"}}, build)
	}()
	var b *BuilderConn
	select {
	case b = <-accepted:
	case err := <-done:
		t.Fatalf("ServeBuilds = %v before it connected", err)
	case <-time.After(time.Minute):
		t.Fatal("the builder did not connect")
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("ServeBuilds = %v", err)
		}
		_ = b.Close()
	})
	return b
}

// TestABuilderRunsBuildsItIsGiven: the builder's hello reaches the server,
// a build's log lines arrive before its result, and a build beyond the
// builder's capacity fails at once.
func TestABuilderRunsBuildsItIsGiven(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	b := connectedBuilder(t, 1, func(ctx context.Context, job BuildJob, log func(string)) BuildResult {
		log("building " + job.Name)
		if job.Name == "slow" {
			close(started)
			<-release
		}
		return BuildResult{BinaryDigest: "sha256:" + strings.Repeat("a", 64), BinarySize: 42, OS: "linux", Arch: "amd64", Hello: &Hello{Runtime: job.Build}}
	})
	if h := b.Hello(); h.Name != "builder-a" || h.Capacity != 1 || h.Protocol != BuilderProtocol || h.OS != "linux" || h.GoVersion != "go1.27.2" {
		t.Fatalf("hello = %+v", h)
	}
	var lines []string
	r, err := b.Build(context.Background(), BuildJob{Build: "build-1", Attempt: 1, Name: "edge"}, func(l []string) { lines = append(lines, l...) })
	if err != nil || r.Error != "" || r.BinarySize != 42 || r.Hello == nil || r.Hello.Runtime != "build-1" {
		t.Fatalf("Build = %+v, %v", r, err)
	}
	if len(lines) != 1 || lines[0] != "building edge" {
		t.Fatalf("log = %q", lines)
	}

	slow := make(chan BuildResult, 1)
	go func() {
		r, _ := b.Build(context.Background(), BuildJob{Build: "build-2", Name: "slow"}, nil)
		slow <- r
	}()
	// Probe only once the slow build holds the one slot: an earlier probe
	// could take the slot and turn the slow build away instead.
	<-started
	r, err = b.Build(context.Background(), BuildJob{Build: "build-3", Name: "extra"}, nil)
	if err != nil || !strings.Contains(r.Error, "capacity") {
		t.Fatalf("a build beyond capacity = %+v, %v", r, err)
	}
	close(release)
	if r := <-slow; r.Error != "" {
		t.Fatalf("slow build = %+v", r)
	}
}

// TestACancelledBuildStillAnswers: cancelling a build's context reaches
// the builder, which stops the build and reports it cancelled.
func TestACancelledBuildStillAnswers(t *testing.T) {
	started := make(chan struct{})
	b := connectedBuilder(t, 1, func(ctx context.Context, job BuildJob, log func(string)) BuildResult {
		close(started)
		<-ctx.Done()
		return BuildResult{Error: "stopped: " + context.Cause(ctx).Error()}
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	r, err := b.Build(ctx, BuildJob{Build: "build-1", Name: "edge"}, nil)
	if err != nil || !r.Cancelled || r.Error == "" {
		t.Fatalf("Build = %+v, %v", r, err)
	}
}

// TestABuildFailsWhenItsBuilderGoes: a build waiting on a builder whose
// connection ends fails with an error, and later builds fail at once.
func TestABuildFailsWhenItsBuilderGoes(t *testing.T) {
	started := make(chan struct{})
	b := connectedBuilder(t, 1, func(ctx context.Context, job BuildJob, log func(string)) BuildResult {
		close(started)
		<-ctx.Done()
		return BuildResult{Error: "stopped"}
	})
	go func() {
		<-started
		_ = b.conn.Close()
	}()
	if _, err := b.Build(context.Background(), BuildJob{Build: "build-1", Name: "edge"}, nil); err == nil {
		t.Fatal("Build on a lost builder succeeded")
	}
	<-b.Done()
	if _, err := b.Build(context.Background(), BuildJob{Build: "build-2", Name: "edge"}, nil); err == nil {
		t.Fatal("Build after the connection ended succeeded")
	}
}

// TestAcceptBuilderRefusesOtherBuilders: a wrong token, a worker's hello
// and a builder of another product version are refused.
func TestAcceptBuilderRefusesOtherBuilders(t *testing.T) {
	authorize := func(token string) bool { return ConstantTimeEqual(token, testToken) }
	refused := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := AcceptBuilder(w, r, authorize)
		if err == nil {
			_ = b.Close()
		}
		refused <- err
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + BuilderPath

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := ServeBuilds(ctx, BuilderOptions{URL: srv.URL, Token: "wrong", Hello: BuilderHello{Name: "b", Capacity: 1}}, nil)
	if !errors.Is(err, ErrUnauthorized) || !errors.Is(<-refused, ErrUnauthorized) {
		t.Fatalf("ServeBuilds with a wrong token = %v", err)
	}

	for _, tc := range []struct {
		name string
		m    *message
		want error
	}{
		{"a worker", &message{Type: typeHello, Hello: &Hello{Protocol: ProtocolVersion}}, errFrame},
		{"another version", &message{Type: typeHello, BuilderHello: &BuilderHello{Protocol: BuilderProtocol, ProductVersion: "0.0.0-other", Name: "b", Capacity: 1}}, ErrBuilderVersion},
		{"another protocol", &message{Type: typeHello, BuilderHello: &BuilderHello{Protocol: BuilderProtocol + 1, Name: "b", Capacity: 1}}, ErrProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + testToken}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			s := newStream(conn)
			if err := writeFrame(s, tc.m); err != nil {
				t.Fatal(err)
			}
			if err := <-refused; !errors.Is(err, tc.want) {
				t.Fatalf("AcceptBuilder = %v, want %v", err, tc.want)
			}
			_, err = readFrame(s)
			var closed *websocket.CloseError
			if !errors.As(err, &closed) || closed.Code != websocket.ClosePolicyViolation {
				t.Fatalf("the builder read %v, want a policy close", err)
			}
		})
	}
}

func TestServeBuildsOptionsAreChecked(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts BuilderOptions
		want string
	}{
		{"no token", BuilderOptions{URL: "http://server", Hello: BuilderHello{Name: "n", Capacity: 1}}, "token"},
		{"no name", BuilderOptions{URL: "http://server", Token: "t", Hello: BuilderHello{Capacity: 1}}, "name"},
		{"no capacity", BuilderOptions{URL: "http://server", Token: "t", Hello: BuilderHello{Name: "n"}}, "capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ServeBuilds(context.Background(), tc.opts, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ServeBuilds = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// TestADrainingBuilderFinishesItsBuildsAndStops: a drain reaches the
// server, a build in progress and one the server sent before it read the
// drain both finish, and once the server answers the drain the builder
// returns without being cancelled and does not dial again.
func TestADrainingBuilderFinishesItsBuildsAndStops(t *testing.T) {
	accepted := make(chan *BuilderConn, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := AcceptBuilder(w, r, func(token string) bool { return ConstantTimeEqual(token, testToken) })
		if err != nil {
			t.Errorf("AcceptBuilder = %v", err)
			return
		}
		accepted <- b
		<-b.Done()
	}))
	t.Cleanup(srv.Close)
	started, release := make(chan struct{}), make(chan struct{})
	drain := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeBuilds(context.Background(), BuilderOptions{URL: srv.URL, Token: testToken, Drain: drain,
			Hello: BuilderHello{Name: "builder-a", Capacity: 2, OS: "linux", Arch: "amd64"}},
			func(ctx context.Context, job BuildJob, log func(string)) BuildResult {
				if job.Name == "slow" {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return BuildResult{Error: "cancelled"}
					}
				}
				return BuildResult{BinarySize: 1}
			})
	}()
	var b *BuilderConn
	select {
	case b = <-accepted:
	case <-time.After(time.Minute):
		t.Fatal("the builder did not connect")
	}
	slow := make(chan BuildResult, 1)
	go func() {
		r, _ := b.Build(context.Background(), BuildJob{Build: "build-1", Name: "slow"}, nil)
		slow <- r
	}()
	<-started
	close(drain)
	select {
	case <-b.Draining():
	case <-time.After(time.Minute):
		t.Fatal("the drain did not reach the server")
	}
	if r, err := b.Build(context.Background(), BuildJob{Build: "build-2", Name: "late"}, nil); err != nil || r.Error != "" {
		t.Fatalf("a build sent while draining = %+v, %v", r, err)
	}
	close(release)
	if r := <-slow; r.Error != "" {
		t.Fatalf("the build in progress = %+v", r)
	}
	if err := b.Drained(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeBuilds = %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the drained builder did not return")
	}
	select {
	case <-accepted:
		t.Fatal("the drained builder dialed again")
	default:
	}
}

// TestADrainWithoutAConnectionStops: a builder that cannot reach its
// server returns once drained instead of dialing again.
func TestADrainWithoutAConnectionStops(t *testing.T) {
	drain := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeBuilds(context.Background(), BuilderOptions{URL: "http://127.0.0.1:1", Token: testToken, Drain: drain,
			Hello: BuilderHello{Name: "builder-a", Capacity: 1}}, func(context.Context, BuildJob, func(string)) BuildResult { return BuildResult{} })
	}()
	close(drain)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeBuilds = %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the drained builder kept dialing")
	}
}
