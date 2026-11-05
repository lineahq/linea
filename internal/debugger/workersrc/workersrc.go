// Package workersrc holds the worker's source, embedded in the debugger so
// that runtime builds compile the debugger's own instrumentation with the
// user's components. The archive in static/ is produced by
// go generate (gen/, from this repository) and is not committed; a debugger
// built without it cannot build runtimes and says so.
//
// The archive holds the Go files (for linux, the worker's target, and those
// excluded there), embedded files, go.mod and go.sum of every package of
// this module that cmd/linea-worker imports, and of every package a
// runtime's manifest may make it import instead: the optional features'
// packages and every OTTL variant, and of the registry generator,
// which writes a build's feature gate owners, and the provider SDK module
// with its go.mod. It holds the linea CLI's packages too (CLI), for linux,
// windows and darwin, so `linea build` compiles a CLI with a manifest's
// components; the archive itself is left out, so a built CLI cannot build
// runtimes or CLIs. A variant that is its own module,
// since it does not compile against this module's release, comes without
// its go.mod and go.sum and compiles as part of the build module. Tests pack
// the repository directly with Pack.
package workersrc

//go:generate go run ./gen -root ../../.. -out static/worker-source.tar.gz

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing/fstest"
	"time"

	"github.com/lineahq/linea/internal/debugger/features"
)

// Module is this repository's module path. A build module keeps it, so the
// copied packages import each other's internal packages unchanged.
const Module = "github.com/lineahq/linea"

// Main is the worker's main package inside the module.
const Main = "./cmd/linea-worker"

// CLI is the linea CLI's main package inside the module.
const CLI = "./cmd/linea"

// cliPlatforms are the operating systems a CLI is built for; the worker
// runs on linux only, so their extra packages are listed for the CLI.
var cliPlatforms = []string{"windows", "darwin"}

// compatDir holds the variant packages of the Collector APIs that changed
// between releases.
const compatDir = "internal/debugger/compat"

// gatesGenerator writes a build's feature gate owners from the resolved
// modules' source before the worker compiles.
const gatesGenerator = "./internal/debugger/registry/gen"

const archiveName = "static/worker-source.tar.gz"

//go:embed all:static
var static embed.FS

// ErrNotBuilt means this debugger was built without the worker source.
var ErrNotBuilt = errors.New("this debugger was built without the worker source; run `go generate ./internal/debugger/workersrc` and rebuild it")

// Embedded returns the embedded worker source.
func Embedded() (fs.FS, error) {
	raw, err := static.ReadFile(archiveName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotBuilt
	}
	if err != nil {
		return nil, err
	}
	return Unpack(raw)
}

// Pack archives the worker's source from a checkout of this module at root.
// It runs `go list` for linux/amd64, the worker's target, and for the CLI's
// other platforms.
func Pack(root string) ([]byte, error) {
	files, err := sourceFiles(root)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		// No times or owners, so the archive depends only on the source.
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unpack reads an archive written by Pack.
func Unpack(raw []byte) (fs.FS, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("workersrc: %w", err)
	}
	tr := tar.NewReader(zr)
	out := fstest.MapFS{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("workersrc: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || !fs.ValidPath(hdr.Name) {
			return nil, fmt.Errorf("workersrc: unexpected archive entry %q", hdr.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("workersrc: %w", err)
		}
		out[hdr.Name] = &fstest.MapFile{Data: data, Mode: 0o644}
	}
	for _, need := range []string{"go.mod", "go.sum"} {
		if _, ok := out[need]; !ok {
			return nil, fmt.Errorf("workersrc: the archive has no %s", need)
		}
	}
	return out, nil
}

type listedPackage struct {
	Dir            string
	ImportPath     string
	Module         *struct{ Path string }
	GoFiles        []string
	IgnoredGoFiles []string
	EmbedFiles     []string
}

// sourceFiles lists the slash-separated paths, relative to root, of every
// file the worker's build needs from this module.
func sourceFiles(root string) ([]string, error) {
	args := []string{"list", "-deps", "-json=Dir,ImportPath,Module,GoFiles,IgnoredGoFiles,EmbedFiles", Main, CLI, "./" + compatDir + "/...", gatesGenerator}
	for _, f := range features.All {
		args = append(args, f.Package)
	}
	out, err := goList(root, "linux", args)
	if err != nil {
		return nil, err
	}
	for _, goos := range cliPlatforms {
		more, err := goList(root, goos, []string{"list", "-deps", "-json=Dir,ImportPath,Module,GoFiles,IgnoredGoFiles,EmbedFiles", CLI})
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	files := []string{"go.mod", "go.sum"}
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p listedPackage
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("workersrc: go list output: %w", err)
		}
		if p.Module == nil || p.Module.Path != Module {
			continue
		}
		rel, err := filepath.Rel(absRoot, p.Dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("workersrc: package %s is outside %s", p.ImportPath, root)
		}
		dir := filepath.ToSlash(rel)
		for _, f := range slices.Concat(p.GoFiles, p.IgnoredGoFiles, p.EmbedFiles) {
			if strings.HasSuffix(f, "_test.go") || p.ImportPath == Module+"/"+packageDir && f == archiveName {
				continue
			}
			files = append(files, path.Join(dir, f))
		}
	}
	nested, err := nestedVariants(root)
	if err != nil {
		return nil, err
	}
	files = append(files, nested...)
	sdk, err := sdkFiles(root)
	if err != nil {
		return nil, err
	}
	files = append(files, sdk...)
	slices.Sort(files)
	return slices.Compact(files), nil
}

// goList runs a go list command in root for goos/amd64.
func goList(root, goos string, args []string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("workersrc: go list for %s: %w: %s", goos, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// packageDir is this package's directory in the module, whose archive the
// archive leaves out.
const packageDir = "internal/debugger/workersrc"

// sdkDir is the provider SDK's module, which every build
// replaces with this copy.
const sdkDir = "collector/provider"

// sdkFiles lists the SDK module's go.mod and Go files, without tests.
func sdkFiles(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, sdkDir))
	if err != nil {
		return nil, fmt.Errorf("workersrc: the provider SDK: %w", err)
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); e.Type().IsRegular() && (n == "go.mod" || strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go")) {
			out = append(out, path.Join(sdkDir, n))
		}
	}
	return out, nil
}

// nestedVariants lists the Go files of the variants under compatDir that
// are their own modules, without tests, go.mod and go.sum.
func nestedVariants(root string) ([]string, error) {
	var out []string
	base := filepath.Join(root, filepath.FromSlash(compatDir))
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if _, err := os.Stat(filepath.Join(p, "go.mod")); err != nil {
			return nil
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if n := e.Name(); e.Type().IsRegular() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
				out = append(out, path.Join(filepath.ToSlash(rel), n))
			}
		}
		return nil
	})
	return out, err
}
