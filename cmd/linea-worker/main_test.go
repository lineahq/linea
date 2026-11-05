package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAConnectingWorkerNeedsAToken(t *testing.T) {
	t.Setenv(tokenEnv, "")
	err := run(context.Background(), []string{"--connect", "https://linea.example", "--name", "team"})
	if err == nil || !strings.Contains(err.Error(), tokenEnv) {
		t.Fatalf("run without a token = %v", err)
	}
	empty := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"--connect", "https://linea.example", "--name", "team", "--token-file", empty}); err == nil {
		t.Fatal("an empty token file was accepted")
	}
}

func TestConnectFlagsAreChecked(t *testing.T) {
	t.Setenv(tokenEnv, "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--name", "team"}, "--connect"},
		{[]string{"--capacity", "4"}, "--connect"},
		{[]string{"--ca-file", "ca.pem"}, "--connect"},
		{[]string{"--token-file", "token"}, "--connect"},
		{[]string{"--listen", "127.0.0.1:0"}, "not defined"},
		{[]string{"--connect", "https://linea.example", "--name", "team"}, tokenEnv},
	} {
		if err := run(context.Background(), tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("run %v = %v, want an error naming %s", tc.args, err, tc.want)
		}
	}
	t.Setenv(tokenEnv, "secret")
	if err := run(context.Background(), []string{"--connect", "https://linea.example", "--name", "team", "--ca-file", "missing.pem"}); err == nil || !strings.Contains(err.Error(), "--ca-file") {
		t.Errorf("a missing CA file = %v", err)
	}
}

func TestPoolFlagsAreChecked(t *testing.T) {
	t.Setenv(tokenEnv, "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--pool"}, "--connect"},
		{[]string{"--warm", "2"}, "--connect"},
		{[]string{"--cache-dir", "cache"}, "--connect"},
		{[]string{"--connect", "https://linea.example", "--warm", "2"}, "--pool"},
		{[]string{"--connect", "https://linea.example", "--cache", "1GiB"}, "--pool"},
		{[]string{"--connect", "https://linea.example", "--pool", "--cache", "0"}, "invalid size"},
		{[]string{"--connect", "https://linea.example", "--pool", "--cache", "lots"}, "invalid size"},
		// A pool needs no --name, only the token.
		{[]string{"--connect", "https://linea.example", "--pool"}, tokenEnv},
	} {
		if err := run(context.Background(), tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("run %v = %v, want an error naming %s", tc.args, err, tc.want)
		}
	}
	t.Setenv(tokenEnv, "secret")
	args := []string{"--connect", "https://linea.example", "--pool", "--ca-file", "missing.pem"}
	if err := run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "--ca-file") {
		t.Errorf("a pool with a missing CA file = %v", err)
	}
}

func TestSizeFlagParsesBinaryUnits(t *testing.T) {
	for in, want := range map[string]int64{"1024": 1024, "7B": 7, "3KiB": 3 << 10, "512mib": 512 << 20, "5GiB": 5 << 30, "1TiB": 1 << 40} {
		var f sizeFlag
		if err := f.Set(in); err != nil || int64(f) != want {
			t.Errorf("Set(%q) = %v, %d, want %d", in, err, f, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "GiB", "1.5GiB", "9999999999TiB", "5GB"} {
		var f sizeFlag
		if err := f.Set(in); err == nil {
			t.Errorf("Set(%q) = %d, want an error", in, f)
		}
	}
}
