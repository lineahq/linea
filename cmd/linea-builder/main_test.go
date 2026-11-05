package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/worker"
)

func TestFlagsAreChecked(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "--connect"},
		{[]string{"--connect", "https://linea.example"}, "--token-file"},
		{[]string{"--connect", "https://linea.example", "--token-file", "t", "--capacity", "0"}, "--capacity"},
		{[]string{"--connect", "https://linea.example", "--token-file", "t", "--cpus", "0"}, "--cpus"},
		{[]string{"--connect", "https://linea.example", "--token-file", "t", "--name", " "}, "--name"},
		{[]string{"--connect", "https://linea.example", "--token-file", "t", "extra"}, "unexpected argument"},
		{[]string{"--listen", "127.0.0.1:0"}, "not defined"},
	} {
		if _, err := parseOptions(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseOptions %v = %v, want an error naming %s", tc.args, err, tc.want)
		}
	}
	o, err := parseOptions([]string{"--connect", "https://linea.example", "--token-file", "t", "--name", "b1"})
	if err != nil {
		t.Fatal(err)
	}
	if o.capacity != 1 || o.cpus != container.DefaultBuildCPUs || o.name != "b1" || filepath.Base(o.workDir) != "linea-builder" {
		t.Fatalf("defaults: %+v", o)
	}
}

func TestRunReadsTheTokenAndCAFirst(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "token")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--connect", "https://linea.example", "--work-dir", dir}
	if err := run(context.Background(), nil, append(base, "--token-file", filepath.Join(dir, "missing"))); err == nil || !strings.Contains(err.Error(), "--token-file") {
		t.Errorf("a missing token file = %v", err)
	}
	if err := run(context.Background(), nil, append(base, "--token-file", empty)); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty token file = %v", err)
	}
	if err := os.WriteFile(empty, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), nil, append(base, "--token-file", empty, "--ca-file", filepath.Join(dir, "missing.pem"))); err == nil || !strings.Contains(err.Error(), "--ca-file") {
		t.Errorf("a missing CA file = %v", err)
	}
}

type fakeToolchain struct {
	version string
	err     error
}

func (f fakeToolchain) Toolchain(context.Context) (string, error) { return f.version, f.err }

// TestTheBuilderNeedsThePinnedToolchainAndTheWorkerSource refuses a go
// command of another release (identical binaries need the pinned one) and a
// builder built without the worker source.
func TestTheBuilderNeedsThePinnedToolchainAndTheWorkerSource(t *testing.T) {
	src := func() (fs.FS, error) { return fstest.MapFS{}, nil }
	if _, err := checkStart(context.Background(), fakeToolchain{version: container.GoVersion}, src); err != nil {
		t.Fatalf("the pinned toolchain: %v", err)
	}
	_, err := checkStart(context.Background(), fakeToolchain{version: "go1.26.0"}, src)
	if err == nil || !strings.Contains(err.Error(), "go1.26.0") || !strings.Contains(err.Error(), container.GoVersion) {
		t.Errorf("another toolchain: %v", err)
	}
	if _, err := checkStart(context.Background(), fakeToolchain{err: errors.New("not found")}, src); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("no go: %v", err)
	}
	notBuilt := func() (fs.FS, error) { return nil, errors.New("this debugger was built without the worker source") }
	if _, err := checkStart(context.Background(), fakeToolchain{version: container.GoVersion}, notBuilt); err == nil || !strings.Contains(err.Error(), "worker source") {
		t.Errorf("no worker source: %v", err)
	}
}

func TestRedactHidesURLCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"git clone https://user:pass@git.example.com/org/repo.git": "git clone https://***@git.example.com/org/repo.git",
		"GOPROXY=http://token@proxy.example.com,direct":            "GOPROXY=http://***@proxy.example.com,direct",
		"a https://x:y@a.example/b and ssh://git:z@c.example/d":    "a https://***@a.example/b and ssh://***@c.example/d",
		"no credentials in https://proxy.golang.org/x@v1.0.0.zip":  "no credentials in https://proxy.golang.org/x@v1.0.0.zip",
		"Put http://s3:8333/b/k?X-Amz-Signature=abc&X-Amz-Date=1":  "Put http://s3:8333/b/k?X-Amz-Signature=***&X-Amz-Date=1",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

// objectStore is a presigned-URL stand-in: GET serves objects, PUT keeps
// them.
type objectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    map[string]int64
}

