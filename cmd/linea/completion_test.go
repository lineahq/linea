package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestCompletionReachesEveryFlagSet checks that every command and listed
// subcommand reaches a flag set of its own name, so that completion offers
// its flags.
func TestCompletionReachesEveryFlagSet(t *testing.T) {
	ctx := context.Background()
	subs := subcommands()
	for _, c := range commands {
		if c.name == "completion" {
			continue
		}
		paths := [][]string{{c.name}}
		for _, s := range subs[c.name] {
			paths = append(paths, []string{c.name, s})
		}
		for _, path := range paths {
			got := probeFlags(ctx, path)
			if len(path) == 1 && len(subs[c.name]) > 0 {
				// A dispatching command may have flags of its own (runs, samples).
				if got != nil && got.fs.Name() != "linea "+c.name {
					t.Errorf("%s: probe reached %q", c.name, got.fs.Name())
				}
				continue
			}
			want := "linea " + strings.Join(path, " ")
			if got == nil {
				t.Errorf("%s: no flag set", want)
			} else if got.fs.Name() != want {
				t.Errorf("%s: probe reached %q", want, got.fs.Name())
			}
		}
	}
}

// TestCompletionListsEverySubcommand fails when a source file names a flag
// set "command subcommand" that completion does not list.
func TestCompletionListsEverySubcommand(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`newFlagSet\(\s*"([a-z-]+) ([a-z-]+)"`)
	subs := subcommands()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			if !slices.Contains(subs[m[1]], m[2]) {
				t.Errorf("%s: subcommand %q %q is missing from subcommands()", f, m[1], m[2])
			}
		}
	}
}

func TestComplete(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		words     []string
		cur       string
		want      []string
		directive string
	}{
		{"commands", nil, "up", []string{"upgrade-check"}, directiveNone},
		{"completion shells", []string{"completion"}, "", completionShells, directiveNone},
		{"subcommands", []string{"suite"}, "re", []string{"report", "rerun"}, directiveNone},
		{"flags", []string{"inspect"}, "--sc", []string{"--scenario"}, directiveNone},
		{"subcommand flags", []string{"runs", "show"}, "--ru", []string{"--run"}, directiveNone},
		{"dispatcher flags", []string{"runs"}, "--resu", []string{"--result"}, directiveNone},
		{"format values", []string{"inspect", "--format"}, "", []string{"json", "text"}, directiveNone},
		{"format from usage", []string{"run", "-format"}, "j", []string{"json"}, directiveNone},
		{"diff formats", []string{"diff", "--format"}, "m", []string{"markdown"}, directiveNone},
		{"enum in synopsis", []string{"library", "list", "--kind"}, "", []string{"configuration", "fixture", "scenario"}, directiveNone},
		{"value after equals", []string{"inspect"}, "--format=t", []string{"--format=text"}, directiveNone},
		{"file flag", []string{"run", "--config"}, "", nil, directiveFiles},
		{"directory flag", []string{"samples", "get", "--dir"}, "", nil, directiveDirs},
		{"plain value", []string{"runs", "show", "--server"}, "", nil, directiveNone},
		{"bool flag is not a value", []string{"run", "--compare"}, "--nor", []string{"--normalize"}, directiveNone},
		{"positional", []string{"runtime", "rm"}, "", nil, directiveFiles},
		{"unknown command", []string{"nope"}, "--x", nil, directiveNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, directive := complete(ctx, tt.words, tt.cur)
			if !slices.Equal(got, tt.want) || directive != tt.directive {
				t.Errorf("complete(%q, %q) = %q %s, want %q %s", tt.words, tt.cur, got, directive, tt.want, tt.directive)
			}
		})
	}
}

func TestCompleteCommandPrintsCandidatesAndDirective(t *testing.T) {
	var out bytes.Buffer
	if code := run(context.Background(), []string{"__complete", "suite", completeCurPrefix + "ju"}, &out, &out); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if got, want := out.String(), "junit\n"+directiveNone+"\n"; got != want {
		t.Errorf("output %q, want %q", got, want)
	}
}

func TestCompletionScripts(t *testing.T) {
	for _, shell := range completionShells {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), []string{"completion", shell}, &out, &errOut); code != exitOK {
			t.Fatalf("%s: exit %d: %s", shell, code, errOut.String())
		}
		if !strings.Contains(out.String(), "__complete") || !strings.Contains(out.String(), completeCurPrefix) {
			t.Errorf("%s: the script does not call __complete with the marked word", shell)
		}
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"completion", "tcsh"}, &out, &errOut); code != exitError {
		t.Errorf("unknown shell: exit %d, want %d", code, exitError)
	}
}
