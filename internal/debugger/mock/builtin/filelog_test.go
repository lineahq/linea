package builtin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/mock"
)

func TestHostPathMapsDeploymentPaths(t *testing.T) {
	root := filepath.Join("sandbox", "run")
	p := &filelog{root: root}
	for _, tc := range []struct{ deployment, want string }{
		{"/var/log/pods/*/*/*.log", "root/var/log/pods/*/*/*.log"},
		{`C:\ProgramData\app\a.log`, "drive-c/ProgramData/app/a.log"},
		{"d:/logs/a.log", "drive-d/logs/a.log"},
		{`\\server\share\a.log`, "unc/server/share/a.log"},
		{"logs/a.log", "cwd/logs/a.log"},
	} {
		t.Run(tc.deployment, func(t *testing.T) {
			if got, want := p.hostPath(tc.deployment), filepath.Join(root, filepath.FromSlash(tc.want)); got != want {
				t.Fatalf("hostPath(%q) = %q, want %q", tc.deployment, got, want)
			}
		})
	}
}

// TestHostPathStaysInTheSandbox: include and exclude patterns are mapped as
// if the sandbox were the filesystem root, so .. cannot leave it and the real
// receiver never globs host files.
func TestHostPathStaysInTheSandbox(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("sandbox", "run"))
	if err != nil {
		t.Fatal(err)
	}
	p := &filelog{root: root}
	for _, tc := range []struct{ deployment, want string }{
		{"/var/log/../../../../../etc/passwd", "etc/passwd"},
		{"/var/log/../app/*.log", "root/var/app/*.log"},
		{`C:\logs\..\..\..\..\Users\*\secret.txt`, "Users/*/secret.txt"},
		{"../../../../outside/*.log", "outside/*.log"},
		{`\\server\share\..\..\..\x`, "x"},
		{"./logs/a.log", "cwd/logs/a.log"},
	} {
		t.Run(tc.deployment, func(t *testing.T) {
			got := p.hostPath(tc.deployment)
			if want := filepath.Join(root, filepath.FromSlash(tc.want)); got != want {
				t.Fatalf("hostPath(%q) = %q, want %q", tc.deployment, got, want)
			}
			if rel, err := filepath.Rel(root, got); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("hostPath(%q) = %q leaves the sandbox %q", tc.deployment, got, root)
			}
		})
	}
}

func TestValidateFilesRejectsInvalidPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []mock.File
		want  string
	}{
		{"empty", []mock.File{{Path: " "}}, "path is required"},
		{"pattern", []mock.File{{Path: "/var/log/*.log"}}, "is a pattern"},
		{"parent", []mock.File{{Path: "/var/log/../../etc/passwd"}}, "contains .."},
		{"duplicate", []mock.File{{Path: `C:\a.log`}, {Path: "C:/a.log"}}, "duplicate path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFiles(tc.files)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateFiles = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if err := validateFiles([]mock.File{{Path: "/var/log/a.log"}, {Path: "/var/log/b.log"}}); err != nil {
		t.Fatalf("valid files: %v", err)
	}
}
