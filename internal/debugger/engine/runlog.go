package engine

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// RunLogLimit bounds the Collector log a run keeps: its tail.
const RunLogLimit = 64 << 10

// redactedValue replaces an environment value in a run's log.
const redactedValue = "[REDACTED]"

// withRunLog tees the Collector's log at info level and above into w as
// console lines, one Write per entry, with every environment value the
// configuration read replaced, raw or JSON-escaped. Values are redacted
// however short, so a short value can also hide unrelated text.
func withRunLog(base *zap.Logger, w io.Writer, secrets []string) *zap.Logger {
	enc := zap.NewProductionEncoderConfig()
	enc.EncodeTime = zapcore.ISO8601TimeEncoder
	enc.EncodeLevel = zapcore.CapitalLevelEncoder
	core := zapcore.NewCore(zapcore.NewConsoleEncoder(enc), zapcore.AddSync(redactingWriter{w, redactor(secrets)}), zapcore.InfoLevel)
	if base == nil {
		return zap.New(core)
	}
	return zap.New(zapcore.NewTee(base.Core(), core))
}

// redactor replaces the longest values first, so a value containing another
// is replaced whole.
func redactor(secrets []string) *strings.Replacer {
	var forms []string
	for _, s := range secrets {
		if s == "" {
			continue
		}
		forms = append(forms, s)
		if raw, err := json.Marshal(s); err == nil {
			if escaped := string(raw[1 : len(raw)-1]); escaped != s {
				forms = append(forms, escaped)
			}
		}
	}
	slices.SortFunc(forms, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	forms = slices.Compact(forms)
	pairs := make([]string, 0, 2*len(forms))
	for _, f := range forms {
		pairs = append(pairs, f, redactedValue)
	}
	return strings.NewReplacer(pairs...)
}

type redactingWriter struct {
	w io.Writer
	r *strings.Replacer
}

func (w redactingWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.w, w.r.Replace(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// LogTail keeps the last RunLogLimit bytes of whole lines written to it. It
// is safe for concurrent use.
type LogTail struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func (t *LogTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - RunLogLimit; over > 0 {
		t.truncated = true
		rest := t.buf[over:]
		// Drop the partial line the cut leaves.
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			rest = rest[i+1:]
		} else {
			rest = nil
		}
		t.buf = append(t.buf[:0], rest...)
	}
	return len(p), nil
}

// Append adds a tail kept elsewhere, as a worker sends it; truncated says
// that one dropped lines.
func (t *LogTail) Append(log string, truncated bool) {
	_, _ = t.Write([]byte(log))
	if truncated {
		t.mu.Lock()
		t.truncated = true
		t.mu.Unlock()
	}
}

// Tail returns the kept lines and whether earlier ones were dropped.
func (t *LogTail) Tail() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf), t.truncated
}
