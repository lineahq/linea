package container

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lineahq/linea/internal/debugger/container/registrytest"
)

// pushedRuntime exports a runtime image of a small binary to a fake
// registry, as linea-builder does, and returns the registry, the pull that
// reads it and the repository.
func pushedRuntime(t *testing.T, labels map[string]string) (*registrytest.Registry, *ImagePull, string) {
	t.Helper()
	reg := newFakeRegistry(t, "team/runtimes", "ci", "pw")
	bin := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(bin, []byte("#!binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(map[string]any{"auths": map[string]any{reg.Host(): map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("ci:pw"))}}})
	e := &ImageExport{Auth: auth, PlainHTTP: []string{reg.Host()}, WorkDir: t.TempDir()}
	repo := reg.Host() + "/team/runtimes"
	if _, err := e.Push(context.Background(), repo, "edge", bin, labels); err != nil {
		t.Fatal(err)
	}
	return reg, &ImagePull{Dir: t.TempDir(), Auth: auth, Platform: "linux/" + runtime.GOARCH}, repo
}

// TestAPulledImageIsTheExportedBinary: a pull reads the exported image's
// binary and labels into the cache and names the image by digest. A pull
// by that digest needs no registry, and a pull by tag reads no blob again.
func TestAPulledImageIsTheExportedBinary(t *testing.T) {
	reg, p, repo := pushedRuntime(t, map[string]string{LabelRuntime: "edge", LabelProtocol: "2"})
	img, err := p.Pull(context.Background(), repo+":edge")
	if err != nil {
		t.Fatal(err)
	}
	manifest := reg.Manifests["edge"]
	if img.Reference != repo+"@"+digestOf(manifest) || img.Digest != digestOf(manifest) {
		t.Fatalf("image = %+v", img)
	}
	got, err := os.ReadFile(img.Path)
	if err != nil || string(got) != "#!binary" || img.BinaryDigest != digestOf([]byte("#!binary")) || img.Size != 8 {
		t.Fatalf("binary = %q, %v, %+v", got, err, img)
	}
	if img.Labels[LabelRuntime] != "edge" || img.Labels[LabelProtocol] != "2" {
		t.Fatalf("labels = %v", img.Labels)
	}
	if st, err := os.Stat(img.Path); err != nil || runtime.GOOS != "windows" && st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("binary mode = %v, %v", st, err)
	}

	reads := reg.BlobReads
	again, err := p.Pull(context.Background(), repo+":edge")
	if err != nil || again.Path != img.Path || reg.BlobReads != reads {
		t.Fatalf("pull by tag again = %+v, %v; %d blob reads after %d", again, err, reg.BlobReads, reads)
	}
	reg.Server.Close()
	pinned, err := p.Pull(context.Background(), img.Reference)
	if err != nil || pinned.Path != img.Path || pinned.BinaryDigest != img.BinaryDigest {
		t.Fatalf("pull by digest with the registry gone = %+v, %v", pinned, err)
	}
}

// TestAPullPicksThisPlatformFromAnIndex: a multi-platform image resolves
// to the manifest of the host's platform, and the cache is keyed by the
// index's digest.
func TestAPullPicksThisPlatformFromAnIndex(t *testing.T) {
	reg, p, repo := pushedRuntime(t, map[string]string{LabelRuntime: "edge"})
	child := reg.Manifests["edge"]
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mediaIndex, "manifests": []any{
		map[string]any{"mediaType": mediaManifest, "digest": "sha256:" + string(bytes.Repeat([]byte("0"), 64)), "size": 1, "platform": map[string]string{"os": "linux", "architecture": "other"}},
		map[string]any{"mediaType": mediaManifest, "digest": digestOf(child), "size": len(child), "platform": map[string]string{"os": "linux", "architecture": runtime.GOARCH}},
	}})
	reg.Manifests["multi"] = index
	reg.Manifests[digestOf(index)] = index
	img, err := p.Pull(context.Background(), repo+":multi")
	if err != nil || img.Digest != digestOf(index) {
		t.Fatalf("pull = %+v, %v", img, err)
	}
	if b, _ := os.ReadFile(img.Path); string(b) != "#!binary" {
		t.Fatalf("binary = %q", b)
	}
}

// TestAPullRefusesWhatDoesNotRunHere: an image for another platform is
// ErrPlatform, an image without the runtime label is ErrNotRuntime, and a
// layer that does not match its digest fails; nothing is cached.
func TestAPullRefusesWhatDoesNotRunHere(t *testing.T) {
	ctx := context.Background()
	_, p, repo := pushedRuntime(t, map[string]string{LabelRuntime: "edge"})
	p.Platform = "linux/elsewhere"
	if _, err := p.Pull(ctx, repo+":edge"); !errors.Is(err, ErrPlatform) {
		t.Fatalf("another platform: %v", err)
	}

	_, p, repo = pushedRuntime(t, map[string]string{"org.example": "x"})
	if _, err := p.Pull(ctx, repo+":edge"); !errors.Is(err, ErrNotRuntime) {
		t.Fatalf("no runtime label: %v", err)
	}

	reg, p, repo := pushedRuntime(t, map[string]string{LabelRuntime: "edge"})
	var m struct {
		Layers []struct{ Digest string } `json:"layers"`
	}
	_ = json.Unmarshal(reg.Manifests["edge"], &m)
	layer := reg.Blobs[m.Layers[0].Digest]
	reg.Blobs[m.Layers[0].Digest] = append(bytes.Clone(layer[:len(layer)-1]), layer[len(layer)-1]^1)
	if _, err := p.Pull(ctx, repo+":edge"); err == nil {
		t.Fatal("a tampered layer was accepted")
	}
	if entries, _ := os.ReadDir(p.Dir); len(entries) != 0 {
		t.Fatalf("the cache holds %v", entries)
	}

	if runtime.GOOS != "linux" {
		host := &ImagePull{Dir: t.TempDir()}
		if _, err := host.Pull(ctx, repo+":edge"); !errors.Is(err, ErrPlatform) {
			t.Fatalf("a %s host: %v", runtime.GOOS, err)
		}
	}
}

// TestPruningEmptiesTheImageCache: PruneImages removes cached images and
// interrupted pulls, and leaves other files.
func TestPruningEmptiesTheImageCache(t *testing.T) {
	_, p, repo := pushedRuntime(t, map[string]string{LabelRuntime: "edge"})
	if _, err := p.Pull(context.Background(), repo+":edge"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(p.Dir, ".pull-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Dir, "notes"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := PruneImages(p.Dir)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	entries, _ := os.ReadDir(p.Dir)
	if len(entries) != 1 || entries[0].Name() != "notes" {
		t.Fatalf("left %v", entries)
	}
}

// TestImageReferencesAreNotRuntimeNames: a selection with '/', ':' or '@'
// is an image reference; a runtime name has none.
func TestImageReferencesAreNotRuntimeNames(t *testing.T) {
	for s, want := range map[string]bool{"edge": false, "built-in": false, "registry.example.com/team/rt:edge": true, "rt:edge": true, "rt@sha256:00": true} {
		if got := IsImageReference(s); got != want || got && ValidName(s) {
			t.Errorf("IsImageReference(%q) = %v", s, got)
		}
	}
}
