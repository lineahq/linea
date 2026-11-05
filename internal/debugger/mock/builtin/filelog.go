package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/filelogreceiver"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/xreceiver"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/lineahq/linea/collector/provider"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// filelogTypes are the receiver's type and its deprecated alias: at
// v0.162.0 the factory type is file_log, and configurations written as
// filelog still resolve to it.
var filelogTypes = []component.Type{component.MustNewType("file_log"), component.MustNewType("filelog")}

// aliasProvider is implemented by factories registered under a deprecated
// type alias.
type aliasProvider interface{ DeprecatedAlias() component.Type }

const (
	// matchedFilesMessage is the debug message fileconsumer logs at the
	// start of every poll (pkg/stanza v0.162.0, Manager.poll).
	matchedFilesMessage = "matched files"
	// restoreOperatorID names the operator the provider puts before the
	// configured operators to restore deployment paths.
	restoreOperatorID = "linea_restore_paths"
	// defaultPollInterval and defaultFlushPeriod are fileconsumer's defaults
	// (v0.162.0), used when a configuration leaves them unset.
	defaultPollInterval = 200 * time.Millisecond
	defaultFlushPeriod  = 500 * time.Millisecond
)

// filelog runs the real filelog receiver against a sandbox directory holding
// the scenario's files under their deployment paths.
type filelog struct {
	host      mock.Host
	files     []mock.File
	drain     time.Duration
	receivers map[component.ID]*fileReceiver
	order     []component.ID

	root string
	// hostPaths maps each file's sandbox path (slash-separated) to its
	// deployment path.
	hostPaths map[string]string
}

// fileReceiver tracks one configured filelog receiver during a run.
type fileReceiver struct {
	id  component.ID
	cfg *filelogreceiver.FileLogConfig

	mu       sync.Mutex
	polls    int
	matched  map[string]bool
	lastEmit time.Time
	created  bool
}

func newFilelog(cfg *otelcol.Config, spec mock.Spec, host mock.Host) *filelog {
	ids := mock.UsedReceivers(cfg, filelogTypes...)
	if len(ids) == 0 {
		return nil
	}
	p := &filelog{host: host, files: spec.Files, drain: spec.Drain, receivers: map[component.ID]*fileReceiver{}, order: ids}
	for _, id := range ids {
		rc, _ := cfg.Receivers[id].(*filelogreceiver.FileLogConfig)
		p.receivers[id] = &fileReceiver{id: id, cfg: rc, matched: map[string]bool{}}
	}
	return p
}

func (p *filelog) Name() string { return "filelog" }

func (p *filelog) Components() []mock.Component {
	out := make([]mock.Component, 0, len(p.order))
	for _, id := range p.order {
		out = append(out, mock.Component{
			Kind: topology.KindReceiver, ID: id, Mode: topology.ModeDependencyMocked,
			Note: "reads the scenario's files from an isolated directory instead of the host filesystem",
		})
	}
	return out
}

func (p *filelog) Connector(f connector.Factory) (connector.Factory, bool) { return f, false }

func (p *filelog) Processor(f processor.Factory) (processor.Factory, bool) { return f, false }

func (p *filelog) Receiver(f receiver.Factory) (receiver.Factory, bool) {
	if !slices.Contains(filelogTypes, f.Type()) {
		return f, false
	}
	var opts []xreceiver.FactoryOption
	if ap, ok := f.(aliasProvider); ok && ap.DeprecatedAlias().String() != "" {
		opts = append(opts, xreceiver.WithDeprecatedTypeAlias(ap.DeprecatedAlias()))
	}
	return xreceiver.NewFactory(f.Type(), f.CreateDefaultConfig, append(opts, xreceiver.WithLogs(
		func(ctx context.Context, set receiver.Settings, cfg component.Config, next consumer.Logs) (receiver.Logs, error) {
			r, ok := p.receivers[set.ID]
			orig, isFilelog := cfg.(*filelogreceiver.FileLogConfig)
			if !ok || !isFilelog || p.root == "" {
				return nil, fmt.Errorf("filelog receiver %s has no sandbox", set.ID)
			}
			sandboxed, err := p.sandboxed(orig)
			if err != nil {
				return nil, err
			}
			r.mu.Lock()
			r.created = true
			r.mu.Unlock()
			set.Logger = set.Logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
				return pollCore{Core: restoreCore{Core: c, restore: p.restorePaths}, r: r, restore: p.deploymentPath}
			}))
			rec := &recordingLogs{next: next, r: r, host: p.host}
			if p.host != nil {
				p.host.Register(set.ID, pipeline.SignalLogs, consumer.Logs(rec))
			}
			return f.CreateLogs(ctx, set, sandboxed, rec)
		}, f.LogsStability()))...), true
}

