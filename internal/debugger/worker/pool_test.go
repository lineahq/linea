package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/engine"
)

// The pool worker's tests run a real pool against a real server connection
// (Accept), with a tiny fake runtime binary instead of a Collector build:
// testdata/fakechild speaks the worker frame protocol, takes its runtime
// identity from its variant number and does what the request
// names ("echo" by default, "slow", "crash", "env", "report", "fail").
// Variants of it are linked with a number, so each has its own digest. Idle
// timers run on a fake clock.

const waitLimit = time.Minute

var fakeChildren struct {
	mu   sync.Mutex
	dir  string
	bins map[int][]byte
}

// variant builds the fake runtime with its own digest, once per test
// process: a distinct binary, linked with the variant's number.
func variant(t *testing.T, i int) []byte {
	t.Helper()
	fakeChildren.mu.Lock()
	defer fakeChildren.mu.Unlock()
	if b, ok := fakeChildren.bins[i]; ok {
		return slices.Clone(b)
	}
	if fakeChildren.bins == nil {
		dir, err := os.MkdirTemp("", "fakechild")
		if err != nil {
			t.Fatal(err)
		}
		fakeChildren.dir, fakeChildren.bins = dir, map[int][]byte{}
	}
	out := variantPath(i)
	flags := fmt.Sprintf("-X main.variant=%d", i)
	if b, err := exec.Command("go", "build", "-ldflags", flags, "-o", out, "./testdata/fakechild").CombinedOutput(); err != nil {
		t.Fatalf("build the fake runtime: %v: %s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	fakeChildren.bins[i] = data
	return slices.Clone(data)
}

// variantPath is where variant builds fake variant i.
func variantPath(i int) string {
	return filepath.Join(fakeChildren.dir, fmt.Sprintf("fakechild%d%s", i, binarySuffix))
}

// fakeIdentity is the runtime identity variant i announces.
func fakeIdentity(i int) string { return Digest([]byte(fmt.Sprintf("fake runtime %d", i))) }

func binaryFor(i int, data []byte, url string) *PoolBinary {
	return &PoolBinary{Identity: fakeIdentity(i), URL: url, Digest: Digest(data), Size: int64(len(data))}
}

// binaryServer serves runtime binaries and counts the fetches.
type binaryServer struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	files   map[string][]byte
	fetches map[string]int
	gate    chan struct{} // when set, a fetch waits for it to close
	entered chan struct{}
}

func newBinaryServer(t *testing.T) *binaryServer {
	b := &binaryServer{t: t, files: map[string][]byte{}, fetches: map[string]int{}, entered: make(chan struct{}, 64)}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		data, ok := b.files[r.URL.Path]
		b.fetches[r.URL.Path]++
		gate := b.gate
		b.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		b.entered <- struct{}{}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// add serves variant i of the fake runtime and returns the binary a request
// names for it.
func (b *binaryServer) add(name string, i int) *PoolBinary {
	data := variant(b.t, i)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.files["/"+name] = data
	return binaryFor(i, data, b.srv.URL+"/"+name)
}

func (b *binaryServer) fetched(name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fetches["/"+name]
}

func (b *binaryServer) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.fetches {
		n += c
	}
	return n
}

// fakeClock is the pool's clock: timers fire when a test says so.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*fakeTimer
	armed  chan struct{}
}

type fakeTimer struct {
	f     func()
	d     time.Duration
	done  bool // stopped or fired
	fired bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), armed: make(chan struct{}, 64)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) afterFunc(d time.Duration, f func()) func() bool {
	t := &fakeTimer{f: f, d: d}
	c.mu.Lock()
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.done
		t.done = true
		return was
	}
}

// waitArmed waits for a timer to be set.
func (c *fakeClock) waitArmed(t *testing.T) {
	t.Helper()
	select {
	case <-c.armed:
	case <-time.After(waitLimit):
		t.Fatal("no idle timer was set")
	}
}

