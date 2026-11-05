package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lineahq/linea/internal/debugger/release"
)

// Builders dial the server as workers do, on their own route
// and with their own token: a builder started with --connect opens GET
// <server>/v1/builders/connect with the build token as a bearer token. The
// WebSocket carries the worker protocol's frames with the builder's
// messages: the builder's hello, then build requests from the server, each
// answered by build_log lines and one result; a cancel ends a build early,
// which the builder still answers with a result. A stopping builder sends
// drain: the server gives it no new build and, once the
// builds it gave have ended, answers drain, and the builder disconnects. A
// builder never serves runs and is never a worker.

// BuilderPath ends the URL builders dial.
const BuilderPath = "/v1/builders/connect"

// BuilderProtocol is the builder link's version; a server and a builder
// talk only when theirs are equal.
const BuilderProtocol = 1

// cancelGrace is how long the server waits for a cancelled build's result
// before it gives up on the builder.
var cancelGrace = 30 * time.Second

// ErrBuilderVersion means a builder of another product version dialed: it
// would build another version's worker.
var ErrBuilderVersion = errors.New("builder version mismatch")

// BuilderHello is a builder's first message.
type BuilderHello struct {
	Protocol       int    `json:"protocol"`
	ProductVersion string `json:"product_version"`
	GoVersion      string `json:"go_version"`
	// OS and Arch are the platform the builder builds for.
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// Name names the builder in logs and leases; Capacity is how many
	// builds it runs at once, at least one.
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}

// BuildInput is one input of a build at a presigned URL: the manifest
// (Module empty) or a module's source archive.
type BuildInput struct {
	Module string `json:"module,omitempty"`
	URL    string `json:"url"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// BuildJob is a build the server gives a builder.
type BuildJob struct {
	// Build and Attempt identify the lease the builder works under.
	Build   string `json:"build"`
	Attempt int    `json:"attempt"`
	// Name is the runtime's name.
	Name     string       `json:"name"`
	Manifest BuildInput   `json:"manifest"`
	Sources  []BuildInput `json:"sources,omitempty"`
	// CollectorVersion is the manifest's Collector release; Verified is
	// false outside the supported ones.
	CollectorVersion string `json:"collector_version"`
	Verified         bool   `json:"verified"`
	// BinaryURL takes the built binary with a PUT; it is at most
	// BinaryLimit bytes.
	BinaryURL   string    `json:"binary_url"`
	BinaryLimit int64     `json:"binary_limit"`
	Deadline    time.Time `json:"deadline"`
	// Export, when set, asks the builder to also push an image of the
	// binary. The registry credentials are the builder's own.
	Export *BuildExport `json:"export,omitempty"`
}

// BuildExport names where a build's image goes: Repository (a full image
// repository name, without tag) and Tag.
type BuildExport struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

// BuildResult is how a build ended: Error empty with the uploaded binary's
// digest and size and the built worker's hello, or Error set. Cancelled
// says the build stopped because it was cancelled.
type BuildResult struct {
	Error        string `json:"error,omitempty"`
	Cancelled    bool   `json:"cancelled,omitempty"`
	BinaryDigest string `json:"binary_digest,omitempty"`
	BinarySize   int64  `json:"binary_size,omitempty"`
	Hello        *Hello `json:"hello,omitempty"`
	OS           string `json:"os,omitempty"`
	Arch         string `json:"arch,omitempty"`
	// Image is the exported image by digest (repository@sha256:...), and
	// ExportError why the export failed; a failed export leaves the build
	// done, with its binary.
	Image       string `json:"image,omitempty"`
	ExportError string `json:"export_error,omitempty"`
}

// BuilderConn is a connected builder, as the server holds it. It is safe
// for concurrent use.
type BuilderConn struct {
	s        *stream
	conn     *websocket.Conn
	hello    BuilderHello
	endpoint string

	mu     sync.Mutex
	nextID uint64
	calls  map[uint64]*buildCall
	err    error
	done   chan struct{}
	// draining is closed when the builder announces it is stopping.
	draining  chan struct{}
	drainOnce sync.Once
}

type buildCall struct {
	log    func([]string)
	result chan *BuildResult
}

// AcceptBuilder checks a dialing builder's token with authorize, upgrades
// its request and waits for its hello. A builder of another protocol or
// product version is refused. On a refused token or a failed upgrade it has
// answered the request and returns the error.
func AcceptBuilder(w http.ResponseWriter, r *http.Request, authorize func(token string) bool) (*BuilderConn, error) {
	if r.Method != http.MethodGet {
		http.Error(w, "builders connect with GET", http.StatusMethodNotAllowed)
		return nil, errors.New("builder connection: not a GET")
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !authorize(token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="linea-builders"`)
		http.Error(w, "the build token is wrong", http.StatusUnauthorized)
		return nil, ErrUnauthorized
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	b := &BuilderConn{s: newStream(conn), conn: conn, endpoint: r.RemoteAddr, calls: map[uint64]*buildCall{}, done: make(chan struct{}),
		draining: make(chan struct{})}
	refuse := func(err error) (*BuilderConn, error) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, clip(err.Error(), 120)), time.Now().Add(time.Second))
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	m, err := readFrame(b.s)
	switch {
	case err != nil:
		return refuse(fmt.Errorf("builder hello: %w", err))
	case m.Type != typeHello || m.BuilderHello == nil:
		return refuse(fmt.Errorf("%w: the first message is not a builder hello", errFrame))
	case m.BuilderHello.Protocol != BuilderProtocol:
		return refuse(fmt.Errorf("%w: the builder speaks protocol %d, this server speaks %d", ErrProtocol, m.BuilderHello.Protocol, BuilderProtocol))
	case m.BuilderHello.ProductVersion != release.ProductVersion:
		return refuse(fmt.Errorf("%w: the builder is %s, this server is %s", ErrBuilderVersion, m.BuilderHello.ProductVersion, release.ProductVersion))
	case strings.TrimSpace(m.BuilderHello.Name) == "" || len(m.BuilderHello.Name) > 128 || m.BuilderHello.Capacity < 1:
		return refuse(fmt.Errorf("%w: a builder needs a name of at most 128 bytes and a capacity of at least 1", errFrame))
	}
	b.hello = *m.BuilderHello
	keepAlive(b.done, conn)
	go b.read()
	return b, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Hello is what the builder announced.
