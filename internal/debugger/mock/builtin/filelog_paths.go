package builtin

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// restorePaths returns text with each sandbox path in it replaced by the
// deployment path it stands for, so what the receiver logs names the
// scenario's paths rather than a directory that differs on every run.
func (p *filelog) restorePaths(text string) string {
	if p.root == "" || !strings.Contains(text, p.root) {
		return text
	}
	var b strings.Builder
	for {
		i := strings.Index(text, p.root)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:i])
		end := i + len(p.root)
		for end < len(text) && !strings.ContainsRune(" \t\r\n\"',;", rune(text[end])) &&
			!(text[end] == ':' && (end+1 == len(text) || text[end+1] == ' ')) {
			end++
		}
		b.WriteString(p.unsandbox(text[i:end]))
		text = text[end:]
	}
}

// unsandbox maps a path under the sandbox to its deployment path: a
// scenario file to its path as written, anything else (a directory, a
// pattern) by inverting sandboxRel.
func (p *filelog) unsandbox(hostPath string) string {
	if d, ok := p.hostPaths[filepath.ToSlash(hostPath)]; ok {
		return d
	}
	rel := strings.TrimPrefix(filepath.ToSlash(strings.TrimPrefix(hostPath, p.root)), "/")
	head, tail, _ := strings.Cut(rel, "/")
	switch {
	case head == "root":
		return "/" + tail
	case head == "unc":
		return "//" + tail
	case head == "cwd":
		return tail
	case strings.HasPrefix(head, "drive-") && len(head) == len("drive-")+1:
		return strings.TrimPrefix(head, "drive-") + ":/" + tail
	}
	return hostPath
}

// restoreCore restores deployment paths in a filelog receiver's log
// entries before they reach its logger: the message, string fields and
// errors.
type restoreCore struct {
	zapcore.Core
	restore func(string) string
}

func (c restoreCore) With(fields []zapcore.Field) zapcore.Core {
	return restoreCore{Core: c.Core.With(c.fields(fields)), restore: c.restore}
}

func (c restoreCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	e.Message = c.restore(e.Message)
	inner := c.Core.Check(e, nil)
	if inner == nil {
		return ce
	}
	return ce.AddCore(e, restoreWriter{inner: inner, c: c})
}

func (c restoreCore) fields(fields []zapcore.Field) []zapcore.Field {
	out := fields
	for i, f := range fields {
		var text string
		switch f.Type {
		case zapcore.StringType:
			text = f.String
		case zapcore.StringerType, zapcore.ErrorType:
			if f.Interface == nil {
				continue
			}
			text = fmt.Sprint(f.Interface)
		default:
			continue
		}
		restored := c.restore(text)
		if restored == text {
			continue
		}
		if len(out) > 0 && &out[0] == &fields[0] {
			out = slices.Clone(fields)
		}
		if f.Type == zapcore.ErrorType {
			out[i] = zap.NamedError(f.Key, errors.New(restored))
		} else {
			out[i] = zap.String(f.Key, restored)
		}
	}
	return out
}

// restoreWriter writes an entry the inner core accepted, with its fields
// restored. Each entry is checked and written once, so the inner checked
// entry is used once.
type restoreWriter struct {
	inner *zapcore.CheckedEntry
	c     restoreCore
}

func (w restoreWriter) Enabled(zapcore.Level) bool        { return true }
func (w restoreWriter) With([]zapcore.Field) zapcore.Core { return w }
func (w restoreWriter) Check(_ zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce
}
func (w restoreWriter) Sync() error { return nil }

func (w restoreWriter) Write(_ zapcore.Entry, fields []zapcore.Field) error {
	w.inner.Write(w.c.fields(fields)...)
	return nil
}