// Start creates the sandbox and writes the files' existing content.
func (p *filelog) start(context.Context) error {
	if err := validateFiles(p.files); err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "linea-files-")
	if err != nil {
		return err
	}
	// Canonical form, so the paths fileconsumer reports (and resolves)
	// match the restore table.
	if root, err = filepath.Abs(root); err == nil {
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
	}
	p.root = root
	p.hostPaths = map[string]string{}
	for _, f := range p.files {
		hp := p.hostPath(f.Path)
		p.hostPaths[filepath.ToSlash(hp)] = f.Path
		if err := os.MkdirAll(filepath.Dir(hp), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(hp, f.Existing, 0o644); err != nil {
			return err
		}
		if err := touch(hp, f.Modified); err != nil {
			return err
		}
	}
	return nil
}

func (p *filelog) ready(context.Context) error { return nil }

// Drive waits until every receiver has completed its first poll, appends the
// files' content, then waits until the receivers have read it and gone quiet.
func (p *filelog) drive(ctx context.Context) error {
	active := p.active()
	if len(active) == 0 {
		return nil
	}
	// The first poll logs its matches before it fixes the start_at offsets of
	// existing files, so content appended while it runs could be skipped as
	// existing. The second poll's start marks the first one's end.
	if err := waitFor(ctx, func() bool {
		for _, r := range active {
			if r.pollCount() < 2 {
				return false
			}
		}
		return true
	}); err != nil {
		return err
	}
	base := map[component.ID]int{}
	for _, r := range active {
		base[r.id] = r.pollCount()
	}
	for _, f := range p.files {
		if len(f.Content) == 0 {
			continue
		}
		if err := appendFile(p.hostPath(f.Path), f.Content); err != nil {
			return err
		}
		if err := touch(p.hostPath(f.Path), f.Modified); err != nil {
			return err
		}
	}
	written := time.Now()
	quiet := p.quietPeriod(active)
	// The poll after the write reads the new content to the end; the one
	// after that confirms nothing is left. Multiline entries still buffered
	// are flushed after force_flush_period, which the quiet period covers.
	return waitFor(ctx, func() bool {
		last := written
		for _, r := range active {
			if r.pollCount() < base[r.id]+2 {
				return false
			}
			if t := r.lastEmitted(); t.After(last) {
				last = t
			}
		}
		return time.Since(last) >= quiet
	})
}

func (p *filelog) close() error {
	if p.root == "" {
		return nil
	}
	return os.RemoveAll(p.root)
}

