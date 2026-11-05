package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lineahq/linea/internal/fleet/plugins"
)

func testConfig() Config {
	c := DefaultConfig()
	c.Storage = "memory"
	c.ManagementAddr, c.AgentAddr = "127.0.0.1:0", "127.0.0.1:0"
	c.ServerID, c.APIToken, c.AgentToken = "test-fleet", strings.Repeat("m", 32), strings.Repeat("a", 32)
	return c
}

func TestConfigurationExposurePolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
		valid  bool
	}{
		{"default", func(c *Config) {}, true},
		{"remote-http", func(c *Config) { c.AgentAddr = "0.0.0.0:4320" }, false},
		{"remote-acknowledged", func(c *Config) { c.AgentAddr = "0.0.0.0:4320"; c.AllowInsecureRemote = true }, true},
		{"hostname-http", func(c *Config) { c.ManagementAddr = "localhost:8080" }, false},
		{"weak-secret", func(c *Config) { c.APIToken = "short" }, false},
		{"shared-secret", func(c *Config) { c.APIToken = c.AgentToken }, false},
		{"no-server-id", func(c *Config) { c.ServerID = "" }, false},
		{"partial-tls", func(c *Config) { c.TLSCert = "cert.pem" }, false},
		{"zero-limit", func(c *Config) { c.Limits.MaxAgents = 0 }, false},
		{"unbounded-message", func(c *Config) { c.Limits.MaxMessageBytes = -1 }, false},
		{"no-plugin-listener", func(c *Config) { c.PluginAddr = "unused-invalid-address" }, true},
		{"remote-plugin-host", func(c *Config) { c.PluginsConfig = "operator.json"; c.PluginAddr = "0.0.0.0:8081" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			tc.change(&c)
			err := c.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestRunStandaloneAndShutdown(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "tls"}[secure], func(t *testing.T) {
			c := testConfig()
			client := &http.Client{Timeout: 3 * time.Second}
			scheme, wsScheme := "http://", "ws://"
			var clientTLS *tls.Config
			if secure {
				// Reuse a locally generated httptest identity; its private key never leaves TempDir.
				fixture := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				identity := fixture.TLS.Certificates[0]
				var err error
				c.TLSCert, c.TLSKey, err = writeTLSFixture(t, identity)
				if err != nil {
					t.Fatal(err)
				}
				client = fixture.Client()
				client.Timeout = 3 * time.Second
				clientTLS = client.Transport.(*http.Transport).TLSClientConfig
				fixture.Close()
				scheme, wsScheme = "https://", "wss://"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			addresses := make(chan [2]string, 1)
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, c, "test", func(m, a net.Addr) { addresses <- [2]string{m.String(), a.String()} })
			}()
			var addrs [2]string
			select {
			case addrs = <-addresses:
			case err := <-done:
				t.Fatalf("startup: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("startup timeout")
			}
			r, _ := http.NewRequest("GET", scheme+addrs[0]+"/api/v1/server", nil)
			r.Header.Set("Authorization", "Bearer "+c.APIToken)
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			var discovery struct {
				Storage  string   `json:"storage"`
				ServerID string   `json:"server_id"`
				Plugins  []string `json:"plugins"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&discovery)
			_ = resp.Body.Close()
			if decodeErr != nil || resp.StatusCode != 200 || discovery.ServerID != c.ServerID || discovery.Storage != "memory" || len(discovery.Plugins) != 0 {
				t.Fatalf("discovery: %+v err=%v", discovery, decodeErr)
			}
			dialer := websocket.Dialer{TLSClientConfig: clientTLS, HandshakeTimeout: 3 * time.Second}
			ws, _, err := dialer.Dial(wsScheme+addrs[1]+"/v1/opamp", http.Header{"Authorization": {"Bearer " + c.AgentToken}})
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("shutdown timeout")
			}
			_ = ws.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := ws.ReadMessage(); err == nil {
				t.Fatal("WebSocket survived shutdown")
			}
			client.CloseIdleConnections()
		})
	}
}

func TestSecondListenerFailureReleasesFirst(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	c := testConfig()
	c.ManagementAddr = addr
	c.AgentAddr = occupied.Addr().String()
	if err := Run(context.Background(), c, "test", nil); err == nil {
		t.Fatal("expected bind failure")
	}
	check, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal("management listener leaked:", err)
	}
	_ = check.Close()
}

func TestPluginListenerFailureReleasesCoreListeners(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	freeAddress := func() string {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		return addr
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "host"), []byte(strings.Repeat("a", 64)), 0600)
	_ = os.WriteFile(filepath.Join(dir, "service"), []byte(strings.Repeat("b", 64)), 0600)
	registration := plugins.Config{ID: "offline", Version: "0.1.0", Endpoint: "http://127.0.0.1:8090", Enabled: false, Permissions: []string{}, HostTokenFile: "host", ServiceTokenFile: "service"}
	body, _ := json.Marshal(map[string]any{"plugins": []plugins.Config{registration}})
	c := testConfig()
	c.PluginsConfig = filepath.Join(dir, "plugins.json")
	_ = os.WriteFile(c.PluginsConfig, body, 0600)
	c.ManagementAddr, c.AgentAddr, c.PluginAddr = freeAddress(), freeAddress(), occupied.Addr().String()
	if err := Run(context.Background(), c, "test", nil); err == nil {
		t.Fatal("host bind conflict was ignored")
	}
	for _, address := range []string{c.ManagementAddr, c.AgentAddr} {
		l, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal("core listener leaked after plugin bind failure")
		}
		_ = l.Close()
	}
}

func TestTCPAdmissionReleasesCapacity(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &limitedListener{Listener: raw, slots: make(chan struct{}, 1)}
	defer l.Close()
	accepted := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		t.Helper()
		c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	take := func() net.Conn {
		t.Helper()
		select {
		case c := <-accepted:
			return c
		case <-time.After(time.Second):
			t.Fatal("accept timed out")
			return nil
		}
	}
	_ = dial()
	first := take()
	defer first.Close()
	second := dial()
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("over-capacity TCP connection accepted")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("over-capacity TCP connection was not closed")
	}
	_ = first.Close()
	_ = dial()
	third := take()
	_ = third.Close()
}

func writeTLSFixture(t *testing.T, cert tls.Certificate) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	key, err := marshalFixtureKey(cert.PrivateKey)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(certPath, certificatePEM(cert.Certificate[0]), 0600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}