// fireStale runs every timer that has not fired, stopped or not: a timer
// callback that was already running when its timer was stopped.
func (c *fakeClock) fireStale() {
	c.mu.Lock()
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.fired {
			t.fired = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

// fireAll runs the timers still pending.
func (c *fakeClock) fireAll() (fired int) {
	c.mu.Lock()
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.done {
			t.done, t.fired = true, true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
	return len(due)
}

// syncBuffer is a log whose readers can wait for a line.
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{changed: make(chan struct{})} }

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	close(b.changed)
	b.changed = make(chan struct{})
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor waits until s appears at least n times.
func (b *syncBuffer) waitFor(t *testing.T, s string, n int) {
	t.Helper()
	deadline := time.After(waitLimit)
	for {
		b.mu.Lock()
		count, ch := strings.Count(b.buf.String(), s), b.changed
		b.mu.Unlock()
		if count >= n {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("the log never had %d x %q:\n%s", n, s, b.String())
		}
	}
}

const noWarm = -1 // rigOptions.warm for a pool that keeps no idle child

type rigOptions struct {
	capacity   int // 4 when zero
	warm       int // 8 when zero, noWarm for none
	idle       time.Duration
	cacheBytes int64
	cacheDir   string
	// own, when set, makes fake variant own-1 the pool's own build;
	// otherwise the pool cannot run itself (it is the test binary).
	own int
	// ownPath, when set, replaces the own build's path.
	ownPath string
}

// poolRig is a pool worker connected to a test server.
type poolRig struct {
	t        *testing.T
	pool     *pool
	client   *Client
	bins     *binaryServer
	clock    *fakeClock
	logs     *syncBuffer
	cacheDir string
	clients  chan *Client
	sessions chan *poolSession
	done     chan error
}

func newRig(t *testing.T, o rigOptions) *poolRig {
	t.Helper()
	if o.capacity == 0 {
		o.capacity = 4
	}
	switch o.warm {
	case 0:
		o.warm = 8
	case noWarm:
		o.warm = 0
	}
	if o.cacheDir == "" {
		o.cacheDir = t.TempDir()
	}
	r := &poolRig{t: t, bins: newBinaryServer(t), clock: newFakeClock(), logs: newSyncBuffer(), cacheDir: o.cacheDir,
		clients: make(chan *Client, 8), sessions: make(chan *poolSession, 8), done: make(chan error, 1)}
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := Accept(w, req, func(token string) bool { return ConstantTimeEqual(token, testToken) })
		if err != nil {
			t.Errorf("Accept = %v", err)
			return
		}
		r.clients <- c
	}))
	t.Cleanup(ws.Close)
	p, err := newPool(PoolOptions{URL: ws.URL, Token: testToken, Capacity: o.capacity, Warm: o.warm, Idle: o.idle,
		CacheDir: o.cacheDir, CacheBytes: o.cacheBytes, Log: r.logs})
	if err != nil {
		t.Fatal(err)
	}
	p.now, p.afterFunc = r.clock.Now, r.clock.afterFunc
	p.ownPath = ""
	if o.own > 0 {
		variant(t, o.own-1)
		p.ownPath, p.ownIdentity = variantPath(o.own-1), fakeIdentity(o.own-1)
		if o.ownPath != "" {
			p.ownPath = o.ownPath
		}
	}
	p.onSession = func(s *poolSession) { r.sessions <- s }
	r.pool = p
	oldMin, oldStop := reconnectMin, poolStopGrace
	reconnectMin, poolStopGrace = time.Millisecond, 5*time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() { r.done <- p.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-r.done; err != nil {
			t.Errorf("run = %v", err)
		}
		reconnectMin, poolStopGrace = oldMin, oldStop
		for {
			select {
			case c := <-r.clients:
				_ = c.Close()
			default:
				_ = r.client.Close()
				return
			}
		}
	})
	r.client = r.nextClient()
	return r
}

// nextClient waits for the pool to connect (again).
func (r *poolRig) nextClient() *Client {
	r.t.Helper()
	select {
	case c := <-r.clients:
		r.client = c
		return c
	case err := <-r.done:
		r.t.Fatalf("the pool ended with %v", err)
	case <-time.After(waitLimit):
		r.t.Fatal("the pool did not connect")
	}
	return nil
}

// waitWarm waits for a pool_state that satisfies ok.
func (r *poolRig) waitWarm(ok func([]string) bool) []string {
	r.t.Helper()
	deadline := time.After(waitLimit)
	for {
		warm, changed := r.client.Warm()
		if ok(warm) {
			return warm
		}
		select {
		case <-changed:
		case <-deadline:
			r.t.Fatalf("the pool never reported the expected runtimes; last %v\n%s", warm, r.logs.String())
		}
	}
}

func warmIs(want ...string) func([]string) bool {
	slices.Sort(want)
	return func(got []string) bool { return slices.Equal(got, want) }
}

type callResult struct {
	events []string
	res    *message
	err    error // the exchange's, or the result's decoded error
}

// call sends a request to the pool.
func (r *poolRig) call(ctx context.Context, typ string, bin *PoolBinary, what string, mutate ...func(*message)) callResult {
	started, done := r.callAsync(ctx, typ, bin, what, mutate...)
	_ = started
	return <-done
}

// callAsync sends a request; started is signalled by its first event.
func (r *poolRig) callAsync(ctx context.Context, typ string, bin *PoolBinary, what string, mutate ...func(*message)) (<-chan struct{}, <-chan callResult) {
	m := &message{Type: typ, Binary: bin}
	if typ == typeExecute {
		m.Bundle = []byte(what)
	} else {
		m.Input = json.RawMessage(fmt.Sprintf("%q", what))
	}
	for _, f := range mutate {
		f(m)
	}
	started := make(chan struct{}, 1)
	done := make(chan callResult, 1)
	go func() {
		var mu sync.Mutex
		var out callResult
		res, err := r.client.exchange(ctx, m, func(raw json.RawMessage) {
			mu.Lock()
			out.events = append(out.events, string(raw))
			mu.Unlock()
			select {
			case started <- struct{}{}:
			default:
			}
		}, nil)
		mu.Lock()
		defer mu.Unlock()
		out.res, out.err = res, err
		if err == nil {
			out.err = decodeError(res.Error)
		}
		done <- out
	}()
	return started, done
}

