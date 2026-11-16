package livecapture

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAPIListenerServesTLSOverHTTP1: with http_tls the plugin API answers
// only over TLS with the listeners' certificate, and offers HTTP/1.1 alone,
// so an HTTP/2 client falls back.
func TestAPIListenerServesTLSOverHTTP1(t *testing.T) {
	// httptest's certificate names 127.0.0.1, so it stands in for the plugin's.
	keyed := httptest.NewTLSServer(nil)
	certificates := keyed.TLS.Certificates
	keyed.Close()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: certificates}
	leaf, err := x509.ParseCertificate(certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)

	serve := func(httpTLS bool) string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Proto) }), ReadHeaderTimeout: time.Second}
		go server.Serve(apiListener(l, &connectionBudget{slots: make(chan struct{}, 4)}, tlsConfig, httpTLS))
		t.Cleanup(func() { server.Close() })
		return l.Addr().String()
	}
	get := func(client *http.Client, url string) (string, error) {
		resp, err := client.Get(url)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return string(body), err
	}

	secure := serve(true)
	h2 := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, ForceAttemptHTTP2: true}}
	if proto, err := get(h2, "https://"+secure+"/"); err != nil || proto != "HTTP/1.1" {
		t.Fatalf("TLS request served as %q: %v", proto, err)
	}
	plain := &http.Client{Timeout: 2 * time.Second}
	// A TLS server answers plain HTTP with a 400, never through the handler.
	if body, err := get(plain, "http://"+secure+"/"); err == nil && strings.HasPrefix(body, "HTTP/") {
		t.Fatal("the TLS listener served plain HTTP")
	}
	if _, err := get(&http.Client{Timeout: 2 * time.Second}, "https://"+secure+"/"); err == nil {
		t.Fatal("a client without the plugin's certificate trusted it")
	}

	open := serve(false)
	if proto, err := get(plain, "http://"+open+"/"); err != nil || proto != "HTTP/1.1" {
		t.Fatalf("plain request served as %q: %v", proto, err)
	}
}
