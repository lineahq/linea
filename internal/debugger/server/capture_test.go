package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFleetServeFlagsReadTheTokenFromAFile(t *testing.T) {
	if opts, err := (fleetServeOptions{}).load(); err != nil || opts.Fleet.Endpoint != "" {
		t.Fatalf("default startup: %+v %v", opts, err)
	}
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("secret-fleet-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts, err := fleetServeOptions{url: "https://fleet:8080", tokenFile: token}.load()
	if err != nil || opts.Fleet.Token != "secret-fleet-token" || opts.Fleet.Endpoint != "https://fleet:8080" {
		t.Fatalf("%v", err)
	}
	malformed := filepath.Join(dir, "malformed")
	if err := os.WriteFile(malformed, []byte("secret part two"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, o := range []fleetServeOptions{
		{url: "https://fleet:8080"},
		{tokenFile: token},
		{allowHTTP: true},
		{url: "https://fleet:8080", tokenFile: filepath.Join(dir, "missing")},
		{url: "https://fleet:8080", tokenFile: malformed},
		{url: "http://fleet:8080", tokenFile: token},
		{url: "https://fleet:8080/path", tokenFile: token},
	} {
		_, err := o.load()
		if err == nil {
			t.Fatalf("accepted %+v", o)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("error exposed the token: %v", err)
		}
	}
	if _, err := (fleetServeOptions{url: "http://fleet:8080", tokenFile: token, allowHTTP: true}).load(); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"--config", writeTeam(t, teamYAML()), "--addr", "127.0.0.1:0", "--fleet-url", "https://fleet:8080"}, &out, &stderr); code != exitError || !strings.Contains(stderr.String(), "--fleet-token-file") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}