func (b *BuilderConn) Hello() BuilderHello { return b.hello }

// Endpoint is the builder's remote address.
func (b *BuilderConn) Endpoint() string { return b.endpoint }

// Done is closed once the connection is gone.
func (b *BuilderConn) Done() <-chan struct{} { return b.done }

// Draining is closed once the builder announced it is stopping: it still
// runs the builds it was given, and wants no new one.
func (b *BuilderConn) Draining() <-chan struct{} { return b.draining }

// Drained tells a draining builder that the server holds no build for it
// any more, so it may disconnect.
func (b *BuilderConn) Drained() error {
	return writeFrame(b.s, &message{Type: typeDrain})
}

// Close ends the connection; builds still waiting fail.
func (b *BuilderConn) Close() error {
	_ = b.s.closeWrite()
	return b.conn.Close()
}

// read dispatches the builder's messages until the connection ends, then
// fails every waiting build.
func (b *BuilderConn) read() {
	var readErr error
	for {
		m, err := readFrame(b.s)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
		if m.Type == typeDrain {
			b.drainOnce.Do(func() { close(b.draining) })
			continue
		}
		b.mu.Lock()
		c := b.calls[m.ID]
		if m.Type == typeResult {
			delete(b.calls, m.ID)
		}
		b.mu.Unlock()
		if c == nil {
			continue // a build the server stopped waiting for
		}
		switch {
		case m.Type == typeBuildLog && len(m.Lines) > 0:
			c.log(m.Lines)
		case m.Type == typeResult:
			r := m.BuildResult
			if r == nil {
				r = &BuildResult{Error: "the builder answered without a result"}
			}
			c.result <- r
		}
	}
	_ = b.conn.Close()
	b.mu.Lock()
	b.err = readErr
	for id, c := range b.calls {
		close(c.result)
		delete(b.calls, id)
	}
	b.calls = nil
	b.mu.Unlock()
	close(b.done)
}

