package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// A lab is the smallest team server that runs work: a server config with
// trusted-header sign-in as the Docker Compose stack uses, memory storage in
// place of PostgreSQL (TestMain), one registered runtime and one connected
// in-process worker of it. serve has no other mode.

// labOption composes the lab's server config.
type labOption func(*labSpec)

type labSpec struct {
	runLimit string
	extra    []string
}

// withRunLimit sets store.run_limit, such as "65MiB".
func withRunLimit(size string) labOption { return func(s *labSpec) { s.runLimit = size } }

// withRuntimes registers more runtime names on the lab's one image: the same
// worker identity serves them all, as one image tagged twice would.
func withRuntimes(names ...string) labOption { return func(s *labSpec) { s.extra = names } }

// lab is a running lab server, the API token of its administrator and the
// registration token its workers present.
type lab struct {
	URL, Token, WorkerToken string
}

// startLab serves a lab until the test ends and sets $LINEA_TOKEN to an
// administrator's API token, so --server commands reach it.
func startLab(t *testing.T, opts ...labOption) lab {
	t.Helper()
	var spec labSpec
	for _, o := range opts {
		o(&spec)
	}
	const image = "registry.example/linea-runtime:lab"
	dir := t.TempDir()
	writeTeamSecrets(t, dir)
	registration := strings.Repeat("w", minWorkerToken)
	if err := os.WriteFile(filepath.Join(dir, "registration"), []byte(registration+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage := "storage_limit: 256MiB"
	if spec.runLimit != "" {
		storage += "\n  run_limit: " + spec.runLimit
	}
	replace := []string{"role: viewer", "role: admin", "storage_limit: 64MiB", storage}
	text := strings.NewReplacer(replace...).Replace(teamYAML()) + "worker_token_file: registration\n"
	server := startTeam(t, writeTeamIn(t, dir, "team", text))

	resp := teamRequests(t, server)(http.MethodPost, "/api/v1/tokens", api.TokenRequest{Label: "lab"},
		map[string]string{"X-Forwarded-User": "alice", "X-Forwarded-Groups": "platform"})
	var created api.CreatedToken
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token: %d, %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	t.Setenv(serverTokenEnv, created.Secret)

	// Registering reads the image's hello through Docker's store; the lab's
	// stands for this process's own worker. Local commands never see it.
	saved := dockerRuntimes
	dockerRuntimes = func() platform.RuntimeStore { return helloRuntimes{t: t, image: image} }
	code, out, stderr := runCLI(t, "runtime", "register", image, "--name", "lab", "--server", server)
	for _, name := range spec.extra {
		if c, o, e := runCLI(t, "runtime", "register", image, "--name", name, "--server", server); c != exitOK {
			dockerRuntimes = saved
			t.Fatalf("register %s: exit %d, %s%s", name, c, o, e)
		}
	}
	dockerRuntimes = saved
	if code != exitOK {
		t.Fatalf("register: exit %d, %s%s", code, out, stderr)
	}
	// Requests that name no runtime run on the lab's.
	if code, out, stderr := runCLI(t, "runtime", "default", "lab", "--server", server); code != exitOK {
		t.Fatalf("runtime default: exit %d, %s%s", code, out, stderr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	connected := make(chan error, 1)
	go func() {
		connected <- worker.Connect(ctx, worker.ConnectOptions{URL: server, Token: registration, Name: "lab-worker", Capacity: 4})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-connected; err != nil {
			t.Errorf("Connect = %v", err)
		}
	})
	// The worker's hello confirms the registration; a request that needs a
	// connected worker (an inspection) is answered from then on. Being of
	// this release, it also registers the built-in runtime.
	deadline := time.Now().Add(30 * time.Second)
	for tick := time.NewTicker(10 * time.Millisecond); ; <-tick.C {
		code, out, _ := runCLI(t, "runtime", "list", "--format", "json", "--server", server)
		var list []api.Runtime
		if code == exitOK && json.Unmarshal([]byte(out), &list) == nil && confirmed(list, 2+len(spec.extra)) {
			tick.Stop()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lab's worker did not connect")
		}
	}
	return lab{URL: server, Token: created.Secret, WorkerToken: registration}
}

// startServe is startLab for tests that need only the server's URL.
func startServe(t *testing.T, opts ...labOption) string {
	t.Helper()
	return startLab(t, opts...).URL
}

// labDo sends a request with $LINEA_TOKEN, the lab administrator's API token.
func labDo(t *testing.T, method, url, contentType string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv(serverTokenEnv))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// confirmed reports whether the list holds want catalog runtimes, all of them
// confirmed by a connected worker's hello.
func confirmed(list []api.Runtime, want int) bool {
	if len(list) != want {
		return false
	}
	for _, rt := range list {
		if rt.Catalog == nil || rt.Catalog.ConfirmedAt == nil {
			return false
		}
	}
	return true
}