func (r *poolRig) wait(started <-chan struct{}) {
	r.t.Helper()
	select {
	case <-started:
	case <-time.After(waitLimit):
		r.t.Fatal("the request never started")
	}
}

func resultFields(t *testing.T, res *message) map[string]any {
	t.Helper()
	var f map[string]any
	if err := json.Unmarshal(res.Result, &f); err != nil {
		t.Fatalf("result %s: %v", res.Result, err)
	}
	return f
}

func cachedFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func cacheName(bin *PoolBinary) string {
	h, _ := digestHex(bin.Digest)
	return h + binarySuffix
}

func bg() context.Context { return context.Background() }

// TestAPoolRelaysAnExecuteWithItsEventsAndResult runs an execute through the
// pool: the child sees no binary, the URLs and run ID pass through, events
// and result come back under the server's request ID, and a child-side ID
// is its own.
func TestAPoolRelaysAnExecuteWithItsEventsAndResult(t *testing.T) {
	r := newRig(t, rigOptions{})
	a := r.bins.add("a", 0)
	b := r.bins.add("b", 1)

	first := r.call(bg(), typeExecute, a, "echo", func(m *message) {
		m.BundleURL, m.ResultURL, m.RunID = "http://objects.invalid/bundle", "http://objects.invalid/result", "run-7"
	})
	if first.err != nil {
		t.Fatal(first.err)
	}
	if want := []string{`{"n":1}`, `{"n":2}`}; !slices.Equal(first.events, want) {
		t.Fatalf("events = %v, want %v", first.events, want)
	}
	f := resultFields(t, first.res)
	if f["type"] != "execute" || f["what"] != "echo" || f["has_binary"] != false || f["run_id"] != "run-7" ||
		f["bundle_url"] != "http://objects.invalid/bundle" || f["result_url"] != "http://objects.invalid/result" {
		t.Fatalf("what the runtime saw = %v", f)
	}
	if first.res.ID != 1 || f["id"] != float64(1) {
		t.Fatalf("server ID %d, child ID %v", first.res.ID, f["id"])
	}

	// The second request has server ID 2 and is the first of another child.
	second := r.call(bg(), typeExecute, b, "echo")
	if second.err != nil {
		t.Fatal(second.err)
	}
	if f := resultFields(t, second.res); second.res.ID != 2 || f["id"] != float64(1) {
		t.Fatalf("server ID %d, child ID %v: ids are not mapped", second.res.ID, f["id"])
	}
	r.waitWarm(warmIs(a.Identity, b.Identity))

	// The public client path, with a result the client decodes as a report.
	rep, err := r.client.ExecuteJob(bg(), Job{Bundle: []byte("report"), Binary: a})
	if err != nil || rep == nil {
		t.Fatalf("ExecuteJob = %v, %v", rep, err)
	}
	// A result error comes back with its kind.
	if res := r.call(bg(), typeExecute, a, "fail"); !errors.Is(res.err, engine.ErrBundle) {
		t.Fatalf("a runtime error = %v, want a bundle error", res.err)
	}
}

// TestAPoolRelaysInspectionsAndOTTLPreviews relays the other two requests.
func TestAPoolRelaysInspectionsAndOTTLPreviews(t *testing.T) {
	r := newRig(t, rigOptions{})
	a := r.bins.add("a", 0)
	for _, typ := range []string{typeInspect, typeOTTL} {
		res := r.call(bg(), typ, a, "look")
		if res.err != nil {
			t.Fatalf("%s: %v", typ, res.err)
		}
		if f := resultFields(t, res.res); f["type"] != typ || f["what"] != "look" || f["has_binary"] != false {
			t.Fatalf("%s: the runtime saw %v", typ, f)
		}
	}
}

// TestAPoolRelaysACancelToItsRuntime cancels a running request, and one
// whose deadline passed.
func TestAPoolRelaysACancelToItsRuntime(t *testing.T) {
	r := newRig(t, rigOptions{})
	a := r.bins.add("a", 0)
	for _, tc := range []struct {
		cause error
		want  error
	}{{context.Canceled, context.Canceled}, {context.DeadlineExceeded, context.DeadlineExceeded}} {
		ctx, cancel := context.WithCancelCause(bg())
		started, done := r.callAsync(ctx, typeExecute, a, "slow")
		r.wait(started)
		cancel(tc.cause)
		res := <-done
		if !errors.Is(res.err, tc.want) {
			t.Fatalf("cancelled with %v: result = %v", tc.cause, res.err)
		}
	}
}

