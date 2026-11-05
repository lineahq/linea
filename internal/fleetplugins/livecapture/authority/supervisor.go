// Package authority supervises a single fenced database authority. It has no
// application-domain state and depends only on the standard library.
package authority

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Lease keeps its owning session pinned until Serve and Close have finished.
// Serve must withdraw admission on cancellation and stop within a bounded time.
type Lease struct {
	Serve func(context.Context) error
	Close func()
}

// Run retries only explicitly classified contention/transient errors. The private
// probes expose liveness/ownership, never application data or credentials.
func Run(parent context.Context, address string, acquire func(context.Context) (Lease, error), retryable func(error) bool, notify func(net.Addr)) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return errors.New("authority probe listen failed")
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var active atomic.Bool
	var acquisitions atomic.Uint64
	stop := context.AfterFunc(ctx, func() { active.Store(false) })
	defer stop()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" || r.URL.RawQuery != "" || (r.URL.Path != "/healthz" && r.URL.Path != "/activez") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		owned := active.Load()
		if r.URL.Path == "/activez" && !owned {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(struct {
			Active       bool   `json:"active"`
			Acquisitions uint64 `json:"acquisitions"`
		}{owned, acquisitions.Load()})
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096}
	defer server.Close()
	failures := make(chan error, 1)
	go func() {
		failures <- server.Serve(&limitedListener{Listener: listener, slots: make(chan struct{}, 16)})
		cancel()
	}()
	if notify != nil {
		notify(listener.Addr())
	}
	for ctx.Err() == nil {
		attempt, finish := context.WithTimeout(ctx, 5*time.Second)
		lease, err := acquire(attempt)
		finish()
		if err == nil {
			if lease.Serve == nil || lease.Close == nil {
				return errors.New("invalid authority lease")
			}
			active.Store(true)
			if ctx.Err() == nil {
				acquisitions.Add(1)
				err = lease.Serve(ctx)
			}
			active.Store(false)
			// Retire callbacks/transports/session before attempting replacement.
			lease.Close()
		}
		if ctx.Err() != nil {
			break
		}
		if err == nil || !retryable(err) {
			if err == nil {
				err = errors.New("authority activity stopped unexpectedly")
			}
			return err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if parent.Err() != nil {
		return nil
	}
	select {
	case <-failures:
		return errors.New("authority probe stopped")
	default:
		return errors.New("authority supervisor stopped")
	}
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
}
type limitedConn struct {
	net.Conn
	once sync.Once
	done func()
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &limitedConn{Conn: c, done: func() { <-l.slots }}, nil
		default:
			_ = c.Close()
		}
	}
}
func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.done)
	return err
}
