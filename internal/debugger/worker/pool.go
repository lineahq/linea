package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lineahq/linea/internal/debugger/engine"
)

// A pool worker dials the server as a worker does, on the
// same route with the same token, but serves no runtime of its own: every
// execute, inspect and ottl request names the runtime binary it runs on
// (message.Binary). The pool downloads that binary once into its cache,
// checks it against the digest and size the server gave, starts it as a
// normal stdin/stdout worker (one child process per runtime in use) and
// relays the request to it. The child fetches the bundle and uploads a
// large result itself, so the URLs in a request pass through unchanged, and
// the pool relays the child's event and result frames without decoding
// them, under the server's request IDs.
//
// A binary that fails verification or does not start as the runtime the
// server named is suspect for the life of the process: the pool answers
// later requests for it at once with engine.ErrRuntimeUnavailable, without
// fetching it again. Each such answer is marked ErrSuspectBinary, so the
// server records the binary and offers no pool its runs. A child that exits fails the requests it ran, and the
// next request starts it again. The pool bounds what it keeps: Warm is a
// soft cap on running children (a request is never refused for it), Idle
// stops a child nobody used for that long, and CacheBytes bounds the
// binaries on disk. A child that runs a request is never stopped, and its
// binary is never evicted.
//
// A request that names no binary runs on the pool's own build: the pool starts
// its own executable as a child, in the same way and under the same bounds,
// with nothing to download or verify. Such a child is never held against the
// build: a failed start fails only its requests.

const (
	// poolBinaryLimit is the largest runtime binary a pool downloads.
	poolBinaryLimit = 4 << 30
	// poolFetchTimeout bounds one binary download.
	poolFetchTimeout = 15 * time.Minute
	// poolStartTimeout bounds a child's start until its hello.
	poolStartTimeout = handshakeTimeout
	// poolLogLine bounds one forwarded line of a child's log.
	poolLogLine = 4 << 10
	// poolTempPrefix names a download in progress inside the cache.
	poolTempPrefix = ".fetch-"
	// envPrefix names the variables a child never inherits: the worker
	// token and every other setting of this product.
	envPrefix = "LINEA_"
	// ownDigest keys the child of the pool's own build among the children,
	// which are keyed by digest; no "sha256:<hex>" digest equals it.
	ownDigest = "own"
)

// poolStopGrace is how long a child gets to exit after its input closes
// before it is killed; tests shorten it.
var poolStopGrace = 10 * time.Second

// binaryClient downloads runtime binaries from presigned URLs; a download
// is bounded by its context, not a client timeout.
var binaryClient = &http.Client{}

// binarySuffix ends the name of a cached binary, which a Windows host needs
// to execute it.
var binarySuffix = func() string {
	if goruntime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}()

// PoolOptions configure ServePool.
type PoolOptions struct {
	// URL, CA and Token are as in ConnectOptions; a pool dials the same
	// route with the worker token.
	URL   string
	CA    []byte
	Token string
	// Name is the name the pool is known by; "pool" when empty. Capacity is
	// how many requests it runs at once across all runtimes, at least one.
	Name     string
	Capacity int
	// Warm is a soft cap on running children: a request is never refused
	// for it, but once a child is idle and more than Warm children run, the
	// least recently used idle ones are stopped.
	Warm int
	// Idle stops a child no request used for that long; zero keeps it.
	Idle time.Duration
	// CacheDir holds the downloaded binaries and must be on a filesystem
	// that may execute files. CacheBytes bounds it by evicting the least
	// recently used binaries no running child uses; zero bounds nothing.
	CacheDir   string
	CacheBytes int64
	// Log receives a line per connection and each child's log lines; nil
	// discards them.
	Log io.Writer
}

// ServePool dials the server and serves it as a pool worker, reconnecting
// after a lost connection, until ctx ends; it then returns nil once the
// children have stopped. A refused token ends it with ErrUnauthorized.
func ServePool(ctx context.Context, opts PoolOptions) error {
	p, err := newPool(opts)
	if err != nil {
		return err
	}
	return p.run(ctx)
}

