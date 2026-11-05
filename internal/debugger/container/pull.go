package container

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/distribution/reference"
)

// The manifest media types a pull accepts, besides mediaManifest.
const (
	mediaIndex          = "application/vnd.oci.image.index.v1+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// Limits of what a pull reads.
const (
	maxManifestSize = 4 << 20
	maxConfigSize   = 1 << 20
	// maxBinarySize bounds the extracted runtime binary.
	maxBinarySize = 1 << 30
)

// ErrPlatform reports that an image's binary cannot run on this host: the
// host is not Linux, or the image is for another platform. The caller may
// run the image in Docker instead.
var ErrPlatform = errors.New("the runtime image does not run natively on this host")

// ErrNotRuntime reports an image that is not a runtime image: no runtime
// label, or not a single layer with /worker.
var ErrNotRuntime = errors.New("not a Linea runtime image")

// ImagePull reads runtime images from a registry without a Docker daemon
// and keeps their binaries in a cache directory, by the
// digest the reference resolves to. A reference by digest that is cached
// needs no registry.
type ImagePull struct {
	// Dir is the cache directory. Required.
	Dir string
	// Client does the registry requests; nil means http.DefaultClient.
	Client *http.Client
	// Auth is the content of a Docker config.json, read as ImageExport.Auth.
	// Empty pulls anonymously.
	Auth []byte
	// CredentialHelper is as ImageExport.CredentialHelper.
	CredentialHelper func(ctx context.Context, name, server string) (user, secret string, found bool, err error)
	// PlainHTTP names registry hosts reached over http; loopback hosts
	// always are, as Docker allows.
	PlainHTTP []string
	// Platform is the "os/arch" whose images run here; empty means this
	// host's, and only Linux hosts run runtime images natively.
	Platform string
}

// PulledImage is a runtime image's binary in the cache.
type PulledImage struct {
	// Reference is the image by digest, repository@sha256:....
	Reference string `json:"reference"`
	// Digest is the digest the reference resolved to: the manifest's, or
	// the index's for a multi-platform image.
	Digest string `json:"digest"`
	// Path is the binary; BinaryDigest is "sha256:<hex>" of it.
	Path         string `json:"-"`
	BinaryDigest string `json:"binary_digest"`
	Size         int64  `json:"size"`
	// Labels are the image's labels (LabelRuntime and the others).
	Labels map[string]string `json:"labels"`
}

// imageMeta is the file beside a cached binary.
const imageMeta = "image.json"

// DockerConfig reads the Docker client's config.json, from $DOCKER_CONFIG
// or ~/.docker, as Docker does; a missing file is no configuration.
func DockerConfig() ([]byte, error) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil
		}
		dir = filepath.Join(home, ".docker")
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// IsImageReference reports whether a runtime selection is an image
// reference rather than a runtime name: it has '/', ':' or '@', which no
// runtime name has.
func IsImageReference(s string) bool { return strings.ContainsAny(s, "/:@") }

func (p *ImagePull) platform() (string, error) {
	if p.Platform != "" {
		return p.Platform, nil
	}
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("%w: runtime images hold Linux binaries, and this host runs %s", ErrPlatform, runtime.GOOS)
	}
	return "linux/" + runtime.GOARCH, nil
}

