package worker

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lineahq/linea/internal/debugger/engine"
)

// A connected worker (connect.go) and the server that accepts it carry the
// protocol's frames over a WebSocket, one frame per binary message. This
// file holds what both ends share: timeouts, the upgrader, the stream and
// the reading of the server's URL.

const (
	// handshakeTimeout bounds the HTTP upgrade and the hello.
	handshakeTimeout = 30 * time.Second
)

var (
	// pingInterval is how often the server pings, so a gateway's idle
	// timeout never closes a connection it holds; idleTimeout is how long
	// either side waits without a message or ping before it gives up.
	// Tests shorten them.
	pingInterval = 30 * time.Second
	idleTimeout  = 3 * pingInterval
)

var upgrader = websocket.Upgrader{
	HandshakeTimeout: handshakeTimeout,
	// A worker is not a browser; a request a browser sends with an Origin
	// is refused.
	CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
}

// stream carries the protocol's byte stream over a WebSocket: each Write is
// one binary message (writeFrame writes a frame at once), and Read reads
// the messages in order. A close message is the end of the stream.
type stream struct {
	conn    *websocket.Conn
	r       io.Reader
	writeMu sync.Mutex
}

func newStream(conn *websocket.Conn) *stream {
	conn.SetReadLimit(maxFrame + 4)
	return &stream{conn: conn}
}

func (s *stream) Read(p []byte) (int, error) {
	for {
		if s.r == nil {
			_ = s.conn.SetReadDeadline(time.Now().Add(idleTimeout))
			typ, r, err := s.conn.NextReader()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					return 0, io.EOF
				}
				return 0, err
			}
			if typ != websocket.BinaryMessage {
				continue
			}
			s.r = r
		}
		n, err := s.r.Read(p)
		if errors.Is(err, io.EOF) {
			s.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (s *stream) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// closeWrite tells the peer the stream ended; it keeps reading until the
// peer closes too.
func (s *stream) closeWrite() error {
	return s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
}

// parseServerURL reads the server URL a worker is given: https://host[:port]
// [/prefix], or http://host[:port][/prefix] to any host the operator chose,
// such as the server's Service. A bare host:port is http for a loopback
// address and https otherwise. A token never travels in the URL.
func parseServerURL(raw string, ca []byte) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: a server URL is required", engine.ErrInvalidRequest)
	}
	if !strings.Contains(raw, "://") {
		host, _, err := net.SplitHostPort(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: server address %q is neither a URL nor host:port", engine.ErrInvalidRequest, raw)
		}
		scheme := "https"
		if loopback(host) {
			scheme = "http"
		}
		raw = scheme + "://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("%w: server URL %q has no host", engine.ErrInvalidRequest, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: server URL %q: give the token separately, without credentials, query or fragment", engine.ErrInvalidRequest, raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if len(ca) > 0 {
			return nil, fmt.Errorf("%w: server URL %q is http; a CA needs https", engine.ErrInvalidRequest, raw)
		}
	default:
		return nil, fmt.Errorf("%w: server URL %q: scheme %q is not https or http", engine.ErrInvalidRequest, raw, u.Scheme)
	}
	if len(ca) > 0 {
		if _, err := certPool(ca); err != nil {
			return nil, err
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func certPool(ca []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("%w: the CA holds no PEM certificate", engine.ErrInvalidRequest)
	}
	return pool, nil
}