// run dials the server and holds each connection until ctx ends.
func (p *pool) run(ctx context.Context) error {
	t, err := newDialTarget(p.opts.URL, p.opts.CA, p.opts.Token, ConnectPath)
	if err != nil {
		return err
	}
	return redial(ctx, t, p.opts.Log, func(ctx context.Context, conn *websocket.Conn, logf func(string, ...any)) error {
		logf("connected to %s as pool %s (capacity %d, warm %d)", t.target, p.opts.Name, p.opts.Capacity, p.opts.Warm)
		s := newStream(conn)
		if err := p.serve(ctx, s, s); err != nil {
			return err
		}
		_ = s.closeWrite()
		return errors.New("the server closed the connection")
	})
}

// pool is what a pool worker keeps across connections: which binaries are
// suspect or verified, and the cache.
type pool struct {
	opts PoolOptions
	// ownPath is the pool's own executable and ownIdentity the runtime it
	// announces; ownPath is empty when the pool cannot run itself. Tests
	// replace them.
	ownPath, ownIdentity string
	// now and afterFunc are the clock; tests replace them.
	now       func() time.Time
	afterFunc func(d time.Duration, f func()) (stop func() bool)

	logMu sync.Mutex
	// onSession, when set, sees each connection's session; tests use it.
	onSession func(*poolSession)

	mu       sync.Mutex
	suspect  map[string]error     // by digest
	verified map[string]bool      // by digest: checked in this process
	used     map[string]time.Time // by digest hex: last use, for eviction
	pinned   map[string]int       // by digest hex: children using the file
}

func newPool(opts PoolOptions) (*pool, error) {
	if opts.Token == "" {
		return nil, errors.New("worker: a pool worker needs a registration token")
	}
	if strings.TrimSpace(opts.Name) == "" {
		opts.Name = "pool"
	}
	if opts.Capacity < 1 {
		return nil, errors.New("worker: a pool worker's capacity is at least 1")
	}
	if opts.Warm < 0 || opts.Idle < 0 || opts.CacheBytes < 0 {
		return nil, errors.New("worker: a pool worker's warm, idle and cache bounds are not negative")
	}
	if opts.CacheDir == "" {
		return nil, errors.New("worker: a pool worker needs a cache directory")
	}
	own, err := Identity()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("worker: pool cache: %w", err)
	}
	p := &pool{
		opts:        opts,
		ownIdentity: own.Runtime,
		now:         time.Now,
		afterFunc:   func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop },
		suspect:     map[string]error{},
		verified:    map[string]bool{},
		used:        map[string]time.Time{},
		pinned:      map[string]int{},
	}
	if path, err := os.Executable(); err == nil {
		p.ownPath = path
	} else {
		p.logf("the pool cannot run its own build: %v", err)
	}
	p.initCache()
	return p, nil
}

