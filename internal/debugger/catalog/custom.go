package catalog

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/manifest"
)

// Custom is a component built from a module source supplied with the build,
// found in that source by manifest.Discover.
type Custom struct {
	Module string
	manifest.Component
}

// AddCustom adds a manifest entry for each custom component to the text of
// a block-style manifest, after the entries of its kind, keeping the rest
// of the text as written. A component the manifest already names (its
// module and import) is not added again. The entries carry no path: the
// build's source supplies the directory.
func AddCustom(text []byte, custom []Custom) ([]byte, error) {
	if len(custom) == 0 {
		return text, nil
	}
	m, err := manifest.Parse(text)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, e := range m.Entries() {
		have[e.ImportPath()] = true
	}
	lines := strings.SplitAfter(string(text), "\n")
	if last := lines[len(lines)-1]; last == "" {
		lines = lines[:len(lines)-1]
	} else {
		lines[len(lines)-1] = last + "\n"
	}
	for _, kind := range manifest.Kinds {
		var add []string
		for _, c := range custom {
			if c.Kind != kind || have[c.Import] {
				continue
			}
			have[c.Import] = true
			e := c.Entry(c.Module)
			add = append(add, "- gomod: "+e.GoMod+"\n")
			if e.Import != "" {
				add = append(add, "  import: "+e.Import+"\n")
			}
		}
		if len(add) == 0 {
			continue
		}
		if lines, err = insertEntries(lines, kind, add); err != nil {
			return nil, err
		}
	}
	out := []byte(strings.Join(lines, ""))
	if _, err := manifest.Parse(out); err != nil {
		return nil, fmt.Errorf("the manifest with custom components does not parse: %w", err)
	}
	return out, nil
}

var topLevel = regexp.MustCompile(`^[^\s#-]`)

// insertEntries adds entry lines (unindented) at the end of kind's list,
// with the list's indentation, or a new list at the end of the manifest.
func insertEntries(lines []string, kind manifest.Kind, entry []string) ([]string, error) {
	key := regexp.MustCompile(`^` + string(kind) + `:\s*(#.*)?$`)
	start := slices.IndexFunc(lines, func(l string) bool { return key.MatchString(strings.TrimRight(l, "\r\n")) })
	if start < 0 {
		if slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, string(kind)+":") }) {
			return nil, fmt.Errorf("the manifest's %s is not a block list: add the custom components to it yourself", kind)
		}
		block := []string{string(kind) + ":\n"}
		for _, l := range entry {
			block = append(block, "  "+l)
		}
		// Before the first later section, in manifest order, with its
		// leading comments; otherwise at the end.
		later := slices.Concat(manifest.Kinds[slices.Index(manifest.Kinds, kind)+1:],
			[]manifest.Kind{"providers", "converters", "telemetry", "replaces", "excludes"})
		at := slices.IndexFunc(lines, func(l string) bool {
			return slices.ContainsFunc(later, func(k manifest.Kind) bool { return strings.HasPrefix(l, string(k)+":") })
		})
		if at < 0 {
			return append(append(lines, "\n"), block...), nil
		}
		for at > 0 && strings.HasPrefix(lines[at-1], "#") {
			at--
		}
		return slices.Concat(lines[:at], block, []string{"\n"}, lines[at:]), nil
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if topLevel.MatchString(lines[i]) {
			end = i
			break
		}
	}
	// Blank lines and comments before the next key belong to it.
	for end > start+1 {
		t := strings.TrimSpace(lines[end-1])
		if t != "" && (!strings.HasPrefix(t, "#") || strings.HasPrefix(lines[end-1], " ")) {
			break
		}
		end--
	}
	indent := "  "
	for _, l := range lines[start+1 : end] {
		if t := strings.TrimLeft(l, " "); strings.HasPrefix(t, "- ") {
			indent = l[:len(l)-len(t)]
			break
		}
	}
	block := make([]string, len(entry))
	for i, l := range entry {
		block[i] = indent + l
	}
	return slices.Concat(lines[:end], block, lines[end:]), nil
}
