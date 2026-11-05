package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/workersrc"
)

// DockerOptions configure DockerRuntimes.
type DockerOptions struct {
	// Docker is the docker executable; empty means "docker" on PATH.
	Docker string
	// Source is the worker source builds compile; nil means the source
	// embedded in this debugger (workersrc.Embedded).
	Source fs.FS
	// Memory limits each worker container; zero means
	// container.DefaultRunMemory.
	Memory int64
	// BuildMemory and BuildCPUs limit a build container; zero means the
	// container package's defaults.
	BuildMemory int64
	BuildCPUs   float64
	// ImageCache is where the binaries of runtime images named by
	// reference are kept; empty runs such images in Docker.
	ImageCache string
}

// DockerRuntimes is the RuntimeStore of the composing application: builds
// and workers run in Docker containers and Docker's image store is the
// runtime list. Adapters never call Docker themselves.
func DockerRuntimes(opts DockerOptions) RuntimeStore {
	return &dockerRuntimes{docker: &container.Docker{Path: opts.Docker}, opts: opts}
}

type dockerRuntimes struct {
	docker *container.Docker
	opts   DockerOptions
}

// dockerError marks Docker being unusable as the runtime being
// unavailable, keeping the message.
func dockerError(err error) error {
	if errors.Is(err, container.ErrUnavailable) {
		return fmt.Errorf("%w: %w", engine.ErrRuntimeUnavailable, err)
	}
	if errors.Is(err, container.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrRuntimeNotFound, err)
	}
	return err
}

func (d *dockerRuntimes) source() (fs.FS, error) {
	if d.opts.Source != nil {
		return d.opts.Source, nil
	}
	src, err := workersrc.Embedded()
	if errors.Is(err, workersrc.ErrNotBuilt) {
		return nil, &Error{Code: CodeUnavailable, Message: err.Error(), cause: err}
	}
	return src, err
}

func (d *dockerRuntimes) Check(spec RuntimeBuildSpec) error {
	src, err := d.source()
	if err != nil {
		return err
	}
	_, err = container.Check(src, spec.Manifest, dependencySet(spec.Release), moduleSources(spec.Sources))
	var modules *container.ModuleError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &modules):
		return manifestDiagnostics("manifest_modules", modules.Problems...)
	default:
		return manifestDiagnostics("invalid_manifest", err.Error())
	}
}

func (d *dockerRuntimes) Build(ctx context.Context, spec RuntimeBuildSpec, log func(string)) (*RuntimeInfo, error) {
	src, err := d.source()
	if err != nil {
		return nil, err
	}
	rt, err := d.docker.Build(ctx, container.BuildRequest{
		Name: spec.Name, Manifest: spec.Manifest, Source: src, GoImage: spec.GoImage,
		Set: dependencySet(spec.Release), Sources: moduleSources(spec.Sources),
		ModuleProxy: spec.ModuleProxy, Offline: spec.Offline,
		Memory: d.opts.BuildMemory, CPUs: d.opts.BuildCPUs, Log: log,
	})
	if err != nil {
		return nil, dockerError(err)
	}
	return d.Get(ctx, rt.ImageID)
}

func dependencySet(r *versions.Release) *container.DependencySet {
	if r == nil {
		return nil
	}
	return &container.DependencySet{GoMod: r.GoMod, GoSum: r.GoSum}
}

func moduleSources(sources []BuildSource) []container.ModuleSource {
	out := make([]container.ModuleSource, 0, len(sources))
	for _, s := range sources {
		out = append(out, container.ModuleSource{Module: s.Module, Files: s.Files, Origin: s.Origin, Commit: s.Commit})
	}
	return out
}

func runtimeInfo(img container.Image) RuntimeInfo {
	l := img.Labels
	protocol, _ := strconv.Atoi(l[container.LabelProtocol])
	info := RuntimeInfo{
		Name: img.Name(), ImageID: img.ID, Tags: slices.Clone(img.Tags), Size: img.Size,
		CollectorVersion: l[container.LabelCollectorVersion], ProductVersion: l[container.LabelProductVersion],
		Protocol: protocol, Verified: l[container.LabelVerified] == "true",
		Manifest: l[container.LabelManifest], ModuleSet: l[container.LabelModuleSet], GoImage: l[container.LabelGoImage],
	}
	if raw := l[container.LabelCustomModules]; raw != "" {
		var custom []container.CustomModule
		if json.Unmarshal([]byte(raw), &custom) == nil {
			for _, c := range custom {
				info.CustomModules = append(info.CustomModules, CustomModule(c))
			}
		}
	}
	if raw := l[container.LabelProviders]; raw != "" {
		var provs []container.Provider
		if json.Unmarshal([]byte(raw), &provs) == nil {
			for _, p := range provs {
				info.Providers = append(info.Providers, RuntimeProvider{Name: p.Name, Package: p.Package})
			}
		}
	}
	return info
}

func (d *dockerRuntimes) List(ctx context.Context) ([]RuntimeInfo, error) {
	images, err := d.docker.Images(ctx)
	if err != nil {
		return nil, dockerError(err)
	}
	out := make([]RuntimeInfo, 0, len(images))
	for _, img := range images {
		out = append(out, runtimeInfo(img))
	}
	return out, nil
}

