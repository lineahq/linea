package container

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// registryImage is the registry the export is gated against.
const registryImage = "registry:2.8.3"

// startRegistry runs registryImage on a loopback port of this machine, with
// dir mounted read-only at /auth and env set, and returns its host:port.
// Nothing leaves the machine.
func startRegistry(t *testing.T, dir string, env ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	args := []string{"run", "-d", "-p", "127.0.0.1::5000", "-v", dir + ":/auth:ro"}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	// Only standard output: a first run reports its pull on standard error.
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "docker", append(args, registryImage)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker run %s: %v: %s", registryImage, err, stderr.String())
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	out, err = exec.CommandContext(ctx, "docker", "port", id, "5000/tcp").Output()
	if err != nil {
		logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
		t.Fatalf("docker port: %v\n%s", err, logs)
	}
	host := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	// The registry answers /v2/ once it listens; with auth on, 401.
	for deadline := time.Now().Add(time.Minute); ; {
		resp, err := http.Get("http://" + host + "/v2/")
		if err == nil {
			resp.Body.Close()
			return host
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Fatalf("the registry did not start: %v\n%s", err, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func workerBinary(t *testing.T, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// manifestDigest asks the registry for ref's manifest with authorization
// and returns the digest it names.
func manifestDigest(t *testing.T, host, ref, authorization string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodHead, "http://"+host+"/v2/team/runtimes/manifests/"+ref, nil)
	req.Header.Set("Accept", mediaManifest)
	req.Header.Set("Authorization", authorization)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest %s: %s", ref, resp.Status)
	}
	return resp.Header.Get("Docker-Content-Digest")
}

// get reads path from the registry with authorization and checks that the
// body has digest.
func get(t *testing.T, host, path, digest, accept, authorization string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+host+"/v2/team/runtimes/"+path, nil)
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", authorization)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s, %v", path, resp.Status, err)
	}
	if got := fmt.Sprintf("sha256:%x", sha256.Sum256(body)); got != digest {
		t.Fatalf("GET %s has digest %s", path, got)
	}
	return body
}

// pullConfig pulls the manifest digest names and its image configuration,
// as a client pulls them, and checks the runtime image's configuration.
func pullConfig(t *testing.T, host, digest, authorization string) {
	t.Helper()
	var m struct {
		Config struct{ Digest string }   `json:"config"`
		Layers []struct{ Digest string } `json:"layers"`
	}
	if err := json.Unmarshal(get(t, host, "manifests/"+digest, digest, mediaManifest, authorization), &m); err != nil || len(m.Layers) != 1 {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	var cfg struct {
		Config struct {
			User       string
			Entrypoint []string
			Labels     map[string]string
		} `json:"config"`
	}
	if err := json.Unmarshal(get(t, host, "blobs/"+m.Config.Digest, m.Config.Digest, "*/*", authorization), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Config.User != "65534:65534" || !slices.Equal(cfg.Config.Entrypoint, []string{"/worker"}) || cfg.Config.Labels[LabelRuntime] != "edge" {
		t.Fatalf("config = %+v", cfg.Config)
	}
	get(t, host, "blobs/"+m.Layers[0].Digest, m.Layers[0].Digest, "*/*", authorization)
}

// TestAnExportPushesToARegistryWithBasicAuth pushes to registryImage with
// htpasswd auth: the push with the user's credentials succeeds and a client
// pulls the image with them, a push without or with wrong credentials
// fails, and a second export moves the tag while the first image stays
// pullable by digest, untagged.
func TestAnExportPushesToARegistryWithBasicAuth(t *testing.T) {
	requireDocker(t)
	dir := t.TempDir()
	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "htpasswd"), append([]byte("ci:"), append(hash, '\n')...), 0o644); err != nil {
		t.Fatal(err)
	}
	host := startRegistry(t, dir, "REGISTRY_AUTH=htpasswd", "REGISTRY_AUTH_HTPASSWD_REALM=linea", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd")
	repo := host + "/team/runtimes"
	authFor := func(user, pwd string) []byte {
		raw, _ := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + pwd))}}})
		return raw
	}
	ctx := context.Background()
	labels := map[string]string{LabelRuntime: "edge"}

	for name, auth := range map[string][]byte{"anonymous": nil, "wrong password": authFor("ci", "nope")} {
		e := &ImageExport{Auth: auth, PlainHTTP: []string{host}, WorkDir: t.TempDir()}
		if _, err := e.Push(ctx, repo, "edge", workerBinary(t, "x"), labels); err == nil || strings.Contains(err.Error(), host) {
			t.Errorf("%s push = %v", name, err)
		}
	}

	e := &ImageExport{Auth: authFor("ci", "pw"), PlainHTTP: []string{host}, WorkDir: t.TempDir()}
	first, err := e.Push(ctx, repo, "edge", workerBinary(t, "first"), labels)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, repo+"@sha256:") {
		t.Fatalf("ref = %q", first)
	}

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("ci:pw"))
	pullConfig(t, host, strings.TrimPrefix(first, repo+"@"), basic)

	second, err := e.Push(ctx, repo, "edge", workerBinary(t, "second"), labels)
	if err != nil || second == first {
		t.Fatalf("second push = %q, %v", second, err)
	}
	if tagged := manifestDigest(t, host, "edge", basic); repo+"@"+tagged != second {
		t.Fatalf("edge names %s after the second export", tagged)
	}
	manifestDigest(t, host, strings.TrimPrefix(first, repo+"@"), basic)

	// A CI runner reads the exported runtime back without Docker: the tag is
	// the second binary, the first stays pullable by digest, and anonymous
	// reads are refused.
	p := &ImagePull{Dir: t.TempDir(), Auth: authFor("ci", "pw"), Platform: "linux/" + runtime.GOARCH}
	for ref, body := range map[string]string{repo + ":edge": "second", first: "first"} {
		img, err := p.Pull(ctx, ref)
		if err != nil {
			t.Fatalf("pull %s: %v", ref, err)
		}
		if b, _ := os.ReadFile(img.Path); string(b) != body || img.Labels[LabelRuntime] != "edge" {
			t.Fatalf("pull %s = %q, %v", ref, b, img.Labels)
		}
	}
	anonymous := &ImagePull{Dir: t.TempDir(), Platform: p.Platform}
	if _, err := anonymous.Pull(ctx, repo+":edge"); err == nil {
		t.Fatal("an anonymous pull succeeded")
	}
}

