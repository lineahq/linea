// Package worker runs the engine out of process: a worker
// process holds one runtime's component registry and answers execution,
// inspection and OTTL preview requests from the debugger over its standard
// input and output. The worker never reads host files: it receives captured
// bytes (execution bundles and captured inspection inputs) and returns the
// engine's results, events and coded errors. Its standard error is its log.
package worker

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/release"
)

// ProtocolVersion is the worker protocol this build speaks. A debugger and
// a worker talk only when their versions are equal. Protocol 2 adds the
// worker's name and capacity to its hello, and payloads that travel through
// presigned object-storage URLs.
const ProtocolVersion = 2

// maxFrame bounds one frame: a 64 MiB bundle in base64 with its envelope, or
// a report within the run's evidence budget. A server with object storage
// keeps frames far smaller (InlineLimit).
const maxFrame = 256 << 20

// InlineLimit is the largest bundle or result a server with object storage
// sends inline; a larger one travels through a presigned URL.
const InlineLimit = 1 << 20

// Message types.
const (
	typeHello   = "hello"
	typeExecute = "execute"
	typeInspect = "inspect"
	typeOTTL    = "ottl"
	typeCancel  = "cancel"
	typeEvent   = "event"
	typeResult  = "result"
	// Pool workers (pool.go): the runtimes a pool worker holds warm.
	typePoolState = "pool_state"
	// Builder link (builder.go).
	typeBuild    = "build"
	typeBuildLog = "build_log"
	// drain: from a builder, it is stopping and takes no new build; from
	// the server, it holds no build for the builder any more.
	typeDrain = "drain"
)

// Hello is the worker's first message: what it is and which runtime it
// serves.
type Hello struct {
	Protocol int `json:"protocol"`
	// Runtime is the runtime identity ("sha256:<hex>") over everything below
	// and the worker's build. Bundles name it (engine.RuntimeTarget.ID).
	Runtime          string                `json:"runtime"`
	ProductVersion   string                `json:"product_version"`
	CollectorVersion string                `json:"collector_version"`
	Modules          []string              `json:"modules"`
	FeatureGates     []release.FeatureGate `json:"feature_gates"`
	// Gates are every feature gate the registry holds, which an upgrade
	// check compares between runtimes; absent from older workers.
	Gates []release.Gate `json:"gates,omitempty"`
	// Verified is false for a Collector version outside the debugger's
	// supported ones.
	Verified bool `json:"verified"`
	// BuiltIn says the worker is the built-in runtime of its release: the
	// release's own image, not a runtime build. It is not part of the
	// runtime identity; absent from older workers.
	BuiltIn bool `json:"built_in,omitempty"`
	// MissingFeatures are the optional analysis features the worker lacks,
	// since its manifest does not have their component.
	MissingFeatures []string `json:"missing_features,omitempty"`
	// Providers are the debugger providers of component modules the
	// worker has; absent from older workers.
	Providers []engine.ReportProvider `json:"providers,omitempty"`
	// Name is the runtime name a worker that dials a server serves under
	// (--name); empty over standard streams.
	Name string `json:"name,omitempty"`
	// Capacity is how many requests a worker that dials a server runs at
	// once; zero over standard streams, which bounds nothing.
	Capacity int `json:"capacity,omitempty"`
	// Pool is set by a pool worker: it serves any runtime
	// with a binary for its platform, and the runtime fields above describe
	// only its own build.
	Pool *PoolHello `json:"pool,omitempty"`
}

// PoolHello is what a pool worker says of itself: its platform and how
// many runtimes it keeps running at most while they are idle.
type PoolHello struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	Warm int    `json:"warm"`
	// Own says the pool also runs its own build, the runtime the hello's
	// other fields describe, for requests that name no binary; older pools
	// never set it.
	Own bool `json:"own,omitempty"`
}

// Platform is the pool's "os/arch".
func (p PoolHello) Platform() string { return p.OS + "/" + p.Arch }

// PoolBinary is the runtime a pool worker runs a request on: the identity
// its hello must announce, and the binary, readable at URL, with its digest
// and size.
type PoolBinary struct {
	Identity string `json:"identity"`
	URL      string `json:"url"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
}

// message is one frame. Requests carry an ID that every event and the result
// of that request repeat; a cancel names the request it cancels.
type message struct {
	ID    uint64 `json:"id,omitempty"`
	Type  string `json:"type"`
	Hello *Hello `json:"hello,omitempty"`

	// execute: the bundle inline, or at BundleURL with its digest and
	// size; ResultURL, when set, takes a result above InlineLimit.
	Bundle       []byte     `json:"bundle,omitempty"`
	BundleURL    string     `json:"bundle_url,omitempty"`
	BundleDigest string     `json:"bundle_digest,omitempty"`
	BundleSize   int64      `json:"bundle_size,omitempty"`
	ResultURL    string     `json:"result_url,omitempty"`
	RunID        string     `json:"run_id,omitempty"`
	Deadline     *time.Time `json:"deadline,omitempty"`
	// execute, inspect and ottl to a pool worker: the runtime to run on;
	// none runs the request on the pool's own build (PoolHello.Own).
	Binary *PoolBinary `json:"binary,omitempty"`
	// pool_state: the runtime identities a pool worker holds running.
	Warm []string `json:"warm,omitempty"`
	// inspect and ottl: the captured input (engine.InspectionInput or
	// engine.OTTLInput).
	Input json.RawMessage `json:"input,omitempty"`
	// cancel: the request's context ended by its deadline, which the run
	// reports as a timeout rather than a cancellation.
	DeadlineExceeded bool `json:"deadline_exceeded,omitempty"`

	// event and result; a result uploaded to the request's ResultURL is
	// named by its digest and size instead.
	Event        json.RawMessage `json:"event,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	ResultDigest string          `json:"result_digest,omitempty"`
	ResultSize   int64           `json:"result_size,omitempty"`
	Error        *wireError      `json:"error,omitempty"`
	// Log is an execute result's redacted Collector log tail, and
	// LogTruncated says it dropped earlier lines; older workers send none.
	Log          string `json:"log,omitempty"`
	LogTruncated bool   `json:"log_truncated,omitempty"`

	// Builder link (builder.go): a builder's hello, a build it is given,
	// its log lines and its result.
	BuilderHello *BuilderHello `json:"builder_hello,omitempty"`
	Build        *BuildJob     `json:"build,omitempty"`
	Lines        []string      `json:"lines,omitempty"`
	BuildResult  *BuildResult  `json:"build_result,omitempty"`
}

// writeFrame writes a big-endian length and the message's JSON.
func writeFrame(w io.Writer, m *message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(body) > maxFrame {
		return fmt.Errorf("worker: %s message of %d bytes exceeds the %d byte frame limit", m.Type, len(body), maxFrame)
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	_, err = w.Write(frame)
	return err
}

// errFrame marks a malformed frame: the peer is not a worker of this
// protocol, or it failed mid-write.
var errFrame = errors.New("malformed worker frame")

// readFrame reads one message. io.EOF means the peer closed between frames.
func readFrame(r io.Reader) (*message, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: %w", errFrame, err)
		}
		return nil, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > maxFrame {
		return nil, fmt.Errorf("%w: frame of %d bytes", errFrame, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("%w: %w", errFrame, err)
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%w: %w", errFrame, err)
	}
	if m.Type == "" {
		return nil, fmt.Errorf("%w: message without a type", errFrame)
	}
	return &m, nil
}
