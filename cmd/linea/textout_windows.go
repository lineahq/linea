package main

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// enableColor turns on escape sequence processing for a Windows console;
// a console that refuses it gets no color.
func enableColor(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
