package worker

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lineahq/linea/internal/debugger/release"
)

// dialRaw dials url with the test token and returns the worker's end of the
// stream, which only writes: the server's messages are read and dropped, so
// its close is answered.
func dialRaw(t *testing.T, url string) *stream {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + testToken}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()
	return newStream(conn)
}

func TestAcceptRefusesAHelloOutOfBounds(t *testing.T) {
	refused := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r, func(token string) bool { return ConstantTimeEqual(token, testToken) })
		if err == nil {
			_ = c.Close()
		}
		refused <- err
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + ConnectPath

	hello := func(name string, capacity int, modules ...string) *message {
		return &message{Type: typeHello, Hello: &Hello{Protocol: ProtocolVersion, Name: name, Capacity: capacity, Modules: modules}}
	}
	for _, tc := range []struct {
		name string
		m    *message
		want error
	}{
		{"a capacity above the bound", hello("w", maxHelloCapacity+1), errFrame},
		{"a name above the bound", hello(strings.Repeat("n", maxHelloName+1), 1), errFrame},
		// The read limit ends the connection before the hello arrives.
		{"a hello larger than its frame bound", hello("w", 1, strings.Repeat("m", maxHelloFrame)), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := writeFrame(dialRaw(t, url), tc.m); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-refused:
				if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
					t.Fatalf("Accept = %v, want %v", err, tc.want)
				}
			case <-time.After(time.Minute):
				t.Fatal("Accept did not return")
			}
		})
	}

	t.Run("a hello within the bounds", func(t *testing.T) {
		if err := writeFrame(dialRaw(t, url), hello(strings.Repeat("n", maxHelloName), maxHelloCapacity)); err != nil {
			t.Fatal(err)
		}
		if err := <-refused; err != nil {
			t.Fatalf("Accept = %v", err)
		}
	})
}

func TestAcceptBuilderRefusesAHelloOutOfBounds(t *testing.T) {
	refused := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := AcceptBuilder(w, r, func(token string) bool { return ConstantTimeEqual(token, testToken) })
		if err == nil {
			_ = b.Close()
		}
		refused <- err
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + BuilderPath

	for _, tc := range []struct {
		name  string
		hello BuilderHello
	}{
		{"a capacity above the bound", BuilderHello{Name: "b", Capacity: maxHelloCapacity + 1}},
		{"a hello larger than its frame bound", BuilderHello{Name: "b", Capacity: 1, GoVersion: strings.Repeat("g", maxHelloFrame)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.hello
			h.Protocol, h.ProductVersion = BuilderProtocol, release.ProductVersion
			if err := writeFrame(dialRaw(t, url), &message{Type: typeHello, BuilderHello: &h}); err != nil {
				t.Fatal(err)
			}
			if err := <-refused; err == nil {
				t.Fatal("AcceptBuilder accepted the hello")
			}
		})
	}
}

// TestAnAcceptedStreamTakesFullFramesAfterTheHello: only the first message
// is held to the hello bound.
func TestAnAcceptedStreamTakesFullFramesAfterTheHello(t *testing.T) {
	got := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			got <- err
			return
		}
		defer conn.Close()
		s := newAcceptedStream(conn)
		for range 2 {
			if _, err := readFrame(s); err != nil {
				got <- err
				return
			}
		}
		got <- nil
	}))
	defer srv.Close()
	s := dialRaw(t, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err := writeFrame(s, &message{Type: typeHello, Hello: &Hello{Protocol: ProtocolVersion}}); err != nil {
		t.Fatal(err)
	}
	big := &message{Type: typeHello, Hello: &Hello{Modules: []string{strings.Repeat("m", 2*maxHelloFrame)}}}
	if err := writeFrame(s, big); err != nil {
		t.Fatal(err)
	}
	if err := <-got; err != nil {
		t.Fatalf("reading a large frame after the hello: %v", err)
	}
}
