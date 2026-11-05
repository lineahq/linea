package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/lineahq/linea/internal/debugger/manifest"
)

// ErrNotFound is an image Docker's local store does not have.
var ErrNotFound = errors.New("no such runtime image")

// Image is a runtime image in Docker's local store.
type Image struct {
	ID   string
	Tags []string
	// Digests are the image's registry references by digest
	// ("repository@sha256:<hex>"); empty for an image never pushed or pulled.
	Digests []string
	Labels  map[string]string
	// Size is the image's size in bytes.
	Size int64
}

// Name is the runtime's name, from its labels.
func (i Image) Name() string { return i.Labels[LabelRuntime] }

// Images lists the runtime images in Docker's local store: images carrying
// the runtime label, untagged ones too (a label filter hides those without
// -a), each once however many tags it has.
func (d *Docker) Images(ctx context.Context) ([]Image, error) {
	out, err := d.output(ctx, nil, "image", "ls", "-a", "-q", "--no-trunc", "--filter", "label="+LabelRuntime)
	if err != nil {
		return nil, err
	}
	ids := imageIDs(out)
	if len(ids) == 0 {
		return nil, nil
	}
	return d.inspect(ctx, ids...)
}

// imageIDs reads docker image ls -q output, which repeats an image's ID
// for each of its tags (a pushed runtime is also tagged as its target),
// keeping each ID once in order.
func imageIDs(out string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, id := range strings.Fields(out) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// Inspect returns one runtime image by reference or ID. An image without
// the runtime label is not a runtime.
func (d *Docker) Inspect(ctx context.Context, ref string) (*Image, error) {
	images, err := d.inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if images[0].Name() == "" {
		return nil, fmt.Errorf("image %s is not a debugger runtime", ref)
	}
	return &images[0], nil
}

func (d *Docker) inspect(ctx context.Context, refs ...string) ([]Image, error) {
	out, err := d.output(ctx, nil, append([]string{"image", "inspect"}, refs...)...)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such image") {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, strings.Join(refs, ", "))
		}
		return nil, err
	}
	var raw []struct {
		ID          string `json:"Id"`
		RepoTags    []string
		RepoDigests []string
		Size        int64
		Config      struct{ Labels map[string]string }
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("docker image inspect: %w", err)
	}
	images := make([]Image, 0, len(raw))
	for _, r := range raw {
		images = append(images, Image{ID: r.ID, Tags: r.RepoTags, Digests: r.RepoDigests, Labels: r.Config.Labels, Size: r.Size})
	}
	return images, nil
}

// Remove removes a runtime image by reference or ID.
func (d *Docker) Remove(ctx context.Context, ref string) error {
	_, err := d.output(ctx, nil, "image", "rm", ref)
	return err
}

// Push tags a runtime image as target and pushes it with the user's Docker
// credentials.
func (d *Docker) Push(ctx context.Context, ref, target string) error {
	if err := d.Tag(ctx, ref, target); err != nil {
		return err
	}
	_, err := d.output(ctx, nil, "image", "push", target)
	return err
}

// Tag tags an image (a reference or ID) as target.
func (d *Docker) Tag(ctx context.Context, ref, target string) error {
	_, err := d.output(ctx, nil, "image", "tag", ref, target)
	return err
}

// Pull pulls an image reference into Docker's local store.
func (d *Docker) Pull(ctx context.Context, ref string) error {
	_, err := d.output(ctx, nil, "image", "pull", "--quiet", ref)
	return err
}

// Check finds what Build would refuse before resolving or compiling,
// without Docker: a manifest that does not parse, modules at another
// version than the dependency set's release, local replaces without a
// source, or a source requiring a later release (a ModuleError). Sources
// need no Files here; those without are checked by Build.
func Check(src fs.FS, raw []byte, set *DependencySet, sources []ModuleSource) (*manifest.Manifest, error) {
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, err
	}
	p, err := planBuild(src, m, set, sourced(sources))
	if err != nil {
		return nil, err
	}
	if err := p.checkSources(sources); err != nil {
		return nil, err
	}
	return m, nil
}
