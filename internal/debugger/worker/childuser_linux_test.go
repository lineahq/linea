package worker

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// probeEnv makes the test binary a probe. Its value is a comma-separated
// list of attempts: "read:PATH" reads a file, "write:DIR" creates a file in
// a directory, "signal:PID" checks that it may signal a process. It prints
// one line per attempt, the attempt and "ok" or "denied".
const probeEnv = "LINEA_TEST_CHILD_PROBE"

func init() {
	attempts, ok := os.LookupEnv(probeEnv)
	if !ok {
		return
	}
	for _, a := range strings.Split(attempts, ",") {
		kind, arg, _ := strings.Cut(a, ":")
		var err error
		switch kind {
		case "read":
			_, err = os.ReadFile(arg)
		case "write":
			err = os.WriteFile(filepath.Join(arg, "planted"), []byte("x"), 0o755)
		case "signal":
			var pid int
			if pid, err = strconv.Atoi(arg); err == nil {
				err = syscall.Kill(pid, 0)
			}
		default:
			err = errors.New("unknown attempt")
		}
		if err != nil {
			println(a, "denied")
		} else {
			println(a, "ok")
		}
	}
	os.Exit(0)
}

// TestAChildUserCannotReadTheParentsSecrets: a child started as the child
// user cannot read a 0400 token file or the parent's environment and
// memory, plant a file in a root-owned 0711 directory like the pool's
// cache, or signal the parent. It needs root with only CAP_SETUID and
// CAP_SETGID, as a pool container has;
// TestAPoolContainersChildCannotReachItsSecrets runs it so.
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
	pid := strconv.Itoa(os.Getpid())
	attempts := []string{"read:" + token, "read:/proc/" + pid + "/environ", "read:/proc/" + pid + "/mem",
		"write:" + dir, "signal:" + pid, "read:" + open}
	cmd := exec.Command(probe)
	cmd.Env = []string{probeEnv + "=" + strings.Join(attempts, ",")}
	if err := u.Apply(cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v: %s", err, out)
	}
	var want strings.Builder
	for _, a := range attempts {
		result := " denied\n"
		if a == "read:"+open {
			result = " ok\n"
		}
		want.WriteString(a + result)
	}
	if string(out) != want.String() {
		t.Fatalf("probe printed\n%s\nwant\n%s", out, want.String())
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
