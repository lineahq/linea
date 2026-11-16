package livecapture

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostCAFileIsTrusted(t *testing.T) {
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer host.Close()
	dir := t.TempDir()
	file := func(name, body string) string {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte(body), 0600)
		return p
	}
	ca := file("ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: host.Certificate().Raw})))
	load := func(endpoint, caFile string) (Config, error) {
		body, _ := json.Marshal(map[string]any{"id": "livecapture", "version": Version, "host_endpoint": endpoint, "host_token_file": file("host", strings.Repeat("a", 64)),
			"service_token_file": file("service", strings.Repeat("b", 64)), "http_listen": "127.0.0.1:0", "control_listen": "127.0.0.1:1", "delivery_listen": "127.0.0.1:2",
			"tls_cert_file": "cert", "tls_key_file": "key", "raw_principals": []string{}, "host_ca_file": caFile, "allow_insecure_host": strings.HasPrefix(endpoint, "http://")})
		return LoadConfig(file("config.json", string(body)))
	}
	c, err := load(host.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.hostClient().Get(host.URL)
	if err != nil {
		t.Fatal("the host CA was not trusted:", err)
	}
	resp.Body.Close()
	if c, err = load(host.URL, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.hostClient().Get(host.URL); err == nil {
		t.Fatal("a private CA was trusted without host_ca_file")
	}
	for _, x := range []struct{ endpoint, ca string }{{host.URL, file("bad.pem", "x")}, {host.URL, filepath.Join(dir, "missing")}, {"http://127.0.0.1:1", ca}} {
		if _, err := load(x.endpoint, x.ca); err == nil {
			t.Fatalf("loaded %s with %s", x.endpoint, x.ca)
		}
	}
}
