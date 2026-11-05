package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type ciWorkflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]struct {
		Steps []struct {
			Uses string            `yaml:"uses"`
			With map[string]string `yaml:"with"`
			Run  string            `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// ciCommands returns a job's linea command lines, with the shell
// variables expanded from env and anything after a redirection dropped. The
// example keeps each command on one line with unspaced, quoted arguments.
func ciCommands(t *testing.T, wf ciWorkflow, job string, env map[string]string) ([][]string, string) {
	t.Helper()
	var commands [][]string
	var script strings.Builder
	for _, step := range wf.Jobs[job].Steps {
		script.WriteString(step.Run)
		for _, line := range strings.Split(step.Run, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != `"$LINEA"` {
				continue
			}
			var args []string
			for _, f := range fields[1:] {
				if f == ">" || f == "|" || f == "||" || f == "&&" {
					break
				}
				args = append(args, os.Expand(strings.Trim(f, `"`), func(name string) string {
					v, ok := env[name]
					if !ok {
						t.Fatalf("job %s uses $%s, which the test does not provide", job, name)
					}
					return v
				}))
			}
			commands = append(commands, args)
		}
	}
	return commands, script.String()
}

// The CI example installs a pinned, checksummed release and its commands, run locally on a
// sample, exit as the workflow handles them.
func TestCIExampleRunsOnASample(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "ci", "github-actions.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(wf.Env["LINEA_VERSION"]) {
		t.Fatalf("LINEA_VERSION %q is not a pinned release", wf.Env["LINEA_VERSION"])
	}
	for name, job := range wf.Jobs {
		installed := false
		for _, step := range job.Steps {
			if step.Uses != "" && !regexp.MustCompile(`@v\d+$`).MatchString(step.Uses) {
				t.Errorf("job %s uses %s without a pinned major version", name, step.Uses)
			}
			if strings.Contains(step.Run, "releases/download/v$LINEA_VERSION") && strings.Contains(step.Run, "sha256sum -c") {
				installed = true
			}
			if strings.Contains(step.Run, "@latest") || strings.Contains(step.Run, "releases/latest") {
				t.Errorf("job %s installs the latest release", name)
			}
		}
		if !installed {
			t.Errorf("job %s does not install the checksummed LINEA_VERSION release", name)
		}
	}

	// The sample stands in for the repository: its configuration, a tests
	// directory with its scenario and input, and the base branch's configuration that
	// `git show` writes.
	sample, err := filepath.Abs(filepath.Join("..", "..", "samples", "logs-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	work, temp := t.TempDir(), t.TempDir()
	tests := filepath.Join(work, "tests")
	config, err := os.ReadFile(filepath.Join(sample, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scenario, err := os.ReadFile(filepath.Join(sample, "scenario.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(filepath.Join(sample, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tests, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		filepath.Join(work, "config.yaml"):               config,
		filepath.Join(tests, "regression.scenario.yaml"): scenario,
		filepath.Join(tests, "input.json"):               input,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"CONFIG": filepath.Join(work, "config.yaml"), "TESTS": tests,
		"SCENARIO": filepath.Join(tests, "regression.scenario.yaml"), "RUNNER_TEMP": temp, "RUNTIME": wf.Env["RUNTIME"]}
	t.Chdir(work)
	ctx := context.Background()
	exec := func(args []string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(ctx, args, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}

	commands, _ := ciCommands(t, wf, "test", env)
	if len(commands) != 1 || commands[0][0] != "test" {
		t.Fatalf("test job commands = %q", commands)
	}
	if code, out := exec(commands[0]); code != exitOK {
		t.Fatalf("test exit %d: %s", code, out)
	}
	if junit, err := os.ReadFile(filepath.Join(work, "junit.xml")); err != nil || !bytes.Contains(junit, []byte("<testsuite")) {
		t.Fatalf("junit.xml = %s, %v", junit, err)
	}

	commands, script := ciCommands(t, wf, "diff", env)
	if len(commands) != 1 || commands[0][0] != "diff" {
		t.Fatalf("diff job commands = %q", commands)
	}
	// The job passes on 0 and 2 and fails otherwise.
	for _, want := range []string{"0) echo", "2) echo", `*) echo "The comparison was inconclusive or failed (exit $code)."; exit 1`} {
		if !strings.Contains(script, want) {
			t.Fatalf("diff job does not handle %q:\n%s", want, script)
		}
	}
	base := filepath.Join(temp, "base-config.yaml")
	for _, tc := range []struct {
		base   string
		code   int
		prefix string
	}{
		{string(config), exitOK, "# Run comparison: same\n"},
		{strings.Replace(string(config), `"upper"`, `"lower"`, 1), exitMismatch, "# Run comparison: different\n"},
		{"receivers: [", exitError, ""},
	} {
		if err := os.WriteFile(base, []byte(tc.base), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, out := exec(commands[0]); code != tc.code || !strings.HasPrefix(out, tc.prefix) {
			t.Fatalf("diff exit %d, want %d: %s", code, tc.code, out)
		}
	}
}
