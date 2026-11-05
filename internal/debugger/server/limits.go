package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// The team server's HTTP bounds. ReadTimeout covers a request's headers and
// body only: net/http clears the read deadline once the body is read, so
// event streams and long requests outlive it. Event streams write for as
// long as a run lasts, so there is no WriteTimeout; WebSocket connections
// from workers and builders set their own deadlines once upgraded.
const (
	serveReadHeaderTimeout = 10 * time.Second
	// serveReadTimeout lets the largest upload, a 65 MiB build request,
	// arrive at about 220 KiB/s.
	serveReadTimeout    = 5 * time.Minute
	serveIdleTimeout    = 2 * time.Minute
	serveMaxHeaderBytes = 64 << 10
)

// newTeamHTTPServer returns the server for one of linea-server's listeners with
// the bounds above.
func newTeamHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: serveReadHeaderTimeout, ReadTimeout: serveReadTimeout,
		IdleTimeout: serveIdleTimeout, MaxHeaderBytes: serveMaxHeaderBytes}
}

// securityHeaders sets the headers every team-server response carries:
// no page may frame the web client (it acts with a signed-in session), no
// response is sniffed as another content type, and no URL leaves in a
// Referer header.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// limitListener holds at most cap(slots) accepted connections open,
// counting TLS handshakes and hijacked WebSockets until they close; it
// closes connections beyond that at once.
type limitListener struct {
	net.Listener
	slots chan struct{}
}

func newLimitListener(l net.Listener, n int) *limitListener {
	return &limitListener{Listener: l, slots: make(chan struct{}, n)}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &limitConn{Conn: c, release: func() { <-l.slots }}, nil
		default:
			_ = c.Close()
		}
	}
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
