package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// helloRuntimes is the Docker store of the machine that registers: reading
// an image's hello yields this process's worker identity, as a worker built
// from the same source reports it when it connects.
type helloRuntimes struct {
	engineRuntimes
	t     *testing.T
	image string
}

func (r helloRuntimes) ReadHello(_ context.Context, ref string, pull bool) (string, []byte, error) {
	if ref != r.image || pull {
		r.t.Errorf("ReadHello(%s, pull %t)", ref, pull)
	}
	h, err := worker.Identity()
	if err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(h)
	return "sha256:" + strings.Repeat("d", 64), raw, err
}

// TestARegisteredRuntimeRunsOnTheWorkerWhoseHelloConfirmsIt covers registration:
// linea runtime register reads an image's hello where Docker runs and records
// it in a team server's catalog; a run on it queues until a worker of that
// identity connects, whose hello confirms the entry and which then executes the
// run. The team server runs no container.
func TestARegisteredRuntimeRunsOnTheWorkerWhoseHelloConfirmsIt(t *testing.T) {
	const image = "registry.example/linea-runtime:edge"
	saved := dockerRuntimes
	dockerRuntimes = func() platform.RuntimeStore { return helloRuntimes{t: t, image: image} }
	t.Cleanup(func() { dockerRuntimes = saved })

	dir := t.TempDir()
	writeTeamSecrets(t, dir)
	token := strings.Repeat("w", minWorkerToken)
	if err := os.WriteFile(filepath.Join(dir, "registration"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	team := strings.NewReplacer("role: viewer", "role: admin").Replace(teamYAML())
	server := startTeam(t, writeTeamIn(t, dir, "team", team+"worker_token_file: registration\n"))
	resp := teamRequests(t, server)(http.MethodPost, "/api/v1/tokens", api.TokenRequest{Label: "laptop"},
		map[string]string{"X-Forwarded-User": "alice", "X-Forwarded-Groups": "platform"})
	var created api.CreatedToken
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token: %d, %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	t.Setenv(serverTokenEnv, created.Secret)

	code, out, stderr := runCLI(t, "runtime", "register", image, "--name", "edge", "--server", server)
	if code != exitOK || !strings.Contains(out, "registered runtime edge") || !strings.Contains(out, "no worker of it is connected") {
		t.Fatalf("register: exit %d, %s%s", code, out, stderr)
	}
	// The run below names no runtime: it runs on the team's default.
	if code, out, stderr := runCLI(t, "runtime", "default", "edge", "--server", server); code != exitOK {
		t.Fatalf("runtime default: exit %d, %s%s", code, out, stderr)
	}
	edge := func() api.Runtime {
		t.Helper()
		code, out, stderr := runCLI(t, "runtime", "list", "--format", "json", "--server", server)
		var list []api.Runtime
		if code != exitOK || json.Unmarshal([]byte(out), &list) != nil {
			t.Fatalf("runtime list: exit %d, %s%s", code, out, stderr)
		}
		// A connected worker of this release also lists the built-in
		// runtime.
		for _, rt := range list {
			if rt.Name == "edge" && rt.Catalog != nil {
				return rt
			}
		}
		t.Fatalf("runtime list has no edge: %s", out)
		return api.Runtime{}
	}
	if rt := edge(); rt.Name != "edge" || rt.Catalog.Image != image || rt.Catalog.Digest != "sha256:"+strings.Repeat("d", 64) ||
		rt.Catalog.ConfirmedAt != nil || rt.Catalog.Workers != 0 || rt.Catalog.RegisteredBy == "" {
		t.Fatalf("registered runtime = %+v, %+v", rt, rt.Catalog)
	}

	samples := filepath.Join("..", "..", "samples", "logs-cleanup")
	ran := make(chan string, 1)
	go func() {
		code, out, stderr := runCLI(t, "run", "--config", filepath.Join(samples, "config.yaml"), "--scenario", filepath.Join(samples, "scenario.yaml"), "--server", server)
		if code != exitOK {
			out = fmt.Sprintf("exit %d: %s%s", code, out, stderr)
		}
		ran <- out
	}()
	select {
	case out := <-ran:
		t.Fatalf("the run ended with no worker connected: %s", out)
	case <-time.After(500 * time.Millisecond):
	}

	ctx, cancel := context.WithCancel(context.Background())
	connected := make(chan error, 1)
	go func() {
		connected <- worker.Connect(ctx, worker.ConnectOptions{URL: server, Token: token, Name: "edge-worker", Capacity: 1})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-connected; err != nil {
			t.Errorf("Connect = %v", err)
		}
	})
	select {
	case out := <-ran:
		if strings.HasPrefix(out, "exit ") {
			t.Fatalf("run on the connected worker: %s", out)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the run did not reach the connected worker")
	}
	if rt := edge(); rt.Catalog.ConfirmedAt == nil || rt.Catalog.Workers != 1 || rt.Catalog.Slots != 1 {
		t.Fatalf("after the worker's hello = %+v", rt.Catalog)
	}
	code, out, stderr = runCLI(t, "runs", "--runtime", "edge", "--format", "json", "--server", server)
	var page struct {
		Runs []json.RawMessage `json:"runs"`
	}
	if code != exitOK || json.Unmarshal([]byte(out), &page) != nil || len(page.Runs) != 1 {
		t.Fatalf("runs on edge: exit %d, %s%s", code, out, stderr)
	}
}