func (s *objectStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		data, ok := s.objects[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	case http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		if r.ContentLength != int64(len(data)) || r.URL.Query().Get("X-Amz-Signature") == "" {
			http.Error(w, "bad upload", http.StatusBadRequest)
			return
		}
		s.objects[r.URL.Path] = data
		s.puts[r.URL.Path] = r.ContentLength
	}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type fixture struct {
	store    *objectStore
	server   *httptest.Server
	job      worker.BuildJob
	manifest []byte
	source   []byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := &objectStore{objects: map[string][]byte{}, puts: map[string]int64{}}
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)
	f := &fixture{store: store, server: srv, manifest: []byte("dist:\n  otelcol_version: 0.162.0\n")}
	f.source = moduleTar(t, "example.com/proc", map[string]string{"proc.go": "package proc\n"})
	store.objects["/in/manifest"], store.objects["/in/proc"] = f.manifest, f.source
	sign := "?X-Amz-Signature=secretsignature"
	f.job = worker.BuildJob{
		Build: "b1", Attempt: 1, Name: "edge",
		Manifest:         worker.BuildInput{URL: srv.URL + "/in/manifest" + sign, Digest: digestOf(f.manifest), Size: int64(len(f.manifest))},
		Sources:          []worker.BuildInput{{Module: "example.com/proc", URL: srv.URL + "/in/proc" + sign, Digest: digestOf(f.source), Size: int64(len(f.source))}},
		CollectorVersion: "0.162.0", Verified: true,
		BinaryURL: srv.URL + "/out/worker" + sign, BinaryLimit: 1 << 20,
	}
	return f
}

func moduleTar(t *testing.T, module string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := newTar(&buf)
	tw.add(t, "go.mod", "module "+module+"\n\ngo 1.27\n")
	for name, data := range files {
		tw.add(t, name, data)
	}
	tw.close(t)
	return buf.Bytes()
}

// fakeBuild stands in for container.Native.Build: it records the request and
// writes a binary file.
type fakeBuild struct {
	dir  string
	req  container.BuildRequest
	path string
	err  error
	size int64
}

func (f *fakeBuild) build(ctx context.Context, req container.BuildRequest) (*container.NativeRuntime, error) {
	f.req = req
	req.Log("compiling https://user:hunter2@modules.example.com/x")
	if f.err != nil {
		return nil, f.err
	}
	data := []byte("worker binary")
	if f.size > 0 {
		data = bytes.Repeat([]byte("x"), int(f.size))
	}
	f.path = filepath.Join(f.dir, "worker")
	if err := os.WriteFile(f.path, data, 0o755); err != nil {
		return nil, err
	}
	return &container.NativeRuntime{Name: req.Name, Path: f.path, Digest: digestOf(data), Size: int64(len(data)),
		Hello: worker.Hello{Runtime: "sha256:rt", CollectorVersion: "0.162.0", Verified: req.Set != nil}}, nil
}

func newBuilder(f *fakeBuild) *builder {
	return &builder{build: f.build, source: fstest.MapFS{"go.mod": {Data: []byte("module x\n")}}, table: versions.Embedded(), client: newTransferClient()}
}

type lines struct {
	mu sync.Mutex
	l  []string
}

func (l *lines) add(s string) { l.mu.Lock(); l.l = append(l.l, s); l.mu.Unlock() }
func (l *lines) text() string { l.mu.Lock(); defer l.mu.Unlock(); return strings.Join(l.l, "\n") }

// TestABuildDownloadsBuildsAndUploads runs a job: the inputs are checked, the
// sources unpacked, the verified release's dependency set chosen, the binary
// uploaded with its length and removed, and the result names it with the
// hello. No credential reaches the log.
func TestABuildDownloadsBuildsAndUploads(t *testing.T) {
	fx := newFixture(t)
	fb := &fakeBuild{dir: t.TempDir()}
	var log lines
	r := newBuilder(fb).run(context.Background(), fx.job, log.add)
	if r.Error != "" {
		t.Fatalf("error %q\n%s", r.Error, log.text())
	}
	if r.BinaryDigest != digestOf([]byte("worker binary")) || r.BinarySize != int64(len("worker binary")) || r.OS != "linux" || r.Arch == "" || r.Hello == nil || r.Hello.Runtime != "sha256:rt" {
		t.Fatalf("result %+v", r)
	}
	if got := fx.store.objects["/out/worker"]; string(got) != "worker binary" || fx.store.puts["/out/worker"] != int64(len(got)) {
		t.Fatalf("uploaded %q", got)
	}
	if _, err := os.Stat(fb.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the binary was not removed: %v", err)
	}
	req := fb.req
	if req.Name != "edge" || !bytes.Equal(req.Manifest, fx.manifest) || req.Set == nil || len(req.Set.GoMod) == 0 || len(req.Set.GoSum) == 0 {
		t.Fatalf("request %+v", req)
	}
	if len(req.Sources) != 1 || req.Sources[0].Module != "example.com/proc" || req.Sources[0].Origin != "upload "+fx.job.Sources[0].Digest {
		t.Fatalf("sources %+v", req.Sources)
	}
	if data, err := fs.ReadFile(req.Sources[0].Files, "proc.go"); err != nil || string(data) != "package proc\n" {
		t.Fatalf("proc.go %q, %v", data, err)
	}
	text := log.text()
	if !strings.Contains(text, "https://***@modules.example.com/x") {
		t.Errorf("the log was not redacted:\n%s", text)
	}
	for _, secret := range []string{"hunter2", "secretsignature"} {
		if strings.Contains(text, secret) {
			t.Errorf("the log holds %q:\n%s", secret, text)
		}
	}
}

