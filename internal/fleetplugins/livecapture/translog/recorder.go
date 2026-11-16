package translog

import (
	"bytes"
	"encoding/json"
	"sync"
)

// Entry is one decoded log line.
type Entry map[string]any

// Name is the event name (the msg field).
func (e Entry) Name() string { s, _ := e["msg"].(string); return s }

// Text is a string field, empty when absent.
func (e Entry) Text(key string) string { s, _ := e[key].(string); return s }

// Recorder collects what a logger writes, for tests. Its logger writes
// synchronously through the same JSON handler as New, so a test reads its
// lines at once.
type Recorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// NewRecorder returns a logger and the recorder that holds its lines.
func NewRecorder() (*Logger, *Recorder) {
	r := &Recorder{}
	return &Logger{out: jsonLogger(r)}, r
}

// Write appends to the recorded text.
func (r *Recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

// Raw is the text written so far.
func (r *Recorder) Raw() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// Entries are the decoded lines written so far.
func (r *Recorder) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Entry
	for _, line := range bytes.Split(bytes.TrimSpace(r.buf.Bytes()), []byte("\n")) {
		var e Entry
		if len(line) > 0 && json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// Events are the event names written so far.
func (r *Recorder) Events() []string {
	var names []string
	for _, e := range r.Entries() {
		names = append(names, e.Name())
	}
	return names
}
