package hostfs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// layout creates an allowed root holding a file and a directory, and an
// outside directory holding a secret file.
func layout(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root, outside = filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "sub", "inside.yaml"), "inside")
	write(t, filepath.Join(outside, "secret.yaml"), "secret")
	return root, outside
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenConfinesReadsToRoots(t *testing.T) {
	root, outside := layout(t)
	policy := Policy{Roots: []string{root}}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.yaml")
	for _, tt := range []struct {
		name, path, want string
		err              error
	}{
		{"inside a root", filepath.Join(root, "sub", "inside.yaml"), "inside", nil},
		{"dot segments that stay inside", filepath.Join(root, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Join("sub", "inside.yaml"), "inside", nil},
		{"dot-dot traversal", root + string(filepath.Separator) + filepath.Join("..", "outside", "secret.yaml"), "", ErrOutsideRoots},
		{"absolute path outside", secret, "", ErrOutsideRoots},
		{"relative path", filepath.Join("sub", "inside.yaml"), "", ErrOutsideRoots},
		{"the root's parent", filepath.Dir(root), "", ErrOutsideRoots},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := policy.ReadFile(tt.path, 0)
			if tt.err != nil {
				if !errors.Is(err, tt.err) || raw != nil {
					t.Fatalf("ReadFile(%s) = %q, %v; want %v", tt.path, raw, err, tt.err)
				}
				if !strings.Contains(err.Error(), tt.path) || strings.Contains(err.Error(), "secret\"") {
					t.Fatalf("refusal must name the path and nothing it holds: %v", err)
				}
				return
			}
			if err != nil || string(raw) != tt.want {
				t.Fatalf("ReadFile(%s) = %q, %v", tt.path, raw, err)
			}
		})
	}
	_, err := policy.ReadFile(filepath.Join("sub", "inside.yaml"), 0)
	if err == nil || !strings.Contains(err.Error(), "use an absolute path under an allowed root") {
		t.Fatalf("relative refusal must say how to fix it: %v", err)
	}
}

func TestOpenRefusesDirectoriesAndMissingFiles(t *testing.T) {
	root, _ := layout(t)
	policy := Policy{Roots: []string{root}}
	for _, path := range []string{filepath.Join(root, "sub"), root} {
		if f, err := policy.Open(path); err == nil || errors.Is(err, ErrOutsideRoots) {
			if f != nil {
				f.Close()
			}
			t.Fatalf("Open(%s) = %v; want a not-a-regular-file error", path, err)
		}
	}
	if _, err := policy.Open(filepath.Join(root, "missing.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := (Policy{Unrestricted: true}).Open(filepath.Join(root, "sub")); err == nil {
		t.Fatal("unrestricted Open accepted a directory")
	}
}

func TestOpenRefusesSymbolicLinksLeavingTheRoot(t *testing.T) {
	root, outside := layout(t)
	link := filepath.Join(root, "link.yaml")
	if err := os.Symlink(filepath.Join(outside, "secret.yaml"), link); err != nil {
		t.Skipf("symbolic links cannot be created here: %v", err)
	}
	// os.Root follows a link with a relative target that stays inside the
	// root; it refuses one with an absolute target, even inside the root.
	inner, absolute := filepath.Join(root, "inner.yaml"), filepath.Join(root, "absolute.yaml")
	if err := os.Symlink(filepath.Join("sub", "inside.yaml"), inner); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "sub", "inside.yaml"), absolute); err != nil {
		t.Fatal(err)
	}
	policy := Policy{Roots: []string{root}}
	if raw, err := policy.ReadFile(link, 0); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("link leaving the root: %q, %v", raw, err)
	}
	if raw, err := policy.ReadFile(inner, 0); err != nil || string(raw) != "inside" {
		t.Fatalf("link inside the root: %q, %v", raw, err)
	}
	if raw, err := policy.ReadFile(absolute, 0); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("link with an absolute target: %q, %v", raw, err)
	}
}

func TestOpenRefusesJunctionsLeavingTheRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("directory junctions are a Windows feature")
	}
	root, outside := layout(t)
	junction := filepath.Join(root, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
	policy := Policy{Roots: []string{root}}
	if raw, err := policy.ReadFile(filepath.Join(junction, "secret.yaml"), 0); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("junction leaving the root: %q, %v", raw, err)
	}
}

func TestRootsMatchCaseInsensitivelyOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows paths are case-insensitive")
	}
	root, _ := layout(t)
	policy := Policy{Roots: []string{strings.ToUpper(root)}}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	if raw, err := policy.ReadFile(filepath.Join(strings.ToLower(root), "sub", "inside.yaml"), 0); err != nil || string(raw) != "inside" {
		t.Fatalf("different-case spelling: %q, %v", raw, err)
	}
	dup := Policy{Roots: []string{root, strings.ToLower(root)}}
	if err := dup.Validate(); err == nil {
		t.Fatal("a root given twice in different case was accepted")
	}
}

func TestPolicyWithoutRootsAcceptsInlineContentOnly(t *testing.T) {
	root, _ := layout(t)
	for _, path := range []string{filepath.Join(root, "sub", "inside.yaml"), "relative.yaml"} {
		if _, err := (Policy{}).Open(path); !errors.Is(err, ErrNoRoots) {
			t.Fatalf("Open(%s) without roots: %v", path, err)
		}
	}
}