// Pull returns the cached binary of ref, reading the image from its
// registry unless ref is by digest and cached. It refuses an image for
// another platform (ErrPlatform) and one that is not a runtime image
// (ErrNotRuntime), and checks every blob against its digest.
func (p *ImagePull) Pull(ctx context.Context, ref string) (*PulledImage, error) {
	if p.Dir == "" {
		return nil, errors.New("pulling a runtime image needs a cache directory")
	}
	platform, err := p.platform()
	if err != nil {
		return nil, err
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return nil, fmt.Errorf("image reference %q: %w", ref, err)
	}
	named = reference.TagNameOnly(named)
	target := ""
	switch r := named.(type) {
	case reference.Digested:
		target = r.Digest().String()
		if img, ok := p.cached(target); ok {
			return img, nil
		}
	case reference.Tagged:
		target = r.Tag()
	}
	host := reference.Domain(named)
	r := &registryClient{client: p.Client, host: host, name: reference.Path(named), scheme: "https", scope: "pull"}
	if r.client == nil {
		r.client = http.DefaultClient
	}
	if slices.Contains(p.PlainHTTP, host) || loopback(host) {
		r.scheme = "http"
	}
	if host == "docker.io" {
		r.host = "registry-1.docker.io"
	}
	helper := p.CredentialHelper
	if helper == nil {
		helper = runCredentialHelper
	}
	if r.user, r.password, err = dockerCredentials(ctx, p.Auth, host, helper); err != nil {
		return nil, err
	}

	raw, media, err := r.manifest(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	top := digestOf(raw)
	if strings.HasPrefix(target, "sha256:") && top != target {
		return nil, fmt.Errorf("image %s: the registry's manifest has digest %s", ref, top)
	}
	if img, ok := p.cached(top); ok {
		return img, nil
	}
	if media == mediaIndex || media == mediaDockerList {
		child, err := pickPlatform(raw, platform)
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", ref, err)
		}
		if raw, media, err = r.manifest(ctx, child); err != nil {
			return nil, fmt.Errorf("image %s: %w", ref, err)
		}
		if digestOf(raw) != child {
			return nil, fmt.Errorf("image %s: the registry's manifest for %s has another digest", ref, platform)
		}
	}
	if media != mediaManifest && media != mediaDockerManifest {
		return nil, fmt.Errorf("image %s: %w: a manifest of type %q", ref, ErrNotRuntime, media)
	}
	var m struct {
		Config descriptor   `json:"config"`
		Layers []descriptor `json:"layers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("image %s: the manifest is not JSON", ref)
	}
	if len(m.Layers) != 1 {
		return nil, fmt.Errorf("image %s: %w: it has %d layers, not 1", ref, ErrNotRuntime, len(m.Layers))
	}
	if m.Config.Size > maxConfigSize || m.Layers[0].Size > maxBinarySize*2 {
		return nil, fmt.Errorf("image %s: %w: its config or layer is too large", ref, ErrNotRuntime)
	}
	config, err := r.smallBlob(ctx, m.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("image %s: the config: %w", ref, err)
	}
	var cfg struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("image %s: the config is not JSON", ref)
	}
	if cfg.Config.Labels[LabelRuntime] == "" {
		return nil, fmt.Errorf("image %s: %w: it has no %s label", ref, ErrNotRuntime, LabelRuntime)
	}
	if got := cfg.OS + "/" + cfg.Architecture; got != platform {
		return nil, fmt.Errorf("%w: image %s is for %s, and this host runs %s", ErrPlatform, ref, got, platform)
	}

	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(p.Dir, ".pull-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	img := &PulledImage{Reference: named.Name() + "@" + top, Digest: top, Labels: cfg.Config.Labels}
	if img.BinaryDigest, img.Size, err = r.extractWorker(ctx, m.Layers[0], filepath.Join(tmp, "worker")); err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	meta, err := json.Marshal(img)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, imageMeta), meta, 0o644); err != nil {
		return nil, err
	}
	dir := p.entry(top)
	if err := os.Rename(tmp, dir); err != nil {
		// Another process cached the same image first.
		if cached, ok := p.cached(top); ok {
			return cached, nil
		}
		return nil, err
	}
	img.Path = filepath.Join(dir, "worker")
	return img, nil
}

// PruneImages removes every cached binary from dir, and what an
// interrupted pull left; it returns how many images it removed.
func PruneImages(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, ".pull-") && !validHex(name) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return n, err
		}
		if !strings.HasPrefix(name, ".pull-") {
			n++
		}
	}
	return n, nil
}

func validHex(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == 64 && err == nil
}

// entry is the cache directory of a digest.
func (p *ImagePull) entry(digest string) string {
	return filepath.Join(p.Dir, strings.TrimPrefix(digest, "sha256:"))
}

// cached returns the cached image of a digest, if a complete one is there.
func (p *ImagePull) cached(digest string) (*PulledImage, bool) {
	if !strings.HasPrefix(digest, "sha256:") || !validHex(strings.TrimPrefix(digest, "sha256:")) {
		return nil, false
	}
	dir := p.entry(digest)
	raw, err := os.ReadFile(filepath.Join(dir, imageMeta))
	if err != nil {
		return nil, false
	}
	var img PulledImage
	if json.Unmarshal(raw, &img) != nil || img.Digest != digest {
		return nil, false
	}
	img.Path = filepath.Join(dir, "worker")
	if st, err := os.Stat(img.Path); err != nil || st.Size() != img.Size {
		return nil, false
	}
	return &img, true
}

// loopback reports whether a registry host (with an optional port) is
// this machine.
func loopback(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// descriptor is a manifest's reference to a blob or another manifest.
type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

// pickPlatform returns the digest of an index's manifest for platform.
func pickPlatform(index []byte, platform string) (string, error) {
	var idx struct {
		Manifests []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(index, &idx); err != nil {
		return "", errors.New("the image index is not JSON")
	}
	var have []string
	for _, d := range idx.Manifests {
		if d.Platform == nil {
			continue
		}
		p := d.Platform.OS + "/" + d.Platform.Architecture
		if p == platform {
			return d.Digest, nil
		}
		have = append(have, p)
	}
	return "", fmt.Errorf("%w: the image has %s, and this host runs %s", ErrPlatform, strings.Join(have, ", "), platform)
}

// manifest reads a manifest by tag or digest and its media type.
func (r *registryClient) manifest(ctx context.Context, target string) ([]byte, string, error) {
	resp, err := r.get(ctx, r.url("/manifests/"+target),
		strings.Join([]string{mediaManifest, mediaIndex, mediaDockerManifest, mediaDockerList}, ", "))
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("reading the manifest: the registry answered %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > maxManifestSize {
		return nil, "", errors.New("the manifest is too large")
	}
	media, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	var body struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(raw, &body) == nil && body.MediaType != "" {
		media = body.MediaType
	}
	return raw, strings.TrimSpace(media), nil
}

// smallBlob reads a blob of at most maxConfigSize and checks its digest.
func (r *registryClient) smallBlob(ctx context.Context, digest string) ([]byte, error) {
	resp, err := r.blob(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxConfigSize || digestOf(raw) != digest {
		return nil, fmt.Errorf("the blob does not match its digest %s", digest)
	}
	return raw, nil
}

func (r *registryClient) blob(ctx context.Context, digest string) (*http.Response, error) {
	if !strings.HasPrefix(digest, "sha256:") || !validHex(strings.TrimPrefix(digest, "sha256:")) {
		return nil, fmt.Errorf("unsupported blob digest %q", digest)
	}
	resp, err := r.get(ctx, r.url("/blobs/"+digest), "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		drain(resp)
		return nil, fmt.Errorf("reading blob %s: the registry answered %s", digest, resp.Status)
	}
	return resp, nil
}

func (r *registryClient) get(ctx context.Context, target, accept string) (*http.Response, error) {
	header := http.Header{}
	if accept != "" {
		header.Set("Accept", accept)
	}
	return r.send(ctx, http.MethodGet, target, nil, 0, header)
}

// extractWorker reads the layer, writes its /worker entry to out (mode
// 0755) and checks the whole layer against its digest before returning the
// binary's digest and size.
func (r *registryClient) extractWorker(ctx context.Context, layer descriptor, out string) (string, int64, error) {
	resp, err := r.blob(ctx, layer.Digest)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	layerHash := sha256.New()
	body := io.TeeReader(io.LimitReader(resp.Body, maxBinarySize*2+1), layerHash)
	content := body
	switch layer.MediaType {
	case mediaLayer, "application/vnd.docker.image.rootfs.diff.tar.gzip":
		gz, err := gzip.NewReader(body)
		if err != nil {
			return "", 0, fmt.Errorf("%w: the layer is not gzip", ErrNotRuntime)
		}
		defer gz.Close()
		content = gz
	case "application/vnd.oci.image.layer.v1.tar", "application/vnd.docker.image.rootfs.diff.tar":
	default:
		return "", 0, fmt.Errorf("%w: a layer of type %q", ErrNotRuntime, layer.MediaType)
	}
	tr := tar.NewReader(content)
	binHash := sha256.New()
	var size int64
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", 0, fmt.Errorf("%w: the layer is not a tar archive", ErrNotRuntime)
		}
		if path.Clean("/"+h.Name) != "/worker" {
			continue
		}
		if found || h.Typeflag != tar.TypeReg || h.Size > maxBinarySize {
			return "", 0, fmt.Errorf("%w: /worker is not one regular file", ErrNotRuntime)
		}
		f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			return "", 0, err
		}
		size, err = io.Copy(io.MultiWriter(f, binHash), tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", 0, fmt.Errorf("reading the layer: %w", err)
		}
		found = true
	}
	// The rest of the stream counts toward the layer's digest.
	if _, err := io.Copy(io.Discard, body); err != nil {
		return "", 0, fmt.Errorf("reading the layer: %w", err)
	}
	if hexDigest(layerHash) != layer.Digest {
		return "", 0, fmt.Errorf("the layer does not match its digest %s", layer.Digest)
	}
	if !found {
		return "", 0, fmt.Errorf("%w: the layer has no /worker", ErrNotRuntime)
	}
	return hexDigest(binHash), size, nil
}
