package engine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"
)

func TestReferenceHTTPAcceptsCapturedInlineCA(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Unix(0, 0),
		NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:     true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	yaml := fmt.Sprintf("receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlphttp: {endpoint: http://example.invalid:4318, tls: {ca_pem: %q}}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlphttp]}}}\n", ca)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := Execute(ctx, Request{ConfigYAML: yaml, FixtureJSON: `{"resourceSpans":[]}`, Compare: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Result.Status != "completed" || report.Comparison == nil || !report.Comparison.Equal {
		t.Fatal("captured HTTP CA changed reference execution")
	}
}

func TestDecodedBundleCannotBypassReferencePreflight(t *testing.T) {
	yaml := strings.Replace(captureConfig, "endpoint: example.invalid:4317", "endpoint: example.invalid:4317, tls: {ca_file: private-ca}", 1)
	req, err := Prepare(context.Background(), Request{ConfigYAML: yaml, FixtureJSON: `{"resourceSpans":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := EncodeBundle(req)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := readBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	a.Request.Compare = true
	raw, err = json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBundle(raw); err != nil {
		t.Fatal("history inspection must not require executable prerequisites", err)
	}
	if _, err := DecodeBundle(context.Background(), raw); !errors.Is(err, runresult.ErrReferencePrerequisite) {
		t.Fatalf("decoded reference bypassed preflight: %v", err)
	}
}
