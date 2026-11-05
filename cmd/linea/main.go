// Command linea replays telemetry through an unchanged Collector
// configuration and reports what each instrumented stage observed.
//
// Usage:
//
//	linea inspect --config <collector.yaml> [--scenario <scenario.yaml>]
//	linea run     --config <collector.yaml> (--fixture <otlp.json> | --scenario <scenario.yaml>)
//	linea test    --config <collector.yaml> (--scenario <scenario.yaml>... | --tests <dir>) [--report junit.xml]
//	linea diff    --before <collector.yaml> --after <collector.yaml> (--fixture <otlp.json> | --scenario <scenario.yaml>) [--ignore PATH]... [--format text|json|markdown]
//	linea diff    --server URL --runs <before-run-id> <after-run-id>
//	linea upgrade-check --config <collector.yaml> --to RUNTIME [--from RUNTIME] [--scenario <scenario.yaml>... | --tests <dir>] [--format text|json|markdown]
//	linea config-bisect --agent ID [--config ID] [--good N] [--bad N] (--scenario <scenario.yaml>... | --tests <dir>) [--local <supervisor-config.yaml>]... --server URL [--format text|json|markdown]
//	linea export  --server URL --run <run-id> --out <scenario.yaml>
//	linea runs    --server URL [--scenario NAME] [--runtime NAME] [--result RESULT] | log|show|cancel|assert|diffs|inputs|snapshot|summary|evidence|evaluations|delete|keep|unkeep --server URL --run ID | prune --server URL [--before DATE] [--project P] [--scenario S] [--runtime R] [--result R] [--dry-run] | storage --server URL
//	linea suite   list|show|report|rerun|junit|delete --server URL [--suite ID]
//	linea ottl    preview --config <collector.yaml> --processor ID --input <otlp.json>
//	linea statement candidate --config <collector.yaml> (--proposed <collector.yaml> | --processor ID --signal SIGNAL --op OP)
//	linea statement test      --server URL --run ID (--proposed <collector.yaml> | --processor ID --signal SIGNAL --op OP)
//	linea artifacts list|show|save|revisions --server URL [--artifact ID] | export --server URL --config ID --scenario ID... --out <file.zip> | import --server URL --archive <file.zip>
//	linea samples [--server URL] | get ID [--dir DIR]
//	linea runtime build --name NAME --manifest <ocb.yaml> | list | rm NAME | push NAME TARGET | retarget --manifest <ocb.yaml> [--version V] | import REF [--pull]
//	linea fleet   overview|agents|agent|retry|events|group-save|group-delete|configs|config|config-create|config-update|config-delete|versions|version|version-create|assignments|assignment|assignment-create|preview|rollouts|rollout|rollout-start|rollout-action|targets --server URL [flags]
//	linea build   --manifest <ocb.yaml> [--out FILE] [--source MODULE=DIR]... [--os OS] [--arch ARCH] [--docker]
//	linea version
//
// inspect, run, test and diff take --runtime NAME to use a built runtime instead of
// the built-in registry (diff also --after-runtime).
//
// inspect, run, test and diff take --server URL to send the command to a team
// server (linea-server) instead of starting a local Collector.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"go.uber.org/zap"

	// The process executes on the built-in runtime in process.
	_ "github.com/lineahq/linea/internal/debugger/engine/local/install"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// cliCaller identifies this adapter's requests: the local owner via the CLI.
var cliCaller = platform.Local("cli")

// Exit codes are shared by all commands, so CI can tell regressions from
// infrastructure failures.
const (
	// exitOK: the command succeeded.
	exitOK = 0
	// exitError: invalid usage, an invalid configuration, or a setup or
	// runtime failure.
	exitError = 1
	// exitMismatch: the instrumented run differed from the reference run,
	// or diff found the two runs different.
	exitMismatch = 2
	// exitFail: at least one assertion failed.
	exitFail = 3
	// exitInconclusive: no assertion failed, but at least one could not be
	// established, for example because the run did not complete.
	exitInconclusive = 4
)

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, stdout, stderr io.Writer) int
}

// commands is initialized in init because usage refers back to it.
var commands []command

