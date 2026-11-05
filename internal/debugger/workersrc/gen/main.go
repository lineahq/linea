// Command gen packs the worker's source into the archive the debugger
// embeds (internal/debugger/workersrc).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lineahq/linea/internal/debugger/workersrc"
)

func main() {
	root := flag.String("root", ".", "checkout of the debugger module")
	out := flag.String("out", "", "archive to write")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "gen: -out is required")
		os.Exit(2)
	}
	raw, err := workersrc.Pack(*root)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(*out), 0o755); err == nil {
			err = os.WriteFile(*out, raw, 0o644)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}
