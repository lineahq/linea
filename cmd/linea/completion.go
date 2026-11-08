package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Shell completion. The shell scripts that 'linea completion SHELL' prints
// call the hidden '__complete' command on every Tab. It reads the flags from
// the commands' own flag sets, so completions follow the flags without a
// second list to keep current.

// completeCurPrefix marks the word being completed, the last argument of
// '__complete'. A shell cannot always pass an empty argument to a native
// program (Windows PowerShell 5.1 drops it), and the marker keeps it non-empty.
const completeCurPrefix = "cur:"

// Completion directives, the last line of the '__complete' output: what the
// shell does besides offering the candidates.
const (
	// directiveNone offers only the candidates.
	directiveNone = ":none"
	// directiveFiles also offers file names.
	directiveFiles = ":files"
	// directiveDirs offers directory names.
	directiveDirs = ":dirs"
)

// subcommands lists each command's subcommands for completion. A test
// checks that every entry reaches a flag set of that name and that every
// literal flag set name is listed here.
func subcommands() map[string][]string {
	runs := make([]string, 0, len(runsSubcommands))
	for name := range runsSubcommands {
		runs = append(runs, name)
	}
	fleet := make([]string, 0, len(fleetCommands))
	for _, c := range fleetCommands {
		fleet = append(fleet, c.name)
	}
	m := map[string][]string{
		"access":    {"list", "grant", "revoke"},
		"artifacts": {"list", "show", "save", "revisions", "export", "import"},
		"capture":   captureCommands,
		"fleet":     fleet,
		"library":   {"list", "share", "publish", "copy"},
		"ottl":      {"preview"},
		"project":   {"list", "create", "update", "archive", "unarchive"},
		"runs":      runs,
		"runtime":   {"build", "list", "versions", "catalog", "builds", "manifest", "retarget", "storage", "prune", "rm", "push", "default", "share", "register", "import"},
		"samples":   {"get"},
		"statement": {"candidate", "test"},
		"suite":     {"list", "show", "report", "rerun", "junit", "delete"},
		"token":     {"create", "list", "revoke"},
	}
	for _, s := range m {
		sort.Strings(s)
	}
	return m
}

// completionCommands are the words completion offers besides commands.
var completionCommands = []string{"help"}

// probedFlags is a command's flag set and the synopsis its usage prints.
type probedFlags struct {
	fs       *flag.FlagSet
	synopsis string
}

// probeHook, when set, receives every flag set newFlagSet makes.
var probeHook func(name, synopsis string, fs *flag.FlagSet)

// probeFlags runs a command with -h and returns the first flag set it
// makes, or nil for a command that only dispatches subcommands. Commands
// parse their flags before doing anything else, so -h stops them there.
func probeFlags(ctx context.Context, path []string) *probedFlags {
	var got *probedFlags
	probeHook = func(_, synopsis string, fs *flag.FlagSet) {
		if got == nil {
			got = &probedFlags{fs: fs, synopsis: synopsis}
		}
	}
	defer func() { probeHook = nil }()
	_ = run(ctx, append(slices.Clone(path), "-h"), io.Discard, io.Discard)
	return got
}

// completeCmd answers one Tab press: its arguments are the words before the
// cursor and the marked word being completed. It prints one candidate per
// line, then a directive line.
func completeCmd(ctx context.Context, args []string, stdout, _ io.Writer) int {
	cur := ""
	if n := len(args); n > 0 {
		if c, ok := strings.CutPrefix(args[n-1], completeCurPrefix); ok {
			cur, args = c, args[:n-1]
		}
	}
	cands, directive := complete(ctx, args, cur)
	for _, c := range cands {
		fmt.Fprintln(stdout, c)
	}
	fmt.Fprintln(stdout, directive)
	return exitOK
}