// TestAPoolRefusesRequestsBeyondItsCapacityAcrossRuntimes fills the pool
// with requests on two runtimes.
func TestAPoolRefusesRequestsBeyondItsCapacityAcrossRuntimes(t *testing.T) {
	r := newRig(t, rigOptions{capacity: 2})
	a := r.bins.add("a", 0)
	b := r.bins.add("b", 1)
	ctx, cancel := context.WithCancel(bg())
	startedA, doneA := r.callAsync(ctx, typeExecute, a, "slow")
	startedB, doneB := r.callAsync(ctx, typeExecute, b, "slow")
	r.wait(startedA)
	r.wait(startedB)
	res := r.call(bg(), typeExecute, a, "echo")
	if !errors.Is(res.err, engine.ErrInvalidRequest) || !strings.Contains(res.err.Error(), "capacity") {
		t.Fatalf("a third request = %v, want a refusal naming the capacity", res.err)
	}
	cancel()
	<-doneA
	<-doneB
	if res := r.call(bg(), typeExecute, a, "echo"); res.err != nil {
		t.Fatalf("after the others finished: %v", res.err)
	}
}

// TestAPoolRefusesARequestWithoutAUsableBinary covers a request with no
// binary and with a malformed one.
func TestAPoolRefusesARequestWithoutAUsableBinary(t *testing.T) {
	r := newRig(t, rigOptions{})
	a := r.bins.add("a", 0)
	for name, bin := range map[string]*PoolBinary{
		"no digest":   {Identity: a.Identity, URL: a.URL, Digest: "nope", Size: a.Size},
		"no URL":      {Identity: a.Identity, Digest: a.Digest, Size: a.Size},
		"no size":     {Identity: a.Identity, URL: a.URL, Digest: a.Digest},
		"no identity": {URL: a.URL, Digest: a.Digest, Size: a.Size},
		"traversal":   {Identity: a.Identity, URL: a.URL, Digest: "sha256:../../etc/passwd", Size: a.Size},
	} {
		if res := r.call(bg(), typeExecute, bin, "echo"); !errors.Is(res.err, engine.ErrInvalidRequest) {
			t.Errorf("%s: result = %v, want an invalid request", name, res.err)
		}
	}
	if n := r.bins.total(); n != 0 {
		t.Fatalf("%d fetches for refused requests", n)
	}
}

// TestAPoolDistrustsABinaryThatFailsVerificationOrStart checks every way a
// binary is wrong: the request fails as runtime unavailable, and later
// requests for the digest fail at once, without a second fetch.
func TestAPoolDistrustsABinaryThatFailsVerificationOrStart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    map[string]string
		break_ func(*PoolBinary)
		want   string
	}{
		{name: "digest", break_: func(b *PoolBinary) { b.Digest = Digest([]byte("another binary")) }, want: "not the one assigned"},
		{name: "size too small", break_: func(b *PoolBinary) { b.Size-- }, want: "not the one assigned"},
		{name: "size too large", break_: func(b *PoolBinary) { b.Size++ }, want: "not the one assigned"},
		{name: "identity", break_: func(b *PoolBinary) { b.Identity = "sha256:" + strings.Repeat("a", 64) }, want: "announced runtime"},
		{name: "protocol", env: map[string]string{"FAKE_CHILD_PROTOCOL": "99"}, break_: func(*PoolBinary) {}, want: "protocol 99"},
		{name: "exit before hello", env: map[string]string{"FAKE_CHILD_MODE": "die"}, break_: func(*PoolBinary) {}, want: "fake runtime failed to load"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			r := newRig(t, rigOptions{})
			bin := r.bins.add("a", 0)
			tc.break_(bin)
			res := r.call(bg(), typeExecute, bin, "echo")
			if !errors.Is(res.err, engine.ErrRuntimeUnavailable) || !errors.Is(res.err, ErrSuspectBinary) || !strings.Contains(res.err.Error(), tc.want) {
				t.Fatalf("result = %v, want a suspect binary naming %q", res.err, tc.want)
			}
			fetches := r.bins.fetched("a")
			again := r.call(bg(), typeExecute, bin, "echo")
			if !errors.Is(again.err, engine.ErrRuntimeUnavailable) || !errors.Is(again.err, ErrSuspectBinary) || !strings.Contains(again.err.Error(), "refuses") {
				t.Fatalf("a second request = %v, want an immediate refusal", again.err)
			}
			if got := r.bins.fetched("a"); got != fetches {
				t.Fatalf("fetches = %d after the refusal, %d before", got, fetches)
			}
			if tc.name == "digest" || strings.HasPrefix(tc.name, "size") {
				if files := cachedFiles(t, r.cacheDir); len(files) != 0 {
					t.Fatalf("a rejected download left %v in the cache", files)
				}
			}
			r.waitWarm(warmIs())
		})
	}
}

// TestAFailedDownloadIsNotHeldAgainstTheBinary retries a binary the object
// store did not serve.
func TestAFailedDownloadIsNotHeldAgainstTheBinary(t *testing.T) {
	r := newRig(t, rigOptions{})
	a := r.bins.add("a", 0)
	missing := *a
	missing.URL = r.bins.srv.URL + "/missing"
	if res := r.call(bg(), typeExecute, &missing, "echo"); !errors.Is(res.err, engine.ErrRuntimeUnavailable) || errors.Is(res.err, ErrSuspectBinary) {
		t.Fatalf("a missing binary = %v, want runtime unavailable, not suspect", res.err)
	}
	if res := r.call(bg(), typeExecute, a, "echo"); res.err != nil {
		t.Fatalf("the binary at its real URL = %v", res.err)
	}
}

