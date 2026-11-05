package container

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// tarEntry is one entry of a test archive; Link marks a symbolic link and
// Dir a directory.
type tarEntry struct {
	Name, Data, Link string
	Dir              bool
}

func tarBytes(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.Name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(e.Data))}
		switch {
		case e.Dir:
			h.Typeflag, h.Size, h.Mode = tar.TypeDir, 0, 0o755
		case e.Link != "":
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.Link, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.Data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tarGz is a gzipped tar of regular files, as writeArchive writes them.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var entries []tarEntry
	for name, data := range files {
		entries = append(entries, tarEntry{Name: name, Data: data})
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(tarBytes(t, entries...)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const procModule = "example.com/proc"

func procEntries(extra ...tarEntry) []tarEntry {
	return append([]tarEntry{{Name: "go.mod", Data: "module " + procModule + "\n\ngo 1.27\n"}}, extra...)
}

// TestModuleArchiveReadsAModuleDirectory unpacks files and directories,
// with or without a leading "./".
func TestModuleArchiveReadsAModuleDirectory(t *testing.T) {
	archive := tarBytes(t, procEntries(
		tarEntry{Name: "./", Dir: true},
		tarEntry{Name: "pkg/", Dir: true},
		tarEntry{Name: "./proc.go", Data: "package proc\n"},
		tarEntry{Name: "pkg/x.go", Data: "package pkg\n"},
	)...)
	src, err := ModuleArchive(procModule, archive)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			got = append(got, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"go.mod", "pkg/x.go", "proc.go"}; !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	if data, err := fs.ReadFile(src, "proc.go"); err != nil || string(data) != "package proc\n" {
		t.Fatalf("proc.go %q, %v", data, err)
	}
	if err := CheckModuleArchive(procModule, archive); err != nil {
		t.Fatal(err)
	}
}

// TestModuleArchiveRefusesWhatTheServerRefuses applies the rules of
// the module archive: no links or devices, no paths out of the archive, a go.mod at the root
// naming the module, and a bound on the unpacked size.
func TestModuleArchiveRefusesWhatTheServerRefuses(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	var bigEntries []tarEntry
	for i := 0; i <= MaxModuleBytes>>20; i++ {
		bigEntries = append(bigEntries, tarEntry{Name: fmt.Sprintf("data/%d.bin", i), Data: big})
	}
	var many []tarEntry
	for i := 0; i <= MaxModuleFiles; i++ {
		many = append(many, tarEntry{Name: fmt.Sprintf("f/%d.go", i)})
	}
	for name, tc := range map[string]struct {
		archive []byte
		want    string
	}{
		"a symbolic link": {tarBytes(t, procEntries(tarEntry{Name: "link", Link: "/etc/passwd"})...), "not a regular file"},
		"an escape":       {tarBytes(t, procEntries(tarEntry{Name: "../x.go", Data: "package x\n"})...), "not a relative path"},
		"an absolute":     {tarBytes(t, procEntries(tarEntry{Name: "/etc/x.go", Data: "x"})...), "not a relative path"},
		"a backslash":     {tarBytes(t, procEntries(tarEntry{Name: `a\b.go`, Data: "x"})...), "not a relative path"},
		"an unclean path": {tarBytes(t, procEntries(tarEntry{Name: "a/../b.go", Data: "x"})...), "not a relative path"},
		"another module":  {tarBytes(t, tarEntry{Name: "go.mod", Data: "module example.com/other\n"}), `declares module "example.com/other"`},
		"no go.mod":       {tarBytes(t, tarEntry{Name: "proc.go", Data: "package proc\n"}), "no go.mod"},
		"a nested go.mod": {tarBytes(t, tarEntry{Name: "sub/go.mod", Data: "module " + procModule + "\n"}), "no go.mod"},
		"not a tar":       {[]byte("not a tar archive, just some bytes that are long enough to be read as a header"), "not a tar archive"},
		"too many bytes":  {tarBytes(t, append(procEntries(), bigEntries...)...), "more than"},
		"too many files":  {tarBytes(t, append(procEntries(), many...)...), "more than"},
	} {
		_, err := ModuleArchive(procModule, tc.archive)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ModuleArchive error %v, want %q", name, err, tc.want)
		}
		if err := CheckModuleArchive(procModule, tc.archive); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: CheckModuleArchive error %v, want %q", name, err, tc.want)
		}
	}
}
