package container

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"testing/fstest"

	"golang.org/x/mod/modfile"
)

// Limits of an uploaded module archive, unpacked.
const (
	MaxModuleFiles = 20000
	MaxModuleBytes = 64 << 20
)

// ModuleArchive reads an uploaded module source: an
// uncompressed tar of regular files and directories whose names are relative
// paths inside the archive, with at most MaxModuleFiles files and
// MaxModuleBytes bytes unpacked, and a go.mod at its root declaring module.
// The server checks an archive with this before it stores it and the builder
// before it unpacks it. The result is the module's directory.
func ModuleArchive(module string, archive []byte) (fs.FS, error) {
	return readModuleArchive(module, archive, true)
}

// CheckModuleArchive applies ModuleArchive's rules without keeping the
// files.
func CheckModuleArchive(module string, archive []byte) error {
	_, err := readModuleArchive(module, archive, false)
	return err
}

func readModuleArchive(module string, archive []byte, keep bool) (fs.FS, error) {
	r := tar.NewReader(bytes.NewReader(archive))
	out := fstest.MapFS{}
	var files int
	var size int64
	var gomod []byte
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not a tar archive: %v", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
			if name == "" || name == "." {
				continue
			}
		}
		if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || !fs.ValidPath(name) || path.Clean(name) != name {
			return nil, fmt.Errorf("entry %q is not a relative path inside the archive", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("entry %q is not a regular file or directory", h.Name)
		}
		files++
		size += h.Size
		if files > MaxModuleFiles || size > MaxModuleBytes {
			return nil, fmt.Errorf("more than %d files or %d bytes", MaxModuleFiles, MaxModuleBytes)
		}
		if name != "go.mod" && !keep {
			continue
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %v", name, err)
		}
		if name == "go.mod" {
			gomod = data
		}
		if keep {
			out[name] = &fstest.MapFile{Data: data, Mode: 0o644}
		}
	}
	if gomod == nil {
		return nil, errors.New("the archive has no go.mod at its root")
	}
	if declared := modfile.ModulePath(gomod); declared != module {
		return nil, fmt.Errorf("its go.mod declares module %q", declared)
	}
	if !keep {
		return nil, nil
	}
	return out, nil
}
