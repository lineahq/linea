package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Kubernetes example (examples/kubernetes/base) deploys connecting
// workers: the manifests are parsed, never applied.

func exampleDocuments(t *testing.T, name string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "kubernetes", "base", name))
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []map[string]any
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			return docs
		} else if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		docs = append(docs, doc)
	}
}

func at(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("no %s in %v", key, v)
		}
		v = m[key]
	}
	return v
}

func TestTheKubernetesExampleDeploysAConnectingWorker(t *testing.T) {
	var deployment map[string]any
	for _, doc := range exampleDocuments(t, "worker.yaml") {
		switch doc["kind"] {
		case "Deployment":
			deployment = doc
		case "Service":
			t.Error("a connecting worker listens on no port, so it needs no Service")
		}
	}
	if deployment == nil {
		t.Fatal("worker.yaml has no Deployment")
	}
	containers, _ := at(t, deployment, "spec", "template", "spec", "containers").([]any)
	if len(containers) != 1 {
		t.Fatalf("containers = %v", containers)
	}
	container := containers[0]
	if ports := at(t, container, "ports"); ports != nil {
		t.Errorf("the worker declares ports %v", ports)
	}
	var args []string
	for _, a := range at(t, container, "args").([]any) {
		args = append(args, a.(string))
	}
	if !hasFlag(args, "--connect=") || !hasFlag(args, "--name=") {
		t.Errorf("args = %v, want --connect and --name", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--listen") || strings.HasPrefix(a, "--tls-") {
			t.Errorf("args = %v: the worker no longer listens", args)
		}
	}
	// The flags are the ones the worker accepts: a token is the only thing
	// missing, and the example supplies it from the Secret.
	t.Setenv(tokenEnv, "")
	if err := run(t.Context(), append(args, "--ca-file", "missing.pem")); err == nil || !strings.Contains(err.Error(), tokenEnv) {
		t.Errorf("run %v = %v, want a missing token", args, err)
	}
	env := at(t, container, "env").([]any)
	if len(env) != 1 || at(t, env[0], "name") != tokenEnv || at(t, env[0], "valueFrom", "secretKeyRef", "key") != "token" {
		t.Errorf("env = %v, want %s from the Secret's token key", env, tokenEnv)
	}
	sc := at(t, container, "securityContext")
	if at(t, sc, "readOnlyRootFilesystem") != true || at(t, sc, "allowPrivilegeEscalation") != false {
		t.Errorf("securityContext = %v", sc)
	}
	if at(t, deployment, "spec", "template", "spec", "securityContext", "runAsNonRoot") != true {
		t.Error("the worker may run as root")
	}
}

func hasFlag(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func TestTheKubernetesExampleAdmitsNoIngress(t *testing.T) {
	docs := exampleDocuments(t, "networkpolicy.yaml")
	if len(docs) != 1 {
		t.Fatalf("networkpolicy.yaml holds %d documents", len(docs))
	}
	spec := at(t, docs[0], "spec")
	if ingress, ok := at(t, spec, "ingress").([]any); !ok || len(ingress) != 0 {
		t.Errorf("ingress = %v, want none", at(t, spec, "ingress"))
	}
	egress, _ := at(t, spec, "egress").([]any)
	if len(egress) != 2 {
		t.Errorf("egress = %v, want the server and DNS", egress)
	}
}
