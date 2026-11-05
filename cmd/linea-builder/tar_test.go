package main

import (
	"archive/tar"
	"io"
	"testing"
)

// tarBuilder writes the uncompressed tar a module upload is.
type tarBuilder struct{ tw *tar.Writer }

func newTar(w io.Writer) *tarBuilder { return &tarBuilder{tw: tar.NewWriter(w)} }

func (b *tarBuilder) add(t *testing.T, name, data string) {
	t.Helper()
	if err := b.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.tw.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
}

func (b *tarBuilder) link(t *testing.T, name, target string) {
	t.Helper()
	if err := b.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: target}); err != nil {
		t.Fatal(err)
	}
}

func (b *tarBuilder) close(t *testing.T) {
	t.Helper()
	if err := b.tw.Close(); err != nil {
		t.Fatal(err)
	}
}