func (p *filelog) declarations() []mock.Declaration {
	var out []mock.Declaration
	matchedAny := map[string]bool{}
	for _, id := range p.order {
		r := p.receivers[id]
		d := mock.Declaration{Provider: p.Name(), Ref: topology.Ref(topology.KindReceiver, id), Mode: topology.ModeDependencyMocked}
		r.mu.Lock()
		for path := range r.matched {
			d.Supplied = append(d.Supplied, path)
			matchedAny[path] = true
		}
		r.mu.Unlock()
		sort.Strings(d.Supplied)
		d.Limitations = append(d.Limitations, "files are read from an isolated directory; include and exclude patterns are mapped into it")
		if r.cfg != nil {
			in := r.cfg.InputConfig
			if in.IncludeFilePath || in.IncludeFilePathResolved {
				d.Limitations = append(d.Limitations, "log.file.path and log.file.path_resolved are restored to the deployment path before the configured operators run")
			}
			if in.IncludeFileOwnerName || in.IncludeFileOwnerGroupName || in.IncludeFilePermissions {
				d.Limitations = append(d.Limitations, "file owner and permissions are those of the debugger's sandbox files")
			}
			if r.cfg.StorageID != nil {
				d.Limitations = append(d.Limitations, "no checkpoint is loaded: offsets start empty, as on a first start")
			}
			if in.StartAt != "beginning" && hasExisting(p.files) {
				d.Limitations = append(d.Limitations, "start_at is end: existing content was skipped, as on a first start without a checkpoint")
			}
		}
		if len(p.files) == 0 {
			d.Warnings = append(d.Warnings, "no mocked files are declared (mocks.files), so the receiver read nothing")
		}
		out = append(out, d)
	}
	var unmatched []string
	for _, f := range p.files {
		if !matchedAny[f.Path] {
			unmatched = append(unmatched, f.Path)
		}
	}
	if len(unmatched) > 0 && len(out) > 0 {
		out[0].Warnings = append(out[0].Warnings, "no filelog receiver read (include and exclude patterns) "+strings.Join(unmatched, ", "))
	}
	return out
}

// Gaps reports receivers that no scenario file will feed: none are
// declared, or none matches the receiver's include patterns without being
// excluded. Patterns are matched as fileconsumer globs them (doublestar),
// after the same mapping the run applies.
func (p *filelog) Gaps() []mock.Gap {
	var out []mock.Gap
	for _, id := range p.order {
		ref := topology.Ref(topology.KindReceiver, id)
		if len(p.files) == 0 {
			out = append(out, mock.Gap{Ref: ref,
				Message: "the receiver reads files, but no mocked files are declared, so it will read nothing",
				Hint:    "add the files it should read, at their deployment paths (mocks.files)"})
			continue
		}
		cfg := p.receivers[id].cfg
		if cfg == nil || slices.ContainsFunc(p.files, func(f mock.File) bool { return globbed(cfg, f.Path) }) {
			continue
		}
		out = append(out, mock.Gap{Ref: ref,
			Message: "the receiver reads none of the mocked files: no file matches its include patterns (" + strings.Join(cfg.InputConfig.Include, ", ") + ") without being excluded",
			Hint:    "give the mocked files the deployment paths the include patterns expect"})
	}
	return out
}

// globbed reports whether a deployment path matches an include pattern of
// cfg and no exclude pattern.
func globbed(cfg *filelogreceiver.FileLogConfig, path string) bool {
	match := func(patterns []string) bool {
		for _, pat := range patterns {
			if ok, _ := doublestar.Match(sandboxRel(pat), sandboxRel(path)); ok {
				return true
			}
		}
		return false
	}
	return match(cfg.InputConfig.Include) && !match(cfg.InputConfig.Exclude)
}

func (p *filelog) active() []*fileReceiver {
	var out []*fileReceiver
	for _, id := range p.order {
		r := p.receivers[id]
		r.mu.Lock()
		created := r.created
		r.mu.Unlock()
		if created {
			out = append(out, r)
		}
	}
	return out
}

// quietPeriod is how long the receivers must emit nothing before the run
// ends: the declared drain, or the slowest receiver's flush period plus a
// poll interval.
func (p *filelog) quietPeriod(rs []*fileReceiver) time.Duration {
	if p.drain > 0 {
		return p.drain
	}
	var out time.Duration
	for _, r := range rs {
		poll, flush := defaultPollInterval, defaultFlushPeriod
		if r.cfg != nil {
			if r.cfg.InputConfig.PollInterval > 0 {
				poll = r.cfg.InputConfig.PollInterval
			}
			if r.cfg.InputConfig.FlushPeriod > 0 {
				flush = r.cfg.InputConfig.FlushPeriod
			}
		}
		out = max(out, flush+poll)
	}
	return out
}