// digestHex is the hex of a "sha256:<hex>" digest, and whether it is one.
func digestHex(digest string) (string, bool) {
	h, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(h) != sha256.Size*2 {
		return "", false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return h, true
}

// cachedDigest is the hex a cache file name carries; false for a file that
// is not a cached binary.
func cachedDigest(name string) (string, bool) {
	h, ok := strings.CutSuffix(name, binarySuffix)
	if !ok {
		return "", false
	}
	_, ok = digestHex("sha256:" + h)
	return h, ok
}

func (p *pool) path(hexDigest string) string {
	return filepath.Join(p.opts.CacheDir, hexDigest+binarySuffix)
}

// initCache takes the recency of the binaries already cached from their
// modification times and drops the downloads a crash left behind.
func (p *pool) initCache() {
	entries, err := os.ReadDir(p.opts.CacheDir)
	if err != nil {
		return
	}
	p.mu.Lock()
	for _, e := range entries {
		switch h, ok := cachedDigest(e.Name()); {
		case strings.HasPrefix(e.Name(), poolTempPrefix) && e.Type().IsRegular():
			_ = os.Remove(filepath.Join(p.opts.CacheDir, e.Name()))
		case ok && e.Type().IsRegular():
			if info, err := e.Info(); err == nil {
				p.used[h] = info.ModTime()
			}
		}
	}
	p.mu.Unlock()
	p.evictCache()
}

func (p *pool) logf(format string, args ...any) {
	if p.opts.Log == nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	fmt.Fprintf(p.opts.Log, format+"\n", args...)
}

func (p *pool) pin(h string) {
	if h == "" {
		return // the pool's own build is not in the cache
	}
	p.mu.Lock()
	p.pinned[h]++
	p.mu.Unlock()
}

func (p *pool) unpin(h string) {
	if h == "" {
		return
	}
	p.mu.Lock()
	if p.pinned[h]--; p.pinned[h] <= 0 {
		delete(p.pinned, h)
	}
	p.mu.Unlock()
}

func (p *pool) touch(h string) {
	if h == "" {
		return
	}
	now := p.now()
	p.mu.Lock()
	p.used[h] = now
	p.mu.Unlock()
}

// suspectErr is the answer for a binary that failed before, nil if it has
// not.
func (p *pool) suspectErr(digest string) error {
	p.mu.Lock()
	cause := p.suspect[digest]
	p.mu.Unlock()
	if cause == nil {
		return nil
	}
	return suspectError{fmt.Errorf("%w: the pool refuses runtime binary %s after it failed earlier: %v", engine.ErrRuntimeUnavailable, shortDigest(digest), cause)}
}

func (p *pool) markSuspect(digest string, cause error) {
	p.mu.Lock()
	if p.suspect[digest] == nil {
		p.suspect[digest] = cause
	}
	p.mu.Unlock()
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

// evictCache removes the least recently used cached binaries no child uses
// until the cache is within CacheBytes. Files that are not named like a
// digest are never counted or removed.
func (p *pool) evictCache() {
	if p.opts.CacheBytes <= 0 {
		return
	}
	entries, err := os.ReadDir(p.opts.CacheDir)
	if err != nil {
		return
	}
	type cached struct {
		hex  string
		size int64
		used time.Time
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var files []cached
	var total int64
	for _, e := range entries {
		h, ok := cachedDigest(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		used, ok := p.used[h]
		if !ok {
			used = info.ModTime()
		}
		files = append(files, cached{h, info.Size(), used})
		total += info.Size()
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total <= p.opts.CacheBytes {
			return
		}
		if p.pinned[f.hex] > 0 {
			continue
		}
		if err := os.Remove(p.path(f.hex)); err != nil {
			continue
		}
		total -= f.size
		delete(p.used, f.hex)
		delete(p.verified, "sha256:"+f.hex)
	}
}

// ensureBinary returns the path of the binary the server named, from the
// cache or downloaded and verified. suspect says the binary itself is wrong
// rather than the transfer.
func (p *pool) ensureBinary(ctx context.Context, bin *PoolBinary, h string) (path string, suspect bool, err error) {
	path = p.path(h)
	if st, err := os.Stat(path); err == nil {
		p.mu.Lock()
		verified := p.verified[bin.Digest]
		p.mu.Unlock()
		if verified {
			return path, false, nil
		}
		if st.Size() == bin.Size {
			if sum, err := hashFile(path); err == nil && sum == bin.Digest {
				p.mu.Lock()
				p.verified[bin.Digest] = true
				p.mu.Unlock()
				return path, false, nil
			}
		}
		// A cached file that is not what its name says is damaged, not the
		// server's doing: fetch it again.
		p.logf("cached runtime binary %s does not match its digest; fetching it again", shortDigest(bin.Digest))
		if err := os.Remove(path); err != nil {
			return "", false, fmt.Errorf("%w: remove a damaged cached binary: %w", engine.ErrRuntimeUnavailable, err)
		}
	}
	return p.download(ctx, bin, h)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// download fetches the binary into a temporary file of the cache, checks
// its size and digest while it streams, and renames it into place.
func (p *pool) download(ctx context.Context, bin *PoolBinary, h string) (string, bool, error) {
	unavailable := func(format string, args ...any) (string, bool, error) {
		return "", false, fmt.Errorf("%w: "+format, append([]any{engine.ErrRuntimeUnavailable}, args...)...)
	}
	ctx, cancel := context.WithTimeout(ctx, poolFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bin.URL, nil)
	if err != nil {
		return "", false, fmt.Errorf("%w: runtime binary URL: %w", engine.ErrInvalidRequest, err)
	}
	resp, err := binaryClient.Do(req)
	if err != nil {
		return unavailable("download runtime binary %s: %w", shortDigest(bin.Digest), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return unavailable("download runtime binary %s: object storage answered %s", shortDigest(bin.Digest), resp.Status)
	}
	f, err := os.CreateTemp(p.opts.CacheDir, poolTempPrefix+"*")
	if err != nil {
		return unavailable("download runtime binary %s: %w", shortDigest(bin.Digest), err)
	}
	tmp := f.Name()
	done := false
	defer func() {
		if !done {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(resp.Body, bin.Size+1))
	if err != nil {
		return unavailable("download runtime binary %s: %w", shortDigest(bin.Digest), err)
	}
	if n != bin.Size || "sha256:"+hex.EncodeToString(sum.Sum(nil)) != bin.Digest {
		return "", true, fmt.Errorf("%w: the downloaded runtime binary is not the one assigned (%s)", engine.ErrRuntimeUnavailable, shortDigest(bin.Digest))
	}
	if err := f.Chmod(0o755); err != nil {
		return unavailable("runtime binary %s: %w", shortDigest(bin.Digest), err)
	}
	if err := f.Close(); err != nil {
		return unavailable("runtime binary %s: %w", shortDigest(bin.Digest), err)
	}
	path := p.path(h)
	if err := os.Rename(tmp, path); err != nil {
		return unavailable("runtime binary %s: %w", shortDigest(bin.Digest), err)
	}
	done = true
	p.mu.Lock()
	p.verified[bin.Digest] = true
	p.mu.Unlock()
	return path, false, nil
}

// childEnv is the pool's environment without the product's own variables:
// the worker token never reaches a child.
func childEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if len(kv) >= len(envPrefix) && strings.EqualFold(kv[:len(envPrefix)], envPrefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// spawn starts the runtime binary, or the pool's own executable, as a
// worker and checks its hello names the runtime the server asked for.
// suspect says the binary is wrong.
func (p *pool) spawn(ctx context.Context, bin *PoolBinary, h string) (*Client, bool, error) {
	path := p.ownPath
	if bin.Digest != ownDigest {
		var suspect bool
		var err error
		if path, suspect, err = p.ensureBinary(ctx, bin, h); err != nil {
			return nil, suspect, err
		}
	}
	cmd := exec.Command(path)
	cmd.Env = childEnv(os.Environ())
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", engine.ErrRuntimeUnavailable, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", engine.ErrRuntimeUnavailable, err)
	}
	c := newClient("pool runtime "+shortDigest(bin.Identity), stdin)
	c.closeIn, c.wait = stdin.Close, cmd.Wait
	c.kill = func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	cmd.Stderr = io.MultiWriter(c.log, &lineLog{pool: p, prefix: "runtime " + shortDigest(bin.Identity) + ": "})
	if err := cmd.Start(); err != nil {
		return nil, true, fmt.Errorf("%w: start runtime binary %s: %w", engine.ErrRuntimeUnavailable, shortDigest(bin.Digest), err)
	}
	hctx, cancel := context.WithTimeout(ctx, poolStartTimeout)
	stopKill := context.AfterFunc(hctx, c.kill)
	_, err = c.handshake(hctx, stdout)
	stopKill()
	cancel()
	if err != nil {
		// The pool stopping is not the binary's fault.
		return nil, ctx.Err() == nil, fmt.Errorf("%w: start runtime %s: %w", engine.ErrRuntimeUnavailable, shortDigest(bin.Identity), err)
	}
	if got := c.hello.Runtime; got != bin.Identity {
		stopClient(c)
		return nil, true, fmt.Errorf("%w: runtime binary %s announced runtime %s, not the assigned %s", engine.ErrRuntimeUnavailable, shortDigest(bin.Digest), shortDigest(got), shortDigest(bin.Identity))
	}
	return c, false, nil
}

// exitReason is why a child's process ended.
func exitReason(c *Client) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitErr
}

// stopClient closes a child's input, waits for it to exit and kills it after
// a grace period.
func stopClient(c *Client) {
	_ = c.closeIn()
	select {
	case <-c.exited:
	case <-time.After(poolStopGrace):
		c.kill()
		<-c.exited
	}
}

// lineLog forwards a child's standard error to the pool's log line by line
// with a prefix; a line over poolLogLine is cut.
type lineLog struct {
	pool   *pool
	prefix string
	buf    []byte
	skip   bool // inside the rest of a cut line
}

func (l *lineLog) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk, last := p, i < 0
		if !last {
			chunk = p[:i]
		}
		if !l.skip {
			l.buf = append(l.buf, chunk...)
			switch {
			case len(l.buf) > poolLogLine:
				l.emit(l.buf[:poolLogLine], true)
				l.skip = true
			case !last:
				l.emit(l.buf, false)
			}
		}
		if last {
			break
		}
		l.buf, l.skip = l.buf[:0], false
		p = p[i+1:]
	}
	if l.skip {
		l.buf = l.buf[:0]
	}
	return n, nil
}

func (l *lineLog) emit(line []byte, cut bool) {
	s := strings.TrimRight(string(line), "\r")
	if cut {
		s += " [line cut]"
	}
	l.pool.logf("%s%s", l.prefix, s)
}

// poolChild is one runtime binary's process, by digest. It is created when a
// request first needs it and ready once its hello was checked.
type poolChild struct {
	digest, hex string
	identity    string
	ready       chan struct{}
	err         error   // why it did not start; set before ready closes
	c           *Client // set before ready closes
	active      int     // requests in flight
	seq         uint64  // recency among children, for the warm cap
	removed     bool    // out of the pool's set: stopping, exited or failed
	idleGen     uint64
	stopIdle    func() bool
}

// poolSession is one connection of a pool worker.
type poolSession struct {
	p   *pool
	ctx context.Context
	out io.Writer

	writeMu sync.Mutex

	stateMu   sync.Mutex // orders pool_state frames
	stateSent bool
	lastState []string

	mu       sync.Mutex
	closed   bool
	wg       sync.WaitGroup
	seq      uint64
	running  map[uint64]context.CancelCauseFunc
	children map[string]*poolChild // by digest, starting or ready
}

// serve is the pool's connection loop: it says hello, then pool_state, and
// relays requests until in closes. On return every child has stopped.
func (p *pool) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	hello, err := Identity()
	if err != nil {
		return err
	}
	hello.Name, hello.Capacity = p.opts.Name, p.opts.Capacity
	hello.Pool = &PoolHello{OS: goruntime.GOOS, Arch: goruntime.GOARCH, Warm: p.opts.Warm, Own: p.ownPath != ""}
	ctx, stop := context.WithCancelCause(ctx)
	s := &poolSession{p: p, ctx: ctx, out: out, running: map[uint64]context.CancelCauseFunc{}, children: map[string]*poolChild{}}
	if p.onSession != nil {
		p.onSession(s)
	}
	defer func() {
		stop(errors.New("the pool's connection ended"))
		s.shutdown()
	}()
	if err := s.send(&message{Type: typeHello, Hello: &hello}); err != nil {
		return err
	}
	s.sendState()
	for {
		m, err := readFrame(in)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch m.Type {
		case typeCancel:
			s.cancel(m)
		case typeExecute, typeInspect, typeOTTL:
			s.admit(m)
		default:
			s.track(func() {
				s.fail(m.ID, fmt.Errorf("%w: unknown worker request %q", engine.ErrInvalidRequest, m.Type))
			})
		}
	}
}

func (s *poolSession) send(m *message) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeFrame(s.out, m)
}

// fail answers a request with an error.
func (s *poolSession) fail(id uint64, err error) {
	_ = s.send(&message{ID: id, Type: typeResult, Error: encodeError(err)})
}

// goLocked runs f tracked by the session unless it is closing; mu is held.
func (s *poolSession) goLocked(f func()) bool {
	if s.closed {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
	return true
}

func (s *poolSession) track(f func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goLocked(f)
}

// sendState tells the server which runtimes are running, when that changed
// since the last message.
func (s *poolSession) sendState() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.mu.Lock()
	var warm []string
	for _, ch := range s.children {
		if ch.c != nil && !ch.removed {
			warm = append(warm, ch.identity)
		}
	}
	s.mu.Unlock()
	slices.Sort(warm)
	warm = slices.Compact(warm)
	if s.stateSent && slices.Equal(warm, s.lastState) {
		return
	}
	s.stateSent, s.lastState = true, warm
	_ = s.send(&message{Type: typePoolState, Warm: warm})
}

// validatePoolRequest checks a request and the runtime it names.
func validatePoolRequest(m *message) error {
	b := m.Binary
	switch {
	case m.ID == 0:
		return fmt.Errorf("%w: request ID %d is missing", engine.ErrInvalidRequest, m.ID)
	case b == nil:
		return nil // the pool's own build
	case strings.TrimSpace(b.Identity) == "" || b.URL == "":
		return fmt.Errorf("%w: the runtime binary needs an identity and a URL", engine.ErrInvalidRequest)
	case b.Size <= 0 || b.Size > poolBinaryLimit:
		return fmt.Errorf("%w: runtime binary of %d bytes", engine.ErrInvalidRequest, b.Size)
	}
	if _, ok := digestHex(b.Digest); !ok {
		return fmt.Errorf("%w: runtime binary digest %q is not sha256:<hex>", engine.ErrInvalidRequest, b.Digest)
	}
	return nil
}

// admit registers a request and relays it on its own goroutine; one beyond
// capacity, a duplicate and an invalid one are refused.
func (s *poolSession) admit(m *message) {
	err := validatePoolRequest(m)
	s.mu.Lock()
	if err == nil {
		if _, dup := s.running[m.ID]; dup {
			err = fmt.Errorf("%w: request ID %d is in use", engine.ErrInvalidRequest, m.ID)
		} else if len(s.running) >= s.p.opts.Capacity {
			err = fmt.Errorf("%w: the worker already runs %d requests, its capacity", engine.ErrInvalidRequest, s.p.opts.Capacity)
		}
	}
	if err != nil {
		s.goLocked(func() { s.fail(m.ID, err) })
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancelCause(s.ctx)
	s.running[m.ID] = cancel
	s.goLocked(func() {
		defer func() {
			cancel(nil)
			s.mu.Lock()
			delete(s.running, m.ID)
			s.mu.Unlock()
		}()
		s.relay(ctx, m)
	})
	s.mu.Unlock()
}

// cancel ends a request: while it waits for its child to start it fails,
// and once it runs the cancel is relayed to the child.
func (s *poolSession) cancel(m *message) {
	s.mu.Lock()
	cancel := s.running[m.ID]
	s.mu.Unlock()
	if cancel == nil {
		return // finished already
	}
	cause := context.Canceled
	if m.DeadlineExceeded {
		cause = context.DeadlineExceeded
	}
	cancel(cause)
}

// relay runs a request on its runtime's child: it forwards the frame under
// a fresh child-side ID, relays the child's events and its result under the
// server's ID, and, when ctx ends, has the child cancel the request.
func (s *poolSession) relay(ctx context.Context, m *message) {
	if m.Deadline != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, *m.Deadline)
		defer cancel()
	}
	ch, err := s.acquire(ctx, m.Binary)
	if err != nil {
		s.fail(m.ID, err)
		return
	}
	defer s.release(ch)
	fwd := *m
	fwd.ID, fwd.Binary = 0, nil
	r, err := ch.c.exchange(ctx, &fwd, func(raw json.RawMessage) {
		_ = s.send(&message{ID: m.ID, Type: typeEvent, Event: raw})
	}, nil)
	if err != nil {
		// The child exited: the request failed with the runtime.
		s.fail(m.ID, err)
		return
	}
	_ = s.send(&message{ID: m.ID, Type: typeResult, Result: r.Result, ResultDigest: r.ResultDigest, ResultSize: r.ResultSize,
		Error: r.Error, Log: r.Log, LogTruncated: r.LogTruncated})
}

