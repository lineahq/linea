package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProbeTrustsOnlyThePluginCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()
	// httptest servers share one certificate, so the other one is generated.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}, &x509.Certificate{SerialNumber: big.NewInt(2)}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	other.StartTLS()
	defer other.Close()
	file := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	transport, err := probeTransport(file)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport}
	if r, err := client.Get(server.URL); err != nil {
		t.Fatalf("own certificate: %v", err)
	} else {
		r.Body.Close()
	}
	if r, err := client.Get(other.URL); err == nil {
		r.Body.Close()
		t.Fatal("another certificate was trusted")
	}
	if _, err := probeTransport(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing certificate accepted")
	}
}
