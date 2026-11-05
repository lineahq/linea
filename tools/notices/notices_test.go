package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadNoticesFindsLicenseAndNoticeFiles(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		want    []string
		wantErr bool
	}{
		{name: "license only", files: map[string]string{"LICENSE": "MIT", "go.mod": "", "main.go": ""}, want: []string{"LICENSE"}},
		{name: "license and notice", files: map[string]string{"LICENSE": "Apache", "NOTICE": "Copyright"}, want: []string{"LICENSE", "NOTICE"}},
		{name: "suffixes and spelling", files: map[string]string{"LICENSE.md": "a", "LICENCE-MIT": "b", "COPYING": "c", "notice.txt": "d"}, want: []string{"COPYING", "LICENCE-MIT", "LICENSE.md", "notice.txt"}},
		{name: "licenses directory", files: map[string]string{"LICENSES/Apache-2.0.txt": "a", "LICENSES/MIT.txt": "b"}, want: []string{"LICENSES/Apache-2.0.txt", "LICENSES/MIT.txt"}},
		{name: "similar names are not terms", files: map[string]string{"licensed.go": "", "LICENSE_test.go.bak": "x", "README": ""}, want: []string{"LICENSE_test.go.bak"}},
		{name: "none", files: map[string]string{"README.md": "", "licensing.go": ""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, tt.files)
			got, err := readNotices(dir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("readNotices() = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, n := range got {
				names = append(names, n.Name)
			}
			if strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("readNotices() names = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestCollectSkipsOwnModulesAndReportsEveryMissingLicense(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"b/LICENSE":   "B terms",
		"a/LICENSE":   "A terms",
		"own/LICENSE": "ours",
		"x/README":    "",
		"y/README":    "",
	})
	dir := func(n string) string { return filepath.Join(root, n) }

	got, err := collect([]module{
		{Path: "example.com/b", Version: "v1.0.0", Dir: dir("b")},
		{Path: "example.com/a", Version: "v2.0.0", Dir: dir("a")},
		{Path: "example.com/a", Version: "v2.0.0", Dir: dir("a")},
		{Path: "github.com/lineahq/linea/pluginsdk", Dir: dir("own")},
		{Path: "github.com/lineahq/linea", Dir: dir("own")},
	}, []string{own})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "example.com/a" || got[1].Name != "example.com/b" {
		t.Fatalf("collect() = %+v, want example.com/a then example.com/b once each", got)
	}

	_, err = collect([]module{
		{Path: "example.com/x", Version: "v1.0.0", Dir: dir("x")},
		{Path: "example.com/y", Version: "v1.0.0", Dir: dir("y")},
		{Path: "example.com/z", Version: "v1.0.0"},
	}, []string{own})
	if err == nil {
		t.Fatal("collect() with unlicensed modules succeeded, want an error")
	}
	for _, m := range []string{"example.com/x", "example.com/y", "example.com/z"} {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("collect() error %q does not name %s", err, m)
		}
	}
}

func TestOwnPrefixDoesNotSkipLookalikeModules(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"github.com/lineahq/linea", true},
		{"github.com/lineahq/linea/collector/provider", true},
		{"github.com/lineahq/linea-extra", false},
		{"github.com/lineahq/lineax/sdk", false},
	}
	for _, tt := range tests {
		if got := hasPrefix(tt.path, []string{own}); got != tt.want {
			t.Errorf("hasPrefix(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestRenderListsComponentsThenTheirFiles(t *testing.T) {
	var b strings.Builder
	err := render(&b, "linea", []component{
		{Name: "Go standard library and runtime", Version: "go1.27.3", Notices: []notice{{Name: "LICENSE", Text: "Go terms"}}},
		{Name: "example.com/a", Version: "v1.0.0", Notices: []notice{{Name: "LICENSE", Text: "A terms"}, {Name: "NOTICE", Text: "A notice"}}},
		{Name: "web", Notices: []notice{{Name: "licenses.md", Text: "web terms"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"Third-party notices for linea\n",
		"  example.com/a v1.0.0\n",
		"  web\n",
		"\n--- NOTICE ---\n\nA notice\n",
		"web terms",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render() output lacks %q", want)
		}
	}
	if strings.Index(out, "  web\n") > strings.Index(out, "Go terms") {
		t.Error("render() wrote a file before the component list ended")
	}
}
