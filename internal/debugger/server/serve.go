package server

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/mcp"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/pg"
	"github.com/lineahq/linea/internal/debugger/webui"
)

// The storage limits of a team server: composition policy named by
// the server config's store.storage_limit and store.run_limit, never persisted
// and never raised by a request. The defaults are the platform's.
const (
	defaultStorageLimit = 256 << 20
	defaultRunLimit     = 16 << 20
	// minRunLimit keeps room for a run's fixed report parts.
	minRunLimit = 2 << 20
)

// storageLimits are the store.storage_limit and store.run_limit values in
// bytes.
type storageLimits struct{ storage, run int64 }

// sizeFlag is a byte size given as a whole number with an optional binary
// unit: 1073741824, 512MiB, 1GiB.
type sizeFlag struct {
	bytes int64
	set   bool
}

var sizeUnits = []struct {
	suffix string
	shift  uint
}{{"KiB", 10}, {"MiB", 20}, {"GiB", 30}, {"TiB", 40}, {"B", 0}}

func (f *sizeFlag) String() string {
	if f == nil || !f.set {
		return ""
	}
	return formatSize(f.bytes)
}

func (f *sizeFlag) Set(v string) error {
	n, err := parseSize(v)
	if err != nil {
		return err
	}
	f.bytes, f.set = n, true
	return nil
}

