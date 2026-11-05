// Package hostfs confines request-triggered host file reads to allowed roots
// (the path policy). Every read a request can cause opens through
// a Policy; an unrestricted policy reads the host as before.
package hostfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutsideRoots marks a path the policy refuses: outside every root,
// relative under roots, or leaving a root through a symbolic link or junction.
var ErrOutsideRoots = errors.New("path is outside the allowed roots")

// ErrNoRoots marks a host path given to a policy without roots.
var ErrNoRoots = errors.New("this service accepts inline content only")

// ErrTooLarge marks a file larger than ReadFile's limit.
var ErrTooLarge = errors.New("file exceeds the size limit")

// ErrInvalidPolicy marks a policy Validate refuses. The policy is trusted
// composition configuration, so this is never a request's fault.
var ErrInvalidPolicy = errors.New("invalid path policy")

// Policy decides which host files a request may make the service read.
// Unrestricted reads any path, relative paths against the working directory.
// Otherwise only regular files under one of Roots (absolute, existing
// directories fixed when the service is composed) can be read, and relative
// paths are refused. The zero value has no roots: inline content only.
type Policy struct {
	Unrestricted bool     `json:"unrestricted,omitempty"`
	Roots        []string `json:"roots,omitempty"`
	// Grant tells the caller how the service's owner allows a refused path,
	// in the composition's own terms, for example "add its directory to
	// the allowed roots"; empty names no way.
	Grant string `json:"-"`
}

// Validate checks the roots: absolute, existing directories, no duplicates.
// Its errors wrap ErrInvalidPolicy.
func (p Policy) Validate() error {
	if p.Unrestricted && len(p.Roots) != 0 {
		return fmt.Errorf("%w: an unrestricted policy has no roots", ErrInvalidPolicy)
	}
	for i, root := range p.Roots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("%w: root %s is not absolute", ErrInvalidPolicy, root)
		}
		info, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("%w: root %s: %v", ErrInvalidPolicy, root, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: root %s is not a directory", ErrInvalidPolicy, root)
		}
		for _, other := range p.Roots[:i] {
			if rel, err := filepath.Rel(other, root); err == nil && rel == "." {
				return fmt.Errorf("%w: root %s is given twice", ErrInvalidPolicy, root)
			}
		}
	}
	return nil
}

// Holding returns the root an absolute host path lies under, so a
// composition can refuse roots that would expose its own files (its
// configuration and secrets). Links are followed on both sides where they
// resolve. An unrestricted policy holds every path and returns "".
func (p Policy) Holding(path string) (string, bool) {
	if p.Unrestricted {
		return "", true
	}
	paths := []string{filepath.Clean(path)}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		paths = append(paths, real)
	}
	for _, root := range p.Roots {
		roots := []string{root}
		if real, err := filepath.EvalSymlinks(root); err == nil {
			roots = append(roots, real)
		}
		for _, r := range roots {
			for _, abs := range paths {
				if _, ok := within(r, abs); ok {
					return root, true
				}
			}
		}
	}
	return "", false
}

// Direct reports whether every host path is read as is: unrestricted. Callers
// may then use their plain readers.
func (p Policy) Direct() bool { return p.Unrestricted }

// Clone returns a copy that does not share the roots slice.
func (p Policy) Clone() Policy {
	if p.Roots != nil {
		p.Roots = append([]string(nil), p.Roots...)
	}
	return p
}

// Abs returns the absolute form of a path a request names. Unrestricted, a
// relative path is resolved against the working directory. Under roots it is
// refused: its meaning would depend on where the service runs.
func (p Policy) Abs(path string) (string, error) {
	if p.Unrestricted {
		return filepath.Abs(path)
	}
	if len(p.Roots) == 0 {
		return "", p.refuse(path, ErrNoRoots)
	}
	if !filepath.IsAbs(path) {
		return "", p.refuse(path, outside{roots: p.Roots, relative: true})
	}
	return filepath.Clean(path), nil
}

// outside is ErrOutsideRoots naming the roots, so a refusal says where files
// may be read from.
type outside struct {
	roots    []string
	relative bool
}

func (e outside) Error() string {
	msg := ErrOutsideRoots.Error() + " (" + strings.Join(e.roots, ", ") + ")"
	if e.relative {
		msg += "; use an absolute path under an allowed root"
	}
	return msg
}

func (outside) Is(target error) bool { return target == ErrOutsideRoots }

// Open opens a regular file the policy allows. Under roots the file is opened
// through os.Root, which refuses .. and links (symbolic links, junctions)
// leaving the root, and the regular-file check uses the opened handle.
func (p Policy) Open(path string) (*os.File, error) {
	if p.Unrestricted {
		return openRegular(path, os.Stat, os.Open)
	}
	abs, err := p.Abs(path)
	if err != nil {
		return nil, err
	}
	for _, root := range p.Roots {
		rel, ok := within(root, abs)
		if !ok {
			continue
		}
		r, err := os.OpenRoot(root)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		f, err := openRegular(rel, r.Stat, r.Open)
		if err != nil && escapes(r, err) {
			return nil, p.refuse(path, outside{roots: p.Roots})
		}
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: path, Err: unwrapPath(err)}
		}
		return f, nil
	}
	return nil, p.refuse(path, outside{roots: p.Roots})
}

// ReadFile reads a regular file the policy allows, refusing one larger than
// max bytes (max <= 0 means no limit).
func (p Policy) ReadFile(path string, max int64) ([]byte, error) {
	f, err := p.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if max <= 0 {
		return io.ReadAll(f)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > max {
		return nil, refuse(path, ErrTooLarge)
	}
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, refuse(path, ErrTooLarge)
	}
	return raw, nil
}

func refuse(path string, err error) error {
	return &fs.PathError{Op: "open", Path: path, Err: err}
}

// refuse refuses a path outside the roots, carrying the policy's Grant.
func (p Policy) refuse(path string, err error) error {
	if p.Grant != "" {
		err = granted{error: err, grant: p.Grant}
	}
	return refuse(path, err)
}

// granted is a refusal with the policy's Grant.
type granted struct {
	error
	grant string
}

func (e granted) Unwrap() error { return e.error }

// GrantOf returns the Grant of the policy that refused err, empty when it
// names none.
func GrantOf(err error) string {
	var g granted
	if errors.As(err, &g) {
		return g.grant
	}
	return ""
}

// within returns abs relative to root when abs lies under it. filepath.Rel
// compares volumes and, on Windows, names case-insensitively.
func within(root, abs string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), abs)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// openRegular checks the name before opening it, so that a FIFO or device is
// never opened (which can block), and then checks the opened handle, which is
// what is read.
func openRegular(name string, stat func(string) (fs.FileInfo, error), open func(string) (*os.File, error)) (*os.File, error) {
	info, err := stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	f, err := open(name)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	return f, nil
}

// escapes reports whether err is os.Root refusing a path that leaves the
// root. The standard library does not export that error, so it is compared
// with the one the same root returns for a lexical escape.
func escapes(r *os.Root, err error) bool {
	_, probe := r.Lstat("..")
	want := unwrapPath(probe)
	return want != nil && errors.Is(err, want)
}

func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
