package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLimitListenerClosesConnectionsBeyondItsLimit holds two connections
// open on a listener limited to two, sees a third closed at once, and
// accepts another once one of the two closes.
func TestLimitListenerClosesConnectionsBeyondItsLimit(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newLimitListener(inner, 2)
	defer ln.Close()
	accepted := make(chan net.Conn)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		t.Helper()
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	next := func() net.Conn {
		t.Helper()
		select {
		case c := <-accepted:
			t.Cleanup(func() { c.Close() })
			return c
		case <-time.After(10 * time.Second):
			t.Fatal("a connection within the limit was not accepted")
			return nil
		}
	}

	dial()
	dial()
	first := next()
	next()

	// The listener closes the third at once: its client reads EOF.
	third := dial()
	_ = third.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := third.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("a connection beyond the limit was kept open: %v", err)
	}

	first.Close()
	dial()
	next()
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// TestSecurityHeadersAreSetOnEveryResponse checks the headers on a page and
// on an error.
func TestSecurityHeadersAreSetOnEveryResponse(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<p>page</p>"))
	}))
	for _, path := range []string{"/", "/missing"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		for name, want := range map[string]string{
			"Content-Security-Policy": "frame-ancestors 'none'",
			"X-Frame-Options":         "DENY",
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
		} {
			if got := rec.Header().Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", path, name, got, want)
			}
		}
	}
}
