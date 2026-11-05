package container

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/distribution/reference"
)

// The media types of an exported runtime image.
const (
	mediaManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaConfig   = "application/vnd.oci.image.config.v1+json"
	mediaLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// ImageExport pushes the image of a natively built runtime to a registry
// without a Docker daemon: the image Docker.Build packages,
// FROM scratch with the binary at /worker, user 65534 and the runtime's
// labels, so `runtime register` and dedicated workers take it as they take
// a CLI-built one.
type ImageExport struct {
	// Client does the registry requests; nil means http.DefaultClient.
	Client *http.Client
	// Auth is the content of a Docker config.json, read as Docker reads
	// it: a credential helper the host names in "credHelpers", else the
	// "credsStore" helper, else "auths" by registry host, with "auth"
	// (base64 user:password) or "username" and "password", or an
	// "identitytoken". A helper is the program docker-credential-<name> on
	// PATH. Empty pushes anonymously.
	Auth []byte
	// CredentialHelper asks a credential helper for server's credentials;
	// nil runs docker-credential-<name> get. found is false when the
	// helper has none.
	CredentialHelper func(ctx context.Context, name, server string) (user, secret string, found bool, err error)
	// PlainHTTP names the registry hosts reached over http instead of
	// https, such as a registry inside a test cluster.
	PlainHTTP []string
	// WorkDir holds the layer while it is pushed; empty means the system
	// temporary directory.
	WorkDir string
}

// ParseRepository checks an image repository without tag or digest, such as
// registry.example.com/team/runtimes, and returns it in full form.
func ParseRepository(repo string) (string, error) {
	named, err := reference.ParseNormalizedNamed(repo)
	if err != nil {
		return "", fmt.Errorf("image repository %q: %w", repo, err)
	}
	if !reference.IsNameOnly(named) {
		return "", fmt.Errorf("image repository %q: name a repository without a tag or digest", repo)
	}
	return named.Name(), nil
}

// Push pushes the runtime binary at path as repo:tag and returns the image
// reference by digest, repo@sha256:....
func (e *ImageExport) Push(ctx context.Context, repo, tag, path string, labels map[string]string) (string, error) {
	named, err := reference.ParseNormalizedNamed(repo)
	if err != nil || !reference.IsNameOnly(named) {
		return "", fmt.Errorf("image repository %q is not a repository name", repo)
	}
	if _, err := reference.WithTag(named, tag); err != nil {
		return "", fmt.Errorf("image tag %q: %w", tag, err)
	}
	host, name := reference.Domain(named), reference.Path(named)
	r := &registryClient{client: e.Client, host: host, name: name, scheme: "https", scope: "push,pull"}
	if r.client == nil {
		r.client = http.DefaultClient
	}
	if slices.Contains(e.PlainHTTP, host) {
		r.scheme = "http"
	}
	if host == "docker.io" {
		r.host = "registry-1.docker.io"
	}
	helper := e.CredentialHelper
	if helper == nil {
		helper = runCredentialHelper
	}
	if r.user, r.password, err = dockerCredentials(ctx, e.Auth, host, helper); err != nil {
		return "", err
	}

	layer, err := e.layer(path)
	if err != nil {
		return "", err
	}
	defer os.Remove(layer.path)
	config, err := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH, // native builds target the host
		"os":           "linux",
		"config": map[string]any{
			"User":       "65534:65534",
			"Entrypoint": []string{"/worker"},
			"Labels":     labels,
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{layer.diffID}},
	})
	if err != nil {
		return "", err
	}
	configDigest := digestOf(config)
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaManifest,
		"config":        map[string]any{"mediaType": mediaConfig, "digest": configDigest, "size": len(config)},
		"layers":        []any{map[string]any{"mediaType": mediaLayer, "digest": layer.digest, "size": layer.size}},
	})
	if err != nil {
		return "", err
	}

	if err := r.pushBlob(ctx, layer.digest, layer.size, func() (io.ReadCloser, error) { return os.Open(layer.path) }); err != nil {
		return "", fmt.Errorf("pushing the layer: %w", err)
	}
	if err := r.pushBlob(ctx, configDigest, int64(len(config)), func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(config)), nil }); err != nil {
		return "", fmt.Errorf("pushing the image config: %w", err)
	}
	resp, err := r.do(ctx, http.MethodPut, r.url("/manifests/"+tag), func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(manifest)), nil },
		int64(len(manifest)), mediaManifest)
	if err != nil {
		return "", fmt.Errorf("pushing the manifest: %w", err)
	}
	drain(resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pushing the manifest: the registry answered %s", resp.Status)
	}
	return named.Name() + "@" + digestOf(manifest), nil
}

