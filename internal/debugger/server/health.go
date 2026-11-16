package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Health endpoints, outside the versioned API and without a credential, for
// container healthchecks and load balancers.
const (
	healthPath = "/healthz"
	readyPath  = "/readyz"
)

// serveState is what the health endpoints report: the server answers from
// the moment it listens, takes requests once its storage is open, and stops
// taking them while it drains.
type serveState struct {
	// api is the API's handler, nil while the server waits for its storage.
	api      atomic.Pointer[http.Handler]
	draining atomic.Bool
	// storage reports whether the store still answers (platform
	// StorageReady); nil until the API serves.
	storage atomic.Pointer[func() bool]
}

// serve sets the API handler: the server takes requests from now on.
func (s *serveState) serve(h http.Handler) { s.api.Store(&h) }

// watchStorage has /readyz follow ready, the store's answer to the
// scheduler.
func (s *serveState) watchStorage(ready func() bool) { s.storage.Store(&ready) }

// startingRetryAfter is the Retry-After of a request refused while the
// server waits for its storage, in seconds.
const startingRetryAfter = "5"

// health answers GET /healthz while the process serves and GET /readyz
// while it also takes new work: /readyz answers 503 until the storage is
// open, once the server drains, and while its storage has not answered for a
// whole run lease, so a load balancer sends requests only to a server that
// can serve them. /healthz stays 200 then: restarting the server does not
// bring the storage back. Every other request goes to the API, or is
// answered 503 while the server waits for its storage.
func health(state *serveState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := state.api.Load()
		if r.URL.Path != healthPath && r.URL.Path != readyPath {
			if next == nil {
				w.Header().Set("Retry-After", startingRetryAfter)
				http.Error(w, "starting: the server is waiting for its storage", http.StatusServiceUnavailable)
				return
			}
			(*next).ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.URL.Path == readyPath {
			var reason string
			switch storage := state.storage.Load(); {
			case next == nil:
				reason = "starting"
			case state.draining.Load():
				reason = "draining"
			case storage != nil && !(*storage)():
				reason = "storage unavailable"
			}
			if reason != "" {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, reason+"\n")
				return
			}
		}
		_, _ = io.WriteString(w, "ok\n")
	})
}

// probeTimeout bounds one probe, below the usual healthcheck timeout.
const probeTimeout = 3 * time.Second

// probe is linea-server probe: it asks a running server whether it is
// ready and exits 0 only on 200, for a healthcheck in an image without
// a shell or curl.
func probe(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("linea-server probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "http://127.0.0.1:7070"+readyPath, "the server's readiness URL; "+healthPath+" checks only that it serves")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "linea-server probe: unexpected argument %q\n", fs.Arg(0))
		return exitError
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		fmt.Fprintf(stderr, "linea-server probe: --url %s: %v\n", *url, err)
		return exitError
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "linea-server probe: %v\n", err)
		return exitError
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "linea-server probe: %s answered %s\n", *url, resp.Status)
		return exitError
	}
	return exitOK
}
