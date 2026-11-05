package gateway

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
)

type tlsTestConnection struct{ net.Conn }

func (c *tlsTestConnection) Connection() net.Conn                                 { return c.Conn }
func (c *tlsTestConnection) Disconnect() error                                    { return c.Conn.Close() }
func (c *tlsTestConnection) Send(context.Context, *protobufs.ServerToAgent) error { return nil }

func TestForcedTLSDisconnectDoesNotWaitForPeer(t *testing.T) {
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	identity := fixture.TLS.Certificates[0]
	fixture.Close()
	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()
	_ = serverSide.SetDeadline(time.Now().Add(3 * time.Second))
	_ = clientSide.SetDeadline(time.Now().Add(3 * time.Second))
	serverTLS := tls.Server(serverSide, &tls.Config{Certificates: []tls.Certificate{identity}, MaxVersion: tls.VersionTLS12})
	clientTLS := tls.Client(clientSide, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}) // local fixture only
	handshake := make(chan error, 1)
	go func() { handshake <- serverTLS.Handshake() }()
	if err := clientTLS.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-handshake; err != nil {
		t.Fatal(err)
	}
	// The peer stops reading. A normal tls.Conn.Close would block sending its
	// close_notify record, serially stalling reporting or shutdown.
	done := make(chan struct{})
	go func() { disconnect(&tlsTestConnection{serverTLS}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forced cleanup waited for a TLS peer")
	}
}