// builtLayer is the image's one layer, gzipped in a file.
type builtLayer struct {
	path           string
	digest, diffID string
	size           int64
}

// layer writes the layer: /worker, mode 0755, owned by root, with the zero
// time, as Docker.Build's COPY does.
func (e *ImageExport) layer(path string) (*builtLayer, error) {
	bin, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer bin.Close()
	st, err := bin.Stat()
	if err != nil {
		return nil, err
	}
	out, err := os.CreateTemp(e.WorkDir, "layer-*.tar.gz")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = out.Close()
			_ = os.Remove(out.Name())
		}
	}()
	compressed, uncompressed := sha256.New(), sha256.New()
	counted := &countingWriter{w: io.MultiWriter(out, compressed)}
	gz := gzip.NewWriter(counted)
	tw := tar.NewWriter(io.MultiWriter(gz, uncompressed))
	if err := tw.WriteHeader(&tar.Header{Name: "worker", Mode: 0o755, Size: st.Size(), ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return nil, err
	}
	if _, err := io.Copy(tw, bin); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	ok = true
	return &builtLayer{path: out.Name(), digest: hexDigest(compressed), diffID: hexDigest(uncompressed), size: counted.n}, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func hexDigest(h hash.Hash) string { return "sha256:" + hex.EncodeToString(h.Sum(nil)) }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// dockerCredentials finds the registry's credentials in a Docker
// config.json: from the host's credential helper, else from the default
// helper (credsStore), else from auths. A helper that has none falls back
// to auths. An identity token is returned as the password of the user
// "<token>", as Docker sends it to a token server.
func dockerCredentials(ctx context.Context, config []byte, host string, helper credentialHelper) (string, string, error) {
	if len(bytes.TrimSpace(config)) == 0 {
		return "", "", nil
	}
	var c struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(config, &c); err != nil {
		return "", "", errors.New("the registry credentials are not a Docker config.json")
	}
	keys := []string{host, "https://" + host, "http://" + host}
	server := host
	if host == "docker.io" {
		keys = append(keys, "https://index.docker.io/v1/", "index.docker.io", "registry-1.docker.io")
		server = "https://index.docker.io/v1/"
	}
	name := c.CredsStore
	for _, k := range keys {
		if h := c.CredHelpers[k]; h != "" {
			name = h
			break
		}
	}
	if name != "" {
		if !helperName.MatchString(name) {
			return "", "", fmt.Errorf("the credential helper of %s is not a helper name", host)
		}
		user, secret, found, err := helper(ctx, name, server)
		if err != nil {
			return "", "", err
		}
		if found {
			return user, secret, nil
		}
	}
	for _, k := range keys {
		a, found := c.Auths[k]
		if !found {
			continue
		}
		switch {
		case a.IdentityToken != "":
			return "<token>", a.IdentityToken, nil
		case a.Auth != "":
			raw, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return "", "", fmt.Errorf("the registry credentials of %s are not base64", host)
			}
			user, password, ok := strings.Cut(string(raw), ":")
			if !ok {
				return "", "", fmt.Errorf("the registry credentials of %s are not user:password", host)
			}
			return user, password, nil
		case a.Username != "" || a.Password != "":
			return a.Username, a.Password, nil
		}
	}
	return "", "", nil
}

// credentialHelper is ImageExport.CredentialHelper.
type credentialHelper = func(ctx context.Context, name, server string) (user, secret string, found bool, err error)

// helperName is a credential helper's name: the suffix of
// docker-credential-<name>, never a path.
var helperName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// credentialHelperTimeout bounds one call of a credential helper.
const credentialHelperTimeout = 30 * time.Second

// runCredentialHelper runs docker-credential-<name> get with server on its
// input, as Docker does, and reads {"Username","Secret"} from its output. A
// helper that answers "credentials not found" has none. Its errors name the
// helper and its first line of standard error, never its output.
func runCredentialHelper(ctx context.Context, name, server string) (string, string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, credentialHelperTimeout)
	defer cancel()
	program := "docker-credential-" + name
	cmd := exec.CommandContext(ctx, program, "get")
	cmd.Stdin = strings.NewReader(server)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limitedBuffer{b: &stdout, n: 64 << 10}, &limitedBuffer{b: &stderr, n: 4 << 10}
	if err := cmd.Run(); err != nil {
		if strings.Contains(stdout.String()+stderr.String(), "credentials not found") {
			return "", "", false, nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", "", false, fmt.Errorf("the credential helper %s is not on PATH", program)
		}
		detail, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if len(detail) > 200 {
			detail = detail[:200]
		}
		if detail != "" {
			return "", "", false, fmt.Errorf("the credential helper %s failed: %s", program, detail)
		}
		return "", "", false, fmt.Errorf("the credential helper %s failed: %v", program, err)
	}
	var out struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return "", "", false, fmt.Errorf("the credential helper %s answered no credentials", program)
	}
	return out.Username, out.Secret, out.Username != "" || out.Secret != "", nil
}