// acquire returns the ready child of a binary for a request, starting it
// when no request did; requests that arrive during a start share it. The
// child counts the request as active until release.
func (s *poolSession) acquire(ctx context.Context, bin *PoolBinary) (*poolChild, error) {
	if bin == nil {
		if s.p.ownPath == "" {
			return nil, fmt.Errorf("%w: this pool cannot run its own build", engine.ErrRuntimeUnavailable)
		}
		bin = &PoolBinary{Identity: s.p.ownIdentity, Digest: ownDigest}
	}
	if err := s.p.suspectErr(bin.Digest); err != nil {
		return nil, err
	}
	h, _ := digestHex(bin.Digest)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: the pool's connection ended", engine.ErrRuntimeUnavailable)
	}
	ch := s.children[bin.Digest]
	if ch != nil && ch.c != nil {
		select {
		case <-ch.c.exited:
			s.dropLocked(ch) // died before its watcher ran
			ch = nil
		default:
		}
	}
	start := ch == nil
	if start {
		ch = &poolChild{digest: bin.Digest, hex: h, identity: bin.Identity, ready: make(chan struct{})}
		s.children[bin.Digest] = ch
		s.p.pin(h)
	}
	ch.active++
	ch.idleGen++
	if ch.stopIdle != nil {
		ch.stopIdle()
		ch.stopIdle = nil
	}
	s.seq++
	ch.seq = s.seq
	if start {
		b := *bin
		s.goLocked(func() { s.startChild(ch, &b) })
	}
	s.mu.Unlock()
	s.p.touch(h)
	select {
	case <-ch.ready:
	case <-ctx.Done():
		s.release(ch)
		return nil, context.Cause(ctx)
	}
	if ch.err != nil {
		s.release(ch)
		return nil, ch.err
	}
	if ch.identity != bin.Identity {
		s.release(ch)
		err := fmt.Errorf("%w: runtime binary %s runs as runtime %s, not the assigned %s", engine.ErrRuntimeUnavailable, shortDigest(bin.Digest), shortDigest(ch.identity), shortDigest(bin.Identity))
		s.p.markSuspect(bin.Digest, err)
		return nil, suspectError{err}
	}
	return ch, nil
}

