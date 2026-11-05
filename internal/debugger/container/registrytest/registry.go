// Package registrytest is an in-memory OCI distribution endpoint for tests
// that push and pull runtime images without a registry.
package registrytest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Registry serves one repository and asks for a bearer token from its own
// realm, which takes basic credentials.
type Registry struct {
	t         *testing.T
	repo      string
	user, pwd string

	mu        sync.Mutex
	Blobs     map[string][]byte
	Manifests map[string][]byte
	Uploads   int
	// BlobReads counts blob GETs.
	BlobReads int
	Server    *httptest.Server
}

// New starts a registry of repo for user and pwd, closed with the test.
func New(t *testing.T, repo, user, pwd string) *Registry {
	r := &Registry{t: t, repo: repo, user: user, pwd: pwd, Blobs: map[string][]byte{}, Manifests: map[string][]byte{}}
	r.Server = httptest.NewServer(r)
	t.Cleanup(r.Server.Close)
	return r
}

// Host is the registry's host:port, as an image reference names it.
func (r *Registry) Host() string { return strings.TrimPrefix(r.Server.URL, "http://") }

// Digest is the OCI digest of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

const mediaManifest = "application/vnd.oci.image.manifest.v1+json"

func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		if u, p, ok := req.BasicAuth(); !ok || u != r.user || p != r.pwd {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got := req.URL.Query().Get("scope"); got != "repository:"+r.repo+":push,pull" && got != "repository:"+r.repo+":pull" {
			r.t.Errorf("token scope = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "secret-token"})
		return
	}
	if req.Header.Get("Authorization") != "Bearer secret-token" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Server.URL+`/token",service="fake"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	prefix := "/v2/" + r.repo
	path, ok := strings.CutPrefix(req.URL.Path, prefix)
	if !ok {
		http.NotFound(w, req)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case req.Method == http.MethodHead && strings.HasPrefix(path, "/blobs/"):
		if _, ok := r.Blobs[strings.TrimPrefix(path, "/blobs/")]; ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case req.Method == http.MethodPost && path == "/blobs/uploads/":
		w.Header().Set("Location", prefix+"/blobs/uploads/session-1?_state=abc")
		w.WriteHeader(http.StatusAccepted)
	case req.Method == http.MethodPut && strings.HasPrefix(path, "/blobs/uploads/"):
		b, _ := io.ReadAll(req.Body)
		d := req.URL.Query().Get("digest")
		if Digest(b) != d || req.URL.Query().Get("_state") != "abc" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Blobs[d] = b
		r.Uploads++
		w.WriteHeader(http.StatusCreated)
	case req.Method == http.MethodPut && strings.HasPrefix(path, "/manifests/"):
		if req.Header.Get("Content-Type") != mediaManifest {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(req.Body)
		r.Manifests[strings.TrimPrefix(path, "/manifests/")] = b
		r.Manifests[Digest(b)] = b
		w.WriteHeader(http.StatusCreated)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/manifests/"):
		b, ok := r.Manifests[strings.TrimPrefix(path, "/manifests/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		var m struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(b, &m)
		w.Header().Set("Content-Type", m.MediaType)
		_, _ = w.Write(b)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/blobs/"):
		b, ok := r.Blobs[strings.TrimPrefix(path, "/blobs/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		r.BlobReads++
		_, _ = w.Write(b)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
