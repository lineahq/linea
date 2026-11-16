package plugins

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistrationTrustsItsCAFile(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer upstream.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "host"), []byte(strings.Repeat("a", 64)), 0600)
	_ = os.WriteFile(filepath.Join(dir, "service"), []byte(strings.Repeat("b", 64)), 0600)
	_ = os.WriteFile(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600)
	_ = os.WriteFile(filepath.Join(dir, "bad.pem"), []byte("not a certificate"), 0600)
	load := func(endpoint, ca string) ([]Config, error) {
		body, _ := json.Marshal(map[string]any{"plugins": []Config{{ID: "example", Version: "0.1.0", Endpoint: endpoint, Enabled: true, Permissions: []string{},
			HostTokenFile: "host", ServiceTokenFile: "service", CAFile: ca, AllowInsecureHTTP: strings.HasPrefix(endpoint, "http://")}}})
		path := filepath.Join(dir, "registrations.json")
		_ = os.WriteFile(path, body, 0600)
		return Load(path)
	}
	get := func(configs []Config) error {
		r := New(context.Background(), configs, nil)
		defer r.cancel()
		resp, err := r.entries[0].client.Get(upstream.URL)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	configs, err := load(upstream.URL, "ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	if err := get(configs); err != nil {
		t.Fatal("the CA file was not trusted:", err)
	}
	if configs, err = load(upstream.URL, ""); err != nil {
		t.Fatal(err)
	}
	if get(configs) == nil {
		t.Fatal("a private CA was trusted without ca_file")
	}
	for _, c := range []struct{ endpoint, ca string }{{upstream.URL, "bad.pem"}, {upstream.URL, "missing.pem"}, {"http://127.0.0.1:1", "ca.pem"}} {
		if _, err := load(c.endpoint, c.ca); err == nil {
			t.Fatalf("loaded %s with %s", c.endpoint, c.ca)
		}
	}
}