// dropLocked takes a child that exited out of the set; mu is held. The
// caller sees to its pin.
func (s *poolSession) dropLocked(ch *poolChild) {
	ch.removed = true
	if s.children[ch.digest] == ch {
		delete(s.children, ch.digest)
	}
	if ch.stopIdle != nil {
		ch.stopIdle()
		ch.stopIdle = nil
	}
	s.goLocked(func() {
		s.p.unpin(ch.hex)
		s.p.evictCache()
		s.sendState()
	})
}

// startChild fetches and starts a child and wakes the requests waiting for
// it.
func (s *poolSession) startChild(ch *poolChild, bin *PoolBinary) {
	c, suspect, err := s.p.spawn(s.ctx, bin, ch.hex)
	s.mu.Lock()
	if err == nil && s.closed {
		err = fmt.Errorf("%w: the pool's connection ended", engine.ErrRuntimeUnavailable)
		defer stopClient(c)
		c = nil
	}
	if err != nil {
		ch.err, ch.removed = err, true
		if s.children[ch.digest] == ch {
			delete(s.children, ch.digest)
		}
		if suspect && ch.digest != ownDigest {
			s.p.markSuspect(ch.digest, err)
			ch.err = suspectError{err}
		}
		s.p.unpin(ch.hex)
		close(ch.ready)
		s.mu.Unlock()
		s.p.evictCache()
		return
	}
	ch.c = c
	close(ch.ready)
	var victims []*poolChild
	if ch.active == 0 { // every request that wanted it gave up
		victims = s.settleLocked(ch)
	}
	s.goLocked(func() {
		<-c.exited
		s.exited(ch)
	})
	s.mu.Unlock()
	s.p.logf("runtime %s started from binary %s", shortDigest(ch.identity), shortDigest(ch.digest))
	s.p.evictCache()
	s.retire(victims)
	s.sendState()
}