// gone is the error of a build the connection ended under.
func (b *BuilderConn) gone() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return fmt.Errorf("builder %s: connection lost: %w", b.hello.Name, b.err)
	}
	return fmt.Errorf("builder %s: connection closed", b.hello.Name)
}

// Build runs job on the builder and returns its result. log receives the
// build's log lines as they arrive, on the connection's reader: it must
// not block for long. When ctx ends, Build cancels the build and waits up
// to cancelGrace for its result; the error is only the connection's or a
// builder that did not answer.
func (b *BuilderConn) Build(ctx context.Context, job BuildJob, log func([]string)) (BuildResult, error) {
	if log == nil {
		log = func([]string) {}
	}
	c := &buildCall{log: log, result: make(chan *BuildResult, 1)}
	b.mu.Lock()
	if b.calls == nil {
		b.mu.Unlock()
		return BuildResult{}, b.gone()
	}
	b.nextID++
	id := b.nextID
	b.calls[id] = c
	b.mu.Unlock()
	forget := func() {
		b.mu.Lock()
		if b.calls != nil {
			delete(b.calls, id)
		}
		b.mu.Unlock()
	}
	if err := writeFrame(b.s, &message{ID: id, Type: typeBuild, Build: &job}); err != nil {
		forget()
		return BuildResult{}, fmt.Errorf("builder %s: send build: %w", b.hello.Name, err)
	}
	done := ctx.Done()
	var grace <-chan time.Time
	for {
		select {
		case r, ok := <-c.result:
			if !ok {
				return BuildResult{}, b.gone()
			}
			return *r, nil
		case <-done:
			done = nil
			grace = time.After(cancelGrace)
			_ = writeFrame(b.s, &message{ID: id, Type: typeCancel})
		case <-grace:
			forget()
			return BuildResult{}, fmt.Errorf("builder %s did not answer a cancelled build within %s: %w", b.hello.Name, cancelGrace, context.Cause(ctx))
		}
	}
}

// BuilderOptions configure ServeBuilds.
type BuilderOptions struct {
	// URL is the server's base URL, as for ConnectOptions.
	URL string
	// CA verifies the server's certificate; empty uses the system roots.
	CA []byte
	// Token is the build token; it is required.
	Token string
	// Hello is what the builder announces; ServeBuilds sets its protocol
	// and product version. Name and a Capacity of at least 1 are required.
	Hello BuilderHello
	// Log receives a line per connection; nil discards them.
	Log io.Writer
	// Drain, when closed, stops the builder gracefully: it
	// asks the server for no new build, finishes the builds it runs and
	// returns once the server holds none for it, or once the connection
	// ends. Ending ctx instead cancels the builds.
	Drain <-chan struct{}
}

// BuildFunc runs one build: it logs lines with log and returns the result.
// It stops when ctx ends, which is a cancellation or a lost connection.
type BuildFunc func(ctx context.Context, job BuildJob, log func(string)) BuildResult

