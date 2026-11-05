// Package build holds the built-in runtime's OCB manifest,
// linea-runtime.yaml, for the "built-in set" preset.
package build

import _ "embed"

// Manifest is build/linea-runtime.yaml, unchanged.
//
//go:embed linea-runtime.yaml
var Manifest []byte