// exited handles a child that is gone: it leaves the set unless it was
// already stopped on purpose.
func (s *poolSession) exited(ch *poolChild) {
	s.mu.Lock()
	if ch.removed {
		s.mu.Unlock()
		return
	}
	ch.removed = true
	if s.children[ch.digest] == ch {
		delete(s.children, ch.digest)
	}
	if ch.stopIdle != nil {
		ch.stopIdle()
		ch.stopIdle = nil
	}
	s.mu.Unlock()
	s.p.logf("runtime %s exited: %v", shortDigest(ch.identity), exitReason(ch.c))
	s.p.unpin(ch.hex)
	s.p.evictCache()
	s.sendState()
}

// release ends a request's use of a child.
func (s *poolSession) release(ch *poolChild) {
	s.mu.Lock()
	ch.active--
	s.seq++
	ch.seq = s.seq
	var victims []*poolChild
	if ch.active == 0 && !ch.removed && ch.c != nil {
		victims = s.settleLocked(ch)
	}
	s.mu.Unlock()
	if len(victims) > 0 {
		s.retire(victims)
		s.sendState()
	}
}

// settleLocked runs when ch became idle: it arms the idle timer and takes
// the least recently used idle children out of the set while more than Warm
// run. It returns the children to stop; mu is held.
func (s *poolSession) settleLocked(ch *poolChild) []*poolChild {
	var victims []*poolChild
	for len(s.children) > s.p.opts.Warm {
		var lru *poolChild
		for _, c := range s.children {
			if c.c != nil && c.active == 0 && (lru == nil || c.seq < lru.seq) {
				lru = c
			}
		}
		if lru == nil {
			break
		}
		lru.removed = true
		delete(s.children, lru.digest)
		if lru.stopIdle != nil {
			lru.stopIdle()
			lru.stopIdle = nil
		}
		victims = append(victims, lru)
	}
	if !ch.removed && s.p.opts.Idle > 0 {
		ch.idleGen++
		gen := ch.idleGen
		ch.stopIdle = s.p.afterFunc(s.p.opts.Idle, func() { s.idleExpired(ch, gen) })
	}
	return victims
}

