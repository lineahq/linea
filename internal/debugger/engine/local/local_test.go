package local_test

import (
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/engine/local"
)

// The engine's interface is what this package provides.
var _ engine.Collector = local.Collector
