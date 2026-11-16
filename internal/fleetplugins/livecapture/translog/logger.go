package translog

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// queueLines bounds the lines waiting for the writer.
const queueLines = 256

// Logger writes transition lines. A nil Logger and Discard() write nothing,
// so a component that was given no logger needs no check.
type Logger struct {
	out   *slog.Logger
	queue *queue
}

func jsonLogger(w io.Writer) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler).With(slog.String("component", "livecapture"))
}

// New returns a logger that writes JSON lines to w through a bounded queue.
// A full queue drops the line and counts it; Log never waits for w.
func New(w io.Writer) *Logger {
	q := newQueue(w)
	return &Logger{out: jsonLogger(q), queue: q}
}

// Discard returns a logger that writes nothing.
func Discard() *Logger { return &Logger{} }

// Log writes one line for e with attrs. The event name is the line's msg.
func (l *Logger) Log(e Event, attrs ...Attr) {
	if l == nil || l.out == nil {
		return
	}
	values := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		values = append(values, a.a)
	}
	l.out.LogAttrs(context.Background(), e.level(), e.String(), values...)
}

// Dropped is the number of lines dropped because the queue was full.
func (l *Logger) Dropped() uint64 {
	if l == nil || l.queue == nil {
		return 0
	}
	return l.queue.dropped.Load()
}

// Close writes the queued lines, waiting at most wait for the writer, and
// stops it. Lines logged afterwards are dropped.
func (l *Logger) Close(wait time.Duration) {
	if l == nil || l.queue == nil {
		return
	}
	l.queue.close(wait)
}

type queue struct {
	w       io.Writer
	lines   chan []byte
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	dropped atomic.Uint64
}

func newQueue(w io.Writer) *queue {
	q := &queue{w: w, lines: make(chan []byte, queueLines), stop: make(chan struct{}), done: make(chan struct{})}
	go q.run()
	return q
}

// Write queues one line; the JSON handler calls it once per record.
func (q *queue) Write(p []byte) (int, error) {
	select {
	case <-q.stop:
		q.dropped.Add(1)
		return len(p), nil
	default:
	}
	line := append([]byte(nil), p...)
	select {
	case q.lines <- line:
	default:
		q.dropped.Add(1)
	}
	return len(p), nil
}

func (q *queue) run() {
	defer close(q.done)
	for {
		select {
		case line := <-q.lines:
			_, _ = q.w.Write(line)
		case <-q.stop:
			for {
				select {
				case line := <-q.lines:
					_, _ = q.w.Write(line)
				default:
					return
				}
			}
		}
	}
}

func (q *queue) close(wait time.Duration) {
	q.once.Do(func() { close(q.stop) })
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-q.done:
	case <-timer.C:
	}
}