func TestAnUnverifiedBuildHasNoDependencySet(t *testing.T) {
	fx := newFixture(t)
	fx.job.Verified, fx.job.CollectorVersion = false, "0.999.0"
	fb := &fakeBuild{dir: t.TempDir()}
	r := newBuilder(fb).run(context.Background(), fx.job, func(string) {})
	if r.Error != "" || fb.req.Set != nil {
		t.Fatalf("error %q, set %+v", r.Error, fb.req.Set)
	}
	if r.Hello == nil || r.Hello.Verified {
		t.Fatalf("hello %+v", r.Hello)
	}
}

// TestABuildRefusesWhatItCannotTrust fails, without building, on a verified
// release the builder does not know, a corrupt or oversized download, an
// unsafe source archive and a missing object; it refuses a binary over the
// limit without uploading it.
func TestABuildRefusesWhatItCannotTrust(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(fx *fixture)
		build  *fakeBuild
		want   string
	}{
		"an unknown verified release": {func(fx *fixture) { fx.job.CollectorVersion = "0.999.0" }, nil, "support table"},
		"a wrong manifest digest":     {func(fx *fixture) { fx.job.Manifest.Digest = digestOf([]byte("other")) }, nil, "digest"},
		"a wrong manifest size":       {func(fx *fixture) { fx.job.Manifest.Size++ }, nil, "bytes the server named"},
		"an oversized source": {func(fx *fixture) {
			fx.job.Sources[0].Size = maxSourceBytes + 1
		}, nil, "limit"},
		"a missing object": {func(fx *fixture) { fx.job.Manifest.URL = fx.server.URL + "/in/none" }, nil, "404"},
		"a symbolic link in a source": {func(fx *fixture) {
			var buf bytes.Buffer
			tw := newTar(&buf)
			tw.add(t, "go.mod", "module example.com/proc\n")
			tw.link(t, "evil", "/etc/passwd")
			tw.close(t)
			fx.store.objects["/in/proc"] = buf.Bytes()
			fx.job.Sources[0].Digest, fx.job.Sources[0].Size = digestOf(buf.Bytes()), int64(buf.Len())
		}, nil, "not a regular file"},
		"a source for another module": {func(fx *fixture) { fx.job.Sources[0].Module = "example.com/other" }, nil, "declares module"},
		"a failed build":              {func(fx *fixture) {}, &fakeBuild{err: errors.New("runtime build failed while compiling: boom https://u:p@host/x")}, "boom https://***@host/x"},
		"a binary over the limit":     {func(fx *fixture) { fx.job.BinaryLimit = 4 }, nil, "limit is 4"},
		"a refused upload":            {func(fx *fixture) { fx.job.BinaryURL = fx.server.URL + "/out/worker" }, nil, "400"},
	} {
		fx := newFixture(t)
		tc.change(fx)
		fb := tc.build
		if fb == nil {
			fb = &fakeBuild{}
		}
		fb.dir = t.TempDir()
		var log lines
		r := newBuilder(fb).run(context.Background(), fx.job, log.add)
		if !strings.Contains(r.Error, tc.want) {
			t.Errorf("%s: error %q, want %q", name, r.Error, tc.want)
		}
		if r.BinaryDigest != "" || r.Hello != nil {
			t.Errorf("%s: a failed build returned a binary: %+v", name, r)
		}
		for _, secret := range []string{"secretsignature", "u:p@"} {
			if strings.Contains(r.Error, secret) || strings.Contains(log.text(), secret) {
				t.Errorf("%s: %q leaked:\n%s\n%s", name, secret, r.Error, log.text())
			}
		}
		if fb.path != "" {
			if _, err := os.Stat(fb.path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: the binary was not removed", name)
			}
		}
	}
}

