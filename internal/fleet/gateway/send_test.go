package gateway

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/open-telemetry/opamp-go/protobufs"
)

type stalledConnection struct{ net.Conn }

func (c *stalledConnection) Connection() net.Conn { return c.Conn }
func (c *stalledConnection) Send(context.Context, *protobufs.ServerToAgent) error {
	_, err := c.Conn.Write([]byte("frame"))
	return err
}
func (c *stalledConnection) Disconnect() error { return c.Conn.Close() }

func TestSendCancellationClosesStalledSocket(t *testing.T) {
	a, _ := auth.New(strings.Repeat("z", 32))
	g, err := New(fleet.NewStore(1), a, 1, 8192)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	local, remote := net.Pipe()
	defer remote.Close()
	peer := &stalledConnection{local}
	s := &session{principal: auth.Principal{ID: "memory-agent", Role: "agent"}, id: strings.Repeat("1", 32), generation: 1, identified: true, writes: make(chan struct{}, 1)}
	g.connections[peer] = s
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.sendFrame(ctx, peer, s, &protobufs.ServerToAgent{}); err == nil {
		t.Fatal("stalled socket write was reported successful")
	}
	if _, err := local.Write([]byte("late")); err == nil {
		t.Fatal("timed-out connection still accepted writes")
	}
}
