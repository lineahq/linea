package worker

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Workers dial the server: a worker started with --connect
// opens GET <server>/v1/workers/connect with the registration token as a
// bearer token, and the WebSocket carries protocol 2's frames, one frame
// per binary message. The worker sends its hello, with its runtime name and
// capacity, and then answers the server's
// requests; the server pings it, and either side gives up after
// idleTimeout without a message or ping. A worker reconnects until it is
// stopped.

// ConnectPath ends the URL workers dial.
const ConnectPath = "/v1/workers/connect"

var (
	// reconnectMin and reconnectMax bound the wait between a worker's
	// connection attempts; tests shorten them.
	reconnectMin = time.Second
	reconnectMax = 30 * time.Second
)

// ErrUnauthorized means the server refused the worker's registration token.
var ErrUnauthorized = errors.New("the server refused the worker's registration token")

// ConnectOptions configure Connect.
type ConnectOptions struct {
	// URL is the server's base URL: https://host[:port][/prefix], or http://
	// to any host the operator chose, such as the server's Service.
	URL string
	// CA verifies the server's certificate; empty uses the system roots.
	CA []byte
	// Token is the registration token; it is required.
	Token string
	// Name is the runtime name the worker serves under; Capacity how many
	// requests it runs at once, at least one.
	Name     string
	Capacity int
	// Log receives a line per connection; nil discards them.
	Log io.Writer
}

// Connect dials the server and serves it, reconnecting after a lost
// connection, until ctx ends; it then returns nil once what was running
// has stopped. A refused token ends it with ErrUnauthorized.
func Connect(ctx context.Context, opts ConnectOptions) error {
	if opts.Token == "" {
		return errors.New("worker: a connecting worker needs a registration token")
	}
	if strings.TrimSpace(opts.Name) == "" {
		return errors.New("worker: a connecting worker needs a runtime name")
	}
	if opts.Capacity < 1 {
		return errors.New("worker: a connecting worker's capacity is at least 1")
	}
	if _, err := Identity(); err != nil {
		return err
	}
	t, err := newDialTarget(opts.URL, opts.CA, opts.Token, ConnectPath)
	if err != nil {
		return err
	}
	return redial(ctx, t, opts.Log, func(ctx context.Context, conn *websocket.Conn, logf func(string, ...any)) error {
		logf("connected to %s as %s (capacity %d)", t.target, opts.Name, opts.Capacity)
		s := newStream(conn)
		if err := serve(ctx, s, s, opts.Name, opts.Capacity); err != nil {
			return err
		}
		_ = s.closeWrite()
		return errors.New("the server closed the connection")
	})
}

// dialTarget is a server endpoint a worker or builder dials, with its
// dialer and credentials.
type dialTarget struct {
	d      websocket.Dialer
	server string
	target string
	header http.Header
}

func newDialTarget(rawURL string, ca []byte, token, path string) (dialTarget, error) {
	u, err := parseServerURL(rawURL, ca)
	if err != nil {
		return dialTarget{}, err
	}
	t := dialTarget{server: u.String(), header: http.Header{"Authorization": {"Bearer " + token}}}
	u.Path += path
	u.Scheme = map[string]string{"https": "wss", "http": "ws"}[u.Scheme]
	t.target = u.String()
	t.d = websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: handshakeTimeout,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if len(ca) > 0 {
		if t.d.TLSClientConfig.RootCAs, err = certPool(ca); err != nil {
			return dialTarget{}, err
		}
	}
	return t, nil
}

// redial dials t and holds each connection with hold, reconnecting with
// backoff after a lost one, until ctx ends; it then returns nil. A refused
// token ends it with ErrUnauthorized.
func redial(ctx context.Context, t dialTarget, log io.Writer, hold func(context.Context, *websocket.Conn, func(string, ...any)) error) error {
	logf := func(format string, args ...any) {
		if log != nil {
			fmt.Fprintf(log, format+"\n", args...)
		}
	}
	wait := reconnectMin
	for ctx.Err() == nil {
		began := time.Now()
		err := dialOnce(ctx, t, func(ctx context.Context, conn *websocket.Conn) error { return hold(ctx, conn, logf) })
		if errors.Is(err, ErrUnauthorized) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(began) > reconnectMax {
			wait = reconnectMin // the connection held; start over
		}
		logf("connection to %s: %v; reconnecting in %s", t.server, err, wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = min(2*wait, reconnectMax)
	}
	return nil
}

// dialOnce holds one connection until it ends.
func dialOnce(ctx context.Context, t dialTarget, hold func(context.Context, *websocket.Conn) error) error {
	conn, resp, err := t.d.DialContext(ctx, t.target, t.header)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			reason := strings.TrimSpace(string(body))
			if resp.StatusCode == http.StatusUnauthorized {
				return fmt.Errorf("%w: %s", ErrUnauthorized, reason)
			}
			return fmt.Errorf("the server answered %s, not as a connection endpoint: %s", resp.Status, reason)
		}
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(handshakeTimeout))
	})
	return hold(ctx, conn)
}

// Accept checks a dialing worker's registration token with authorize,
// upgrades its request and waits for its hello. On a refused token or a
// failed upgrade it has answered the request and returns the error. The
// client's name is the worker's runtime name.
func Accept(w http.ResponseWriter, r *http.Request, authorize func(token string) bool) (*Client, error) {
	if r.Method != http.MethodGet {
		http.Error(w, "workers connect with GET", http.StatusMethodNotAllowed)
		return nil, errors.New("worker connection: not a GET")
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !authorize(token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="linea-workers"`)
		http.Error(w, "the registration token is wrong", http.StatusUnauthorized)
		return nil, ErrUnauthorized
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	s := newStream(conn)
	c := newClient("worker", s)
	c.endpoint = r.RemoteAddr
	c.closeIn = s.closeWrite
	c.wait = conn.Close
	c.kill = func() { _ = conn.Close() }
	keepAlive(c.exited, conn)
	ctx, cancel := context.WithTimeout(r.Context(), handshakeTimeout)
	defer cancel()
	if _, err := c.handshake(ctx, s); err != nil {
		return nil, err
	}
	c.name = c.hello.Name
	return c, nil
}

// keepAlive pings the peer until done is closed; its pongs extend the
// connection's read deadline.
func keepAlive(done <-chan struct{}, conn *websocket.Conn) {
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(idleTimeout))
	})
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(handshakeTimeout))
			}
		}
	}()
}

// ConstantTimeEqual reports whether a presented token is the expected one,
// in time independent of where they differ.
func ConstantTimeEqual(presented, expected string) bool {
	return expected != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}
