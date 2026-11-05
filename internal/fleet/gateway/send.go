package gateway

import (
	"context"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
)

// respond writes the reply itself instead of returning it to the library, so
// every frame carries a write deadline and a stalled peer is disconnected.
func (g *Gateway) respond(ctx context.Context, c types.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
	response := g.message(ctx, c, message)
	if response != nil {
		g.mu.Lock()
		s := g.connections[c]
		g.mu.Unlock()
		if s == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(g.ctx, 3*time.Second)
		if err := g.sendFrame(ctx, c, s, response); err != nil {
			disconnect(c)
		}
		cancel()
	}
	return nil
}

// sendFrame serializes writes per session. A canceled context forcibly closes
// the socket because opamp-go v0.25.0's WebSocket Send ignores its context.
func (g *Gateway) sendFrame(ctx context.Context, c types.Connection, s *session, message *protobufs.ServerToAgent) error {
	select {
	case s.writes <- struct{}{}:
	case <-ctx.Done():
		return fleet.ErrUnavailable
	}
	defer func() { <-s.writes }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.Connection().SetWriteDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { disconnect(c) })
	defer stop()
	if ctx.Err() != nil {
		return fleet.ErrUnavailable
	}
	return c.Send(ctx, message)
}