func init() {
	commands = []command{
		{"inspect", "validate a configuration and show its topology", inspectCmd},
		{"run", "replay a fixture or scenario through the configuration", runCmd},
		{"test", "evaluate scenario assertions (exit 3 fail, 4 inconclusive)", testCmd},
		{"diff", "compare two configurations on one input, or two runs (exit 2 different, 4 inconclusive)", diffCmd},
		{"upgrade-check", "check a configuration and its scenarios on another Collector release (exit 2 breaks or changes, 4 inconclusive)", upgradeCheckCmd},
		{"config-bisect", "find the fleet config version that made the scenarios fail on an agent (exit 2 no regression or already bad, 4 inconclusive)", configBisectCmd},
		{"runs", "list a server's runs with their results, or show, cancel, assert, summarize one, page its evidence, read its evaluations, stage changes, a snapshot, the inputs or the kept log, delete or keep it, prune old runs, or show the storage and retention policy", runsCmd},
		{"artifacts", "list, show and save artifacts, list an artifact's revisions, export saved work for CI, or import it back", artifactsCmd},
		{"samples", "list the shipped samples, or write one to disk to run it", samplesCmd},
		{"suite", "list a server's suites, show one or its report, rerun one from its recorded inputs, write its JUnit report, or delete one with its runs", suiteCmd},
		{"ottl", "preview a transform processor's statements on an input (never verified)", ottlCmd},
		{"statement", "validate a proposed configuration or one statement edit, or test one on a run's recorded inputs", statementCmd},
		{"export", "write a server run as a scenario file for CI", exportCmd},
		{"runtime", "build, list, remove, push and import runtimes (debug Collectors in Docker)", runtimeCmd},
		{"capture", "discover Collectors and control bounded live captures", captureCmd},
		{"fleet", "manage fleet's agents, groups, configs, assignments and rollouts through a server's fleet connection", fleetCmd},
		{"audit", "page a server's audit log: who did what, through which interface, allowed or denied", auditCmd},
		{"token", "create, list and revoke a team server's API tokens", tokenCmd},
		{"access", "list a team server's role bindings, and grant or revoke the ones it manages", accessCmd},
		{"project", "list a team server's projects, and create, update, archive or unarchive one", projectCmd},
		{"library", "list the library, publish an artifact to it, share an item with projects, or copy one into a project", libraryCmd},
		{"login", "sign in to a team server's identity provider; later --server commands present the sign-in", loginCmd},
		{"logout", "forget the sign-in to a team server", logoutCmd},
		{"build", "compile this CLI with an OCB manifest's Collector release and components, on this machine", buildCmd},
		{"version", "show debugger, Collector and schema versions", versionCmd},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitError
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		usage(stdout)
		return exitOK
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(ctx, args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "unknown command %q\n", args[0])
	usage(stderr)
	return exitError
}

// dockerRuntimes is the Docker-backed runtime store; it calls docker only
// when a runtime is used, so composing it needs no Docker. Tests replace it.
var dockerRuntimes = func() platform.RuntimeStore {
	return platform.DockerRuntimes(platform.DockerOptions{ImageCache: runtimeCache})
}

// runtimeCache is where a direct command keeps the binaries of runtime
// images it names by reference; runtimeCacheFlag sets it.
var runtimeCache = platform.DefaultImageCache()

// runtimeCacheFlag adds --runtime-cache to a command that takes an image
// reference as --runtime.
func runtimeCacheFlag(fs *flag.FlagSet) {
	fs.StringVar(&runtimeCache, "runtime-cache", platform.DefaultImageCache(), "directory of the binaries of runtime images named by reference in --runtime")
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: linea <command> [flags]")
	fmt.Fprintln(w, "\ncommands:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	_ = tw.Flush()
	fmt.Fprintln(w, "\nNew here? 'linea samples' lists ready-to-run examples.")
	fmt.Fprintln(w, "Run 'linea <command> -h' for the flags of a command.")
}

// newFlagSet returns a flag set whose usage names the command.
func newFlagSet(name, synopsis string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("linea "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: linea %s %s\n", name, synopsis)
		fs.PrintDefaults()
		switch name {
		case "run", "test", "diff", "upgrade-check", "config-bisect", "inspect", "ottl preview", "statement candidate":
			fmt.Fprintln(stderr, "\n${env:NAME} reads the scenario/setup value, a host value only for --import-env NAME, then the URI default; any other name gets a stand-in.")
		}
	}
	return fs
}

// formatFlag registers --format, accepting json or text.
func formatFlag(fs *flag.FlagSet, def string) *string {
	return fs.String("format", def, "output format: json or text")
}

// parse parses args and validates the format, rejecting positional
// arguments. It returns the exit code to use when parsing fails: -h is not
// an error.
func parse(fs *flag.FlagSet, args []string, format *string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK, false
		}
		return exitError, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return exitError, false
	}
	if format != nil {
		return checkFormat(fs, *format)
	}
	return exitOK, true
}

// checkFormat accepts json or text.
func checkFormat(fs *flag.FlagSet, format string) (int, bool) {
	if format != "json" && format != "text" {
		fmt.Fprintf(fs.Output(), "unknown format %q\n", format)
		fs.Usage()
		return exitError, false
	}
	return exitOK, true
}

// terminal reports whether w is a terminal, where a person reads the output.
func terminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// newLogger returns a development logger for --verbose, or nil.
func newLogger(verbose bool) (*zap.Logger, error) {
	if !verbose {
		return nil, nil
	}
	return zap.NewDevelopment()
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "error:", err)
	return exitError
}