// limitedBuffer keeps the first n bytes written to it and discards the rest.
type limitedBuffer struct {
	b *bytes.Buffer
	n int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// registryClient speaks the OCI distribution API to one repository, with
// basic or bearer token authentication on a 401 challenge. Pushes and
// pulls share it (ImageExport, ImagePull).
type registryClient struct {
	client         *http.Client
	scheme, host   string
	name           string
	user, password string
	// scope is the access a token is asked for: "pull" or "push,pull".
	scope string
	// authorization is the header that answered the last challenge.
	authorization string
}

func (r *registryClient) url(path string) string {
	return r.scheme + "://" + r.host + "/v2/" + r.name + path
}

// pushBlob uploads a blob unless the repository has it, in one PUT.
func (r *registryClient) pushBlob(ctx context.Context, digest string, size int64, open func() (io.ReadCloser, error)) error {
	resp, err := r.do(ctx, http.MethodHead, r.url("/blobs/"+digest), nil, 0, "")
	if err != nil {
		return err
	}
	drain(resp)
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	resp, err = r.do(ctx, http.MethodPost, r.url("/blobs/uploads/"), nil, 0, "")
	if err != nil {
		return err
	}
	drain(resp)
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("starting the upload: the registry answered %s", resp.Status)
	}
	loc, err := resp.Location()
	if err != nil {
		return fmt.Errorf("starting the upload: the registry gave no upload location")
	}
	q := loc.Query()
	q.Set("digest", digest)
	loc.RawQuery = q.Encode()
	resp, err = r.do(ctx, http.MethodPut, loc.String(), open, size, "application/octet-stream")
	if err != nil {
		return err
	}
	drain(resp)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("uploading: the registry answered %s", resp.Status)
	}
	return nil
}

// do sends a request, answering one authentication challenge. body opens
// the request body afresh for each try.
func (r *registryClient) do(ctx context.Context, method, target string, body func() (io.ReadCloser, error), size int64, contentType string) (*http.Response, error) {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return r.send(ctx, method, target, body, size, header)
}

// send is do with the request's headers.
func (r *registryClient) send(ctx context.Context, method, target string, body func() (io.ReadCloser, error), size int64, header http.Header) (*http.Response, error) {
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			return nil, unwrapURLError(err)
		}
		if body != nil {
			rc, err := body()
			if err != nil {
				return nil, err
			}
			req.Body, req.ContentLength = rc, size
		}
		for k, v := range header {
			req.Header[k] = v
		}
		if r.authorization != "" {
			req.Header.Set("Authorization", r.authorization)
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, unwrapURLError(err)
		}
		if resp.StatusCode != http.StatusUnauthorized || try > 0 {
			return resp, nil
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		drain(resp)
		if err := r.authorize(ctx, challenge); err != nil {
			return nil, err
		}
	}
}

// authorize answers a challenge: basic with the credentials, or a bearer
// token from the realm the challenge names, for push and pull.
func (r *registryClient) authorize(ctx context.Context, challenge string) error {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if r.user == "" && r.password == "" {
			return errors.New("the registry asks for credentials and none are configured for it")
		}
		r.authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(r.user+":"+r.password))
		return nil
	case "bearer":
	default:
		return fmt.Errorf("the registry asks for %q authentication, which is not supported", scheme)
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || (realm.Scheme != "https" && realm.Scheme != "http") {
		return errors.New("the registry's token realm is not a URL")
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+r.name+":"+r.scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return unwrapURLError(err)
	}
	if r.user != "" || r.password != "" {
		req.SetBasicAuth(r.user, r.password)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("the registry's token server: %w", unwrapURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the registry's token server answered %s", resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return errors.New("the registry's token server answered no token")
	}
	token := t.Token
	if token == "" {
		token = t.AccessToken
	}
	if token == "" {
		return errors.New("the registry's token server answered no token")
	}
	r.authorization = "Bearer " + token
	return nil
}

// parseChallenge splits a WWW-Authenticate value into its scheme and its
// key="value" parameters.
func parseChallenge(v string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(v), " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var val string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				break
			}
			val, rest = after[1:1+end], after[2+end:]
		} else {
			val, rest, _ = strings.Cut(after, ",")
		}
		params[key] = val
		rest = strings.TrimLeft(strings.TrimSpace(rest), ",")
		rest = strings.TrimSpace(rest)
	}
	return scheme, params
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// unwrapURLError drops the URL a net/url error carries: an upload location
// may hold a session credential.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
