package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/release"
)

// logLimit bounds how much of a worker's log the debugger keeps: the tail
// that explains an exit.
const logLimit = 64 << 10

// ErrWorkerExited means the worker process ended while a request was
// waiting for it.
var ErrWorkerExited = errors.New("worker exited")

// ErrProtocol means the worker speaks another protocol version.
var ErrProtocol = errors.New("worker protocol mismatch")

// Client drives one worker: a process over its standard streams (Start)
// or a remote worker over a connection (Dial). It is safe for concurrent
// use; every request waits only for its own answer.
type Client struct {
	name string
	in   io.Writer
	// closeIn ends the worker's input, which cancels what it still runs;
	// wait waits for the worker to end once its output has; kill stops it
	// when it does not end.
	closeIn func() error
	wait    func() error
	kill    func()
	hello   Hello
	log     *tail
	// endpoint names a connected worker (Accept); empty for a process.
	endpoint string

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	calls   map[uint64]*call
	exitErr error // set once the worker is gone
	exited  chan struct{}
	// warm is a pool worker's last pool_state, and warmed is closed and
	// replaced when it changes.
	warm   []string
	warmed chan struct{}
}

type call struct {
	event  func(json.RawMessage)
	result chan *message
}

// Start runs cmd as a worker and waits for its hello. name identifies the
// worker in errors. cmd must not have its standard streams set. A worker of
// another protocol version is refused and stopped.
func Start(ctx context.Context, name string, cmd *exec.Cmd) (*Client, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := newClient(name, stdin)
	c.closeIn, c.wait = stdin.Close, cmd.Wait
	c.kill = func() { _ = cmd.Process.Kill() }
	cmd.Stderr = c.log
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start worker %s: %w", name, err)
	}
	return c.handshake(ctx, stdout)
}

func newClient(name string, in io.Writer) *Client {
	return &Client{name: name, in: in, log: &tail{}, calls: map[uint64]*call{}, exited: make(chan struct{}), warmed: make(chan struct{})}
}

// handshake reads the worker's output and waits for its hello. A worker
// of another protocol version is refused and stopped.
func (c *Client) handshake(ctx context.Context, out io.Reader) (*Client, error) {
	name := c.name
	hello := make(chan *message, 1)
	go c.read(out, hello)
	select {
	case m := <-hello:
		if m == nil {
			_ = c.Close()
			return nil, c.gone()
		}
		if m.Hello.Protocol != ProtocolVersion {
			_ = c.Close()
			return nil, fmt.Errorf("%w: worker %s speaks protocol %d, this debugger speaks %d", ErrProtocol, name, m.Hello.Protocol, ProtocolVersion)
		}
		c.hello = *m.Hello
		return c, nil
	case <-ctx.Done():
		_ = c.Close()
		return nil, fmt.Errorf("start worker %s: %w", name, context.Cause(ctx))
	}
}

// Hello is what the worker announced.
func (c *Client) Hello() Hello { return c.hello }

// Warm returns the runtime identities a pool worker last said it holds
// running, and a channel closed when that changes.
func (c *Client) Warm() ([]string, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.warm), c.warmed
}

// Exited is closed once the worker is gone: it exited, its connection
// closed or the client was closed.
func (c *Client) Exited() <-chan struct{} { return c.exited }

// FeatureGates are the worker's feature gates; nil when its hello does not
// list them.
func (c *Client) FeatureGates() []release.Gate { return c.hello.Gates }

// Target is the runtime target that bundles for this worker name.
func (c *Client) Target() engine.RuntimeTarget {
	return engine.RuntimeTarget{ID: c.hello.Runtime, Name: c.name}
}

// read dispatches frames until the worker's output ends, then fails every
// waiting request.
func (c *Client) read(stdout io.Reader, hello chan<- *message) {
	first := true
	var readErr error
	for {
		m, err := readFrame(stdout)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
		if first {
			if m.Type != typeHello || m.Hello == nil {
				readErr = fmt.Errorf("%w: expected a hello, got %q", errFrame, m.Type)
				break
			}
			first = false
			hello <- m
			continue
		}
		if m.Type == typePoolState {
			c.mu.Lock()
			c.warm = slices.Clone(m.Warm)
			close(c.warmed)
			c.warmed = make(chan struct{})
			c.mu.Unlock()
			continue
		}
		c.mu.Lock()
		call := c.calls[m.ID]
		if m.Type == typeResult {
			delete(c.calls, m.ID)
		}
		c.mu.Unlock()
		switch {
		case call == nil:
		case m.Type == typeEvent:
			call.event(m.Event)
		case m.Type == typeResult:
			call.result <- m
		}
	}
	waitErr := c.wait()
	c.mu.Lock()
	switch {
	case readErr != nil:
		c.exitErr = readErr
	case waitErr != nil:
		c.exitErr = waitErr
	default:
		c.exitErr = errors.New("exit status 0")
	}
	calls := c.calls
	c.calls = nil
	c.mu.Unlock()
	close(c.exited)
	if first {
		close(hello)
	}
	for _, call := range calls {
		close(call.result)
	}
}

