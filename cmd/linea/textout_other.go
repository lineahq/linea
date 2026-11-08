//go:build !windows

package main

import "io"

// enableColor needs nothing outside Windows: terminals interpret escape
// sequences.
func enableColor(io.Writer) bool { return true }
