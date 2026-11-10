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

// health answers GET /healthz while the process serves and GET /readyz
// while it also takes new work: /readyz answers 503 once the server drains,
// so a load balancer stops sending it requests. Every other request goes to
// next.
func health(next http.Handler, draining *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPath && r.URL.Path != readyPath {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.URL.Path == readyPath && draining.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "draining\n")
			return
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