// TestADownloadStopsWithItsContext does not fetch once the build is
// cancelled.
func TestADownloadStopsWithItsContext(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fb := &fakeBuild{dir: t.TempDir()}
	r := newBuilder(fb).run(ctx, fx.job, func(string) {})
	if r.Error == "" || !strings.Contains(r.Error, "canceled") {
		t.Fatalf("result %+v", r)
	}
	if strings.Contains(r.Error, "secretsignature") {
		t.Fatalf("the error holds the URL's signature: %s", r.Error)
	}
	if fb.path != "" || fb.req.Name != "" {
		t.Fatal("a cancelled build ran")
	}
}

// TestTheImageTakesItsToolchainFromThePinnedGoImage keeps the builder
// image's toolchain the one the Docker builds use: its toolchain stage is
// container.GoImage, and the final stage copies that stage's GOROOT.
func TestTheImageTakesItsToolchainFromThePinnedGoImage(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/docker/linea-builder.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var toolchain string
	copied := false
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		f := strings.Fields(line)
		if len(f) == 4 && f[0] == "FROM" && f[2] == "AS" && f[3] == "toolchain" {
			toolchain = f[1]
		}
		if line == "COPY --from=toolchain /usr/local/go /usr/local/go" {
			copied = true
		}
	}
	if toolchain != container.GoImage {
		t.Fatalf("toolchain stage %q, want %s", toolchain, container.GoImage)
	}
	if !copied {
		t.Fatal("the final stage does not copy /usr/local/go from the toolchain stage")
	}
}

// TestABuildExportsItsImageWhenAsked: a job with Export pushes the image
// with the builder's own credentials and names it by digest; a registry
// that refuses leaves the build done, with its binary and the reason.
func TestABuildExportsItsImageWhenAsked(t *testing.T) {
	var mu sync.Mutex
	refuse, manifests := false, 0
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if u, p, _ := r.BasicAuth(); u != "ci" || p != "pw" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case refuse:
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost:
			w.Header().Set("Location", "/v2/team/runtimes/blobs/uploads/1")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/edge"):
			manifests++
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")
	auth := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(auth, []byte(`{"auths":{"`+host+`":{"username":"ci","password":"pw"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		refuse bool
	}{{"pushed", false}, {"refused", true}} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			refuse = tc.refuse
			mu.Unlock()
			fx := newFixture(t)
			fx.job.Export = &worker.BuildExport{Repository: host + "/team/runtimes", Tag: "edge"}
			b := newBuilder(&fakeBuild{dir: t.TempDir()})
			b.export = &container.ImageExport{PlainHTTP: []string{host}, WorkDir: t.TempDir()}
			b.registryAuth = auth
			var log lines
			r := b.run(context.Background(), fx.job, log.add)
			if r.Error != "" || r.BinaryDigest == "" {
				t.Fatalf("result %+v\n%s", r, log.text())
			}
			if tc.refuse {
				if r.Image != "" || r.ExportError == "" || !strings.Contains(log.text(), "exporting the image failed") {
					t.Fatalf("result %+v\n%s", r, log.text())
				}
				return
			}
			if !strings.HasPrefix(r.Image, host+"/team/runtimes@sha256:") || r.ExportError != "" || manifests != 1 {
				t.Fatalf("result %+v, %d manifests\n%s", r, manifests, log.text())
			}
			if strings.Contains(log.text(), "pw") {
				t.Errorf("the log holds the password:\n%s", log.text())
			}
		})
	}
}

// TestStoppingDrainsThenCancels: the first signal drains the builder, the
// second cancels its builds.
func TestStoppingDrainsThenCancels(t *testing.T) {
	signals := make(chan os.Signal, 2)
	ctx, drain, cancel := stopping(context.Background(), signals, io.Discard)
	defer cancel()
	signals <- syscall.SIGTERM
	select {
	case <-drain:
	case <-time.After(time.Minute):
		t.Fatal("the first signal did not drain")
	}
	if ctx.Err() != nil {
		t.Fatal("the first signal cancelled the builds")
	}
	signals <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(time.Minute):
		t.Fatal("the second signal did not cancel")
	}
}
