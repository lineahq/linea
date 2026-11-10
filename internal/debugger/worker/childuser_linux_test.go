package worker

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// probeEnv makes the test binary a probe: it tries to read each path in the
// variable's value and prints one line per path, "ok" or "denied".
const probeEnv = "LINEA_TEST_CHILD_PROBE"

func init() {
	paths, ok := os.LookupEnv(probeEnv)
	if !ok {
		return
	}
	for _, p := range strings.Split(paths, ",") {
		if _, err := os.ReadFile(p); err != nil {
			println(p, "denied")
		} else {
			println(p, "ok")
		}
	}
	os.Exit(0)
}

// TestAChildUserCannotReadTheParentsSecrets runs as root with only
// CAP_SETUID and CAP_SETGID, as a pool container does:
//
//	docker run --rm --cap-drop ALL --cap-add SETUID --cap-add SETGID \
//	  -v "$PWD:/src" -w /src golang:1.27.3 \
//	  go test -run TestAChildUserCannotReadTheParentsSecrets ./internal/debugger/worker
func TestAChildUserCannotReadTheParentsSecrets(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root with CAP_SETUID and CAP_SETGID; run it in a container")
	}
	if err := protectProcess(); err != nil {
		t.Fatal(err)
	}
	u := ChildUser{UID: 65534, GID: 65534}
	// Not t.TempDir: its parent is not traversable by the child user.
	dir, err := os.MkdirTemp("", "childuser-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("secret"), 0o400); err != nil {
		t.Fatal(err)
	}
	if why, err := TokenFileExposed(token, u); err != nil || why != "" {
		t.Fatalf("a 0400 root file is reported exposed: %q, %v", why, err)
	}
	open := filepath.Join(dir, "open")
	if err := os.WriteFile(open, []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	if why, _ := TokenFileExposed(open, u); why == "" {
		t.Fatal("a 0644 file is not reported exposed")
	}
	// The child runs a copy of this binary the child user may execute.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe")
	copyFile(t, self, probe)
	environ := "/proc/" + strconv.Itoa(os.Getpid()) + "/environ"
	cmd := exec.Command(probe)
	cmd.Env = []string{probeEnv + "=" + strings.Join([]string{token, environ, open}, ",")}
	if err := u.Apply(cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v: %s", err, out)
	}
	want := token + " denied\n" + environ + " denied\n" + open + " ok\n"
	if string(out) != want {
		t.Fatalf("probe printed\n%s\nwant\n%s", out, want)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
