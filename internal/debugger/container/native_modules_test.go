package container

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// privateProxy serves the module example.com/private v1.0.0 over TLS to
// requests that present user u, password p, as an internal module proxy
// does, and counts the requests it refused.
func privateProxy(t *testing.T) (srv *httptest.Server, refused *int) {
	t.Helper()
	const mod = "module example.com/private\n\ngo 1.22\n"
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	for name, body := range map[string]string{"go.mod": mod, "private.go": "package private\n"} {
		w, err := zw.Create("example.com/private@v1.0.0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"/example.com/private/@v/list":        []byte("v1.0.0\n"),
		"/example.com/private/@v/v1.0.0.info": []byte(`{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`),
		"/example.com/private/@v/v1.0.0.mod":  []byte(mod),
		"/example.com/private/@v/v1.0.0.zip":  zipped.Bytes(),
	}
	refused = new(int)
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "u" || pass != "p" {
			*refused++
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, refused
}

// TestNativeModuleDownloadsUseTheBuildersCredentialsAndCA: a build reaches
// a private module proxy with the credentials (NETRC) and certificate
// authority (SSL_CERT_FILE) of the builder's environment, and is refused
// without the credentials. It runs the real go command; Go reads
// SSL_CERT_FILE on Linux only.
func TestNativeModuleDownloadsUseTheBuildersCredentialsAndCA(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the go command reads SSL_CERT_FILE on Linux only")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	srv, refused := privateProxy(t)
	secrets := t.TempDir()
	ca := filepath.Join(secrets, "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	netrc := filepath.Join(secrets, "netrc")
	// The go command matches a netrc machine against the URL's host with
	// its port.
	if err := os.WriteFile(netrc, []byte("machine "+srv.Listener.Addr().String()+" login u password p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOPROXY", srv.URL)
	t.Setenv("GONOSUMDB", "example.com")
	t.Setenv("SSL_CERT_FILE", ca)
	t.Setenv("GOAUTH", "netrc")

	n := &Native{WorkDir: t.TempDir()}
	d, err := n.dirs()
	if err != nil {
		t.Fatal(err)
	}
	module := t.TempDir()
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module t\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	download := func() (string, error) {
		cmd := n.goCommand(context.Background(), "mod", "download", "example.com/private@v1.0.0")
		cmd.Dir, cmd.Env = module, n.env(d, "", false, "linux", runtime.GOARCH)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// The module cache is read-only; go clean removes it.
	t.Cleanup(func() {
		cmd := n.goCommand(context.Background(), "clean", "-modcache")
		cmd.Dir, cmd.Env = module, n.env(d, "", true, "linux", runtime.GOARCH)
		_ = cmd.Run()
	})

	t.Setenv("NETRC", filepath.Join(secrets, "none"))
	if out, err := download(); err == nil || *refused == 0 {
		t.Fatalf("a download without credentials = %v, %d refused\n%s", err, *refused, out)
	}
	t.Setenv("NETRC", netrc)
	t.Setenv("SSL_CERT_FILE", filepath.Join(secrets, "none"))
	if out, err := download(); err == nil || !strings.Contains(out, "certificate") {
		t.Fatalf("a download without the CA = %v\n%s", err, out)
	}
	t.Setenv("SSL_CERT_FILE", ca)
	if out, err := download(); err != nil {
		t.Fatalf("a download with the builder's credentials and CA = %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(d.modCache, "example.com", "private@v1.0.0", "private.go")); err != nil {
		t.Fatalf("the module is not in the builder's cache: %v", err)
	}
}