// TestConcurrentFirstRequestsShareOneFetchAndStart sends three requests for
// a cold runtime while its download is held.
func TestConcurrentFirstRequestsShareOneFetchAndStart(t *testing.T) {
	r := newRig(t, rigOptions{})
	a := r.bins.add("a", 0)
	gate := make(chan struct{})
	r.bins.mu.Lock()
	r.bins.gate = gate
	r.bins.mu.Unlock()
	var dones []<-chan callResult
	for range 3 {
		_, done := r.callAsync(bg(), typeExecute, a, "echo")
		dones = append(dones, done)
	}
	select {
	case <-r.bins.entered:
	case <-time.After(waitLimit):
		t.Fatal("no fetch began")
	}
	s := <-r.sessions
	for deadline := time.Now().Add(waitLimit); ; {
		s.mu.Lock()
		n := len(s.running)
		s.mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests admitted", n)
		}
		runtime.Gosched()
	}
	close(gate)
	for i, done := range dones {
		if res := <-done; res.err != nil {
			t.Fatalf("request %d: %v", i, res.err)
		}
	}
	if n := r.bins.fetched("a"); n != 1 {
		t.Fatalf("%d fetches for one runtime", n)
	}
	r.logs.waitFor(t, "fake runtime ready", 1)
	if n := strings.Count(r.logs.String(), "fake runtime ready"); n != 1 {
		t.Fatalf("%d children started:\n%s", n, r.logs.String())
	}
	r.waitWarm(warmIs(a.Identity))
}

// TestAnIdleChildStopsAfterTheIdleTime stops a child on the idle timer and
// starts it again for the next request, from the cache.
func TestAnIdleChildStopsAfterTheIdleTime(t *testing.T) {
	r := newRig(t, rigOptions{idle: time.Minute})
	a := r.bins.add("a", 0)
	if res := r.call(bg(), typeExecute, a, "echo"); res.err != nil {
		t.Fatal(res.err)
	}
	r.clock.waitArmed(t)
	r.waitWarm(warmIs(a.Identity))
	if n := r.clock.fireAll(); n != 1 {
		t.Fatalf("%d timers pending", n)
	}
	r.waitWarm(warmIs())
	r.logs.waitFor(t, "stopped", 1)

	if res := r.call(bg(), typeExecute, a, "echo"); res.err != nil {
		t.Fatal(res.err)
	}
	r.waitWarm(warmIs(a.Identity))
	if n := r.bins.fetched("a"); n != 1 {
		t.Fatalf("%d fetches: the restart did not use the cache", n)
	}
	r.logs.waitFor(t, "started from binary", 2)
}

// TestARequestKeepsAChildFromGoingIdle checks that a timer which fires
// while a request runs, or after one began, stops nothing.
func TestARequestKeepsAChildFromGoingIdle(t *testing.T) {
	r := newRig(t, rigOptions{idle: time.Minute})
	a := r.bins.add("a", 0)
	if res := r.call(bg(), typeExecute, a, "echo"); res.err != nil {
		t.Fatal(res.err)
	}
	r.clock.waitArmed(t)
	ctx, cancel := context.WithCancel(bg())
	started, done := r.callAsync(ctx, typeExecute, a, "slow")
	r.wait(started)
	r.clock.fireStale() // the timer set before the second request
	r.waitWarm(warmIs(a.Identity))
	cancel()
	<-done
	r.clock.waitArmed(t)
	r.waitWarm(warmIs(a.Identity))
}

// TestWarmIsASoftCapOnRunningChildren keeps one idle child, never refuses a
// request for the cap, and stops the least recently used idle child.
func TestWarmIsASoftCapOnRunningChildren(t *testing.T) {
	r := newRig(t, rigOptions{warm: 1})
	a := r.bins.add("a", 0)
	b := r.bins.add("b", 1)
	c := r.bins.add("c", 2)
	if res := r.call(bg(), typeExecute, a, "echo"); res.err != nil {
		t.Fatal(res.err)
	}
	r.waitWarm(warmIs(a.Identity))
	if res := r.call(bg(), typeExecute, b, "echo"); res.err != nil {
		t.Fatal(res.err)
	}
	r.waitWarm(warmIs(b.Identity)) // a was the least recently used
	r.logs.waitFor(t, "stopped", 1)

	// Two requests at once exceed the cap and neither is refused; once both
	// finish, the older runtime goes.
	ctx, cancel := context.WithCancel(bg())
	startedB, doneB := r.callAsync(ctx, typeExecute, b, "slow")
	startedC, doneC := r.callAsync(ctx, typeExecute, c, "slow")
	r.wait(startedB)
	r.wait(startedC)
	r.waitWarm(warmIs(b.Identity, c.Identity))
	cancel()
	<-doneB
	<-doneC
	warm := r.waitWarm(func(w []string) bool { return len(w) == 1 })
	if warm[0] != b.Identity && warm[0] != c.Identity {
		t.Fatalf("warm = %v", warm)
	}
}