// Get finds a runtime by name first (the linea-runtime:<name>
// tag), then as an image reference or ID.
func (d *dockerRuntimes) Get(ctx context.Context, ref string) (*RuntimeInfo, error) {
	if container.ValidName(ref) {
		img, err := d.docker.Inspect(ctx, container.Ref(ref))
		if err == nil {
			info := runtimeInfo(*img)
			return &info, nil
		}
		if !errors.Is(err, container.ErrNotFound) {
			return nil, dockerError(err)
		}
	}
	img, err := d.docker.Inspect(ctx, ref)
	if err != nil {
		return nil, dockerError(err)
	}
	info := runtimeInfo(*img)
	return &info, nil
}

// Delete removes the runtime's name tag, which removes the image unless
// another tag (a pushed one) still names it; an untagged runtime is
// removed by ID.
func (d *dockerRuntimes) Delete(ctx context.Context, info *RuntimeInfo) error {
	ref := info.ImageID
	if tag := container.Ref(info.Name); slices.Contains(info.Tags, tag) {
		ref = tag
	}
	return dockerError(d.docker.Remove(ctx, ref))
}

func (d *dockerRuntimes) Push(ctx context.Context, info *RuntimeInfo, target string) error {
	return dockerError(d.docker.Push(ctx, info.ImageID, target))
}

// Import pulls ref when asked and, once accept agrees, tags it with its
// runtime name so that requests can name it.
func (d *dockerRuntimes) Import(ctx context.Context, ref string, pull bool, accept func(*RuntimeInfo) error) (*RuntimeInfo, error) {
	if pull {
		if err := d.docker.Pull(ctx, ref); err != nil {
			return nil, dockerError(err)
		}
	}
	img, err := d.docker.Inspect(ctx, ref)
	if err != nil {
		return nil, dockerError(err)
	}
	info := runtimeInfo(*img)
	if !container.ValidName(info.Name) {
		return nil, fmt.Errorf("image %s names the runtime %q, which is not a valid runtime name", ref, info.Name)
	}
	if err := accept(&info); err != nil {
		return nil, err
	}
	if tag := container.Ref(info.Name); !slices.Contains(info.Tags, tag) {
		if err := d.docker.Tag(ctx, info.ImageID, tag); err != nil {
			return nil, dockerError(err)
		}
		info.Tags = append(info.Tags, tag)
	}
	return &info, nil
}

func (d *dockerRuntimes) Start(ctx context.Context, info *RuntimeInfo) (RuntimeWorker, error) {
	if info.Binary != "" {
		return startBinary(ctx, info)
	}
	w, err := d.docker.Start(ctx, info.ImageID, info.Name, container.RunOptions{Memory: d.opts.Memory})
	if err != nil {
		return nil, dockerError(err)
	}
	return w, nil
}

func (d *dockerRuntimes) CacheVolumes(ctx context.Context) ([]CacheVolume, error) {
	volumes, err := d.docker.CacheVolumes(ctx)
	if err != nil {
		return nil, dockerError(err)
	}
	out := make([]CacheVolume, 0, len(volumes))
	for _, v := range volumes {
		out = append(out, CacheVolume{Name: v.Name, Size: v.Size})
	}
	return out, nil
}

func (d *dockerRuntimes) RemoveCacheVolume(ctx context.Context, name string) error {
	return dockerError(d.docker.RemoveVolume(ctx, name))
}

func (d *dockerRuntimes) Leftovers(ctx context.Context) ([]LeftoverContainer, error) {
	list, err := d.docker.Leftovers(ctx)
	if err != nil {
		return nil, dockerError(err)
	}
	out := make([]LeftoverContainer, 0, len(list))
	for _, c := range list {
		out = append(out, LeftoverContainer{Name: c.Name, State: c.State, Running: c.Running})
	}
	return out, nil
}

func (d *dockerRuntimes) RemoveLeftover(ctx context.Context, c LeftoverContainer) error {
	return dockerError(d.docker.RemoveStopped(ctx, container.LeftoverContainer{Name: c.Name, State: c.State, Running: c.Running}))
}

// ReadHello starts ref's worker, pulled first when asked, and returns its
// hello with the image's registry digest ("sha256:<hex>", empty for an
// image never pushed or pulled), as linea runtime register sends them.
func (d *dockerRuntimes) ReadHello(ctx context.Context, ref string, pull bool) (digest string, hello []byte, err error) {
	if pull {
		if err := d.docker.Pull(ctx, ref); err != nil {
			return "", nil, dockerError(err)
		}
	}
	img, err := d.docker.Inspect(ctx, ref)
	if err != nil {
		return "", nil, dockerError(err)
	}
	w, err := d.docker.Start(ctx, img.ID, img.Name(), container.RunOptions{Memory: d.opts.Memory})
	if err != nil {
		return "", nil, dockerError(err)
	}
	defer w.Close()
	if hello, err = json.Marshal(w.Hello()); err != nil {
		return "", nil, err
	}
	return repoDigest(img.Digests, ref), hello, nil
}

// repoDigest picks the digest of ref's repository among an image's
// "<repository>@sha256:<hex>" references, else the first one.
func repoDigest(digests []string, ref string) string {
	repo := ref
	if i := strings.LastIndex(repo, "@"); i >= 0 {
		repo = repo[:i]
	} else if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	pick := ""
	for _, d := range digests {
		r, digest, ok := strings.Cut(d, "@")
		if !ok {
			continue
		}
		if r == repo {
			return digest
		}
		if pick == "" {
			pick = digest
		}
	}
	return pick
}
