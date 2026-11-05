// Command notices writes the third-party notices file for Linea binaries:
// the license and notice files of every Go module the given packages link,
// of the Go standard library and runtime, and of any extra file passed with
// -extra (the web client's bundled licenses for linea-server). It fails when
// a linked module has no license or notice file.
//
//	go run ./tools/notices -title linea-server -extra web=internal/debugger/webui/static/app/licenses.md -o THIRD_PARTY_NOTICES.txt ./cmd/linea-server
//
// GOOS and GOARCH select the target the way go build does, so the list
// matches the binary built for that platform.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// own is this project's module path; its modules are not third-party.
const own = "github.com/lineahq/linea"

type extras []string

func (e *extras) String() string     { return strings.Join(*e, ",") }
func (e *extras) Set(v string) error { *e = append(*e, v); return nil }

func main() {
	title := flag.String("title", "", "name of the software the notices are for")
	out := flag.String("o", "", "output file (default standard output)")
	var extra extras
	flag.Var(&extra, "extra", "name=file: add file as the notices of the named component (repeatable)")
	flag.Parse()
	if *title == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: notices -title <name> [-extra name=file] [-o file] <package>...")
		os.Exit(2)
	}
	if err := run(*title, *out, extra, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func run(title, out string, extra []string, pkgs []string) error {
	mods, err := linkedModules(pkgs)
	if err != nil {
		return err
	}
	cs, err := collect(mods, []string{own})
	if err != nil {
		return err
	}
	std, err := goNotices()
	if err != nil {
		return err
	}
	cs = append([]component{std}, cs...)
	for _, e := range extra {
		name, file, ok := strings.Cut(e, "=")
		if !ok || name == "" || file == "" {
			return fmt.Errorf("-extra %q: want name=file", e)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		cs = append(cs, component{Name: name, Notices: []notice{{Name: filepath.Base(file), Text: strings.TrimSpace(string(b))}}})
	}
	w := os.Stdout
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	return render(w, title, cs)
}

// linkedModules lists the modules of every package the given packages
// depend on, for the target go build would use.
func linkedModules(pkgs []string) ([]module, error) {
	args := append([]string{"list", "-deps", "-f", "{{with .Module}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}"}, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	var mods []module
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 {
			continue
		}
		mods = append(mods, module{Path: f[0], Version: f[1], Dir: f[2]})
	}
	return mods, sc.Err()
}

// goNotices returns the Go distribution's license, which covers the
// standard library and runtime compiled into every binary.
func goNotices() (component, error) {
	b, err := exec.Command("go", "env", "GOROOT", "GOVERSION").Output()
	if err != nil {
		return component{}, fmt.Errorf("go env: %w", err)
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return component{}, fmt.Errorf("go env: unexpected output %q", b)
	}
	n, err := readNotice(f[0], "LICENSE")
	if err != nil {
		return component{}, err
	}
	return component{Name: "Go standard library and runtime", Version: f[1], Notices: []notice{n}}, nil
}
