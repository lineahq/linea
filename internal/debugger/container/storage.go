package container

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Container name prefixes of builds and workers. Both run with --rm, so a
// stopped one is left over only when Docker could not remove it.
const (
	buildContainerPrefix  = "linea-build-"
	workerContainerPrefix = "linea-worker-"
)

// Volume is a cache volume of this user's builds.
type Volume struct {
	Name string
	// Size is Docker's estimate in bytes; -1 when Docker did not report it.
	Size int64
}

// CacheVolumes returns the build cache volumes that exist, with Docker's
// size estimate (docker system df, which measures every volume and can take
// seconds).
func (d *Docker) CacheVolumes(ctx context.Context) ([]Volume, error) {
	out, err := d.output(ctx, nil, "volume", "ls", "-q", "--filter", "name=linea-")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Fields(out) {
		if n == ModuleCacheVolume || n == BuildCacheVolume {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	raw, err := d.output(ctx, nil, "system", "df", "-v", "--format", "{{json .Volumes}}")
	if err != nil {
		return nil, err
	}
	var listed []struct{ Name, Size string }
	if err := json.Unmarshal([]byte(raw), &listed); err != nil {
		return nil, fmt.Errorf("docker system df: %w", err)
	}
	volumes := make([]Volume, 0, len(names))
	for _, n := range names {
		v := Volume{Name: n, Size: -1}
		for _, l := range listed {
			if l.Name == n {
				v.Size = ParseSize(l.Size)
			}
		}
		volumes = append(volumes, v)
	}
	return volumes, nil
}

// RemoveVolume removes a cache volume. Docker refuses while a container
// uses it.
func (d *Docker) RemoveVolume(ctx context.Context, name string) error {
	if name != ModuleCacheVolume && name != BuildCacheVolume {
		return fmt.Errorf("%s is not a debugger cache volume", name)
	}
	_, err := d.output(ctx, nil, "volume", "rm", name)
	return err
}

// LeftoverContainer is a build or worker container Docker still has.
type LeftoverContainer struct {
	Name string
	// Running containers may belong to another debugger process and are
	// never removed.
	Running bool
	State   string
}

// Leftovers lists the build and worker containers Docker has, running or
// not.
func (d *Docker) Leftovers(ctx context.Context) ([]LeftoverContainer, error) {
	out, err := d.output(ctx, nil, "ps", "-a", "--filter", "name=linea-", "--format", "{{.Names}}\t{{.State}}")
	if err != nil {
		return nil, err
	}
	var list []LeftoverContainer
	for _, line := range strings.Split(out, "\n") {
		name, state, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if !strings.HasPrefix(name, buildContainerPrefix) && !strings.HasPrefix(name, workerContainerPrefix) {
			continue
		}
		list = append(list, LeftoverContainer{Name: name, State: state, Running: state == "running" || state == "paused" || state == "restarting"})
	}
	slices.SortFunc(list, func(a, b LeftoverContainer) int { return strings.Compare(a.Name, b.Name) })
	return list, nil
}

// RemoveStopped removes a stopped build or worker container; a running one
// is refused.
func (d *Docker) RemoveStopped(ctx context.Context, c LeftoverContainer) error {
	if c.Running {
		return fmt.Errorf("container %s is running", c.Name)
	}
	if !strings.HasPrefix(c.Name, buildContainerPrefix) && !strings.HasPrefix(c.Name, workerContainerPrefix) {
		return fmt.Errorf("%s is not a debugger container", c.Name)
	}
	_, err := d.output(ctx, nil, "rm", c.Name)
	return err
}

var sizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([kKMGTP]?i?B)$`)

// ParseSize reads a size as Docker prints it ("5.2GB", "512MB", "0B", decimal
// units, or binary ones such as "1.5GiB"); -1 when it is not one.
func ParseSize(s string) int64 {
	m := sizePattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return -1
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return -1
	}
	unit := strings.ToUpper(m[2])
	base := 1000.0
	if strings.Contains(unit, "I") {
		base = 1024
	}
	exp := strings.Index("KMGTP", unit[:1]) + 1 // "B" is 0
	return int64(math.Round(n * math.Pow(base, float64(exp))))
}