// gone describes the worker's exit, with the tail of its log.
func (c *Client) gone() error {
	c.mu.Lock()
	cause := c.exitErr
	c.mu.Unlock()
	if c.endpoint != "" {
		return &exitError{msg: fmt.Sprintf("worker %s at %s: connection ended (%v)", c.name, c.endpoint, cause), cause: cause}
	}
	msg := fmt.Sprintf("%v: worker %s (%v)", ErrWorkerExited, c.name, cause)
	if log := strings.TrimSpace(c.log.String()); log != "" {
		msg += "; its log ends with: " + log
	}
	return &exitError{msg: msg}
}

// exitError is a worker that is gone or unreachable; cause, when set, is
// why.
type exitError struct {
	msg   string
	cause error
}

func (e *exitError) Error() string { return e.msg }
func (e *exitError) Unwrap() error { return e.cause }
func (e *exitError) Is(target error) bool {
	return target == ErrWorkerExited || target == engine.ErrRuntimeUnavailable
}

// do sends a request and waits for its result. When ctx ends first, the
// worker is asked to cancel the request and do still waits for the result:
// a cancelled run returns only after its Collector has shut down. log, when
// set, receives the log tail the result carries.
func (c *Client) do(ctx context.Context, m *message, event func(json.RawMessage), log *engine.LogTail) (json.RawMessage, error) {
	r, err := c.exchange(ctx, m, event, log)
	if err != nil {
		return nil, err
	}
	if r.ResultDigest != "" {
		return nil, fmt.Errorf("worker %s uploaded a result it was given no URL for", c.name)
	}
	return r.Result, decodeError(r.Error)
}

// exchange is do returning the result message itself, for a result the
// worker may have uploaded; its error is only the exchange's.
func (c *Client) exchange(ctx context.Context, m *message, event func(json.RawMessage), log *engine.LogTail) (*message, error) {
	if event == nil {
		event = func(json.RawMessage) {}
	}
	cl := &call{event: event, result: make(chan *message, 1)}
	c.mu.Lock()
	if c.calls == nil {
		c.mu.Unlock()
		return nil, c.gone()
	}
	c.nextID++
	m.ID = c.nextID
	c.calls[m.ID] = cl
	c.mu.Unlock()
	if err := c.send(m); err != nil {
		c.mu.Lock()
		if c.calls != nil {
			delete(c.calls, m.ID)
		}
		c.mu.Unlock()
		select {
		case <-c.exited:
			return nil, c.gone()
		default:
			return nil, fmt.Errorf("worker %s: send %s: %w", c.name, m.Type, err)
		}
	}
	done := ctx.Done()
	for {
		select {
		case r, ok := <-cl.result:
			if !ok {
				return nil, c.gone()
			}
			if log != nil && (r.Log != "" || r.LogTruncated) {
				log.Append(r.Log, r.LogTruncated)
			}
			return r, nil
		case <-done:
			done = nil
			cancel := &message{ID: m.ID, Type: typeCancel, DeadlineExceeded: errors.Is(context.Cause(ctx), context.DeadlineExceeded)}
			_ = c.send(cancel)
		}
	}
}

func (c *Client) send(m *message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.in, m)
}

// Execute runs a prepared request on the worker, as engine.Execute runs it
// in process: events reach req.Observer as they are recorded, and the
// report or error comes back when the worker's Collector has shut down. A
// request without a runtime is stamped with this worker's target.
func (c *Client) Execute(ctx context.Context, req engine.Request) (*engine.Report, error) {
	if req.Runtime == nil {
		t := c.Target()
		req.Runtime = &t
	}
	bundle, _, err := engine.EncodeBundle(req)
	if err != nil {
		return nil, err
	}
	return c.ExecuteJob(ctx, Job{RunID: req.RunID, Bundle: bundle, Observer: req.Observer, Log: req.Log})
}

// Job is an admitted run as a server assigns it: its bundle
// inline, or at a presigned URL with its digest and size, and where a
// result above InlineLimit goes.
type Job struct {
	RunID        string
	Bundle       []byte
	BundleURL    string
	BundleDigest string
	BundleSize   int64
	// ResultURL takes a large result; FetchResult then reads it back by
	// the digest and size the worker reports. Both or neither are set.
	ResultURL   string
	FetchResult func(ctx context.Context, digest string, size int64) ([]byte, error)
	Observer    func(instrument.Event)
	Log         *engine.LogTail
	// Binary is the runtime a pool worker runs the job on; nil for a
	// worker of one runtime.
	Binary *PoolBinary
}

