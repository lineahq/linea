package builtin

import (
	"errors"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestReceiverLogsNameDeploymentPaths: what the receiver logs names the
// scenario's paths, not the sandbox, whose directory differs on every run,
// in the message, string fields, fields added with With and errors.
func TestReceiverLogsNameDeploymentPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "linea-files-1")
	p := &filelog{root: root}
	file := "/var/log/pods/a/0.log"
	p.hostPaths = map[string]string{filepath.ToSlash(p.hostPath(file)): file}

	core, logs := observer.New(zap.InfoLevel)
	log := zap.New(restoreCore{Core: core, restore: p.restorePaths}).With(zap.String("dir", p.hostPath("/var/log/pods")))
	log.Debug("not enabled " + p.hostPath(file))
	log.Info("Started watching file", zap.String("path", p.hostPath(file)), zap.Int("n", 1))
	log.Error("read "+p.hostPath(`C:\app\b.log`)+": denied", zap.Error(errors.New("open "+p.hostPath(file)+": denied")))

	entries := logs.All()
	if len(entries) != 2 {
		t.Fatalf("%d entries, want 2", len(entries))
	}
	first, second := entries[0].ContextMap(), entries[1]
	if first["path"] != file || first["dir"] != "/var/log/pods" || first["n"] != int64(1) {
		t.Errorf("fields %v", first)
	}
	if second.Message != "read c:/app/b.log: denied" || second.ContextMap()["error"] != "open "+file+": denied" {
		t.Errorf("entry %q %v", second.Message, second.ContextMap())
	}
	if got := p.restorePaths("no sandbox here"); got != "no sandbox here" {
		t.Errorf("unrelated text %q", got)
	}
}