// sandboxed returns a copy of cfg reading the sandbox: include and exclude
// patterns are mapped into it, and an operator restoring deployment paths
// runs before the configured ones.
func (p *filelog) sandboxed(orig *filelogreceiver.FileLogConfig) (*filelogreceiver.FileLogConfig, error) {
	c := *orig
	c.InputConfig.Include = p.mapPatterns(orig.InputConfig.Include)
	c.InputConfig.Exclude = p.mapPatterns(orig.InputConfig.Exclude)
	if orig.InputConfig.IncludeFilePath || orig.InputConfig.IncludeFilePathResolved {
		ops, err := p.restoreOperators(orig.InputConfig.IncludeFilePath, orig.InputConfig.IncludeFilePathResolved)
		if err != nil {
			return nil, err
		}
		c.Operators = append(ops, orig.Operators...)
	}
	return &c, nil
}

func (p *filelog) mapPatterns(patterns []string) []string {
	if patterns == nil {
		return nil
	}
	out := make([]string, len(patterns))
	for i, pat := range patterns {
		out[i] = p.hostPath(pat)
	}
	return out
}

// restoreOperators builds add operators that replace the sandbox path in
// log.file.path (and log.file.path_resolved) with the deployment path, so
// the configured operators, such as the container parser, see the paths
// of a real deployment.
func (p *filelog) restoreOperators(path, resolved bool) ([]operator.Config, error) {
	var table strings.Builder
	table.WriteString("{")
	keys := make([]string, 0, len(p.hostPaths))
	for k := range p.hostPaths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			table.WriteString(", ")
		}
		table.WriteString(strconv.Quote(k) + ": " + strconv.Quote(p.hostPaths[k]))
	}
	table.WriteString("}")
	var out []operator.Config
	for _, attr := range []struct {
		on   bool
		name string
	}{{path, "log.file.path"}, {resolved, "log.file.path_resolved"}} {
		if !attr.on {
			continue
		}
		field := `attributes["` + attr.name + `"]`
		conf := confmap.NewFromStringMap(map[string]any{
			"type":  "add",
			"id":    restoreOperatorID + "_" + strings.ReplaceAll(strings.TrimPrefix(attr.name, "log.file."), ".", "_"),
			"if":    field + " != nil",
			"field": field,
			"value": "EXPR(" + table.String() + `[replace(` + field + `, "\\", "/")] ?? ` + field + ")",
		})
		var oc operator.Config
		if err := conf.Unmarshal(&oc); err != nil {
			return nil, fmt.Errorf("build path restore operator: %w", err)
		}
		out = append(out, oc)
	}
	return out, nil
}

// hostPath maps a deployment path or pattern into the sandbox. Absolute
// POSIX paths go under root/, Windows drive paths under drive-<letter>/,
// UNC paths under unc/ and relative paths (relative to the Collector's
// working directory) under cwd/.
func (p *filelog) hostPath(deployment string) string {
	return filepath.Join(p.root, filepath.FromSlash(sandboxRel(deployment)))
}