// ServeBuilds dials the server and runs the builds it gives with build,
// reconnecting after a lost connection, until ctx ends or opts.Drain is
// closed and drained; it then returns nil once the builds it ran have
// stopped. A refused token ends it with ErrUnauthorized.
func ServeBuilds(ctx context.Context, opts BuilderOptions, build BuildFunc) error {
	if opts.Token == "" {
		return errors.New("builder: a builder needs a build token")
	}
	if strings.TrimSpace(opts.Hello.Name) == "" {
		return errors.New("builder: a builder needs a name")
	}
	if opts.Hello.Capacity < 1 {
		return errors.New("builder: a builder's capacity is at least 1")
	}
	hello := opts.Hello
	hello.Protocol, hello.ProductVersion = BuilderProtocol, release.ProductVersion
	t, err := newDialTarget(opts.URL, opts.CA, opts.Token, BuilderPath)
	if err != nil {
		return err
	}
	// A drain ends the redial loop: at once while no connection is held,
	// and after the held connection ends otherwise. Its builds are not
	// cancelled by that: they ended before the server answered drain.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	var (
		mu   sync.Mutex
		held bool
	)
	draining := func() bool {
		select {
		case <-opts.Drain:
			return true
		default:
			return false
		}
	}
	if opts.Drain != nil {
		go func() {
			select {
			case <-opts.Drain:
			case <-ctx.Done():
				return
			}
			mu.Lock()
			if !held {
				stop()
			}
			mu.Unlock()
		}()
	}
	return redial(ctx, t, opts.Log, func(ctx context.Context, conn *websocket.Conn, logf func(string, ...any)) error {
		mu.Lock()
		if draining() {
			mu.Unlock()
			stop()
			return nil
		}
		held = true
		mu.Unlock()
		defer func() {
			mu.Lock()
			held = false
			mu.Unlock()
			if draining() {
				stop()
			}
		}()
		logf("connected to %s as builder %s (capacity %d)", t.target, hello.Name, hello.Capacity)
		err := serveBuilds(ctx, newStream(conn), hello, build, opts.Drain)
		if errors.Is(err, errDrained) {
			logf("drained: the server holds no build for builder %s", hello.Name)
			return nil
		}
		var closed *websocket.CloseError
		if errors.As(err, &closed) && closed.Code == websocket.ClosePolicyViolation {
			return fmt.Errorf("the server refused the builder: %s", closed.Text)
		}
		return err
	})
}

// errDrained ends a connection the server drained.
var errDrained = errors.New("the server drained the builder")

// serveBuilds sends the hello and runs builds until the connection ends or
// the server answers a drain; it then cancels the builds still running and
// waits for them. When drain is closed it sends drain; builds the server
// sent before it read that still run.
func serveBuilds(ctx context.Context, s *stream, hello BuilderHello, build BuildFunc, drain <-chan struct{}) error {
	if err := writeFrame(s, &message{Type: typeHello, BuilderHello: &hello}); err != nil {
		return err
	}
	ctx, cancelAll := context.WithCancel(ctx)
	if drain != nil {
		go func() {
			select {
			case <-drain:
				_ = writeFrame(s, &message{Type: typeDrain})
			case <-ctx.Done():
			}
		}()
	}
	var (
		mu      sync.Mutex
		running = map[uint64]context.CancelFunc{}
		wg      sync.WaitGroup
	)
	defer func() {
		cancelAll()
		wg.Wait()
	}()
	for {
		m, err := readFrame(s)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("the server closed the connection")
			}
			return err
		}
		switch m.Type {
		case typeDrain:
			return errDrained
		case typeCancel:
			mu.Lock()
			if cancel := running[m.ID]; cancel != nil {
				cancel()
			}
			mu.Unlock()
		case typeBuild:
			id, job := m.ID, m.Build
			mu.Lock()
			full := len(running) >= hello.Capacity
			mu.Unlock()
			if job == nil || full {
				reason := "the builder is at capacity"
				if job == nil {
					reason = "a build request without a build"
				}
				_ = writeFrame(s, &message{ID: id, Type: typeResult, BuildResult: &BuildResult{Error: reason}})
				continue
			}
			bctx, cancel := context.WithCancel(ctx)
			if !job.Deadline.IsZero() {
				bctx, cancel = context.WithDeadline(ctx, job.Deadline)
			}
			mu.Lock()
			running[id] = cancel
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := build(bctx, *job, func(line string) {
					_ = writeFrame(s, &message{ID: id, Type: typeBuildLog, Lines: []string{line}})
				})
				if r.Error != "" && errors.Is(context.Cause(bctx), context.Canceled) && ctx.Err() == nil {
					r.Cancelled = true
				}
				_ = writeFrame(s, &message{ID: id, Type: typeResult, BuildResult: &r})
				mu.Lock()
				delete(running, id)
				mu.Unlock()
				cancel()
			}()
		}
	}
}