// TestACacheBoundEvictsTheLeastRecentlyUsedBinaries runs three runtimes on a
// pool that keeps none and a cache that holds two.
func TestACacheBoundEvictsTheLeastRecentlyUsedBinaries(t *testing.T) {
	r := newRig(t, rigOptions{warm: noWarm, cacheBytes: int64(2*len(variant(t, 0)) + 100)})
	bins := []*PoolBinary{r.bins.add("a", 0), r.bins.add("b", 1), r.bins.add("c", 2)}
	for i, bin := range bins {
		r.clock.Advance(time.Hour)
		if res := r.call(bg(), typeExecute, bin, "echo"); res.err != nil {
			t.Fatal(res.err)
		}
		r.logs.waitFor(t, "stopped", i+1)
	}
	files := cachedFiles(t, r.cacheDir)
	slices.Sort(files)
	want := []string{cacheName(bins[1]), cacheName(bins[2])}
	slices.Sort(want)
	if !slices.Equal(files, want) {
		t.Fatalf("cache = %v, want %v (the oldest is evicted)", files, want)
	}
}

// TestACacheBoundNeverEvictsABinaryARunningChildUses holds the cache over
// its bound with children running.
func TestACacheBoundNeverEvictsABinaryARunningChildUses(t *testing.T) {
	r := newRig(t, rigOptions{warm: 1, cacheBytes: 1000})
	a := r.bins.add("a", 0)
	b := r.bins.add("b", 1)
	ctx, cancel := context.WithCancel(bg())
	started, done := r.callAsync(ctx, typeExecute, a, "slow")
	r.wait(started)
	r.clock.Advance(time.Hour)
	if res := r.call(bg(), typeExecute, b, "echo"); res.err != nil {
		t.Fatal(res.err)
	}
	// Warm is 1 and a is busy, so b stops once idle; a stays, though it is
	// the older binary and the cache is over its bound.
	r.logs.waitFor(t, "stopped", 1)
	files := cachedFiles(t, r.cacheDir)
	if !slices.Equal(files, []string{cacheName(a)}) {
		t.Fatalf("cache = %v, want only the running child's binary", files)
	}
	cancel()
	<-done
	r.waitWarm(warmIs(a.Identity))
}

// TestTheCacheIsBoundedAtStartFromFileTimes starts a pool on a cache with
// old binaries, other files and a download a crash left.
func TestTheCacheIsBoundedAtStartFromFileTimes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var names []string
	for i, age := range []time.Duration{3 * time.Hour, time.Hour, 2 * time.Hour} {
		name := Digest([]byte{byte(i)})
		h, _ := digestHex(name)
		path := filepath.Join(dir, h+binarySuffix)
		if err := os.WriteFile(path, bytes.Repeat([]byte{byte(i)}, 100), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		names = append(names, h+binarySuffix)
	}
	for _, name := range []string{"notes.txt", "sha256-short"} {
		if err := os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte("x"), 500), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, poolTempPrefix+"123"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newPool(PoolOptions{Token: "t", Capacity: 1, CacheDir: dir, CacheBytes: 250}); err != nil {
		t.Fatal(err)
	}
	got := cachedFiles(t, dir)
	slices.Sort(got)
	want := []string{names[1], names[2], "notes.txt", "sha256-short"} // names[0] is the oldest
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("cache = %v, want %v", got, want)
	}
}

// TestACachedBinaryIsVerifiedBeforeItsFirstStart uses a good cached binary
// without a fetch and replaces a damaged one.
func TestACachedBinaryIsVerifiedBeforeItsFirstStart(t *testing.T) {
	data := variant(t, 0)
	good := binaryFor(0, data, "")
	for _, tc := range []struct {
		name    string
		content []byte
		fetches int
	}{
		{"good", data, 0},
		{"damaged", append(slices.Clone(data[:len(data)-1]), 'X'), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, cacheName(good)), tc.content, 0o755); err != nil {
				t.Fatal(err)
			}
			r := newRig(t, rigOptions{cacheDir: dir})
			bin := r.bins.add("a", 0)
			if res := r.call(bg(), typeExecute, bin, "echo"); res.err != nil {
				t.Fatal(res.err)
			}
			if n := r.bins.fetched("a"); n != tc.fetches {
				t.Fatalf("%d fetches, want %d", n, tc.fetches)
			}
		})
	}
}

