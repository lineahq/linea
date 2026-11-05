package scenario

import (
	"fmt"
	"os"
	"path/filepath"
)

// Inline returns the scenario with every file it names (inputs[].file,
// mocks.files[].file and mocks.kubernetes.files) read from this host and
// written into the scenario, so it no longer depends on dir. A client uses
// it to send a scenario to a server that does not share its filesystem.
// A scenario without file references comes back unchanged.
func Inline(raw []byte, dir string) ([]byte, error) {
	s, err := Parse(raw, dir)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	read := func(location, name string) error {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("%s: %w", location, err)
		}
		if _, ok := files[abs]; ok {
			return nil
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Errorf("%s: %w", location, err)
		}
		files[abs] = data
		return nil
	}
	for i, in := range s.Inputs {
		if in.File != "" {
			if err := read(fmt.Sprintf("inputs[%d]", i), in.File); err != nil {
				return nil, err
			}
		}
	}
	for i, f := range s.Mocks.Files {
		if f.File != "" {
			if err := read(fmt.Sprintf("mocks.files[%d]", i), f.File); err != nil {
				return nil, err
			}
		}
	}
	for i, name := range s.Mocks.Kubernetes.Files {
		if err := read(fmt.Sprintf("mocks.kubernetes.files[%d]", i), name); err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return raw, nil
	}
	return FromRun(RunSources{Mode: "scenario", Scenario: raw, Dir: dir, Files: files}, "", nil)
}
