// Command linea-worker is a debug Collector runtime:
// the debugger's engine with the component registry of one manifest. It has
// two modes.
//
// By default the debugger starts it and talks over its standard input and
// output (internal/debugger/worker); it is not meant to be run by hand. Its
// standard error is its log.
//
// With --connect it dials a team server: it presents the
// server's registration token from LINEA_WORKER_TOKEN or --token-file,
// serves under the runtime name --name, runs up to --capacity requests at
// once and reconnects until it is stopped. It listens on no port.
//
// With --connect --pool it is a pool worker: it serves any
// runtime the server built for its platform instead of one of its own,
// downloading each runtime binary into --cache-dir and running one child
// process per runtime in use, up to --capacity requests at once across them.
// --warm, --idle and --cache bound the children and the cache.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	// The process executes on the built-in runtime in process.
	_ "github.com/lineahq/linea/internal/debugger/engine/local/install"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// tokenEnv holds a connecting worker's registration token.
const tokenEnv = "LINEA_WORKER_TOKEN"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "linea-worker:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("linea-worker", flag.ContinueOnError)
	connect := fs.String("connect", "", "dial the team server at this URL and serve it instead of standard input and output")
	tokenFile := fs.String("token-file", "", "file holding the server's registration token (default: $"+tokenEnv+"; with --connect)")
	name := fs.String("name", "", "the runtime name the worker serves under (with --connect)")
	capacity := fs.Int("capacity", runtime.NumCPU(), "how many requests the worker runs at once (with --connect)")
	caFile := fs.String("ca-file", "", "verify the --connect server's certificate with this PEM CA bundle")
	pool := fs.Bool("pool", false, "serve any runtime the server built for this platform, one child process per runtime (with --connect)")
	warm := fs.Int("warm", defaultPoolWarm, "with --pool: how many idle runtimes to keep running; more are stopped, least recently used first")
	idle := fs.Duration("idle", defaultPoolIdle, "with --pool: stop a runtime idle this long; 0 keeps it")
	cacheBytes := sizeFlag(defaultPoolCache)
	fs.Var(&cacheBytes, "cache", "with --pool: the runtime binary cache's size, such as 5GiB; least recently used binaries not running are evicted")
	cacheDir := fs.String("cache-dir", filepath.Join(os.TempDir(), "linea-pool-cache"), "with --pool: where runtime binaries are cached; it must allow executing files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *connect == "" {
		if *pool || set["warm"] || set["idle"] || set["cache"] || set["cache-dir"] {
			return errors.New("--pool, --warm, --idle, --cache and --cache-dir need --connect")
		}
		if *name != "" || *caFile != "" || *tokenFile != "" || set["capacity"] {
			return errors.New("--name, --capacity, --token-file and --ca-file need --connect")
		}
		return worker.Serve(ctx, os.Stdin, os.Stdout)
	}
	if *pool {
		if !set["capacity"] {
			*capacity = defaultPoolCapacity
		}
		if *name == "" {
			*name = "pool"
		}
		return connectPool(ctx, *connect, *tokenFile, *caFile, worker.PoolOptions{
			Name: *name, Capacity: *capacity, Warm: *warm, Idle: *idle, CacheDir: *cacheDir, CacheBytes: int64(cacheBytes),
		})
	}
	if set["warm"] || set["idle"] || set["cache"] || set["cache-dir"] {
		return errors.New("--warm, --idle, --cache and --cache-dir need --pool")
	}
	return connectServer(ctx, *connect, *tokenFile, *name, *capacity, *caFile)
}

// A pool worker's defaults.
const (
	defaultPoolCapacity = 8
	defaultPoolWarm     = 4
	defaultPoolIdle     = 10 * time.Minute
	defaultPoolCache    = 5 << 30
)

// sizeFlag is a byte size given as a whole number with an optional binary
// unit: 1073741824, 512MiB, 5GiB.
type sizeFlag int64

func (f *sizeFlag) String() string {
	if f == nil {
		return ""
	}
	return strconv.FormatInt(int64(*f), 10)
}

func (f *sizeFlag) Set(v string) error {
	digits, shift := v, uint(0)
	for _, u := range []struct {
		suffix string
		shift  uint
	}{{"KiB", 10}, {"MiB", 20}, {"GiB", 30}, {"TiB", 40}, {"B", 0}} {
		if len(v) > len(u.suffix) && strings.EqualFold(v[len(v)-len(u.suffix):], u.suffix) {
			digits, shift = v[:len(v)-len(u.suffix)], u.shift
			break
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)>>shift {
		return fmt.Errorf("invalid size %q: want a positive whole number of bytes, KiB, MiB, GiB or TiB, such as 512MiB", v)
	}
	*f = sizeFlag(n << shift)
	return nil
}

// connectPool dials a team server as a pool worker and serves it until ctx
// ends.
func connectPool(ctx context.Context, url, tokenFile, caFile string, opts worker.PoolOptions) error {
	token, err := readToken(tokenFile)
	if err != nil {
		return err
	}
	if caFile != "" {
		if opts.CA, err = os.ReadFile(caFile); err != nil {
			return fmt.Errorf("--ca-file: %w", err)
		}
	}
	opts.URL, opts.Token, opts.Log = url, token, os.Stderr
	return worker.ServePool(ctx, opts)
}

// connectServer dials a team server and serves it until ctx ends.
func connectServer(ctx context.Context, url, tokenFile, name string, capacity int, caFile string) error {
	token, err := readToken(tokenFile)
	if err != nil {
		return err
	}
	var ca []byte
	if caFile != "" {
		if ca, err = os.ReadFile(caFile); err != nil {
			return fmt.Errorf("--ca-file: %w", err)
		}
	}
	return worker.Connect(ctx, worker.ConnectOptions{URL: url, CA: ca, Token: token, Name: name, Capacity: capacity, Log: os.Stderr})
}

func readToken(file string) (string, error) {
	token := os.Getenv(tokenEnv)
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("--token-file: %w", err)
		}
		token = string(b)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("a connecting worker needs the registration token: set %s or pass --token-file", tokenEnv)
	}
	return token, nil
}
