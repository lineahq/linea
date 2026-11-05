package mcp

import "github.com/lineahq/linea/internal/debugger/platform"

// caller is the local owner via MCP, as identify sets it in local mode, for
// tests that call the platform directly.
var caller = platform.Local("mcp")
