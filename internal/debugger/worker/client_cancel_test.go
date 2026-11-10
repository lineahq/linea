package worker

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// TestAWorkerThatIgnoresACancelIsStopped: a request whose context ends
// sends a cancel; a worker that answers neither it nor the request within
// cancelGrace is stopped, the request fails naming it, and the worker's
// other requests end as its connection does.
func TestAWorkerThatIgnoresACancelIsStopped(t *testing.T) {
	defer func(g time.Duration) { cancelGrace = g }(cancelGrace)
	cancelGrace = 50 * time.Millisecond

	toWorker, fromServer := io.Pipe()
	fromWorker, toServer := io.Pipe()
	c := newClient("stubborn", fromServer)
	c.closeIn, c.wait = fromServer.Close, func() error { return nil }
	c.kill = func() {
		_ = toWorker.Close()
		_ = toServer.Close()
	}
	received := make(chan *message, 8)
	go func() {
		// The worker reads every request and cancel and answers none.
		if err := writeFrame(toServer, &message{Type: typeHello, Hello: &Hello{Protocol: ProtocolVersion, Runtime: "sha256:stub"}}); err != nil {
			return
		}
		for {
			m, err := readFrame(toWorker)
			if err != nil {
				return
			}
			received <- m
		}
	}()
	if _, err := c.handshake(context.Background(), fromWorker); err != nil {
		t.Fatal(err)
	}

	other := make(chan error, 1)
	go func() {
		_, err := c.do(context.Background(), &message{Type: typeInspect}, nil, nil)
		other <- err
	}()
	ctx, cancel := context.WithCancel(context.Background())
	failed := make(chan error, 1)
	go func() {
		_, err := c.do(ctx, &message{Type: typeExecute}, nil, nil)
		failed <- err
	}()
	for range 2 {
		<-received
	}
	cancel()
	if m := <-received; m.Type != typeCancel {
		t.Fatalf("after the context ended the worker got %q, want a cancel", m.Type)
	}
	select {
	case err := <-failed:
		if !errors.Is(err, ErrCancelIgnored) || errors.Is(err, ErrWorkerExited) {
			t.Fatalf("cancelled request = %v, want ErrCancelIgnored", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the cancelled request still waits on the worker")
	}
	select {
	case err := <-other:
		if !errors.Is(err, ErrWorkerExited) {
			t.Fatalf("other request = %v, want the worker gone", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the worker's other request still waits")
	}
	<-c.Exited()
}