// idleExpired stops a child that nobody used for Idle.
func (s *poolSession) idleExpired(ch *poolChild, gen uint64) {
	s.mu.Lock()
	if s.closed || ch.removed || ch.active > 0 || ch.idleGen != gen {
		s.mu.Unlock()
		return
	}
	ch.removed = true
	delete(s.children, ch.digest)
	ch.stopIdle = nil
	s.mu.Unlock()
	s.retire([]*poolChild{ch})
	s.sendState()
}

// retire stops children that already left the set, in the background.
func (s *poolSession) retire(victims []*poolChild) {
	for _, ch := range victims {
		s.track(func() { s.stopChild(ch) })
	}
}

func (s *poolSession) stopChild(ch *poolChild) {
	stopClient(ch.c)
	s.p.unpin(ch.hex)
	s.p.evictCache()
	s.p.logf("runtime %s stopped", shortDigest(ch.identity))
}

// shutdown ends the connection's work: it stops every child, which cancels
// what they run, and waits for the requests and starts in progress. The
// cache stays.
func (s *poolSession) shutdown() {
	s.mu.Lock()
	s.closed = true
	var stopping []*poolChild
	for _, ch := range s.children {
		ch.removed = true
		if ch.stopIdle != nil {
			ch.stopIdle()
			ch.stopIdle = nil
		}
		if ch.c != nil {
			stopping = append(stopping, ch)
		}
	}
	clear(s.children)
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, ch := range stopping {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.stopChild(ch)
		}()
	}
	wg.Wait()
	s.wg.Wait()
}