func parseSize(v string) (int64, error) {
	digits, shift := v, uint(0)
	for _, u := range sizeUnits {
		if len(v) > len(u.suffix) && strings.EqualFold(v[len(v)-len(u.suffix):], u.suffix) {
			digits, shift = v[:len(v)-len(u.suffix)], u.shift
			break
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 || n > math.MaxInt64>>shift {
		return 0, fmt.Errorf("invalid size %q: want a positive whole number of bytes, KiB, MiB, GiB or TiB, such as 512MiB", v)
	}
	return n << shift, nil
}

// formatSize writes bytes in the largest binary unit, with one decimal when
// it is not whole.
func formatSize(n int64) string {
	for i := len(sizeUnits) - 2; i >= 0; i-- {
		u := sizeUnits[i]
		if n < 1<<u.shift {
			continue
		}
		if n%(1<<u.shift) == 0 {
			return fmt.Sprintf("%d %s", n>>u.shift, u.suffix)
		}
		return strconv.FormatFloat(float64(n)/float64(int64(1)<<u.shift), 'f', 1, 64) + " " + u.suffix
	}
	return fmt.Sprintf("%d B", n)
}

// serveStorage checks the server config's storage limits: a run limit is at
// least 2 MiB and at most the storage limit.
func serveStorage(storage, run sizeFlag) (storageLimits, error) {
	limits := storageLimits{storage: defaultStorageLimit, run: defaultRunLimit}
	if storage.set {
		limits.storage = storage.bytes
	}
	if run.set {
		limits.run = run.bytes
	}
	if limits.run < minRunLimit {
		return storageLimits{}, fmt.Errorf("run_limit %s is below the minimum %s", formatSize(limits.run), formatSize(minRunLimit))
	}
	if limits.run > limits.storage {
		return storageLimits{}, fmt.Errorf("run_limit %s exceeds storage_limit %s", formatSize(limits.run), formatSize(limits.storage))
	}
	return limits, nil
}

// OpenStore opens a team server's PostgreSQL and object storage. Tests
// replace it, as the CLI's tests do to keep a team server's records in
// memory.
var OpenStore = func(ctx context.Context, c pg.Config) (artifact.Repository, error) {
	return pg.Open(ctx, c)
}

// teamServe is linea-server's composition: the server config and how the listener
// is exposed.
type teamServe struct {
	*team
	allowHTTP         bool
	certFile, keyFile string
	// drain bounds how long running work may finish at shutdown before
	// it is handed back to the queue for another instance.
	drain time.Duration
	// metricsAddr, when set, serves the build metrics there.
	metricsAddr string
}

// serveAPI serves the team server until ctx is cancelled. It authenticates
// every request and may listen anywhere, over TLS unless --allow-http.
func serveAPI(ctx context.Context, addr string, tm *teamServe, enableMCP bool, logger *zap.Logger, stderr io.Writer, fleet fleetServeOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	captureOptions, err := fleet.load()
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	var tlsConfig *tls.Config
	switch {
	case tm.certFile != "":
		cert, err := tls.LoadX509KeyPair(tm.certFile, tm.keyFile)
		if err != nil {
			return fmt.Errorf("--tls-cert-file and --tls-key-file: %w", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	case !loopbackHost(host) && !tm.allowHTTP:
		return fmt.Errorf("--addr %s: a team server off loopback serves TLS (--tls-cert-file and --tls-key-file), or plain HTTP behind a TLS-terminating proxy with --allow-http", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	ln = newLimitListener(ln, tm.connections)
	var metricsLn net.Listener
	if tm.metricsAddr != "" {
		if metricsLn, err = net.Listen("tcp", tm.metricsAddr); err != nil {
			return fmt.Errorf("--metrics-listen: %w", err)
		}
		defer metricsLn.Close()
	}
	repo, err := OpenStore(ctx, tm.store)
	if err != nil {
		return err
	}
	// A team server needs no Docker: its runtimes are the catalog's, served
	// by connected workers. Its storage may be shared with
	// other instances.
	service, err := platform.Open(repo, platform.Options{
		Logger: logger,
		Access: tm.access, BuiltInOnWorkers: true, WorkerToken: tm.workerToken, BuilderToken: tm.buildToken, BuildExport: tm.buildExport,
		QueueCapacity: tm.queue, Limits: tm.principals, Retention: tm.retention, Shared: true, Capture: captureOptions,
		GitCloneBytes: tm.gitClone,
	})
	if err != nil {
		return err
	}
	defer service.Close()
	chain := platform.Authenticators{service.TokenAuthenticator()}
	var signIn *platform.OIDC
	if tm.oidc != nil {
		o := *tm.oidc
		// A browser reaching plain HTTP on loopback drops Secure cookies;
		// behind a TLS-terminating proxy it speaks https.
		o.InsecureCookie = tlsConfig == nil && !tm.allowHTTP
		if signIn, err = service.OIDC(ctx, o); err != nil {
			return err
		}
		chain = append(chain, signIn)
	}
	if tm.headers != nil {
		chain = append(chain, *tm.headers)
	}
	var auth platform.Authenticator = chain
	h := api.New(api.Options{Platform: service, Fallback: webui.Handler(), Logger: logger, Authenticator: auth, RemoteAccess: true})
	var handler http.Handler = h
	scheme := "http"
	if tlsConfig != nil {
		scheme = "https"
	}
	if signIn != nil || enableMCP {
		mux := http.NewServeMux()
		mux.Handle("/", h)
		handler = mux
		if signIn != nil {
			mux.Handle("/auth/", signIn.Handler())
		}
		if enableMCP {
			// MCP clients present an API token; the listening address is
			// not the URL they reach.
			mux.Handle("/mcp", mcp.New(mcp.Options{Platform: service, Logger: logger, Authenticator: auth}))
		}
	}
	srv := newTeamHTTPServer(securityHeaders(handler))
	srv.TLSConfig = tlsConfig
	stopped := make(chan struct{})
	defer close(stopped)
	if metricsLn != nil {
		// Counts only and no credentials, on a listener of its own that no
		// ingress routes to.
		metrics := newTeamHTTPServer(api.Metrics(service))
		go func() { _ = metrics.Serve(metricsLn) }()
		defer metrics.Close()
		fmt.Fprintf(stderr, "serving build metrics on http://%s/metrics\n", metricsLn.Addr())
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-stopped:
			return
		}
		// Claim nothing more and let running work finish while the API
		// still answers; Close hands the rest to another instance.
		drainCtx, cancel := context.WithTimeout(context.Background(), tm.drain)
		if err := service.Drain(drainCtx); err != nil {
			fmt.Fprintf(stderr, "drain: work still running after %s is handed back to the queue\n", tm.drain)
		}
		cancel()
		// Cancel runs first: their event streams end with them, so the
		// graceful shutdown does not wait for streams of queued runs. Close
		// also stops running fleet captures, bounded.
		service.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(stderr, "serving API on %s://%s/api/v1 (contract: /api/v1/openapi.json)\n", scheme, ln.Addr())
	if enableMCP {
		fmt.Fprintf(stderr, "serving MCP on %s://%s/mcp\n", scheme, ln.Addr())
	}
	methods := []string{"API token"}
	if signIn != nil {
		methods = append(methods, "OIDC sign-in with "+tm.oidc.Issuer)
	}
	if tm.headers != nil {
		methods = append(methods, "identity headers from "+joinPrefixes(tm.headers.Proxies))
	}
	fmt.Fprintf(stderr, "team mode: requests are authenticated by %s, authorized by the server config's bindings, and run on the team's default runtime unless they name one\n",
		strings.Join(methods, ", "))
	if captureOptions.Fleet.Endpoint != "" {
		fmt.Fprintf(stderr, "live capture through fleet at %s\n", captureOptions.Fleet.Endpoint)
	}
	fmt.Fprintf(stderr, "storage: PostgreSQL, payloads in object storage bucket %s\n", tm.store.S3.Bucket)
	fmt.Fprintln(stderr, "requests read no host file or host environment variable: they send their files inline or name saved ones, and ${env:...} reads declared values only")
	serve := srv.Serve
	if tlsConfig != nil {
		serve = func(ln net.Listener) error { return srv.ServeTLS(ln, "", "") }
	}
	if err := serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loopbackHost reports whether a host name or address is this machine's.
func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func joinPrefixes(prefixes []netip.Prefix) string {
	out := make([]string, len(prefixes))
	for i, p := range prefixes {
		out[i] = p.String()
	}
	return strings.Join(out, ", ")
}

// Exit codes of Main.
const (
	exitOK    = 0
	exitError = 1
)

// usage is how the server is started.
const usage = "--config <file> [--addr 127.0.0.1:7070] [--tls-cert-file FILE --tls-key-file FILE | --allow-http] [--drain-timeout D] [--metrics-listen ADDR] [--mcp] [--fleet-url URL --fleet-token-file FILE [--fleet-allow-http]] [--verbose]"

// noConfig refuses a start without a server config.
const noConfig = "linea-server needs --config <file>: run a team server on Kubernetes (docs/debugger/deploy-kubernetes.md) or locally with Docker Compose (docs/debugger/deploy-compose.md)"

// Main serves the team server's HTTP API until ctx ends and returns the
// process's exit code. The server config composes it; there is no other
// server mode.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("linea-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: linea-server %s\n", usage)
		fs.PrintDefaults()
		fmt.Fprintln(stderr, "\nThe server config names the storage (PostgreSQL and object storage), authentication, role bindings and worker registration; see docs/debugger/team-server.md.\nRequests read no host file or host environment variable: they send their files inline or name saved ones, and ${env:NAME} reads only scenario/setup values and URI defaults; any other name gets a stand-in.")
	}
	addr := fs.String("addr", "127.0.0.1:7070", "address to listen on")
	configPath := fs.String("config", "", "the server config that composes the server: storage, secrets, authentication, role bindings, retention and limits (required)")
	var tm teamServe
	fs.StringVar(&tm.certFile, "tls-cert-file", "", "serve TLS with this PEM certificate chain")
	fs.StringVar(&tm.keyFile, "tls-key-file", "", "the PEM private key of --tls-cert-file")
	fs.BoolVar(&tm.allowHTTP, "allow-http", false, "serve plain HTTP off loopback, behind a TLS-terminating proxy on a protected network")
	fs.DurationVar(&tm.drain, "drain-timeout", 25*time.Second, "how long running work may finish when the server stops before it is handed to another instance")
	fs.StringVar(&tm.metricsAddr, "metrics-listen", "", "serve the build queue's metrics for an autoscaler at http://ADDR/metrics, on a listener of its own without credentials (counts only)")
	enableMCP := fs.Bool("mcp", false, "serve the MCP endpoint at /mcp; clients present an API token")
	verbose := fs.Bool("verbose", false, "write debug logs to stderr")
	var fleet fleetServeOptions
	fs.StringVar(&fleet.url, "fleet-url", "", "linea-fleet management API, such as https://fleet:8080, for live capture through its livecapture plugin")
	fs.StringVar(&fleet.tokenFile, "fleet-token-file", "", "file holding the fleet API token (never passed as an argument)")
	fs.BoolVar(&fleet.allowHTTP, "fleet-allow-http", false, "allow plaintext HTTP to a non-loopback fleet on a protected network")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	refuse := func(msg string) int {
		fmt.Fprintln(stderr, msg)
		fs.Usage()
		return exitError
	}
	switch {
	case fs.NArg() > 0:
		return refuse(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	case *configPath == "":
		return refuse(noConfig)
	case (tm.certFile == "") != (tm.keyFile == ""):
		return refuse("--tls-cert-file and --tls-key-file go together")
	case tm.certFile != "" && tm.allowHTTP:
		return refuse("--allow-http cannot be used with --tls-cert-file")
	}
	t, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	tm.team = t
	var logger *zap.Logger
	if *verbose {
		if logger, err = zap.NewDevelopment(); err != nil {
			return fail(stderr, err)
		}
	}
	if err := serveAPI(ctx, *addr, &tm, *enableMCP, logger, stderr, fleet); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "error:", err)
	return exitError
}
