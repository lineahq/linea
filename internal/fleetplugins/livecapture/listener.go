package livecapture

import (
	"net"
	"sync"
	"sync/atomic"
)

type connectionBudget struct {
	slots   chan struct{}
	refused atomic.Uint64
}
type boundedListener struct {
	net.Listener
	budget *connectionBudget
}
type boundedConnection struct {
	net.Conn
	once    sync.Once
	release func()
}

// Refuse excess TCP peers promptly, including unfinished TLS/HTTP handshakes.
func (l *boundedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.budget.slots <- struct{}{}:
			return &boundedConnection{Conn: c, release: func() { <-l.budget.slots }}, nil
		default:
			l.budget.refused.Add(1)
			_ = c.Close()
		}
	}
}
func (c *boundedConnection) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }
