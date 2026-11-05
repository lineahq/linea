package container

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/container/registrytest"
)

// newFakeRegistry is a registry of repo for user and pwd.
func newFakeRegistry(t *testing.T, repo, user, pwd string) *registrytest.Registry {
	return registrytest.New(t, repo, user, pwd)
}

// TestAnExportedImageIsTheRuntimeImage: the pushed image is FROM scratch
// with the binary at /worker (mode 0755), user 65534, the /worker
// entrypoint and the labels, and Push names it by its manifest's digest.
// A second push uploads no blob again.
func TestAnExportedImageIsTheRuntimeImage(t *testing.T) {
	reg := newFakeRegistry(t, "team/runtimes", "ci", "pw")
	bin := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(bin, []byte("#!binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(map[string]any{"auths": map[string]any{reg.Host(): map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("ci:pw"))}}})
	e := &ImageExport{Auth: auth, PlainHTTP: []string{reg.Host()}, WorkDir: t.TempDir()}
	labels := map[string]string{LabelRuntime: "edge", LabelManifest: "dist: {}\n"}
	repo := reg.Host() + "/team/runtimes"

	ref, err := e.Push(context.Background(), repo, "edge", bin, labels)
	if err != nil {
		t.Fatal(err)
	}
	manifest := reg.Manifests["edge"]
	if ref != repo+"@"+digestOf(manifest) {
		t.Fatalf("ref = %q", ref)
	}
	var m struct {
		Config struct{ Digest string } `json:"config"`
		Layers []struct {
			MediaType, Digest string
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil || len(m.Layers) != 1 || m.Layers[0].MediaType != mediaLayer {
		t.Fatalf("manifest = %s, %v", manifest, err)
	}
	var cfg struct {
		OS     string `json:"os"`
		Config struct {
			User       string
			Entrypoint []string
			Labels     map[string]string
		} `json:"config"`
	}
	if err := json.Unmarshal(reg.Blobs[m.Config.Digest], &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.OS != "linux" || cfg.Config.User != "65534:65534" || len(cfg.Config.Entrypoint) != 1 || cfg.Config.Entrypoint[0] != "/worker" ||
		cfg.Config.Labels[LabelRuntime] != "edge" || cfg.Config.Labels[LabelManifest] != "dist: {}\n" {
		t.Fatalf("config = %+v", cfg)
	}
	gz, err := gzip.NewReader(bytes.NewReader(reg.Blobs[m.Layers[0].Digest]))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != "worker" || h.Mode != 0o755 || h.Typeflag != tar.TypeReg {
		t.Fatalf("layer entry = %+v, %v", h, err)
	}
	if body, _ := io.ReadAll(tr); string(body) != "#!binary" {
		t.Fatalf("layer body = %q", body)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("the layer has more than /worker: %v", err)
	}

	uploads := reg.Uploads
	again, err := e.Push(context.Background(), repo, "edge", bin, labels)
	if err != nil || again != ref || reg.Uploads != uploads {
		t.Fatalf("second push = %q, %v, %d uploads after %d", again, err, reg.Uploads, uploads)
	}
}

// TestAnExportFailsWithoutCredentials: a registry that asks for a token the
// builder cannot get fails the push, and the error carries no URL.
func TestAnExportFailsWithoutCredentials(t *testing.T) {
	reg := newFakeRegistry(t, "team/runtimes", "ci", "pw")
	bin := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &ImageExport{PlainHTTP: []string{reg.Host()}, WorkDir: t.TempDir()}
	_, err := e.Push(context.Background(), reg.Host()+"/team/runtimes", "edge", bin, nil)
	if err == nil || strings.Contains(err.Error(), reg.Server.URL) {
		t.Fatalf("Push = %v", err)
	}
}

func TestRepositoriesAndChallengesAreParsed(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"registry.example.com/team/runtimes", "registry.example.com/team/runtimes"},
		{"runtimes", "docker.io/library/runtimes"},
		{"registry.example.com/team/runtimes:tag", ""},
		{"team/Runtimes", ""},
	} {
		got, err := ParseRepository(tc.in)
		if (err != nil) != (tc.want == "") || got != tc.want {
			t.Errorf("ParseRepository(%q) = %q, %v", tc.in, got, err)
		}
	}
	scheme, params := parseChallenge(`Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repository:a:pull"`)
	if scheme != "Bearer" || params["realm"] != "https://auth.example.com/token" || params["service"] != "registry.example.com" || params["scope"] != "repository:a:pull" {
		t.Fatalf("challenge = %s %v", scheme, params)
	}
	if _, err := url.Parse(params["realm"]); err != nil {
		t.Fatal(err)
	}
}

func TestDockerCredentialsAreRead(t *testing.T) {
	noHelper := func(context.Context, string, string) (string, string, bool, error) {
		t.Fatal("a config without helpers ran one")
		return "", "", false, nil
	}
	cfg := []byte(`{"auths":{"https://index.docker.io/v1/":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("u:p:x")) + `"},"ghcr.io":{"username":"a","password":"b"},"reg.example":{"identitytoken":"tok"}}}`)
	for _, tc := range []struct{ host, user, pwd string }{
		{"docker.io", "u", "p:x"},
		{"ghcr.io", "a", "b"},
		{"reg.example", "<token>", "tok"},
		{"other.example", "", ""},
	} {
		u, p, err := dockerCredentials(context.Background(), cfg, tc.host, noHelper)
		if err != nil || u != tc.user || p != tc.pwd {
			t.Errorf("dockerCredentials(%s) = %q, %q, %v", tc.host, u, p, err)
		}
	}
	if _, _, err := dockerCredentials(context.Background(), []byte("not json"), "x", noHelper); err == nil {
		t.Error("a broken config was read")
	}
}

// TestCredentialHelpersComeFirst: a host's credHelpers entry wins over
// credsStore, which wins over auths; a helper without credentials falls
// back to auths; a helper's error fails; a helper name that is a path is
// refused. Docker Hub's helper is asked for its index URL.
func TestCredentialHelpersComeFirst(t *testing.T) {
	var asked []string
	helper := func(_ context.Context, name, server string) (string, string, bool, error) {
		asked = append(asked, name+" "+server)
		switch name {
		case "ecr-login":
			return "AWS", "ecr-secret", true, nil
		case "desktop":
			if server == "https://index.docker.io/v1/" {
				return "<token>", "hub-token", true, nil
			}
			return "", "", false, nil
		}
		return "", "", false, errors.New("helper broke")
	}
	cfg := []byte(`{"credsStore":"desktop","credHelpers":{"123.dkr.ecr.eu-west-1.amazonaws.com":"ecr-login","broken.example":"broken"},
		"auths":{"ghcr.io":{"username":"a","password":"b"},"123.dkr.ecr.eu-west-1.amazonaws.com":{"username":"x","password":"y"}}}`)
	for _, tc := range []struct{ host, user, pwd, asked string }{
		{"123.dkr.ecr.eu-west-1.amazonaws.com", "AWS", "ecr-secret", "ecr-login 123.dkr.ecr.eu-west-1.amazonaws.com"},
		{"docker.io", "<token>", "hub-token", "desktop https://index.docker.io/v1/"},
		{"ghcr.io", "a", "b", "desktop ghcr.io"},
		{"other.example", "", "", "desktop other.example"},
	} {
		asked = nil
		u, p, err := dockerCredentials(context.Background(), cfg, tc.host, helper)
		if err != nil || u != tc.user || p != tc.pwd || len(asked) != 1 || asked[0] != tc.asked {
			t.Errorf("%s = %q, %q, %v; asked %v", tc.host, u, p, err, asked)
		}
	}
	if _, _, err := dockerCredentials(context.Background(), cfg, "broken.example", helper); err == nil {
		t.Error("a failing helper gave credentials")
	}
	if _, _, err := dockerCredentials(context.Background(), []byte(`{"credsStore":"../bin/sh"}`), "x", helper); err == nil {
		t.Error("a helper path was run")
	}
}

// fakeHelperMode makes the test binary a credential helper:
// docker-credential-fake answers from its environment as a real helper does.
const fakeHelperMode = "CONTAINER_TEST_CREDENTIAL_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeHelperMode) {
	case "":
		os.Exit(m.Run())
	case "found":
		server, _ := io.ReadAll(os.Stdin)
		if len(os.Args) != 2 || os.Args[1] != "get" {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"ServerURL": string(server), "Username": "helper-user", "Secret": "helper-secret:" + string(server)})
	case "missing":
		os.Stdout.WriteString("credentials not found in native keychain\n")
		os.Exit(1)
	default:
		os.Stderr.WriteString("keychain locked\nsecond line\n")
		os.Exit(1)
	}
	os.Exit(0)
}

// TestACredentialHelperProgramIsRun runs the test binary as
// docker-credential-fake on PATH, the way Push finds a helper.
func TestACredentialHelperProgramIsRun(t *testing.T) {
	installFakeHelper(t)
	ctx := context.Background()

	t.Setenv(fakeHelperMode, "found")
	u, p, found, err := runCredentialHelper(ctx, "fake", "reg.example:5000")
	if err != nil || !found || u != "helper-user" || p != "helper-secret:reg.example:5000" {
		t.Fatalf("found = %q, %q, %v, %v", u, p, found, err)
	}
	t.Setenv(fakeHelperMode, "missing")
	if _, _, found, err := runCredentialHelper(ctx, "fake", "reg.example"); err != nil || found {
		t.Fatalf("missing = %v, %v", found, err)
	}
	t.Setenv(fakeHelperMode, "fail")
	if _, _, _, err := runCredentialHelper(ctx, "fake", "reg.example"); err == nil || !strings.Contains(err.Error(), "keychain locked") || strings.Contains(err.Error(), "second line") {
		t.Fatalf("failing = %v", err)
	}
	if _, _, _, err := runCredentialHelper(ctx, "absent", "reg.example"); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("absent = %v", err)
	}
}

// installFakeHelper puts the test binary on PATH as docker-credential-fake.
func installFakeHelper(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "docker-credential-fake"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