// TestAChildNeverSeesTheWorkersEnvironment starts a child with LINEA_
// variables set in the pool's environment.
func TestAChildNeverSeesTheWorkersEnvironment(t *testing.T) {
	t.Setenv("LINEA_WORKER_TOKEN", "pool-secret")
	t.Setenv("linea_other", "x")
	t.Setenv("POOL_TEST_KEEP", "kept")
	r := newRig(t, rigOptions{})
	a := r.bins.add("a", 0)
	res := r.call(bg(), typeExecute, a, "env")
	if res.err != nil {
		t.Fatal(res.err)
	}
	var got struct{ Env []string }
	if err := json.Unmarshal(res.res.Result, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Env, "POOL_TEST_KEEP=kept") {
		t.Fatalf("the child lost its other variables: %v", got.Env)
	}
	for _, kv := range got.Env {
		if strings.HasPrefix(strings.ToUpper(kv), "LINEA_") {
			t.Fatalf("the child inherited %q", strings.SplitN(kv, "=", 2)[0])
		}
	}
	if strings.Contains(r.logs.String(), "pool-secret") {
		t.Fatal("the token reached the log")
	}
	env := childEnv([]string{"PATH=/bin", "LINEA_X=1", "Linea_Y=2", "LINEAR=3"})
	if !slices.Equal(env, []string{"PATH=/bin", "LINEAR=3"}) {
		t.Fatalf("childEnv = %v", env)
	}
}

// TestAChildThatExitsFailsItsRequestsAndIsStartedAgain crashes a child
// with two requests in flight.
func TestAChildThatExitsFailsItsRequestsAndIsStartedAgain(t *testing.T) {
	r := newRig(t, rigOptions{})
	a := r.bins.add("a", 0)
	started, slow := r.callAsync(bg(), typeExecute, a, "slow")
	r.wait(started)
	r.waitWarm(warmIs(a.Identity))
	crash := r.call(bg(), typeExecute, a, "crash")
	if !errors.Is(crash.err, engine.ErrRuntimeUnavailable) {
		t.Fatalf("the crashing request = %v", crash.err)
	}
	if res := <-slow; !errors.Is(res.err, engine.ErrRuntimeUnavailable) {
		t.Fatalf("the request in flight = %v, want runtime unavailable", res.err)
	}
	r.waitWarm(warmIs())
	r.logs.waitFor(t, "exited", 1)

	if res := r.call(bg(), typeExecute, a, "echo"); res.err != nil {
		t.Fatalf("after the crash: %v", res.err)
	}
	r.waitWarm(warmIs(a.Identity))
	if n := r.bins.fetched("a"); n != 1 {
		t.Fatalf("%d fetches: a crashed child is not a suspect binary", n)
	}
	r.logs.waitFor(t, "fake runtime ready", 2)
}

// TestALostConnectionStopsEveryChildAndKeepsTheCache drops the connection
// with a request in flight, and serves the next one from the cache.
func TestALostConnectionStopsEveryChildAndKeepsTheCache(t *testing.T) {
	r := newRig(t, rigOptions{})
	a := r.bins.add("a", 0)
	started, done := r.callAsync(bg(), typeExecute, a, "slow")
	r.wait(started)
	old := r.client
	_ = old.Close()
	<-done
	r.logs.waitFor(t, "stopped", 1)
	if files := cachedFiles(t, r.cacheDir); !slices.Equal(files, []string{cacheName(a)}) {
		t.Fatalf("cache = %v", files)
	}
	r.nextClient()
	if res := r.call(bg(), typeExecute, a, "echo"); res.err != nil {
		t.Fatal(res.err)
	}
	if n := r.bins.fetched("a"); n != 1 {
		t.Fatalf("%d fetches after reconnecting", n)
	}
}

// TestAPoolSaysHelloAndItsRuntimesOnConnect reads the first frames of a
// connection.
func TestAPoolSaysHelloAndItsRuntimesOnConnect(t *testing.T) {
	p, err := newPool(PoolOptions{Token: "t", Name: "edge-pool", Capacity: 3, Warm: 2, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- p.serve(bg(), inR, outW) }()
	hello, err := readFrame(outR)
	if err != nil || hello.Type != typeHello || hello.Hello == nil {
		t.Fatalf("first frame = %+v, %v", hello, err)
	}
	h, want := hello.Hello, func() Hello { h, _ := Identity(); return h }()
	if h.Name != "edge-pool" || h.Capacity != 3 || h.Protocol != ProtocolVersion || h.Runtime != want.Runtime ||
		h.Pool == nil || h.Pool.OS != runtime.GOOS || h.Pool.Arch != runtime.GOARCH || h.Pool.Warm != 2 || !h.Pool.Own {
		t.Fatalf("hello = %+v pool %+v", h, h.Pool)
	}
	state, err := readFrame(outR)
	if err != nil || state.Type != typePoolState || len(state.Warm) != 0 {
		t.Fatalf("second frame = %+v, %v", state, err)
	}
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatalf("serve = %v", err)
	}
}

func TestAPoolStopsOnARefusedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = Accept(w, r, func(string) bool { return false })
	}))
	defer srv.Close()
	err := ServePool(bg(), PoolOptions{URL: srv.URL, Token: "wrong", Capacity: 1, CacheDir: t.TempDir()})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ServePool = %v, want ErrUnauthorized", err)
	}
}