// tokenServer is a Docker registry token server: it checks the basic
// credentials it is given and signs ES256 tokens whose x5c header carries
// its self-signed certificate, the root the registry trusts.
type tokenServer struct {
	key      *ecdsa.PrivateKey
	cert     []byte
	user     string
	password string

	mu     sync.Mutex
	scopes []string
}

const (
	tokenIssuer  = "linea-test-issuer"
	tokenService = "linea-test-registry"
)

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: tokenIssuer},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	cert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tokenServer{key: key, cert: cert}
}

func (s *tokenServer) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.cert})
}

// sign makes a token that grants actions on repository name.
func (s *tokenServer) sign(t *testing.T, name string, actions []string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]any{"typ": "JWT", "alg": "ES256", "x5c": []string{base64.StdEncoding.EncodeToString(s.cert)}})
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{
		"iss": tokenIssuer, "sub": "ci", "aud": tokenService, "jti": fmt.Sprint(now.UnixNano()),
		"iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"access": []map[string]any{{"type": "repository", "name": name, "actions": actions}},
	})
	signed := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	r, sig, err := ecdsa.Sign(rand.Reader, s.key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	sig.FillBytes(raw[32:])
	return signed + "." + enc.EncodeToString(raw)
}

func (s *tokenServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		user, pwd, ok := req.BasicAuth()
		s.mu.Lock()
		want, wantPwd := s.user, s.password
		s.scopes = append(s.scopes, req.URL.Query().Get("scope"))
		s.mu.Unlock()
		if !ok || user != want || pwd != wantPwd || req.URL.Query().Get("service") != tokenService {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		scope := strings.Split(req.URL.Query().Get("scope"), ":")
		if len(scope) != 3 || scope[0] != "repository" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": s.sign(t, scope[1], strings.Split(scope[2], ","))})
	}
}

// TestAnExportPushesToARegistryWithTokenAuth pushes to registryImage with
// token auth, the scheme of Docker Hub, GHCR, Harbor and GitLab: the
// builder takes the credentials from a credential helper (the test binary
// as docker-credential-fake), trades them at the realm for a push token
// and pushes; the tag names the pushed manifest. Without credentials the
// token server refuses and the push fails without naming a URL.
func TestAnExportPushesToARegistryWithTokenAuth(t *testing.T) {
	requireDocker(t)
	installFakeHelper(t)
	ts := newTokenServer(t)
	srv := httptest.NewServer(ts.handler(t))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "root.pem"), ts.pem(), 0o644); err != nil {
		t.Fatal(err)
	}
	// The realm is only handed to clients; the registry never calls it.
	host := startRegistry(t, dir, "REGISTRY_AUTH=token", "REGISTRY_AUTH_TOKEN_REALM="+srv.URL+"/token",
		"REGISTRY_AUTH_TOKEN_SERVICE="+tokenService, "REGISTRY_AUTH_TOKEN_ISSUER="+tokenIssuer,
		"REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=/auth/root.pem")
	ts.mu.Lock()
	ts.user, ts.password = "helper-user", "helper-secret:"+host
	ts.mu.Unlock()
	repo := host + "/team/runtimes"
	e := &ImageExport{Auth: []byte(`{"credsStore":"fake"}`), PlainHTTP: []string{host}, WorkDir: t.TempDir()}
	ctx := context.Background()

	t.Setenv(fakeHelperMode, "found")
	ref, err := e.Push(ctx, repo, "edge", workerBinary(t, "token"), map[string]string{LabelRuntime: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	pull := "Bearer " + ts.sign(t, "team/runtimes", []string{"pull"})
	if tagged := manifestDigest(t, host, "edge", pull); repo+"@"+tagged != ref {
		t.Fatalf("edge names %s, the push returned %s", tagged, ref)
	}
	pullConfig(t, host, strings.TrimPrefix(ref, repo+"@"), pull)
	ts.mu.Lock()
	scopes := slices.Compact(slices.Clone(ts.scopes))
	ts.mu.Unlock()
	if len(scopes) != 1 || scopes[0] != "repository:team/runtimes:push,pull" {
		t.Fatalf("token scopes = %v", scopes)
	}
	// A pull with the helper's credentials asks for pull scope only.
	p := &ImagePull{Dir: t.TempDir(), Auth: []byte(`{"credsStore":"fake"}`), Platform: "linux/" + runtime.GOARCH}
	if img, err := p.Pull(ctx, repo+":edge"); err != nil || repo+"@"+img.Digest != ref {
		t.Fatalf("pull = %+v, %v", img, err)
	}
	ts.mu.Lock()
	last := ts.scopes[len(ts.scopes)-1]
	ts.mu.Unlock()
	if last != "repository:team/runtimes:pull" {
		t.Fatalf("pull token scope = %q", last)
	}

	t.Setenv(fakeHelperMode, "missing")
	if _, err := e.Push(ctx, repo, "edge", workerBinary(t, "refused"), nil); err == nil || strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), host) {
		t.Fatalf("push without credentials = %v", err)
	}
}