func TestUnrestrictedPolicyReadsAnyFile(t *testing.T) {
	root, outside := layout(t)
	t.Chdir(root)
	policy := Policy{Unrestricted: true}
	for path, want := range map[string]string{
		filepath.Join(outside, "secret.yaml"):         "secret",
		filepath.Join("sub", "inside.yaml"):           "inside",
		filepath.Join("..", "outside", "secret.yaml"): "secret",
	} {
		if raw, err := policy.ReadFile(path, 0); err != nil || string(raw) != want {
			t.Fatalf("ReadFile(%s) = %q, %v", path, raw, err)
		}
	}
	if abs, err := policy.Abs("x.yaml"); err != nil || abs != filepath.Join(root, "x.yaml") {
		t.Fatalf("Abs = %q, %v", abs, err)
	}
}

func TestReadFileEnforcesItsLimit(t *testing.T) {
	root, _ := layout(t)
	policy := Policy{Roots: []string{root}}
	path := filepath.Join(root, "sub", "inside.yaml")
	if _, err := policy.ReadFile(path, 3); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	if raw, err := policy.ReadFile(path, 6); err != nil || string(raw) != "inside" {
		t.Fatalf("at the limit: %q, %v", raw, err)
	}
}

func TestValidateRefusesUnusableRoots(t *testing.T) {
	root, _ := layout(t)
	file := filepath.Join(root, "sub", "inside.yaml")
	for name, policy := range map[string]Policy{
		"relative":          {Roots: []string{"sub"}},
		"missing":           {Roots: []string{filepath.Join(root, "missing")}},
		"file":              {Roots: []string{file}},
		"duplicate":         {Roots: []string{root, root + string(filepath.Separator)}},
		"unrestricted+root": {Unrestricted: true, Roots: []string{root}},
	} {
		if err := policy.Validate(); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%s: %v; want ErrInvalidPolicy", name, err)
		}
	}
	p := Policy{Roots: []string{root}}
	c := p.Clone()
	c.Roots[0] = "mutated"
	if p.Roots[0] != root {
		t.Fatal("Clone shares the roots")
	}
}

// TestRefusalsNameTheRoots: a refusal says where files may be read from.
func TestRefusalsNameTheRoots(t *testing.T) {
	root, outside := layout(t)
	_, err := Policy{Roots: []string{root}}.ReadFile(filepath.Join(outside, "secret.yaml"), 0)
	if !errors.Is(err, ErrOutsideRoots) || !strings.Contains(err.Error(), "("+root+")") {
		t.Fatalf("refusal = %v", err)
	}
}

func TestReadTreeReadsADirectoryUnderThePolicy(t *testing.T) {
	root, outside := layout(t)
	if err := os.MkdirAll(filepath.Join(root, "sub", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "sub", ".git", "HEAD"), "ref")
	write(t, filepath.Join(root, "sub", "go.mod"), "module m")
	limits := TreeLimits{Files: 10, Bytes: 1 << 20, Skip: []string{".git"}}
	policy := Policy{Roots: []string{root}}
	tree, err := policy.ReadTree(filepath.Join(root, "sub"), limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 || string(tree["inside.yaml"]) != "inside" || string(tree["go.mod"]) != "module m" {
		t.Fatalf("tree %q", tree)
	}
	if _, err := policy.ReadTree(outside, limits); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("outside the roots: %v", err)
	}
	if _, err := policy.ReadTree(filepath.Join(root, "sub"), TreeLimits{Files: 1}); !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("too many files: %v", err)
	}
	if _, err := policy.ReadTree(filepath.Join(root, "sub"), TreeLimits{Bytes: 8}); !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("too many bytes: %v", err)
	}
	tree, err = Policy{Unrestricted: true}.ReadTree(filepath.Join(root, "sub"), limits)
	if err != nil || len(tree) != 2 {
		t.Fatalf("unrestricted: %q, %v", tree, err)
	}
}

func TestRefusalsCarryThePolicysGrant(t *testing.T) {
	root, outside := layout(t)
	secret := filepath.Join(outside, "secret.yaml")
	for _, p := range []Policy{{Grant: "add it"}, {Roots: []string{root}, Grant: "add it"}} {
		_, err := p.Open(secret)
		if GrantOf(err) != "add it" || !(errors.Is(err, ErrNoRoots) || errors.Is(err, ErrOutsideRoots)) || strings.Contains(err.Error(), "add it") {
			t.Fatalf("roots %v: %v (grant %q)", p.Roots, err, GrantOf(err))
		}
	}
	if _, err := (Policy{Roots: []string{root}}).Open(secret); GrantOf(err) != "" {
		t.Fatalf("a policy without a grant: %q", GrantOf(err))
	}
}

func TestHoldingNamesTheRootAFileLiesUnder(t *testing.T) {
	root, outside := layout(t)
	p := Policy{Roots: []string{root}}
	if got, ok := p.Holding(filepath.Join(root, "sub", "inside.yaml")); !ok || got != root {
		t.Fatalf("inside: %q %v", got, ok)
	}
	if got, ok := p.Holding(root); !ok || got != root {
		t.Fatalf("the root itself: %q %v", got, ok)
	}
	if _, ok := p.Holding(filepath.Join(outside, "secret.yaml")); ok {
		t.Fatal("a file outside the roots is held")
	}
	if _, ok := (Policy{}).Holding(filepath.Join(outside, "secret.yaml")); ok {
		t.Fatal("a policy without roots holds a file")
	}
	if _, ok := (Policy{Unrestricted: true}).Holding(filepath.Join(outside, "secret.yaml")); !ok {
		t.Fatal("an unrestricted policy does not hold a file")
	}
	link := filepath.Join(outside, "link")
	if err := os.Symlink(filepath.Join(root, "sub"), link); err != nil {
		t.Skipf("symbolic links: %v", err)
	}
	if got, ok := p.Holding(filepath.Join(link, "inside.yaml")); !ok || got != root {
		t.Fatalf("through a link into the root: %q %v", got, ok)
	}
}
