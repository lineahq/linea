package mcp

import (
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/engine/local"
)

// Tests execute on the built-in runtime in process.
func init() { engine.Use(local.Collector) }
