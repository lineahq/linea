package catalog

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/manifest"
)

// Retargeted is a manifest moved to another release.
type Retargeted struct {
	Manifest []byte
	// From is the manifest's release and To the release it was moved to.
	From, To string
	// Moved lists each module whose version changed, as "<module> <from>
	// => <to>".
	Moved []string
	// Missing lists the core and contrib modules the target release does
	// not have; they are left unchanged and the build will refuse them.
	Missing []string
	// Custom lists the modules outside core and contrib; they keep their
	// version until the user confirms one for the target release.
	Custom []string
	// Notes say what else changed, such as the release's replaces.
	Notes []string
}

const serviceModule = "go.opentelemetry.io/collector/service"

// upstream reports whether a module is a core or contrib module, which a
// release's catalog versions.
func upstream(module string) bool {
	return strings.HasPrefix(module, "go.opentelemetry.io/collector/") || module == "go.opentelemetry.io/collector" ||
		strings.HasPrefix(module, "github.com/open-telemetry/opentelemetry-collector-contrib/")
}

// Retarget moves a manifest to the release of to: every core and contrib
// module the target catalog has takes its version there (stable v1 module
// lines included), dist.otelcol_version names the target, and the replaces
// of the manifest's own release, when from is its catalog, become the
// target's. Everything else in the text, comments included, is unchanged.
// from may be nil when the manifest's release has no catalog.
func Retarget(from, to *versions.Catalog, raw []byte) (*Retargeted, error) {
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, err
	}
	out := &Retargeted{From: m.CollectorVersion(), To: to.CollectorVersion, Moved: []string{}, Missing: []string{}, Custom: []string{}, Notes: []string{}}
	target := map[string]string{}
	for _, c := range to.Components {
		target[c.Module()] = c.GoMod
	}
	for _, gomod := range slices.Concat(to.Providers, to.Converters) {
		mod, _, _ := strings.Cut(gomod, " ")
		target[mod] = gomod
	}
	// The telemetry entry's module is the Collector service, which the
	// catalog does not list; it is released with the core components.
	if _, ok := target[serviceModule]; !ok {
		target[serviceModule] = serviceModule + " v" + to.CollectorVersion
	}
	var entries []manifest.Module
	for _, kind := range manifest.Kinds {
		entries = append(entries, m.Components(kind)...)
	}
	entries = append(entries, m.Providers...)
	entries = append(entries, m.Converters...)
	if m.Telemetry != nil {
		entries = append(entries, *m.Telemetry)
	}
	text := string(raw)
	seen := map[string]bool{}
	for _, e := range entries {
		mod, ver := e.ModulePath(), e.Version()
		if seen[mod+" "+ver] {
			continue
		}
		seen[mod+" "+ver] = true
		gomod, ok := target[mod]
		switch {
		case ok:
			_, newVer, _ := strings.Cut(gomod, " ")
			if newVer == ver {
				continue
			}
			re := regexp.MustCompile(`(gomod:\s*["']?)` + regexp.QuoteMeta(mod) + `(\s+)` + regexp.QuoteMeta(ver) + `(["'\s]|$)`)
			text = re.ReplaceAllString(text, "${1}"+mod+"${2}"+newVer+"${3}")
			out.Moved = append(out.Moved, fmt.Sprintf("%s %s => %s", mod, ver, newVer))
		case upstream(mod):
			out.Missing = append(out.Missing, mod)
		default:
			out.Custom = append(out.Custom, mod)
		}
	}
	if old := m.Dist.OtelcolVersion; old != "" && strings.TrimPrefix(old, "v") != to.CollectorVersion {
		re := regexp.MustCompile(`(?m)^(\s*otelcol_version:\s*["']?)(v?)` + regexp.QuoteMeta(strings.TrimPrefix(old, "v")) + `(["'\s]|$)`)
		text = re.ReplaceAllString(text, "${1}${2}"+to.CollectorVersion+"${3}")
	}
	text = retargetReplaces(text, m.Replaces, from, to, out)
	moved := []byte(text)
	if _, err := manifest.Parse(moved); err != nil {
		return nil, fmt.Errorf("the moved manifest does not parse: %w", err)
	}
	out.Manifest = moved
	return out, nil
}

// retargetReplaces swaps the manifest's replaces that are its release's
// own for the target's: a line of the from catalog is removed when the
// target lacks it, a line whose module the target replaces differently
// takes the target's line, and the target's other replaces are added when
// the manifest followed its release's. User replaces are kept.
func retargetReplaces(text string, replaces []string, from, to *versions.Catalog, out *Retargeted) string {
	if from == nil {
		if len(to.Replaces) > 0 {
			out.Notes = append(out.Notes, fmt.Sprintf("Collector %s has no catalog here, so its replaces were kept; Collector %s's are %s", out.From, to.CollectorVersion, strings.Join(to.Replaces, "; ")))
		}
		return text
	}
	left := func(r string) string {
		l, _, _ := strings.Cut(r, "=>")
		return strings.Fields(l + " ")[0]
	}
	followed := false
	present := map[string]bool{}
	for _, r := range replaces {
		r = strings.TrimSpace(r)
		if !slices.Contains(from.Replaces, r) {
			continue
		}
		followed = true
		i := slices.IndexFunc(to.Replaces, func(t string) bool { return left(t) == left(r) })
		switch {
		case i >= 0 && to.Replaces[i] == r:
			present[r] = true
		case i >= 0:
			text = replaceLine(text, r, to.Replaces[i])
			present[to.Replaces[i]] = true
			out.Notes = append(out.Notes, fmt.Sprintf("replace %s became %s", r, to.Replaces[i]))
		default:
			text = replaceLine(text, r, "")
			out.Notes = append(out.Notes, fmt.Sprintf("replace %s was removed: Collector %s does not have it", r, to.CollectorVersion))
		}
	}
	if !followed {
		return text
	}
	for _, t := range to.Replaces {
		if present[t] {
			continue
		}
		text = addReplace(text, t)
		out.Notes = append(out.Notes, fmt.Sprintf("replace %s was added from Collector %s", t, to.CollectorVersion))
	}
	return text
}

// replaceLine rewrites the list item holding a replace, or removes it when
// with is empty.
func replaceLine(text, replace, with string) string {
	re := regexp.MustCompile(`(?m)^([ \t]*-[ \t]*["']?)` + regexp.QuoteMeta(replace) + `(["']?[ \t]*(#.*)?)\r?\n?`)
	if with == "" {
		return re.ReplaceAllString(text, "")
	}
	return re.ReplaceAllStringFunc(text, func(line string) string {
		return strings.Replace(line, replace, with, 1)
	})
}

// addReplace appends a replace after the last item of the replaces list.
func addReplace(text, replace string) string {
	lines := strings.SplitAfter(text, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "replaces:") })
	if start < 0 {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		return text + "\nreplaces:\n  - " + replace + "\n"
	}
	last, indent := start, "  "
	for i := start + 1; i < len(lines); i++ {
		t := strings.TrimLeft(lines[i], " \t")
		if strings.HasPrefix(t, "-") {
			last, indent = i, lines[i][:len(lines[i])-len(t)]
			continue
		}
		if strings.TrimSpace(t) != "" && !strings.HasPrefix(t, "#") {
			break
		}
	}
	item := indent + "- " + replace + "\n"
	if !strings.HasSuffix(lines[last], "\n") {
		lines[last] += "\n"
	}
	return strings.Join(slices.Insert(lines, last+1, item), "")
}