// ExecuteJob runs an encoded bundle on the worker, as Execute runs a
// request.
func (c *Client) ExecuteJob(ctx context.Context, job Job) (*engine.Report, error) {
	if (job.ResultURL == "") != (job.FetchResult == nil) {
		return nil, fmt.Errorf("%w: a job's result URL and result reader go together", engine.ErrInvalidRequest)
	}
	m := &message{Type: typeExecute, Bundle: job.Bundle, BundleURL: job.BundleURL, BundleDigest: job.BundleDigest,
		BundleSize: job.BundleSize, ResultURL: job.ResultURL, RunID: job.RunID, Binary: job.Binary}
	if deadline, ok := ctx.Deadline(); ok {
		m.Deadline = &deadline
	}
	var eventErr error
	r, err := c.exchange(ctx, m, func(raw json.RawMessage) {
		var e instrument.Event
		if err := engine.Decode(raw, &e); err != nil {
			eventErr = errors.Join(eventErr, err)
			return
		}
		if job.Observer != nil {
			job.Observer(e)
		}
	}, job.Log)
	if err != nil {
		return nil, err
	}
	if err := decodeError(r.Error); err != nil {
		return nil, err
	}
	if eventErr != nil {
		return nil, fmt.Errorf("worker %s sent an unreadable event: %w", c.name, eventErr)
	}
	raw := r.Result
	if r.ResultDigest != "" {
		if job.FetchResult == nil {
			return nil, fmt.Errorf("worker %s uploaded a result it was given no URL for", c.name)
		}
		// The worker is done with the run; reading its result back is not
		// cancelled with it.
		if raw, err = job.FetchResult(context.WithoutCancel(ctx), r.ResultDigest, r.ResultSize); err != nil {
			return nil, fmt.Errorf("worker %s: read its uploaded report: %w", c.name, err)
		}
	}
	var rep engine.Report
	if err := engine.Decode(raw, &rep); err != nil {
		return nil, fmt.Errorf("worker %s sent an unreadable report: %w", c.name, err)
	}
	return &rep, nil
}

// Inspect completes an inspection captured on the host.
func (c *Client) Inspect(ctx context.Context, in *engine.InspectionInput) (*engine.Inspection, error) {
	return c.InspectOn(ctx, nil, in)
}

// InspectOn is Inspect on a pool worker's runtime b; nil is Inspect.
func (c *Client) InspectOn(ctx context.Context, b *PoolBinary, in *engine.InspectionInput) (*engine.Inspection, error) {
	input, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	raw, err := c.do(ctx, &message{Type: typeInspect, Input: input, Binary: b}, nil, nil)
	if err != nil {
		return nil, err
	}
	var out engine.Inspection
	if err := engine.Decode(raw, &out); err != nil {
		return nil, fmt.Errorf("worker %s sent an unreadable inspection: %w", c.name, err)
	}
	return &out, nil
}

// EvaluateOTTL completes an OTTL preview captured on the host.
func (c *Client) EvaluateOTTL(ctx context.Context, in *engine.OTTLInput) (*engine.OTTLEvaluation, error) {
	return c.EvaluateOTTLOn(ctx, nil, in)
}

// EvaluateOTTLOn is EvaluateOTTL on a pool worker's runtime b; nil is
// EvaluateOTTL.
func (c *Client) EvaluateOTTLOn(ctx context.Context, b *PoolBinary, in *engine.OTTLInput) (*engine.OTTLEvaluation, error) {
	input, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	raw, err := c.do(ctx, &message{Type: typeOTTL, Input: input, Binary: b}, nil, nil)
	if err != nil {
		return nil, err
	}
	var out engine.OTTLEvaluation
	if err := engine.Decode(raw, &out); err != nil {
		return nil, fmt.Errorf("worker %s sent an unreadable OTTL evaluation: %w", c.name, err)
	}
	return &out, nil
}

// Close closes the worker's input, which cancels what it still runs, and
// waits for it to exit; after a grace period it is killed.
func (c *Client) Close() error {
	_ = c.closeIn()
	select {
	case <-c.exited:
	case <-time.After(30 * time.Second):
		c.kill()
		<-c.exited
	}
	return nil
}

// Log returns the tail of the worker's log.
func (c *Client) Log() string { return c.log.String() }

// tail keeps the last logLimit bytes written to it.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - logLimit; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
