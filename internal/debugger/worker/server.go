package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/release"
)

// unverified is "true" in a runtime built for a Collector release outside
// the debugger's support table; runtime builds set it at link time
// (container.compileArgs).
var unverified string

// custom is "true" in every runtime build's worker (container.compileArgs),
// so only a release's own worker says it is the built-in runtime.
var custom string

// errNoIdentity means the worker binary carries no Go build information, so
// its runtime cannot be identified.
var errNoIdentity = errors.New("worker: the binary has no build information")

// Identity describes this process as a worker: its registry, its feature
// gates and its runtime identity, a digest over those and the linked
// modules, so two builds that could behave differently never share one. It
// describes the in-process Collector (engine.Use) and fails with
// engine.ErrNoCollector in a process without one.
func Identity() (Hello, error) {
	c, err := engine.InProcess()
	if err != nil {
		return Hello{}, err
	}
	h := Hello{
		Protocol:         ProtocolVersion,
		ProductVersion:   release.ProductVersion,
		CollectorVersion: release.CollectorVersion,
		Modules:          slices.Clone(release.Modules),
		FeatureGates:     c.ChangedFeatureGates(),
		Gates:            c.FeatureGates(),
		MissingFeatures:  engine.MissingFeatures(),
		Verified:         unverified != "true",
	}
	// A provider without a name, or two with one, keeps the worker from
	// starting.
	plugins, err := c.Plugins()
	if err != nil {
		return Hello{}, err
	}
	h.Providers = engine.Providers(plugins)
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Hello{}, errNoIdentity
	}
	deps := make([]string, 0, len(info.Deps))
	for _, m := range info.Deps {
		if m.Replace != nil {
			m = m.Replace
		}
		deps = append(deps, m.Path+" "+m.Version+" "+m.Sum)
	}
	slices.Sort(deps)
	identity, err := json.Marshal(struct {
		Hello
		Main string   `json:"main"`
		Deps []string `json:"deps"`
	}{h, info.Main.Path + "@" + info.Main.Version, deps})
	if err != nil {
		return Hello{}, err
	}
	sum := sha256.Sum256(identity)
	h.Runtime = "sha256:" + hex.EncodeToString(sum[:])
	h.BuiltIn = custom != "true"
	return h, nil
}

// Serve answers requests read from in until it is closed, writing the hello
// first. Requests run concurrently, so an inspection is answered while a run
// executes. When in closes, Serve cancels what is still running, waits for
// it and returns nil.
func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	return serve(ctx, in, out, "", 0)
}

// inlineLimit is InlineLimit; tests lower it to upload small results.
var inlineLimit = InlineLimit

// serve is Serve for a worker that announces a name and a capacity: a
// worker that dialed a server (Connect). A request beyond capacity is
// refused; zero bounds nothing.
func serve(ctx context.Context, in io.Reader, out io.Writer, name string, capacity int) error {
	hello, err := Identity()
	if err != nil {
		return err
	}
	hello.Name, hello.Capacity = name, capacity
	s := &server{out: out, hello: hello, running: map[uint64]context.CancelCauseFunc{}}
	if err := s.send(&message{Type: typeHello, Hello: &hello}); err != nil {
		return err
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		m, err := readFrame(in)
		if errors.Is(err, io.EOF) {
			stop(errors.New("the debugger closed the worker's input"))
			return nil
		}
		if err != nil {
			stop(err)
			return err
		}
		switch m.Type {
		case typeCancel:
			s.cancel(m)
		case typeExecute, typeInspect, typeOTTL:
			reqCtx, cancel := s.start(ctx, m)
			if reqCtx == nil {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer s.finish(m.ID, cancel)
				var log *engine.LogTail
				if m.Type == typeExecute {
					log = &engine.LogTail{}
				}
				result, err := s.handle(reqCtx, m, log)
				s.reply(m.ID, result, err, log, m.ResultURL)
			}()
		default:
			s.reply(m.ID, nil, fmt.Errorf("%w: unknown worker request %q", engine.ErrInvalidRequest, m.Type), nil, "")
		}
	}
}

