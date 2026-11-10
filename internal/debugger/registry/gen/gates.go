package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/lineahq/linea/internal/debugger/gatescan"
)

// writeGates writes gateowners.go for the module of the working directory.
func writeGates(out, source string) error {
	src, err := gatescan.Generate(goList, source)
	if err != nil {
		return err
	}
	return os.WriteFile(out, src, 0o644)
}

// goList runs a go command with extra environment variables.
func goList(env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