// complete returns the candidates for cur after words, and the directive.
func complete(ctx context.Context, words []string, cur string) ([]string, string) {
	if len(words) == 0 {
		if strings.HasPrefix(cur, "-") {
			return matching([]string{"--help"}, cur), directiveNone
		}
		var names []string
		for _, c := range commands {
			names = append(names, c.name)
		}
		return matching(append(names, completionCommands...), cur), directiveNone
	}
	path := words[:1]
	switch words[0] {
	case "completion":
		if len(words) == 1 {
			return matching(completionShells, cur), directiveNone
		}
		return nil, directiveNone
	case "help", "__complete":
		return nil, directiveNone
	}
	subs := subcommands()[words[0]]
	if len(words) > 1 && slices.Contains(subs, words[1]) {
		path = words[:2]
	}
	if len(path) == 1 && len(subs) > 0 && len(words) == 1 && !strings.HasPrefix(cur, "-") {
		return matching(subs, cur), directiveNone
	}
	probe := probeFlags(ctx, path)
	if probe == nil {
		return nil, directiveNone
	}
	if prev := words[len(words)-1]; len(words) > len(path) && !strings.Contains(prev, "=") {
		if f := lookupFlag(probe.fs, prev); f != nil && !isBoolFlag(f) {
			return flagValues(probe, f, cur, "")
		}
	}
	if !strings.HasPrefix(cur, "-") {
		return nil, directiveFiles
	}
	if name, value, ok := strings.Cut(cur, "="); ok {
		if f := lookupFlag(probe.fs, name); f != nil && !isBoolFlag(f) {
			return flagValues(probe, f, value, name+"=")
		}
		return nil, directiveNone
	}
	var names []string
	probe.fs.VisitAll(func(f *flag.Flag) { names = append(names, "--"+f.Name) })
	return matching(names, cur), directiveNone
}

// lookupFlag returns the flag that word names (-name or --name), or nil.
func lookupFlag(fs *flag.FlagSet, word string) *flag.Flag {
	if !strings.HasPrefix(word, "-") {
		return nil
	}
	return fs.Lookup(strings.TrimLeft(word, "-"))
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// synopsisValue finds "--name placeholder" in a synopsis.
func synopsisValue(synopsis, name string) string {
	re := regexp.MustCompile(`--` + regexp.QuoteMeta(name) + `[ =]([^ \]\)|]+(\|[^ \]\)|]+)*)`)
	m := re.FindStringSubmatch(synopsis)
	if m == nil {
		return ""
	}
	return m[1]
}

// enumWord is a lowercase choice such as "json" or "traces".
var enumWord = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)

// flagValues completes a flag's value: the choices its synopsis or usage
// lists, otherwise files or directories when the flag names one.
func flagValues(p *probedFlags, f *flag.Flag, cur, prefix string) ([]string, string) {
	placeholder := synopsisValue(p.synopsis, f.Name)
	if choices := strings.Split(placeholder, "|"); len(choices) > 1 && allEnum(choices) {
		return withPrefix(matching(choices, cur), prefix), directiveNone
	}
	if f.Name == "format" {
		// formatFlag's usage lists the formats: "output format: json or text".
		if _, list, ok := strings.Cut(f.Usage, ":"); ok {
			list, _, _ = strings.Cut(list, "(")
			choices := strings.FieldsFunc(strings.ReplaceAll(list, " or ", ","), func(r rune) bool { return r == ',' || r == ' ' })
			if len(choices) > 1 && allEnum(choices) {
				return withPrefix(matching(choices, cur), prefix), directiveNone
			}
		}
	}
	if prefix != "" {
		// A shell completes the value after "--name=" itself only as a word.
		return nil, directiveNone
	}
	upper := strings.ToUpper(placeholder)
	usage := strings.ToLower(f.Usage)
	switch {
	case strings.Contains(upper, "DIR") || strings.Contains(usage, "directory"):
		return nil, directiveDirs
	case strings.ContainsAny(placeholder, ".<") || strings.Contains(upper, "FILE") || strings.Contains(upper, "PATH"),
		placeholder == "" && (strings.Contains(usage, "file") || strings.Contains(usage, "yaml") || strings.Contains(usage, "json")):
		return nil, directiveFiles
	}
	return nil, directiveNone
}

func allEnum(words []string) bool {
	for _, w := range words {
		if !enumWord.MatchString(w) {
			return false
		}
	}
	return true
}

// matching returns the sorted, distinct words that start with prefix.
func matching(words []string, prefix string) []string {
	var out []string
	for _, w := range words {
		if strings.HasPrefix(w, prefix) {
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func withPrefix(words []string, prefix string) []string {
	if prefix == "" {
		return words
	}
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = prefix + w
	}
	return out
}
