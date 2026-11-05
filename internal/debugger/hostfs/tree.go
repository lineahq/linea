package hostfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
)

// ErrTreeTooLarge marks a directory larger than ReadTree's limits.
var ErrTreeTooLarge = errors.New("directory exceeds the size limit")

// Tree limits for ReadTree.
type TreeLimits struct {
	// Files and Bytes bound the files read and their total size.
	Files int
	Bytes int64
	// Skip names directories that are not read, such as ".git".
	Skip []string
}

// ReadTree reads every regular file under a directory the policy allows,
// keyed by slash-separated path relative to it. Symbolic links and
// junctions are not followed, directories named in limits.Skip are left
// out, and every file is read through ReadFile, so the policy holds for
// each one. A directory beyond the limits is ErrTreeTooLarge.
func (p Policy) ReadTree(dir string, limits TreeLimits) (map[string][]byte, error) {
	abs, err := p.Abs(dir)
	if err != nil {
		return nil, err
	}
	walkFS, base, closeRoot, err := p.treeFS(dir, abs)
	if err != nil {
		return nil, err
	}
	defer closeRoot()
	out := map[string][]byte{}
	var total int64
	err = fs.WalkDir(walkFS, base, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := relative(base, name)
		if d.IsDir() {
			if rel != "." && slices.Contains(limits.Skip, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // Links are not followed; devices and pipes are not read.
		}
		if limits.Files > 0 && len(out) >= limits.Files {
			return refuse(dir, fmt.Errorf("%w: more than %d files", ErrTreeTooLarge, limits.Files))
		}
		left := int64(0)
		if limits.Bytes > 0 {
			left = limits.Bytes - total
			if left <= 0 {
				return refuse(dir, fmt.Errorf("%w: more than %d bytes", ErrTreeTooLarge, limits.Bytes))
			}
		}
		raw, err := p.ReadFile(filepath.Join(abs, filepath.FromSlash(rel)), left)
		if errors.Is(err, ErrTooLarge) {
			return refuse(dir, fmt.Errorf("%w: more than %d bytes", ErrTreeTooLarge, limits.Bytes))
		}
		if err != nil {
			return err
		}
		total += int64(len(raw))
		out[rel] = raw
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// treeFS is the file system ReadTree walks and the directory's name in it:
// the host for an unrestricted policy, otherwise the root holding it.
func (p Policy) treeFS(dir, abs string) (fs.FS, string, func(), error) {
	if p.Unrestricted {
		return os.DirFS(abs), ".", func() {}, nil
	}
	for _, root := range p.Roots {
		rel, ok := within(root, abs)
		if !ok {
			continue
		}
		r, err := os.OpenRoot(root)
		if err != nil {
			return nil, "", nil, err
		}
		base := filepath.ToSlash(rel)
		if _, err := r.Stat(rel); err != nil {
			r.Close()
			if escapes(r, err) {
				return nil, "", nil, refuse(dir, outside{roots: p.Roots})
			}
			return nil, "", nil, &fs.PathError{Op: "open", Path: dir, Err: unwrapPath(err)}
		}
		return r.FS(), base, func() { r.Close() }, nil
	}
	return nil, "", nil, refuse(dir, outside{roots: p.Roots})
}

func relative(base, name string) string {
	if base == "." {
		return name
	}
	if name == base {
		return "."
	}
	rel, _ := filepath.Rel(filepath.FromSlash(base), filepath.FromSlash(name))
	return path.Clean(filepath.ToSlash(rel))
}
