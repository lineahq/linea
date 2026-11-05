package authority

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompetingReplicasReleaseBeforePromotion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	busy := errors.New("busy")
	failed := errors.New("failed")
	lock := make(chan struct{}, 1)
	crash := make(chan struct{}, 1)
	var serving, promoted, closed atomic.Int64
	addresses := make(chan string, 2)
	done := make(chan error, 2)
	acquire := func(ctx context.Context) (Lease, error) {
		select {
		case lock <- struct{}{}:
		default:
			return Lease{}, busy
		}
		return Lease{Serve: func(ctx context.Context) error {
			if serving.Add(1) != 1 {
				t.Error("overlapping authorities")
			}
			promoted.Add(1)
			select {
			case <-crash:
				return failed
			case <-ctx.Done():
				return nil
			}
		}, Close: func() { serving.Add(-1); closed.Add(1); <-lock }}, nil
	}
	for range 2 {
		go func() {
			done <- Run(ctx, "127.0.0.1:0", acquire, func(err error) bool { return errors.Is(err, busy) || errors.Is(err, failed) }, func(a net.Addr) { addresses <- "http://" + a.String() })
		}()
	}
	client := &http.Client{Timeout: time.Second}
	a, b := <-addresses, <-addresses
	wait := func(check func() bool) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			if check() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("authority transition timed out")
	}
	wait(func() bool { return promoted.Load() == 1 })
	active := 0
	for _, address := range []string{a, b} {
		r, err := client.Get(address + "/healthz")
		if err != nil || r.StatusCode != 200 {
			t.Fatal("standby must remain live")
		}
		r.Body.Close()
		r, err = client.Get(address + "/activez")
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode == 200 {
			active++
		}
		r.Body.Close()
	}
	if active != 1 {
		t.Fatal("standby advertised authority")
	}
	crash <- struct{}{}
	wait(func() bool { return promoted.Load() >= 2 })
	if closed.Load() < 1 || serving.Load() != 1 {
		t.Fatal("promotion preceded old authority cleanup")
	}
	cancel()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if serving.Load() != 0 || len(lock) != 0 {
		t.Fatal("shutdown retained authority")
	}
}

func TestPermanentFailureDoesNotRetry(t *testing.T) {
	var attempts int
	fatal := errors.New("schema mismatch")
	err := Run(context.Background(), "127.0.0.1:0", func(context.Context) (Lease, error) {
		attempts++
		return Lease{}, fatal
	}, func(error) bool { return false }, nil)
	if !errors.Is(err, fatal) || attempts != 1 {
		t.Fatal("permanent configuration failure was retried")
	}
}
