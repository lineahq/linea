package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// module is one third-party Go module linked into a binary.
type module struct {
	Path    string
	Version string
	Dir     string
}

// notice is one license or notice file of a component.
type notice struct {
	Name string
	Text string
}

// component is what the notices file lists: a module, the Go standard
// library or an extra file such as the web client's bundled licenses.
type component struct {
	Name    string
	Version string
	Notices []notice
}

// noticeFile matches the files a module ships its terms in: LICENSE,
// LICENCE, COPYING and NOTICE, with or without an extension or suffix
// (LICENSE.md, LICENSE-APACHE, NOTICE.txt).
var noticeFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice)([._-].*)?$`)

// licenseDir is a directory of license files, as REUSE lays them out.
var licenseDir = regexp.MustCompile(`(?i)^licen[cs]es$`)

// readNotices returns the license and notice files at the root of dir and in
// a LICENSES directory there, in name order. A module without any is an
// error: its terms cannot be passed on.
func readNotices(dir string) ([]notice, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []notice
	for _, e := range entries {
		switch {
		case e.Type().IsRegular() && noticeFile.MatchString(e.Name()):
			n, err := readNotice(dir, e.Name())
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		case e.IsDir() && licenseDir.MatchString(e.Name()):
			sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			for _, s := range sub {
				if !s.Type().IsRegular() {
					continue
				}
				n, err := readNotice(dir, filepath.ToSlash(filepath.Join(e.Name(), s.Name())))
				if err != nil {
					return nil, err
				}
				out = append(out, n)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no license or notice file")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func readNotice(dir, name string) (notice, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		return notice{}, err
	}
	return notice{Name: name, Text: strings.TrimSpace(string(b))}, nil
}

// collect turns the linked modules into components. Modules whose path has
// one of the skip prefixes are this project's own and are left out. Every
// module missing its terms is reported together.
func collect(mods []module, skip []string) ([]component, error) {
	var out []component
	var errs []error
	seen := map[string]bool{}
	for _, m := range mods {
		if hasPrefix(m.Path, skip) || seen[m.Path+"@"+m.Version] {
			continue
		}
		seen[m.Path+"@"+m.Version] = true
		if m.Dir == "" {
			errs = append(errs, fmt.Errorf("%s %s: module is not downloaded", m.Path, m.Version))
			continue
		}
		ns, err := readNotices(m.Dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", m.Path, m.Version, err))
			continue
		}
		out = append(out, component{Name: m.Path, Version: m.Version, Notices: ns})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func hasPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

const rule = "================================================================================"

// render writes the notices file: a heading, the list of components, then
// each component's files in full.
func render(w io.Writer, title string, cs []component) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Third-party notices for %s\n\n", title)
	b.WriteString("This software includes the third-party components listed below. Each\n")
	b.WriteString("component's license and notice files follow the list, unchanged.\n\n")
	for _, c := range cs {
		fmt.Fprintf(&b, "  %s\n", label(c))
	}
	for _, c := range cs {
		fmt.Fprintf(&b, "\n%s\n%s\n%s\n", rule, label(c), rule)
		for _, n := range c.Notices {
			fmt.Fprintf(&b, "\n--- %s ---\n\n%s\n", n.Name, n.Text)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func label(c component) string {
	if c.Version == "" {
		return c.Name
	}
	return c.Name + " " + c.Version
}