type server struct {
	out   io.Writer
	hello Hello

	writeMu sync.Mutex
	mu      sync.Mutex
	running map[uint64]context.CancelCauseFunc
}

func (s *server) send(m *message) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeFrame(s.out, m)
}

// reply answers a request; log, when set, is an execution's log tail, sent
// with its result or error. A result above InlineLimit goes to resultURL,
// when the request gave one, and the reply names its digest and size.
func (s *server) reply(id uint64, result any, err error, log *engine.LogTail, resultURL string) {
	m := &message{ID: id, Type: typeResult, Error: encodeError(err)}
	if log != nil {
		m.Log, m.LogTruncated = log.Tail()
	}
	if result != nil {
		raw, merr := json.Marshal(result)
		switch {
		case merr != nil:
			m.Error = encodeError(errors.Join(err, merr))
		case resultURL != "" && len(raw) > inlineLimit:
			if uerr := uploadResult(context.Background(), resultURL, raw); uerr != nil {
				m.Error = encodeError(errors.Join(err, uerr))
			} else {
				m.ResultDigest, m.ResultSize = Digest(raw), int64(len(raw))
			}
		default:
			m.Result = raw
		}
	}
	if werr := s.send(m); werr != nil && m.Result != nil {
		// A result too large for a frame is still answered.
		_ = s.send(&message{ID: id, Type: typeResult, Error: encodeError(werr), Log: m.Log, LogTruncated: m.LogTruncated})
	}
}

// start registers a request; a duplicate ID is refused.
func (s *server) start(ctx context.Context, m *message) (context.Context, context.CancelCauseFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.running[m.ID]; dup || m.ID == 0 {
		go s.reply(m.ID, nil, fmt.Errorf("%w: request ID %d is missing or in use", engine.ErrInvalidRequest, m.ID), nil, "")
		return nil, nil
	}
	if s.hello.Capacity > 0 && len(s.running) >= s.hello.Capacity {
		go s.reply(m.ID, nil, fmt.Errorf("%w: the worker already runs %d requests, its capacity", engine.ErrInvalidRequest, s.hello.Capacity), nil, "")
		return nil, nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	s.running[m.ID] = cancel
	return ctx, cancel
}

func (s *server) finish(id uint64, cancel context.CancelCauseFunc) {
	cancel(nil)
	s.mu.Lock()
	delete(s.running, id)
	s.mu.Unlock()
}

func (s *server) cancel(m *message) {
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

func (s *server) handle(ctx context.Context, m *message, log *engine.LogTail) (any, error) {
	switch m.Type {
	case typeExecute:
		return s.execute(ctx, m, log)
	case typeInspect:
		var in engine.InspectionInput
		if err := json.Unmarshal(m.Input, &in); err != nil {
			return nil, fmt.Errorf("%w: inspection input: %w", engine.ErrInvalidRequest, err)
		}
		return engine.CompleteInspection(ctx, &in)
	default:
		var in engine.OTTLInput
		if err := json.Unmarshal(m.Input, &in); err != nil {
			return nil, fmt.Errorf("%w: OTTL input: %w", engine.ErrInvalidRequest, err)
		}
		return engine.CompleteOTTL(ctx, &in)
	}
}

func (s *server) execute(ctx context.Context, m *message, log *engine.LogTail) (*engine.Report, error) {
	if m.Deadline != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, *m.Deadline)
		defer cancel()
	}
	bundle := m.Bundle
	if m.BundleURL != "" {
		var err error
		if bundle, err = fetchBundle(ctx, m.BundleURL, m.BundleDigest, m.BundleSize); err != nil {
			return nil, err
		}
	}
	req, err := engine.DecodeBundleForRuntime(ctx, bundle, s.hello.Runtime)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.RunID) != "" {
		req.RunID = m.RunID
	}
	req.Observer = func(e instrument.Event) {
		raw, err := json.Marshal(e)
		if err != nil {
			return
		}
		_ = s.send(&message{ID: m.ID, Type: typeEvent, Event: raw})
	}
	req.Log = log
	return engine.Execute(ctx, req)
}
