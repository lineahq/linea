package engine

import (
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/lineahq/linea/internal/debugger/scenario"
)

// Captured dependencies are keyed by the absolute path the host read them
// from. A worker on another operating system (a Windows host's bundle in a
// Linux container) must make the same keys from a scenario's
// directory and file names, so it resolves them with the host's rules.

// hostAbs resolves a scenario file name against its directory with the path
// rules of the host operating system hostOS, as filepath.Abs does there.
func hostAbs(hostOS, dir, name string) (string, error) {
	if hostOS == "" || hostOS == goruntime.GOOS {
		path := name
		if !filepath.IsAbs(path) {
			if dir == "" {
				return "", scenario.ErrRelativeWithoutDir
			}
			path = filepath.Join(dir, path)
		}
		return filepath.Abs(path)
	}
	if hostOS == "windows" {
		return windowsAbs(dir, name)
	}
	return slashAbs(dir, name)
}

// hostKey is the capture key of a path already resolved on the host's rules.
func hostKey(hostOS, path string) (string, error) {
	if hostOS == "" || hostOS == goruntime.GOOS {
		return filepath.Abs(path)
	}
	return path, nil
}

func slashAbs(dir, name string) (string, error) {
	if !strings.HasPrefix(name, "/") {
		if dir == "" {
			return "", scenario.ErrRelativeWithoutDir
		}
		name = dir + "/" + name
	}
	return "/" + cleanSegments(strings.Split(name, "/"), "/"), nil
}

// windowsAbs resolves as the scenario reader does on Windows: a name with a
// volume and a separator after it is absolute (filepath.IsAbs), any other
// name joins the directory; then filepath.Abs cleans it, accepting either
// separator.
func windowsAbs(dir, name string) (string, error) {
	name = strings.ReplaceAll(name, "/", `\`)
	if vol := windowsVolume(name); vol == "" || !strings.HasPrefix(name[len(vol):], `\`) {
		if dir == "" {
			return "", scenario.ErrRelativeWithoutDir
		}
		name = strings.ReplaceAll(dir, "/", `\`) + `\` + name
	}
	vol := windowsVolume(name)
	return vol + `\` + cleanSegments(strings.Split(name[len(vol):], `\`), `\`), nil
}

// windowsVolume is a path's drive ("C:") or UNC share (`\\host\share`).
func windowsVolume(p string) string {
	if len(p) >= 2 && p[1] == ':' && ('a' <= p[0]|0x20 && p[0]|0x20 <= 'z') {
		return p[:2]
	}
	if strings.HasPrefix(p, `\\`) {
		if parts := strings.SplitN(p[2:], `\`, 3); len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return `\\` + parts[0] + `\` + parts[1]
		}
	}
	return ""
}

func cleanSegments(segments []string, sep string) string {
	out := make([]string, 0, len(segments))
	for _, s := range segments {
		switch s {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	return strings.Join(out, sep)
}
