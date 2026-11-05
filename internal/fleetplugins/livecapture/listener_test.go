package livecapture

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestConnectionBudgetRejectsAndReleasesUnfinishedPeers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	budget := &connectionBudget{slots: make(chan struct{}, 1)}
	bounded := &boundedListener{Listener: listener, budget: budget}
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := bounded.Accept(); accepted <- c }()
	one, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	first := <-accepted
	defer first.Close()
	go func() {
		c, _ := bounded.Accept()
		if c != nil {
			accepted <- c
		}
	}()
	two, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	_ = two.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := two.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal("excess unfinished peer was not promptly refused")
	}
	if budget.refused.Load() != 1 || len(budget.slots) != 1 {
		t.Fatal("connection refusal not accounted")
	}
	_ = first.Close()
	_ = first.Close()
	if len(budget.slots) != 0 {
		t.Fatal("closed peer did not release capacity exactly once")
	}
}