func TestPoolOptionsAreChecked(t *testing.T) {
	ok := PoolOptions{Token: "t", Capacity: 1, CacheDir: t.TempDir()}
	for name, mutate := range map[string]func(*PoolOptions){
		"token":    func(o *PoolOptions) { o.Token = "" },
		"capacity": func(o *PoolOptions) { o.Capacity = 0 },
		"cache":    func(o *PoolOptions) { o.CacheDir = "" },
		"warm":     func(o *PoolOptions) { o.Warm = -1 },
		"idle":     func(o *PoolOptions) { o.Idle = -time.Second },
		"bytes":    func(o *PoolOptions) { o.CacheBytes = -1 },
	} {
		o := ok
		mutate(&o)
		if _, err := newPool(o); err == nil {
			t.Errorf("%s: options %+v were accepted", name, o)
		}
	}
	p, err := newPool(ok)
	if err != nil || p.opts.Name != "pool" {
		t.Fatalf("defaults: %v, name %q", err, p.opts.Name)
	}
}

func TestAChildsLogLinesAreForwardedWithAPrefixAndBounded(t *testing.T) {
	logs := newSyncBuffer()
	p := &pool{opts: PoolOptions{Log: logs}}
	l := &lineLog{pool: p, prefix: "runtime abc: "}
	for _, chunk := range []string{"first li", "ne\nsecond\r\n", strings.Repeat("x", poolLogLine+50) + "\n", strings.Repeat("y", poolLogLine/2), strings.Repeat("y", poolLogLine/2+9) + "\nlast\n"} {
		if n, err := l.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
	if len(lines) != 5 || lines[0] != "runtime abc: first line" || lines[1] != "runtime abc: second" || lines[4] != "runtime abc: last" {
		t.Fatalf("lines = %q", lines)
	}
	for _, i := range []int{2, 3} {
		if !strings.HasSuffix(lines[i], " [line cut]") || len(lines[i]) > len("runtime abc: ")+poolLogLine+len(" [line cut]") {
			t.Fatalf("line %d = %d bytes: not cut", i, len(lines[i]))
		}
	}
}

// TestAPoolRunsItsOwnBuildOnDemand sends requests that name no binary: the
// pool starts its own executable once, downloads and caches nothing,
// reports it warm under its own identity and stops it when idle.
func TestAPoolRunsItsOwnBuildOnDemand(t *testing.T) {
	r := newRig(t, rigOptions{idle: time.Minute, own: 6})
	own := fakeIdentity(5)
	for range 2 {
		res := r.call(bg(), typeExecute, nil, "echo")
		if res.err != nil {
			t.Fatal(res.err)
		}
		if f := resultFields(t, res.res); f["what"] != "echo" || f["has_binary"] != false {
			t.Fatalf("what the runtime saw = %v", f)
		}
	}
	if res := r.call(bg(), typeOTTL, nil, "echo"); res.err != nil {
		t.Fatal(res.err)
	}
	r.waitWarm(warmIs(own))
	r.logs.waitFor(t, "started from binary", 1)
	if n := r.bins.total(); n != 0 {
		t.Fatalf("%d fetches for the pool's own build", n)
	}
	if files := cachedFiles(t, r.cacheDir); len(files) != 0 {
		t.Fatalf("cache = %v", files)
	}
	r.clock.waitArmed(t)
	if n := r.clock.fireAll(); n != 1 {
		t.Fatalf("%d timers pending", n)
	}
	r.waitWarm(warmIs())
	if res := r.call(bg(), typeExecute, nil, "echo"); res.err != nil {
		t.Fatal(res.err)
	}
	r.logs.waitFor(t, "started from binary", 2)
}

// TestAFailedStartOfItsOwnBuildIsNotHeldAgainstIt: the request fails as
// runtime unavailable, not as a suspect binary, and the next request tries
// again.
func TestAFailedStartOfItsOwnBuildIsNotHeldAgainstIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "self"+binarySuffix)
	r := newRig(t, rigOptions{own: 7, ownPath: path})
	res := r.call(bg(), typeExecute, nil, "echo")
	if !errors.Is(res.err, engine.ErrRuntimeUnavailable) || errors.Is(res.err, ErrSuspectBinary) {
		t.Fatalf("result = %v, want runtime unavailable and not suspect", res.err)
	}
	if err := os.WriteFile(path, variant(t, 6), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := r.call(bg(), typeExecute, nil, "echo"); res.err != nil {
		t.Fatalf("the retry = %v", res.err)
	}
}

// TestAPoolThatCannotRunItselfSaysSo: no Own in its hello, and a request
// without a binary fails as runtime unavailable.
func TestAPoolThatCannotRunItselfSaysSo(t *testing.T) {
	r := newRig(t, rigOptions{})
	if h := r.client.Hello(); h.Pool == nil || h.Pool.Own {
		t.Fatalf("hello pool = %+v", h.Pool)
	}
	if res := r.call(bg(), typeExecute, nil, "echo"); !errors.Is(res.err, engine.ErrRuntimeUnavailable) {
		t.Fatalf("result = %v, want runtime unavailable", res.err)
	}
}