// sandboxRel is the slash-separated path of a deployment path (or pattern)
// relative to the sandbox root. It is cleaned as if the sandbox were the
// filesystem root, so .. can never leave the sandbox: the receiver reads no
// host file outside it, whatever its include and exclude patterns say.
func sandboxRel(deployment string) string {
	s := strings.ReplaceAll(deployment, `\`, "/")
	var rel string
	switch {
	case len(s) >= 2 && s[1] == ':' && isLetter(s[0]):
		rel = "drive-" + strings.ToLower(s[:1]) + "/" + strings.TrimLeft(s[2:], "/")
	case strings.HasPrefix(s, "//"):
		rel = "unc/" + strings.TrimLeft(s, "/")
	case strings.HasPrefix(s, "/"):
		rel = "root/" + strings.TrimLeft(s, "/")
	default:
		rel = "cwd/" + s
	}
	return strings.TrimPrefix(path.Clean("/"+rel), "/")
}

// deploymentPath maps a sandbox path back; other paths are returned as is.
func (p *filelog) deploymentPath(hostPath string) string {
	if d, ok := p.hostPaths[filepath.ToSlash(hostPath)]; ok {
		return d
	}
	return hostPath
}

func (r *fileReceiver) pollCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.polls
}

func (r *fileReceiver) lastEmitted() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastEmit
}

// pollCore observes a filelog receiver's polls through the debug message
// fileconsumer logs at the start of each one, whatever the log level.
type pollCore struct {
	zapcore.Core
	r       *fileReceiver
	restore func(string) string
}

func (c pollCore) Enabled(zapcore.Level) bool { return true }

func (c pollCore) With(fields []zapcore.Field) zapcore.Core {
	return pollCore{Core: c.Core.With(fields), r: c.r, restore: c.restore}
}

func (c pollCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if e.Message == matchedFilesMessage {
		ce = ce.AddCore(e, pollWriter(c))
	}
	return c.Core.Check(e, ce)
}

// pollWriter is the core added for poll entries; its Write records the
// poll instead of logging.
type pollWriter pollCore

func (w pollWriter) Enabled(zapcore.Level) bool { return true }
func (w pollWriter) With([]zapcore.Field) zapcore.Core {
	return w
}
func (w pollWriter) Check(_ zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce
}
func (w pollWriter) Sync() error { return nil }

func (w pollWriter) Write(_ zapcore.Entry, fields []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	w.r.polls++
	if paths, ok := enc.Fields["paths"].([]any); ok {
		for _, v := range paths {
			if s, ok := v.(string); ok {
				w.r.matched[w.restore(s)] = true
			}
		}
	}
	return nil
}

// recordingLogs reports what the receiver emits before passing it on.
type recordingLogs struct {
	next consumer.Logs
	r    *fileReceiver
	host mock.Host
}

func (c *recordingLogs) Capabilities() consumer.Capabilities { return c.next.Capabilities() }

func (c *recordingLogs) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	c.r.mu.Lock()
	c.r.lastEmit = time.Now()
	c.r.mu.Unlock()
	if c.host != nil {
		ctx = c.host.Received(ctx, c.r.id, telemetry.FromLogs(ld))
	}
	return c.next.ConsumeLogs(ctx, ld)
}

func validateFiles(files []mock.File) error {
	seen := map[string]bool{}
	for i, f := range files {
		if strings.TrimSpace(f.Path) == "" {
			return fmt.Errorf("files[%d]: path is required", i)
		}
		if strings.ContainsAny(f.Path, "*?[{") {
			return fmt.Errorf("files[%d]: path %q is a pattern; give the file's path", i, f.Path)
		}
		key := strings.ReplaceAll(f.Path, `\`, "/")
		if slices.Contains(strings.Split(key, "/"), "..") {
			return fmt.Errorf("files[%d]: path %s contains .., which would leave the mocked filesystem", i, f.Path)
		}
		if seen[key] {
			return fmt.Errorf("files[%d]: duplicate path %s", i, f.Path)
		}
		seen[key] = true
	}
	return nil
}

func hasExisting(files []mock.File) bool {
	return slices.ContainsFunc(files, func(f mock.File) bool { return len(f.Existing) > 0 })
}

func appendFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	return errors.Join(werr, f.Close())
}

func touch(path string, t time.Time) error {
	if t.IsZero() {
		return nil
	}
	return os.Chtimes(path, t, t)
}

// waitFor polls cond until it holds or ctx ends.
func waitFor(ctx context.Context, cond func() bool) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for !cond() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func sortIDs(ids []component.ID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
}

// Prepare runs the provider on the run's lifecycle.
func (p *filelog) Prepare(ctx context.Context, run provider.Run) error {
	return mock.PrepareBuiltin(ctx, run, mock.Lifecycle{Start: p.start, Ready: p.ready, Drive: p.drive, Declarations: p.declarations, Close: p.close})
}
