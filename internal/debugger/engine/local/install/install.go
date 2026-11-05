// Package install makes this build's in-process Collector the process's:
// importing it for its side effect calls engine.Use(local.Collector) in
// init. Binaries that execute on the built-in runtime import it; a binary
// that does not, such as the team server, runs no Collector in process.
package install

import (
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/engine/local"
)

func init() { engine.Use(local.Collector) }
