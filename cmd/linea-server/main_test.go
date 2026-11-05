package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestTheServerRefusesToStartWithoutAServerConfig runs the binary: it serves
// only what a server config composes.
func TestTheServerRefusesToStartWithoutAServerConfig(t *testing.T) {
	if os.Getenv("LINEA_SERVER_MAIN") == "1" {
		os.Args = []string{"linea-server"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTheServerRefusesToStartWithoutAServerConfig$")
	cmd.Env = append(os.Environ(), "LINEA_SERVER_MAIN=1")
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || !strings.Contains(string(out), "linea-server needs --config <file>") {
		t.Fatalf("exit %v: %s", err, out)
	}
}
