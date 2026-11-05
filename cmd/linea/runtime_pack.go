package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Module sources for a team server's builders: each local
// module directory the manifest builds from, and each --source directory,
// is packed into an uncompressed tar the server stores and a builder
// unpacks. The server checks the archive again; this side refuses early.

const (
	// maxPackedFiles and maxPackedBytes are the server's bounds on one
	// module archive's files and unpacked bytes.
	maxPackedFiles = 20000
	maxPackedBytes = 64 << 20
)

// skippedSourceDirs are never packed, as a local build's source tree
// leaves them out.
var skippedSourceDirs = []string{".git", ".hg", ".svn", "node_modules"}

// localModule is a module the build takes from a directory on this machine.
type localModule struct {
	Module string
	// Dir is the absolute directory holding the module's go.mod.
	Dir string
}

// manifestLocalDirs lists the directories a manifest builds modules from:
// `path:` entries and replaces with a directory, each relative to the
// manifest file's directory, a replace winning over an entry of the same
// module as in a local build. It reads only the fields it needs, so a
// manifest from a newer builder is accepted.
func manifestLocalDirs(text []byte, manifestDir string) (map[string]string, error) {
	type entry struct {
		GoMod string `yaml:"gomod"`
		Path  string `yaml:"path"`
	}
	var m struct {
		Receivers  []entry  `yaml:"receivers"`
		Processors []entry  `yaml:"processors"`
		Exporters  []entry  `yaml:"exporters"`
		Connectors []entry  `yaml:"connectors"`
		Extensions []entry  `yaml:"extensions"`
		Providers  []entry  `yaml:"providers"`
		Converters []entry  `yaml:"converters"`
		Telemetry  *entry   `yaml:"telemetry"`
		Replaces   []string `yaml:"replaces"`
	}
	if err := yaml.Unmarshal(text, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	dirs := map[string]string{}
	resolve := func(dir string) string {
		if filepath.IsAbs(dir) {
			return filepath.Clean(dir)
		}
		return filepath.Join(manifestDir, dir)
	}
	lists := [][]entry{m.Receivers, m.Processors, m.Exporters, m.Connectors, m.Extensions, m.Providers, m.Converters}
	if m.Telemetry != nil {
		lists = append(lists, []entry{*m.Telemetry})
	}
	for _, list := range lists {
		for _, e := range list {
			if module := strings.Fields(e.GoMod); e.Path != "" && len(module) > 0 {
				dirs[module[0]] = resolve(e.Path)
			}
		}
	}
	for _, r := range m.Replaces {
		left, right, ok := strings.Cut(r, "=>")
		lf, rf := strings.Fields(left), strings.Fields(right)
		if !ok || len(lf) < 1 || len(lf) > 2 || len(rf) < 1 || len(rf) > 2 {
			return nil, fmt.Errorf("manifest: replaces: %q must be \"<module> [version] => <module> <version>\" or \"<module> [version] => <directory>\"", r)
		}
		if len(rf) == 1 {
			dirs[lf[0]] = resolve(rf[0])
		}
	}
	return dirs, nil
}

// buildLocalModules is every module a server build uploads: the manifest's
// local directories, a --source directory replacing the manifest's for its
// module, sorted by module.
func buildLocalModules(text []byte, manifestPath string, overrides map[string]string) ([]localModule, error) {
	abs, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, err
	}
	dirs, err := manifestLocalDirs(text, filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	for module, dir := range overrides {
		dirs[module] = dir
	}
	out := make([]localModule, 0, len(dirs))
	for module, dir := range dirs {
		out = append(out, localModule{Module: module, Dir: dir})
	}
	slices.SortFunc(out, func(a, b localModule) int { return strings.Compare(a.Module, b.Module) })
	return out, nil
}

// packModule packs a module directory into an uncompressed tar: go.mod at
// the archive root, directories and regular files only, relative paths with
// forward slashes in lexical order, without owners or times, so an unchanged
// directory packs to the same bytes and a build of it can be reused.
// Symbolic links and other special files are not followed or packed, and
// skippedSourceDirs are left out. It returns the archive and its file count.
func packModule(dir string) ([]byte, int, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, 0, err
	}
	if !info.IsDir() {
		return nil, 0, fmt.Errorf("%s is not a directory", dir)
	}
	if mod, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil || !mod.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s has no go.mod", dir)
	}
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	var files int
	var size int64
	err = filepath.WalkDir(dir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() && slices.Contains(skippedSourceDirs, d.Name()) {
			return fs.SkipDir
		}
		slash := filepath.ToSlash(rel)
		if strings.Contains(slash, "\\") {
			return fmt.Errorf("%s: a file name with a backslash cannot be uploaded", slash)
		}
		if d.IsDir() {
			return w.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: slash + "/", Mode: 0o755, ModTime: time.Unix(0, 0)})
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		files++
		size += fi.Size()
		if files > maxPackedFiles || size > maxPackedBytes {
			return fmt.Errorf("%s has more than %d files or %d bytes; a server takes no more", dir, maxPackedFiles, int64(maxPackedBytes))
		}
		mode := int64(0o644)
		if fi.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: slash, Mode: mode, Size: fi.Size(), ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		// A file that changes while it is read would corrupt the archive.
		if n, err := io.CopyN(w, f, fi.Size()); err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%s changed while it was packed (%d of %d bytes)", slash, n, fi.Size())
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if err := w.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), files, nil
}
